// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package nmhost

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/bridge"
)

// End to end with the real host program: the test builds
// cmd/lectureflow-nmhost and runs it the way a browser does (frames on
// stdin / stdout), against an app endpoint served by the test, or with a
// fake app that the host has to start (this test binary itself, see
// TestMain). Nothing here touches the real registry, the real endpoint or
// a browser directory: every run has its own endpoint and LECTUREFLOW_NM_DIR.

const fakeAppEnv = "LECTUREFLOW_NMHOST_TEST_FAKEAPP"

func TestMain(m *testing.M) {
	if os.Getenv(fakeAppEnv) == "1" {
		os.Exit(runFakeApp())
	}
	if code, ok := runTestChild(); ok { // pipe_windows_test.go
		os.Exit(code)
	}
	code := m.Run()
	if hostBuildDir != "" {
		os.RemoveAll(hostBuildDir)
	}
	os.Exit(code)
}

// runFakeApp is "LectureFlow" as started by the host: it records its
// arguments, serves the endpoint until its first connection ends, and
// gives up after 30 seconds.
func runFakeApp() int {
	dir := os.Getenv(EnvDir)
	os.WriteFile(filepath.Join(dir, "fakeapp-args.txt"), []byte(strings.Join(os.Args[1:], " ")), 0o644)
	ln, err := Listen(os.Getenv(EnvEndpoint))
	if err != nil {
		return 3
	}
	defer ln.Close()
	h := newFakeHandler()
	h.version = "fake-app"
	s := NewServer(ServerOptions{Handler: h})
	done := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err == nil {
			s.ServeConn(c, true)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
	}
	return 0
}

var (
	hostBuildOnce sync.Once
	hostBuildDir  string
	hostBin       string
	hostBuildErr  error
)

// buildHost builds the host program once per test run.
func buildHost(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the host program")
	}
	hostBuildOnce.Do(func() {
		hostBuildDir, hostBuildErr = os.MkdirTemp("", "lf-nmhost-build-")
		if hostBuildErr != nil {
			return
		}
		hostBin = filepath.Join(hostBuildDir, HostExe())
		cmd := exec.Command("go", "build", "-o", hostBin, "lectureflow/cmd/lectureflow-nmhost")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			hostBuildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if hostBuildErr != nil {
		t.Fatal(hostBuildErr)
	}
	return hostBin
}

// testEndpointFor is a fresh endpoint of the platform's kind.
func testEndpointFor(t *testing.T) string {
	if runtime.GOOS == "windows" {
		return uniquePipe(t)
	}
	return uniqueSocket(t)
}

type hostProc struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *logBuffer
	exited chan struct{} // closed when the process has ended
}

func runHostBinary(t *testing.T, endpoint, dir string, extraEnv ...string) *hostProc {
	t.Helper()
	bin := buildHost(t)
	// The browser's arguments: the caller's origin and a window handle.
	cmd := exec.Command(bin, "chrome-extension://abcdefghijklmnopabcdefghijklmnop/", "--parent-window=0")
	cmd.Env = append(os.Environ(), EnvEndpoint+"="+endpoint, EnvDir+"="+dir)
	cmd.Env = append(cmd.Env, extraEnv...)
	p := &hostProc{t: t, cmd: cmd, stderr: &logBuffer{}, exited: make(chan struct{})}
	var err error
	if p.stdin, err = cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if p.stdout, err = cmd.StdoutPipe(); err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		cmd.Wait()
		close(p.exited)
	}()
	t.Cleanup(func() {
		p.stdin.Close()
		select {
		case <-p.exited:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
		}
	})
	return p
}

func (p *hostProc) send(v any) {
	p.t.Helper()
	b, _ := json.Marshal(v)
	if err := bridge.WriteFrame(p.stdin, b, bridge.MaxRequest); err != nil {
		p.t.Fatal(err)
	}
}

func (p *hostProc) recv(d time.Duration) map[string]any {
	p.t.Helper()
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		b, err := bridge.ReadFrame(p.stdout, bridge.MaxToBrowser)
		ch <- res{b, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			p.t.Fatalf("read from host: %v; stderr: %s", r.err, p.stderr.String())
		}
		var m map[string]any
		if err := json.Unmarshal(r.b, &m); err != nil {
			p.t.Fatal(err)
		}
		return m
	case <-time.After(d):
		p.t.Fatalf("no frame from the host; stderr: %s", p.stderr.String())
	}
	return nil
}

func (p *hostProc) waitExit(d time.Duration) {
	p.t.Helper()
	select {
	case <-p.exited:
	case <-time.After(d):
		p.t.Fatal("the host did not exit")
	}
}

func TestHostBinaryRelays(t *testing.T) {
	ep := testEndpointFor(t)
	dir := t.TempDir()
	fh := newFakeHandler()
	log, logs := newTestLogger()
	serveFake(t, ep, fh, log)

	p := runHostBinary(t, ep, dir)
	p.send(hello("1"))
	if rep := p.recv(10 * time.Second); rep["ok"] != true || rep["id"] != "1" || rep["app"].(map[string]any)["version"] != "9.9.9" {
		t.Fatalf("hello: %v", rep)
	}
	p.send(enqueueReq("2", "ok"))
	if rep := p.recv(5 * time.Second); rep["taskId"] != float64(42) {
		t.Fatalf("enqueue: %v", rep)
	}
	waitFor(t, func() bool { return fh.subscribers() == 1 })
	fh.push(bridge.Status{TaskID: 42, State: bridge.StateMerging})
	if st := p.recv(5 * time.Second); st["state"] != "merging" {
		t.Fatalf("push: %v", st)
	}
	// Frames of every size pass unchanged, in order.
	big := enqueueReq("3", "ok")
	big.Lecture.Title = strings.Repeat("长", 200000) // 600 kB of UTF-8
	p.send(big)
	if rep := p.recv(5 * time.Second); rep["id"] != "3" {
		t.Fatalf("big: %v", rep)
	}
	fh.mu.Lock()
	gotTitle := fh.enqueued[len(fh.enqueued)-1].Lecture.Title
	fh.mu.Unlock()
	if gotTitle != big.Lecture.Title {
		t.Fatal("the large frame changed on the way")
	}

	// The browser closes the port: the host exits at once.
	p.stdin.Close()
	p.waitExit(5 * time.Second)
	waitFor(t, func() bool { return fh.subscribers() == 0 })

	// Credentials: nothing on stderr, in the server's log or on disk.
	if p.stderr.String() != "" {
		t.Errorf("stderr while relaying: %q", p.stderr.String())
	}
	assertNoSecrets(t, "server log", logs.String())
	scanDirForSecrets(t, dir)
}

func TestHostBinaryNotInstalled(t *testing.T) {
	p := runHostBinary(t, testEndpointFor(t), t.TempDir())
	p.send(hello("h"))
	rep := p.recv(10 * time.Second)
	if errCode(rep) != bridge.CodeAppNotRunning || errParams(rep)["reason"] != ReasonNotInstalled || rep["id"] != "h" {
		t.Fatalf("reply: %v", rep)
	}
	p.waitExit(10 * time.Second)
	if !strings.Contains(p.stderr.String(), "not running") {
		t.Fatalf("stderr: %q", p.stderr.String())
	}
}

func TestHostBinaryStartsApp(t *testing.T) {
	ep := testEndpointFor(t)
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeLaunch(t, dir, LaunchConfig{Exe: self, Args: []string{BackgroundArg}})
	// The started program inherits the host's environment: this test
	// binary becomes the fake app.
	p := runHostBinary(t, ep, dir, fakeAppEnv+"=1")
	p.send(hello("first"))
	rep := p.recv(25 * time.Second)
	if rep["ok"] != true || rep["id"] != "first" || rep["app"].(map[string]any)["version"] != "fake-app" {
		t.Fatalf("hello: %v; stderr %s", rep, p.stderr.String())
	}
	args, _ := os.ReadFile(filepath.Join(dir, "fakeapp-args.txt"))
	if strings.TrimSpace(string(args)) != "--background" {
		t.Fatalf("the app was started with %q", args)
	}
	p.stdin.Close()
	p.waitExit(5 * time.Second)
	// The fake app ends with its connection; the endpoint is free again.
	waitFor(t, func() bool {
		c, err := Dial(t.Context(), ep)
		if err == nil {
			c.Close()
		}
		return err != nil
	})
}

// scanDirForSecrets walks dir like the desktop e2e token scan: no file may
// hold a test cookie or signature, in UTF-8 or UTF-16LE.
func scanDirForSecrets(t *testing.T, dir string) {
	t.Helper()
	filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(path)
		for _, s := range []string{secretCookie, "SECRETSIGNATURE"} {
			if bytes.Contains(b, []byte(s)) || bytes.Contains(b, utf16le(s)) {
				t.Errorf("%s holds %q", path, s)
			}
		}
		return nil
	})
}

func utf16le(s string) []byte {
	out := make([]byte, 0, 2*len(s))
	for _, r := range s {
		out = append(out, byte(r), byte(r>>8))
	}
	return out
}

var _ net.Conn

// The host program with a noLaunch hello and an installed app that is not
// running: the app is not started, the hello is answered not_running.
func TestHostBinaryNoLaunch(t *testing.T) {
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeLaunch(t, dir, LaunchConfig{Exe: self, Args: []string{BackgroundArg}})
	p := runHostBinary(t, testEndpointFor(t), dir, fakeAppEnv+"=1")
	h := hello("passive")
	h.NoLaunch = true
	p.send(h)
	rep := p.recv(10 * time.Second)
	if errCode(rep) != bridge.CodeAppNotRunning || errParams(rep)["reason"] != ReasonNotRunning || rep["id"] != "passive" {
		t.Fatalf("reply: %v; stderr %s", rep, p.stderr.String())
	}
	p.waitExit(10 * time.Second)
	if _, err := os.Stat(filepath.Join(dir, "fakeapp-args.txt")); !os.IsNotExist(err) {
		t.Fatalf("the app was started: %v", err)
	}
}
