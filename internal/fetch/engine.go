// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/procutil"
)

// The aria2c process (ext-brief §6.2): started when a task needs it, in RPC
// mode only, listening on a random port of the loopback interface with a
// random secret (--rpc-listen-all=false binds both 127.0.0.1 and [::1];
// the client uses 127.0.0.1; --disable-ipv6 is not set, it would also
// keep IPv6 download sources away); stopped after IdleTimeout without
// users. The secret reaches aria2c in a temporary --conf-path file (0600)
// that is deleted as soon as the RPC answers; the file also keeps aria2c
// from reading the user's own aria2 configuration. Nothing secret is on
// its command line or in its environment, it writes no log and no session
// file, and --stop-with-process ends it with this process. The secret goes
// to 127.0.0.1:P only once the system shows aria2c listening there
// (listener.go, security review H1).
//
// aria2c reads none of the user's files and opens nothing it was not
// asked for (security review, NOTES): --no-netrc keeps the credentials of
// ~/.netrc from going to a host that matches it; a response that is a
// Metalink or a .torrent is not followed (aria2c would fetch the hosts a
// Metalink lists, with the task's Cookie and Referer, or join BitTorrent
// and listen on 6881-6999), and DHT, local peer discovery and peer
// exchange stay off. --load-cookies, --netrc-path, --input-file,
// --save-session, --log and --dht-file-path are not set; the CA
// certificates are the system's (--ca-certificate is not set).
//
// Those BitTorrent and Metalink options exist only in an aria2c built with
// them: the official Windows build has both, the macOS build of
// packaging/aria2c neither (Enabled Features: GZip, HTTPS, Message Digest),
// and it refuses an option it does not know ("unrecognized option", exit
// 28: every download failed, lead 2026-10-27). So "aria2c --version" is
// read once per engine (ariaFeatures) and each group is passed only when
// its feature is there; a build without the feature cannot follow a
// torrent or a Metalink anyway, and followedBy stays the last check.

// Process is a started aria2c.
type Process interface {
	// Wait blocks until the process has exited.
	Wait() error
	Kill() error
	// Pid is its process id: the RPC port must be this process's
	// (listener.go).
	Pid() int
}

// Launcher starts aria2c (exe with args) with its stdout and stderr going
// to out. The default runs the program (hidden window on Windows); tests
// start a fake.
type Launcher func(exe string, args []string, out io.Writer) (Process, error)

// maxStarts bounds the starts of one acquire: another one follows only when
// aria2c exited at once or its RPC port was taken (a new random port).
const maxStarts = 5

// maxActive is aria2's --max-concurrent-downloads: the queue keeps the
// real limits itself (tasks × tracks × connections ≤ 6 × 2 × 16).
const maxActive = 6 * 2 * 16

// errEngineMissing: the aria2c executable does not exist.
var errEngineMissing = errors.New("aria2c not found")

type engine struct {
	exe    string
	launch Launcher
	idle   time.Duration
	hc     *http.Client
	log    *slog.Logger
	// startTimeout bounds the wait for the first RPC answer.
	startTimeout time.Duration
	// pickPort chooses the RPC port (default freePort; tests).
	pickPort func() (int, error)
	// versionOf runs "exe --version" (default runVersion; tests).
	versionOf func(exe string) (string, error)
	featOnce  sync.Once
	feats     ariaFeatures

	mu     sync.Mutex
	inst   *instance
	users  int
	timer  *time.Timer
	closed bool
}

type instance struct {
	rpc    *rpcClient
	proc   Process
	exited chan struct{}
	tr     *http.Transport // the RPC client's connections (closed once it stopped)
}

func (in *instance) alive() bool {
	select {
	case <-in.exited:
		return false
	default:
		return true
	}
}

// acquire returns the running aria2c, starting it when needed. Every
// successful acquire must be paired with release.
func (e *engine) acquire(ctx context.Context) (*instance, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, ErrClosed
	}
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	if e.inst != nil && !e.inst.alive() {
		e.inst = nil
	}
	if e.inst == nil {
		var err error
		for attempt := 0; attempt < maxStarts; attempt++ {
			// another attempt only for a lost port race (aria2c exited) or
			// a port another program holds
			if e.inst, err = e.start(ctx); err == nil || !(errors.Is(err, errExitedEarly) || errors.Is(err, errPortTaken)) {
				break
			}
		}
		if err != nil {
			e.inst = nil
			if e.users == 0 {
				e.armIdleLocked()
			}
			return nil, err
		}
	}
	e.users++
	return e.inst, nil
}

// release ends one use; the last one arms the idle timer.
func (e *engine) release() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.users > 0 {
		e.users--
	}
	if e.users == 0 {
		e.armIdleLocked()
	}
}

func (e *engine) armIdleLocked() {
	if e.closed || e.inst == nil {
		return
	}
	if e.timer != nil {
		e.timer.Stop()
	}
	e.timer = time.AfterFunc(e.idle, e.idleStop)
}

func (e *engine) idleStop() {
	e.mu.Lock()
	if e.users > 0 || e.inst == nil || e.closed {
		e.mu.Unlock()
		return
	}
	in := e.inst
	e.inst, e.timer = nil, nil
	e.mu.Unlock()
	e.stop(in, false)
	e.log.Info("downloads: aria2c stopped (idle)")
}

// running reports whether an aria2c process is up (tests, diagnostics).
func (e *engine) running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.inst != nil && e.inst.alive()
}

// close stops aria2c for good.
func (e *engine) close() {
	e.mu.Lock()
	e.closed = true
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	in := e.inst
	e.inst = nil
	e.mu.Unlock()
	if in != nil {
		e.stop(in, true)
	}
}

// Waits for aria2c to exit after its shutdown call before it is killed:
// idle (nobody waits) and at the application's close (forced). aria2c
// 1.37.0 takes 4.3–4.5 s to exit after answering forceShutdown (M1
// acceptance), and the whole graceful stop of the application has 10 s
// (SPEC-v3 V1.1). By then every download was removed (stopGIDs), so its
// control files are written and killing it loses nothing; were the kill to
// fail, --stop-with-process ends it with this process.
var (
	idleExitWait  = 5 * time.Second
	forceExitWait = time.Second
)

// stop asks aria2c to shut down and kills it when it does not exit in time
// (force: the application closes; forceShutdown and a short wait).
func (e *engine) stop(in *instance, force bool) {
	rpcWait, exitWait := 2*time.Second, idleExitWait
	if force {
		rpcWait, exitWait = 500*time.Millisecond, forceExitWait
	}
	if in.tr != nil {
		defer in.tr.CloseIdleConnections()
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcWait)
	_ = in.rpc.shutdown(ctx, force)
	cancel()
	select {
	case <-in.exited:
		return
	case <-time.After(exitWait):
	}
	_ = in.proc.Kill()
	select {
	case <-in.exited:
	case <-time.After(5 * time.Second):
		e.log.Warn("downloads: aria2c did not exit after kill")
	}
}

var errExitedEarly = errors.New("aria2c exited before answering")

// start launches aria2c, waits until it listens on 127.0.0.1:port
// (awaitListener: nothing is sent there before), then for its first RPC
// answer.
func (e *engine) start(ctx context.Context) (*instance, error) {
	exe := e.exe
	if exe == "" {
		exe = "aria2c"
	}
	if !strings.ContainsAny(exe, `/\`) {
		p, err := exec.LookPath(exe)
		if err != nil {
			return nil, errEngineMissing
		}
		exe = p
	} else if _, err := os.Stat(exe); err != nil {
		return nil, errEngineMissing
	}
	secret, err := randomHex(24)
	if err != nil {
		return nil, err
	}
	pick := e.pickPort
	if pick == nil {
		pick = freePort
	}
	port, err := pick()
	if err != nil {
		return nil, fmt.Errorf("aria2c port: %w", err)
	}
	feats := e.features(exe)
	conf, err := writeConf(secret)
	if err != nil {
		return nil, fmt.Errorf("aria2c conf file: %w", err)
	}
	removeConf := func() { _ = os.Remove(conf) }
	args := ariaArgs(conf, port, feats)
	watch := newBindWatch()
	proc, err := e.launch(exe, args, watch)
	if err != nil {
		removeConf()
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, exec.ErrNotFound) {
			return nil, errEngineMissing
		}
		return nil, fmt.Errorf("start aria2c: %w", err)
	}
	in := &instance{proc: proc, exited: make(chan struct{})}
	go func() {
		_ = proc.Wait()
		close(in.exited)
	}()
	fail := func(err error) (*instance, error) {
		removeConf()
		_ = proc.Kill()
		if in.tr != nil {
			in.tr.CloseIdleConnections()
		}
		return nil, err
	}
	timeout := e.startTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	deadline := time.Now().Add(timeout)
	if err := e.awaitListener(ctx, in, port, watch, deadline); err != nil {
		if errors.Is(err, errExitedEarly) {
			removeConf()
			return nil, err
		}
		return fail(err)
	}
	in.tr = e.rpcTransport(port, proc.Pid())
	in.rpc = newRPC(port, secret, &http.Client{Timeout: e.hc.Timeout, Transport: in.tr})
	for {
		cctx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := in.rpc.version(cctx)
		cancel()
		if err == nil {
			removeConf() // the secret is in aria2c's memory now
			e.log.Info("downloads: aria2c started", "port", port)
			return in, nil
		}
		var re *rpcError
		if errors.As(err, &re) {
			// it answers but refuses the secret: not our aria2c
			return fail(fmt.Errorf("aria2c rpc refused the secret: %w", err))
		}
		if errors.Is(err, errPortTaken) {
			return fail(err) // (the connection check found another listener)
		}
		select {
		case <-in.exited:
			removeConf()
			return nil, errExitedEarly
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fail(fmt.Errorf("aria2c did not answer within %s", timeout))
		}
	}
}

// ariaArgs is aria2c's command line: the conf file with the secret, the RPC
// port, and the options of the features it was built with.
func ariaArgs(conf string, port int, f ariaFeatures) []string {
	args := []string{
		"--conf-path=" + conf,
		"--enable-rpc=true",
		"--rpc-listen-all=false", // the loopback interface: 127.0.0.1 and [::1]
		"--rpc-listen-port=" + strconv.Itoa(port),
		"--stop-with-process=" + strconv.Itoa(os.Getpid()),
		"--continue=true",
		"--always-resume=false", // a server without ranges: start the file again instead of failing
		"--auto-file-renaming=false",
		"--allow-overwrite=false",
		"--file-allocation=none",
		"--check-certificate=true",
		"--console-log-level=warn", // (a failure to bind the RPC port is an error: bindWatch sees it)
		"--summary-interval=0",
		"--show-console-readout=false",
		"--enable-color=false",
		"--max-concurrent-downloads=" + strconv.Itoa(maxActive),
		"--max-tries=5",
		"--retry-wait=2",
		// nothing of the user's, nothing beyond the URL asked for
		"--no-netrc=true",
	}
	if f.metalink {
		args = append(args, "--follow-metalink=false")
	}
	if f.bitTorrent {
		args = append(args, "--follow-torrent=false", "--enable-dht=false", "--enable-dht6=false",
			"--bt-enable-lpd=false", "--enable-peer-exchange=false")
	}
	return args
}

// ariaFeatures are the optional parts aria2c was built with.
type ariaFeatures struct {
	bitTorrent, metalink bool
}

// parseFeatures reads the "Enabled Features:" line of aria2c --version
// ("Async DNS, BitTorrent, GZip, HTTPS, Message Digest, Metalink, ..."); no
// such line means none.
func parseFeatures(version string) ariaFeatures {
	var f ariaFeatures
	for _, line := range strings.Split(version, "\n") {
		list, ok := strings.CutPrefix(strings.TrimSpace(line), "Enabled Features:")
		if !ok {
			continue
		}
		for _, name := range strings.Split(list, ",") {
			switch strings.TrimSpace(name) {
			case "BitTorrent":
				f.bitTorrent = true
			case "Metalink":
				f.metalink = true
			}
		}
	}
	return f
}

// features is what the aria2c exe was built with, read once per engine;
// unreadable means none (the options of a missing feature would stop it).
func (e *engine) features(exe string) ariaFeatures {
	e.featOnce.Do(func() {
		run := e.versionOf
		if run == nil {
			run = runVersion
		}
		out, err := run(exe)
		if err != nil {
			e.log.Warn("downloads: aria2c --version could not be read; no BitTorrent or Metalink options", "err", err)
		}
		e.feats = parseFeatures(out)
		e.log.Info("downloads: aria2c features", "bittorrent", e.feats.bitTorrent, "metalink", e.feats.metalink)
	})
	return e.feats
}

// runVersion runs "exe --version" (hidden window, at most 10 s).
func runVersion(exe string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "--version")
	cmd.Dir = filepath.Dir(exe)
	hideWindow(cmd)
	out, err := cmd.Output()
	return string(out), err
}

// writeConf writes the --conf-path file: only the RPC secret.
func writeConf(secret string) (string, error) {
	f, err := os.CreateTemp("", "lectureflow-aria2-*.conf")
	if err != nil {
		return "", err
	}
	name := f.Name()
	_ = f.Chmod(0o600)
	_, err = io.WriteString(f, "rpc-secret="+secret+"\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// execLauncher runs the real aria2c. Its output goes to out only (the
// engine's bindWatch, which keeps nothing of it: aria2c's warnings may
// quote a download's full URL).
func execLauncher(exe string, args []string, out io.Writer) (Process, error) {
	if out == nil {
		out = io.Discard
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = filepath.Dir(exe)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Stdin = nil
	hideWindow(cmd)
	// Besides --stop-with-process (aria2c polls for it): on Windows the job
	// object ends aria2c at once when LectureFlow ends, also on a crash
	// (procutil.Tied; test-chaos 1).
	proc, err := procutil.Start(cmd, procutil.Tied)
	if err != nil {
		return nil, err
	}
	return &execProcess{cmd: cmd, proc: proc}, nil
}

type execProcess struct {
	cmd  *exec.Cmd
	proc *procutil.Process
}

func (p *execProcess) Wait() error {
	err := p.cmd.Wait()
	p.proc.Close()
	return err
}
func (p *execProcess) Kill() error { return p.cmd.Process.Kill() }
func (p *execProcess) Pid() int    { return p.cmd.Process.Pid }
