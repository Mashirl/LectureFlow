// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package agent is lf-agent, the program LectureFlow runs on a user's SSH
// inference server (ssh-mvp; .claude/research/remote-ssh-plan.md §4, §5;
// the protocol is internal/remote/protocol.go). It starts and ends with
// an SSH session: it never listens on a port, never needs root, never
// runs a shell, takes only the fixed commands below with checked
// arguments, and touches only its own two 0700 directories (dirs.go).
//
//	lf-agent version | probe | serve | ensure | reap | uninstall [--keep-models]
//	lf-agent put  --job ID --kind audio --offset N
//	lf-agent stat --job ID --kind audio
//	lf-agent get  --job ID --kind result
//
// Every command but get answers with JSON event lines on stdout; exit
// code 0 = done, 1 = failed (a "fail" event says why), 2 = bad usage.
// The worker scripts run with the same §6.0 protocol as on the local
// machine; their stderr is never forwarded (only codes are).
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"sync"
	"time"

	"lectureflow/internal/remote"
)

// Version is set at build time (-ldflags "-X lectureflow/internal/agent.Version=X.Y.Z").
var Version = "0.0.0-dev"

func version() string {
	if remote.VersionStringPattern.MatchString(Version) {
		return Version
	}
	return "0.0.0-dev"
}

// Platform is this machine's platform name (remote.Platforms), "" when the
// agent does not support it.
func Platform() string { return platformOf(runtime.GOOS, runtime.GOARCH) }

func platformOf(goos, goarch string) string {
	switch goos + "/" + goarch {
	case "linux/amd64":
		return remote.PlatformLinuxX64
	case "darwin/arm64":
		return remote.PlatformDarwinArm64
	case "windows/amd64":
		return remote.PlatformWindowsX64
	}
	return ""
}

// env is one invocation.
type env struct {
	dirs   Dirs
	stdin  io.Reader
	out    *eventWriter
	stderr io.Writer
	now    func() time.Time
}

// Main runs lf-agent with args (without the program name) and returns the
// exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	setUmask()
	e := &env{stdin: stdin, out: &eventWriter{w: stdout}, stderr: stderr, now: time.Now}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: lf-agent version|probe|serve|ensure|reap|uninstall|put|stat|get")
		return 2
	}
	cmd, rest := args[0], args[1:]
	if Platform() == "" {
		e.fail("", remote.AgentUnsupported)
		return 1
	}
	if cmd == "version" {
		if len(rest) != 0 {
			return usage(stderr, cmd)
		}
		return e.version()
	}
	dirs, err := DefaultDirs()
	if err != nil {
		e.fail("", remote.AgentInternal)
		return 1
	}
	e.dirs = dirs
	if err := ensurePrivateDir(dirs.Root); err != nil {
		e.fail("", codeOf(err))
		return 1
	}
	if err := ensurePrivateDir(dirs.Jobs); err != nil {
		e.fail("", codeOf(err))
		return 1
	}
	switch cmd {
	case "probe", "serve", "ensure", "reap":
		if len(rest) != 0 {
			return usage(stderr, cmd)
		}
	}
	if cmd != "uninstall" {
		e.reap() // every start sweeps the expired job directories (plan §7)
	}
	switch cmd {
	case "probe":
		return e.probe()
	case "serve":
		return e.serve()
	case "ensure":
		return e.ensure()
	case "reap":
		e.out.emit(map[string]any{"type": remote.EvOK})
		return 0
	case "uninstall":
		f, err := parseFlags(rest, map[string]*regexp.Regexp{"keep-models": regexp.MustCompile(`^(yes|no)$`)})
		if err != nil {
			return usage(stderr, cmd)
		}
		return e.uninstall(f["keep-models"] == "yes")
	case "put", "stat", "get":
		f, err := parseFlags(rest, fileFlags[cmd])
		if err != nil || f["job"] == "" || f["kind"] == "" || (cmd == "put" && f["offset"] == "") {
			return usage(stderr, cmd)
		}
		switch cmd {
		case "put":
			return e.put(f)
		case "stat":
			return e.stat(f)
		default:
			return e.get(f)
		}
	}
	return usage(stderr, cmd)
}

var fileFlags = map[string]map[string]*regexp.Regexp{
	"put":  {"job": remote.JobIDPattern, "kind": regexp.MustCompile(`^audio$`), "offset": regexp.MustCompile(`^[0-9]{1,12}$`)},
	"stat": {"job": remote.JobIDPattern, "kind": regexp.MustCompile(`^audio$`)},
	"get":  {"job": remote.JobIDPattern, "kind": regexp.MustCompile(`^result$`)},
}

func usage(w io.Writer, cmd string) int {
	fmt.Fprintf(w, "lf-agent: bad arguments for %q\n", cmd)
	return 2
}

// parseFlags reads "--name value" pairs: only allowed names, each once,
// values matching their pattern.
func parseFlags(args []string, allowed map[string]*regexp.Regexp) (map[string]string, error) {
	out := map[string]string{}
	if len(args)%2 != 0 {
		return nil, errors.New("odd arguments")
	}
	for i := 0; i < len(args); i += 2 {
		name := args[i]
		if len(name) < 3 || name[:2] != "--" {
			return nil, errors.New("not a flag")
		}
		name = name[2:]
		re, ok := allowed[name]
		if !ok {
			return nil, errors.New("unknown flag")
		}
		if _, dup := out[name]; dup {
			return nil, errors.New("duplicate flag")
		}
		if !re.MatchString(args[i+1]) {
			return nil, errors.New("bad value")
		}
		out[name] = args[i+1]
	}
	return out, nil
}

// codeOf maps an internal error to an agent code.
func codeOf(err error) string {
	var ce *codedErr
	switch {
	case errors.As(err, &ce):
		return ce.code
	case errors.Is(err, errUnsafe):
		return remote.AgentUnsafePath
	}
	return remote.AgentInternal
}

// codedErr carries an agent code.
type codedErr struct {
	code   string
	err    error
	freeMB int64
	needMB int64
}

func (e *codedErr) Error() string {
	if e.err != nil {
		return e.code + ": " + e.err.Error()
	}
	return e.code
}

func (e *codedErr) Unwrap() error { return e.err }

func coded(code string, err error) error { return &codedErr{code: code, err: err} }

// eventWriter writes event lines (one at a time, each at most
// remote.MaxEventLine bytes).
type eventWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *eventWriter) emit(ev map[string]any) {
	b, err := remote.Encode(ev)
	if err != nil || len(b) > remote.MaxEventLine {
		b, _ = remote.Encode(map[string]any{"type": remote.EvFail, "code": remote.AgentInternal})
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = w.w.Write(b)
}

// fail emits a fail event (job may be "").
func (e *env) fail(job, code string) { e.failErr(job, coded(code, nil)) }

func (e *env) failErr(job string, err error) {
	ev := map[string]any{"type": remote.EvFail, "code": codeOf(err)}
	if job != "" {
		ev["job"] = job
	}
	var ce *codedErr
	if errors.As(err, &ce) && ce.code == remote.AgentNoSpace {
		ev["free_mb"], ev["need_mb"] = ce.freeMB, ce.needMB
	}
	e.out.emit(ev)
}

func (e *env) version() int {
	ev := map[string]any{"type": remote.EvVersion, "version": version(), "platform": Platform()}
	if sum, err := selfSHA256(); err == nil {
		ev["sha256"] = sum
	}
	e.out.emit(ev)
	return 0
}

// selfSHA256 hashes the running executable.
func selfSHA256() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
