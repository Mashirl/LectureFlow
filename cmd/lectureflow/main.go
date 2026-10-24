// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Command lectureflow: load config, migrate, recover, start scheduler and HTTP (SPEC §1.1).
//
// Command-line contract (SPEC-v3 V1, v3-brief §1): -config, -listen,
// -data-dir, -desktop, -resources, -app-version. Once HTTP is ready it
// prints "LECTUREFLOW_READY http://127.0.0.1:PORT" on stdout; a failed
// start ends with "ERROR: <reason>" as the last line of stderr and exit
// code 1; exit code 3 asks the desktop shell to start the service again.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/config"
	"lectureflow/internal/relocate"
)

// version is set at build time with -ldflags "-X main.version=X.Y.Z";
// -app-version takes precedence.
var version = "dev"

// applyStaged is the startup hook run before the database is opened: it
// switches to the staged data of an "import existing data" (SPEC-v3 V1.3,
// internal/relocate). Tests replace it; nil = nothing to apply.
var applyStaged func(ctx context.Context, dataDir string) error = relocate.ApplyStaged

// runApplyStaged runs applyStaged. A *relocate.ApplyError with Restored
// (the import failed but the data directory is back as it was) is logged
// and the start goes on with the previous database; any other error (the
// directory could not be restored, or something else failed) stops the
// start: the database must not be opened half switched.
func runApplyStaged(ctx context.Context, dataDir string) error {
	if applyStaged == nil {
		return nil
	}
	err := applyStaged(ctx, dataDir)
	if err == nil {
		return nil
	}
	var ae *relocate.ApplyError
	if errors.As(err, &ae) && ae.Restored {
		slog.Error("staged import not applied; starting with the previous data", "err", err)
		return nil
	}
	return fmt.Errorf("staged data: %w", err)
}

// Exit codes (v3-brief §1).
const (
	exitOK      = 0
	exitFailure = 1
	exitRestart = 3 // the desktop shell starts the service again
)

// ReadyPrefix starts the line printed on stdout once HTTP accepts requests.
const ReadyPrefix = "LECTUREFLOW_READY "

// MinTokenLen is the minimum length of the desktop access token (the shell
// sends 32 random bytes as hex, 64 characters).
const MinTokenLen = 32

// Time limits of the start and the graceful stop.
var (
	tokenReadTimeout = 30 * time.Second
	shutdownLimit    = 10 * time.Second // SPEC-v3 V1.1: the whole graceful stop
)

// webModeNotice is printed in web mode (SPEC-v3 V1.2, v3-brief §1).
const webModeNotice = "网页模式：只监听 127.0.0.1，不需要访问令牌，仅供开发与高级用户使用。 / Web mode: listening on 127.0.0.1 only, no access token required; for development and advanced users only."

func main() {
	os.Exit(runMain(runEnv{
		args:   os.Args[1:],
		stdin:  os.Stdin,
		stdout: os.Stdout,
		stderr: os.Stderr,
		exit:   os.Exit,
	}))
}

// cliOptions are the parsed command-line flags.
type cliOptions struct {
	Config     string
	Listen     string
	DataDir    string
	Desktop    bool
	Resources  string
	AppVersion string
	// LocationFile is {default data dir}/location.json, passed by the
	// desktop shell unless LECTUREFLOW_DATA_DIR chose the data directory
	// (DesktopOptions.LocationFile; step 5 "change the data directory").
	LocationFile string
}

// parseArgs parses the command line. Usage and parse errors go to stderr.
func parseArgs(args []string, stderr io.Writer) (cliOptions, error) {
	var o cliOptions
	fs := flag.NewFlagSet("lectureflow", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.Config, "config", "config.toml", "path to config.toml (a missing file means all defaults)")
	fs.StringVar(&o.Listen, "listen", "", "listen address 127.0.0.1:PORT (overrides the config; port 0 picks a free one)")
	fs.StringVar(&o.DataDir, "data-dir", "", "data directory (overrides the config)")
	fs.BoolVar(&o.Desktop, "desktop", false, "desktop mode: read the access token from the first line of stdin; stdin EOF stops the service")
	fs.StringVar(&o.Resources, "resources", "", "packaged resources directory (worker/, bin/ffmpeg, bin/uv, components.json)")
	fs.StringVar(&o.AppVersion, "app-version", "", "application version (default: the build version)")
	fs.StringVar(&o.LocationFile, "location-file", "", "desktop mode: path of location.json in the default data directory")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if o.AppVersion == "" {
		o.AppVersion = version
	}
	if o.AppVersion == "" {
		o.AppVersion = "dev"
	}
	return o, nil
}

// readToken reads the desktop access token from the first line of r
// (trimmed; at least MinTokenLen characters). It returns the reader to keep
// watching for EOF. A shell that never writes the line fails the start
// after tokenReadTimeout.
func readToken(r io.Reader) (string, *bufio.Reader, error) {
	br := bufio.NewReader(r)
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		line, err := br.ReadString('\n')
		ch <- res{line, err}
	}()
	var got res
	select {
	case got = <-ch:
	case <-time.After(tokenReadTimeout):
		return "", nil, errors.New("desktop mode: no access token on stdin")
	}
	if got.err != nil && !errors.Is(got.err, io.EOF) {
		return "", nil, fmt.Errorf("desktop mode: read access token: %w", got.err)
	}
	tok := strings.TrimSpace(got.line)
	if len(tok) < MinTokenLen {
		return "", nil, fmt.Errorf("desktop mode: access token too short (%d characters, at least %d)", len(tok), MinTokenLen)
	}
	return tok, br, nil
}

// watchEOF calls stop once r reaches EOF (or fails): the desktop shell has
// exited or closed the pipe.
func watchEOF(r io.Reader, stop func()) {
	go func() {
		_, _ = io.Copy(io.Discard, r)
		stop()
	}()
}

// runEnv is the process environment of runMain (tests replace it).
type runEnv struct {
	args           []string
	stdin          io.Reader
	stdout, stderr io.Writer
	// exit ends the process at once (os.Exit): the hard stop when the
	// graceful stop exceeds shutdownLimit.
	exit func(int)
	// signals delivers SIGINT; nil = signal.Notify on os.Interrupt.
	signals <-chan os.Signal
	// appOpts adjusts the application (tests: fake GPU, short intervals).
	appOpts AppOptions
	// started, when set, is called once the ready line is printed (tests:
	// the application and, in desktop mode, its options).
	started func(*App, *api.DesktopOptions)
}

// stopper is the single entry of the graceful stop: SIGINT, stdin EOF,
// DesktopOptions.Shutdown (code 0) and DesktopOptions.Restart (code 3).
// The first request wins; later ones are ignored.
type stopper struct {
	once sync.Once
	ch   chan int
}

func newStopper() *stopper { return &stopper{ch: make(chan int, 1)} }

func (s *stopper) request(code int) {
	s.once.Do(func() { s.ch <- code })
}

// runMain runs the service and returns the process exit code.
func runMain(env runEnv) int {
	fail := func(err error) int {
		slog.Error("fatal", "err", err)
		fmt.Fprintf(env.stderr, "ERROR: %s\n", oneLine(err.Error()))
		return exitFailure
	}
	opts, err := parseArgs(env.args, env.stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		return fail(err)
	}

	stop := newStopper()
	var token string
	if opts.Desktop {
		tok, rest, err := readToken(env.stdin)
		if err != nil {
			return fail(err)
		}
		token = tok
		// The shell keeps stdin open; EOF means it is gone (v3-brief §1).
		watchEOF(rest, func() {
			slog.Info("stdin closed: stopping")
			stop.request(exitOK)
		})
	}

	cfg, err := config.LoadWith(opts.Config, config.Overrides{Listen: opts.Listen, DataDir: opts.DataDir, ResourcesDir: opts.Resources})
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "logs"), 0o755); err != nil {
		return fail(err)
	}
	logFile, err := os.OpenFile(filepath.Join(cfg.DataDir, "logs", "lectureflow.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fail(err)
	}
	defer logFile.Close()
	level := slog.LevelInfo
	if cfg.LogLevel == "debug" {
		level = slog.LevelDebug
	}
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(env.stdout, logFile), &slog.HandlerOptions{Level: level})))
	defer slog.SetDefault(prevLogger)

	sigc := env.signals
	if sigc == nil {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		defer signal.Stop(c)
		sigc = c
	}
	go func() {
		if _, ok := <-sigc; ok {
			slog.Info("interrupt: stopping")
			stop.request(exitOK)
		}
	}()

	// Listen first: the actual port (-listen 127.0.0.1:0) goes into the
	// desktop options (Host check) and the ready line.
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fail(fmt.Errorf("listen %s: %w", cfg.Listen, err))
	}
	port := ln.Addr().(*net.TCPAddr).Port

	appOpts := env.appOpts
	appOpts.Version = opts.AppVersion
	if opts.Desktop {
		appOpts.Desktop = &api.DesktopOptions{
			Token:        token,
			Port:         port,
			ResourcesDir: cfg.ResourcesDir,
			LocationFile: absOrEmpty(opts.LocationFile),
			Shutdown:     func() { stop.request(exitOK) },
			Restart:      func() { stop.request(exitRestart) },
		}
	}

	// The application's context: cancelled only by the graceful stop, after
	// the scheduler (which has its own cancellation) has stopped.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Before the database is opened: switch to the staged data of an
	// "import existing data" (step 5, internal/relocate).
	if err := runApplyStaged(ctx, cfg.DataDir); err != nil {
		ln.Close()
		return fail(err)
	}
	app, err := buildApp(ctx, cfg, appOpts)
	if err != nil {
		ln.Close()
		return fail(err)
	}
	if err := app.Start(ctx); err != nil { // scheduler + first device detection
		ln.Close()
		app.Close()
		return fail(err)
	}

	srv := &http.Server{Handler: app.Server.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	mode := "web"
	if opts.Desktop {
		mode = "desktop"
	}
	slog.Info("lectureflow started", "url", url, "data_dir", cfg.DataDir, "mode", mode, "version", opts.AppVersion)
	if !opts.Desktop {
		fmt.Fprintln(env.stdout, webModeNotice)
	}
	fmt.Fprintln(env.stdout, ReadyPrefix+url)
	if env.started != nil {
		env.started(app, appOpts.Desktop)
	}

	code := exitOK
	select {
	case code = <-stop.ch:
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			cancel()
			app.Close()
			return fail(fmt.Errorf("http: %w", err))
		}
	}

	slog.Info("shutting down", "exit_code", code)
	withLimit(shutdownLimit, code, env.exit, func(deadline time.Time) {
		app.Shutdown(deadline, srv)
		cancel()
	})
	slog.Info("stopped", "exit_code", code)
	return code
}

// withLimit runs the graceful stop f under the hard limit of SPEC-v3 V1.1:
// when f has not returned after limit, exit(code) ends the process at once
// (the interrupted jobs are left to startup recovery, §4.7). f gets a
// deadline a little before the limit for its own bounded waits.
func withLimit(limit time.Duration, code int, exit func(int), f func(deadline time.Time)) {
	watchdog := time.AfterFunc(limit, func() {
		slog.Warn("graceful stop exceeded its time limit: exiting now", "limit", limit)
		exit(code)
	})
	defer watchdog.Stop()
	f(time.Now().Add(limit - limit/20))
}

// absOrEmpty makes a path absolute (empty stays empty).
func absOrEmpty(p string) string {
	if p == "" {
		return ""
	}
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// oneLine keeps the ERROR line a single line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
