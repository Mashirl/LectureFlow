// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package nmhost

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/bridge"
)

// hostRun drives a Host in-process through pipes.
type hostRun struct {
	t    *testing.T
	inW  *io.PipeWriter
	outR *io.PipeReader
	errs *logBuffer
	done chan int
}

func startHost(t *testing.T, h *Host) *hostRun {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	r := &hostRun{t: t, inW: inW, outR: outR, errs: &logBuffer{}, done: make(chan int, 1)}
	h.In, h.Out, h.Err = inR, outW, r.errs
	go func() {
		code := h.Run()
		outW.Close()
		r.done <- code
	}()
	t.Cleanup(func() { inW.Close(); outR.Close() })
	return r
}

func (r *hostRun) send(v any) {
	r.t.Helper()
	b, _ := json.Marshal(v)
	if err := bridge.WriteFrame(r.inW, b, bridge.MaxRequest); err != nil {
		r.t.Fatal(err)
	}
}

func (r *hostRun) recv() map[string]any {
	r.t.Helper()
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		b, err := bridge.ReadFrame(r.outR, bridge.MaxToBrowser)
		ch <- res{b, err}
	}()
	select {
	case x := <-ch:
		if x.err != nil {
			r.t.Fatalf("read from host: %v (stderr %q)", x.err, r.errs.String())
		}
		var m map[string]any
		json.Unmarshal(x.b, &m)
		return m
	case <-time.After(10 * time.Second):
		r.t.Fatal("no frame from the host")
	}
	return nil
}

func (r *hostRun) wait() int {
	r.t.Helper()
	select {
	case c := <-r.done:
		return c
	case <-time.After(10 * time.Second):
		r.t.Fatal("the host did not exit")
	}
	return -1
}

func TestHostRelaysToRunningApp(t *testing.T) {
	ep := uniqueSocket(t)
	fh := newFakeHandler()
	log, logs := newTestLogger()
	serveFake(t, ep, fh, log)
	r := startHost(t, &Host{Endpoint: ep, ConfigDir: t.TempDir()})
	r.send(hello("1"))
	if rep := r.recv(); rep["type"] != "hello" || rep["ok"] != true || rep["id"] != "1" {
		t.Fatalf("hello: %v", rep)
	}
	r.send(enqueueReq("2", "ok"))
	if rep := r.recv(); rep["taskId"] != float64(42) {
		t.Fatalf("enqueue: %v", rep)
	}
	waitFor(t, func() bool { return fh.subscribers() == 1 })
	fh.push(bridge.Status{TaskID: 42, State: bridge.StateDownloading})
	if st := r.recv(); st["type"] != "status" || st["taskId"] != float64(42) {
		t.Fatalf("push: %v", st)
	}
	// The browser closes the port: the host exits and the app sees the
	// connection end.
	r.inW.Close()
	if code := r.wait(); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	waitFor(t, func() bool { return fh.subscribers() == 0 })
	if fh.enqueued[0].Cookies[0].Value != secretCookie {
		t.Fatal("the cookie did not reach the app")
	}
	assertNoSecrets(t, "host stderr", r.errs.String())
	assertNoSecrets(t, "server log", logs.String())
}

func TestHostExitsWhenAppCloses(t *testing.T) {
	ep := uniqueSocket(t)
	log, _ := newTestLogger()
	s := serveFake(t, ep, newFakeHandler(), log)
	r := startHost(t, &Host{Endpoint: ep, ConfigDir: t.TempDir()})
	r.send(hello("1"))
	r.recv()
	s.Close()
	if code := r.wait(); code != 0 {
		t.Fatalf("exit code %d", code)
	}
}

func notRunningReason(t *testing.T, rep map[string]any) string {
	t.Helper()
	if errCode(rep) != bridge.CodeAppNotRunning || rep["ok"] != false {
		t.Fatalf("reply %v, want app.not_running", rep)
	}
	r, _ := errParams(rep)["reason"].(string)
	return r
}

func TestHostNotInstalledWithoutLaunchJSON(t *testing.T) {
	r := startHost(t, &Host{Endpoint: uniqueSocket(t), ConfigDir: t.TempDir(), Linger: 100 * time.Millisecond})
	r.send(hello("h1"))
	r.send(bridge.CheckRequest{Envelope: bridge.Envelope{Type: bridge.TypeCheck, ID: "c1"}})
	rep := r.recv()
	if notRunningReason(t, rep) != ReasonNotInstalled || rep["id"] != "h1" || rep["type"] != "hello" {
		t.Fatalf("first: %v", rep)
	}
	rep = r.recv()
	if notRunningReason(t, rep) != ReasonNotInstalled || rep["id"] != "c1" || rep["type"] != "check" {
		t.Fatalf("second: %v", rep)
	}
	r.wait()
	if r.errs.String() == "" {
		t.Fatal("no reason on stderr")
	}
}

func TestHostEndpointOff(t *testing.T) {
	r := startHost(t, &Host{Endpoint: "", ConfigDir: t.TempDir(), Linger: 50 * time.Millisecond})
	r.send(hello("h"))
	if notRunningReason(t, r.recv()) != ReasonNotInstalled {
		t.Fatal()
	}
	r.wait()
}

func writeLaunch(t *testing.T, dir string, lc LaunchConfig) {
	t.Helper()
	b, _ := json.Marshal(lc)
	if err := os.WriteFile(filepath.Join(dir, LaunchFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHostStartsTheApp(t *testing.T) {
	ep := uniqueSocket(t)
	dir := t.TempDir()
	writeLaunch(t, dir, LaunchConfig{Exe: "LectureFlow.exe", Args: []string{BackgroundArg}})
	fh := newFakeHandler()
	log, _ := newTestLogger()
	var started atomic.Int32
	var got LaunchConfig
	start := func(lc LaunchConfig) error {
		started.Add(1)
		got = lc
		// The app comes up a little later.
		time.AfterFunc(300*time.Millisecond, func() { serveFake(t, ep, fh, log) })
		return nil
	}
	r := startHost(t, &Host{Endpoint: ep, ConfigDir: dir, Start: start, RetryEvery: 50 * time.Millisecond, RetryFor: 5 * time.Second})
	// Sent before the app is up: kept and relayed once connected.
	r.send(hello("early"))
	if rep := r.recv(); rep["id"] != "early" || rep["ok"] != true {
		t.Fatalf("hello: %v", rep)
	}
	if started.Load() != 1 || got.Exe != "LectureFlow.exe" || len(got.Args) != 1 || got.Args[0] != "--background" {
		t.Fatalf("started %d times with %+v", started.Load(), got)
	}
	r.inW.Close()
	r.wait()
}

func TestHostStartFailures(t *testing.T) {
	cases := []struct {
		name   string
		start  func(LaunchConfig) error
		reason string
	}{
		{"start error", func(LaunchConfig) error { return errors.New("access denied") }, ReasonStartFailed},
		{"program gone", func(LaunchConfig) error { return &os.PathError{Op: "stat", Path: "x", Err: os.ErrNotExist} }, ReasonNotInstalled},
		{"never listens", func(LaunchConfig) error { return nil }, ReasonTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeLaunch(t, dir, LaunchConfig{Exe: "x"})
			r := startHost(t, &Host{Endpoint: uniqueSocket(t), ConfigDir: dir, Start: c.start,
				RetryEvery: 20 * time.Millisecond, RetryFor: 200 * time.Millisecond, Linger: 300 * time.Millisecond})
			r.send(hello("h"))
			if got := notRunningReason(t, r.recv()); got != c.reason {
				t.Fatalf("reason %q, want %q", got, c.reason)
			}
			// A request that arrives within the linger is answered too.
			r.send(hello("late"))
			if rep := r.recv(); rep["id"] != "late" {
				t.Fatalf("late: %v", rep)
			}
			r.wait()
		})
	}
}

func TestHostBadLaunchJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, LaunchFile), []byte("{"), 0o644)
	r := startHost(t, &Host{Endpoint: uniqueSocket(t), ConfigDir: dir, Linger: 50 * time.Millisecond})
	r.send(hello("h"))
	if notRunningReason(t, r.recv()) != ReasonStartFailed {
		t.Fatal()
	}
	r.wait()
}

func TestHostBrowserLeavesWhileStarting(t *testing.T) {
	dir := t.TempDir()
	writeLaunch(t, dir, LaunchConfig{Exe: "x"})
	r := startHost(t, &Host{Endpoint: uniqueSocket(t), ConfigDir: dir, Start: func(LaunchConfig) error { return nil },
		RetryEvery: 20 * time.Millisecond, RetryFor: time.Minute})
	r.inW.Close()
	start := time.Now()
	r.wait()
	if time.Since(start) > 5*time.Second {
		t.Fatal("the host waited for the app after the browser left")
	}
}

func TestEnvelopeOfUnreadable(t *testing.T) {
	if e := envelopeOf([]byte("nope")); e.Type != "" || e.ID != "" {
		t.Fatal(e)
	}
	if e := envelopeOf([]byte(`{"type":"check","id":7}`)); e.Type != "check" || e.ID != "" {
		t.Fatal(e)
	}
}

// A hello with noLaunch (the extension checking the row states of a page
// the user merely opened) does not start the app: it and what follows
// within the linger are answered app.not_running {reason: not_running}.
func TestHostNoLaunchHello(t *testing.T) {
	dir := t.TempDir()
	writeLaunch(t, dir, LaunchConfig{Exe: "LectureFlow.exe", Args: []string{BackgroundArg}})
	var started atomic.Int32
	r := startHost(t, &Host{Endpoint: uniqueSocket(t), ConfigDir: dir, Linger: 200 * time.Millisecond,
		Start: func(LaunchConfig) error { started.Add(1); return nil }})
	h := hello("passive")
	h.NoLaunch = true
	r.send(h)
	rep := r.recv()
	if notRunningReason(t, rep) != ReasonNotRunning || rep["id"] != "passive" || rep["type"] != "hello" {
		t.Fatalf("hello: %v", rep)
	}
	r.send(bridge.CheckRequest{Envelope: bridge.Envelope{Type: bridge.TypeCheck, ID: "c"}})
	if rep := r.recv(); notRunningReason(t, rep) != ReasonNotRunning || rep["id"] != "c" {
		t.Fatalf("check: %v", rep)
	}
	if code := r.wait(); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if started.Load() != 0 {
		t.Fatal("the app was started for a noLaunch hello")
	}
	if !strings.Contains(r.errs.String(), "not_running") {
		t.Fatalf("stderr %q", r.errs.String())
	}
}

// noLaunch only counts in a hello; a running app is reached as always, and
// gets the hello (with noLaunch) relayed unchanged.
func TestHostNoLaunchOnlyForHello(t *testing.T) {
	ep := uniqueSocket(t)
	dir := t.TempDir()
	writeLaunch(t, dir, LaunchConfig{Exe: "LectureFlow.exe"})
	fh := newFakeHandler()
	log, _ := newTestLogger()
	var started atomic.Int32
	start := func(LaunchConfig) error {
		started.Add(1)
		time.AfterFunc(100*time.Millisecond, func() { serveFake(t, ep, fh, log) })
		return nil
	}
	r := startHost(t, &Host{Endpoint: ep, ConfigDir: dir, Start: start, RetryEvery: 20 * time.Millisecond, RetryFor: 5 * time.Second})
	// a check with "noLaunch" is not a passive hello: the app is started
	r.send(map[string]any{"type": "check", "id": "c1", "platform": "panopto", "sessionIds": []string{}, "noLaunch": true})
	if rep := r.recv(); rep["id"] != "c1" || rep["ok"] != true {
		t.Fatalf("check: %v", rep)
	}
	if started.Load() != 1 {
		t.Fatalf("started %d times", started.Load())
	}
	r.inW.Close()
	r.wait()
	// the app runs now: a noLaunch hello reaches it
	r2 := startHost(t, &Host{Endpoint: ep, ConfigDir: dir, Start: start})
	h := hello("h2")
	h.NoLaunch = true
	r2.send(h)
	if rep := r2.recv(); rep["id"] != "h2" || rep["ok"] != true {
		t.Fatalf("hello to the running app: %v", rep)
	}
	if started.Load() != 1 {
		t.Fatalf("started %d times", started.Load())
	}
	r2.inW.Close()
	r2.wait()
}
