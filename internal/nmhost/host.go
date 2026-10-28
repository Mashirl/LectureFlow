// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package nmhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"lectureflow/internal/bridge"
)

// Host is the native messaging host (cmd/lectureflow-nmhost): it relays
// frames between the browser (In / Out) and the running app's endpoint,
// byte for byte, until either side closes. When the app is not running it
// starts it as launch.json says and retries; when that fails it answers
// every request it received with app.not_running and exits. A first frame
// that is a hello with noLaunch (a passive check of a page the user merely
// opened) is answered app.not_running {reason: not_running} instead of
// starting the app. It parses only the envelope (type, id) of those
// requests and the first frame's noLaunch, and writes to Err one line
// without any message content.
type Host struct {
	In  io.Reader // the browser's messages (stdin)
	Out io.Writer // to the browser (stdout)
	Err io.Writer // one line on failure (stderr; the browser drops or logs it)

	Endpoint  string // "" = off: never connected
	ConfigDir string // where launch.json is

	// Dial and Start default to Dial and StartApp (tests replace them).
	Dial  func(ctx context.Context, endpoint string) (net.Conn, error)
	Start func(LaunchConfig) error

	RetryEvery time.Duration // default 250 ms
	RetryFor   time.Duration // default 20 s after the start
	// Linger is how long the host still answers new requests after its
	// last app.not_running reply (default 1 s).
	Linger time.Duration
	// FirstFrameWait bounds the wait for the browser's first frame before
	// the app is started (default 5 s; without a frame by then the app is
	// started as before).
	FirstFrameWait time.Duration
}

// Reasons of app.not_running (bridge.CodeAppNotRunning).
const (
	ReasonNotInstalled = "not_installed"
	ReasonStartFailed  = "start_failed"
	ReasonTimeout      = "timeout"
	// ReasonNotRunning: the app is not running and the first message (a
	// hello with noLaunch) asked not to start it.
	ReasonNotRunning = "not_running"
)

// Run relays until either side closes and returns the exit code.
func (h *Host) Run() int {
	if h.Dial == nil {
		h.Dial = Dial
	}
	if h.Start == nil {
		h.Start = StartApp
	}
	if h.RetryEvery <= 0 {
		h.RetryEvery = 250 * time.Millisecond
	}
	if h.RetryFor <= 0 {
		h.RetryFor = 20 * time.Second
	}
	if h.Linger <= 0 {
		h.Linger = time.Second
	}
	if h.FirstFrameWait <= 0 {
		h.FirstFrameWait = 5 * time.Second
	}

	// The browser's frames are read from the start, so a hello sent while
	// the app is starting waits here.
	in := make(chan []byte, 64)
	stdinGone := make(chan struct{})
	go func() {
		defer close(in)
		defer close(stdinGone)
		for {
			f, err := bridge.ReadFrame(h.In, bridge.MaxRequest)
			if err != nil {
				return
			}
			in <- f
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, first, reason, why := h.connect(ctx, in, stdinGone)
	if conn == nil {
		if reason == "" { // the browser went away first
			return 0
		}
		fmt.Fprintf(h.Err, "lectureflow-nmhost: LectureFlow is not running (%s): %s\n", reason, why)
		h.answerNotRunning(first, in, reason)
		return 0
	}
	defer conn.Close()
	return h.relay(conn, first, in)
}

// firstFrame waits for the browser's first frame (at most FirstFrameWait);
// gone is true when the browser closed the port without one.
func (h *Host) firstFrame(ctx context.Context, in <-chan []byte) (f []byte, gone bool) {
	select {
	case f, ok := <-in:
		return f, !ok
	case <-time.After(h.FirstFrameWait):
		return nil, false
	case <-ctx.Done():
		return nil, true
	}
}

// noLaunch reports whether a frame is a hello that asks not to start the
// app (bridge.HelloRequest.NoLaunch).
func noLaunch(f []byte) bool {
	var m struct {
		Type     string `json:"type"`
		NoLaunch bool   `json:"noLaunch"`
	}
	return json.Unmarshal(f, &m) == nil && m.Type == bridge.TypeHello && m.NoLaunch
}

// connect dials the endpoint, starting the app once when nothing listens.
// Before starting it, it reads the browser's first frame (returned as
// first, to be relayed or answered before the frames in in): a hello with
// noLaunch ends here with ReasonNotRunning. Frames that arrive meanwhile
// stay in in. With no connection, reason is the app.not_running reason
// ("" when stdin closed first) and why a short cause for stderr.
func (h *Host) connect(ctx context.Context, in <-chan []byte, stdinGone <-chan struct{}) (conn net.Conn, first []byte, reason, why string) {
	conn, reason, why = h.dialOrStart(ctx, in, stdinGone, &first)
	return conn, first, reason, why
}

func (h *Host) dialOrStart(ctx context.Context, in <-chan []byte, stdinGone <-chan struct{}, first *[]byte) (net.Conn, string, string) {
	if h.Endpoint == "" {
		return nil, ReasonNotInstalled, "the IPC endpoint is off"
	}
	try := func() (net.Conn, error) {
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		return h.Dial(dctx, h.Endpoint)
	}
	c, err := try()
	if err == nil {
		return c, "", ""
	}
	if errors.Is(err, ErrForeign) {
		return nil, ReasonStartFailed, "the IPC endpoint is not served by a trusted process of this user"
	}
	if h.ConfigDir == "" {
		return nil, ReasonNotInstalled, "no configuration directory"
	}
	lc, err := ReadLaunch(h.ConfigDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ReasonNotInstalled, "no launch.json (web mode, or the desktop app has not run yet)"
		}
		return nil, ReasonStartFailed, "launch.json is not usable"
	}
	f, gone := h.firstFrame(ctx, in)
	if gone {
		return nil, "", "" // the browser left; nobody to answer
	}
	*first = f
	if f != nil && noLaunch(f) {
		return nil, ReasonNotRunning, "the extension asked not to start it"
	}
	if err := h.Start(lc); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ReasonNotInstalled, "the program named in launch.json does not exist"
		}
		return nil, ReasonStartFailed, "starting the app failed"
	}
	deadline := time.Now().Add(h.RetryFor)
	tick := time.NewTicker(h.RetryEvery)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
		case <-ctx.Done():
			return nil, "", ""
		case <-stdinGone:
			if len(in) == 0 && *first == nil {
				return nil, "", "" // the browser left; nobody to answer
			}
			stdinGone = nil // answer what it sent once connected or failed
		}
		c, err := try()
		if err == nil {
			return c, "", ""
		}
		if errors.Is(err, ErrForeign) {
			return nil, ReasonStartFailed, "the IPC endpoint is not served by a trusted process of this user"
		}
		if time.Now().After(deadline) {
			return nil, ReasonTimeout, "the app did not answer in time"
		}
	}
}

// relay copies frames both ways until one side ends; first (if any) goes
// to the app before the frames in in.
func (h *Host) relay(conn net.Conn, first []byte, in <-chan []byte) int {
	var once sync.Once
	done := make(chan struct{})
	end := func() { once.Do(func() { close(done) }) }
	go func() {
		defer end()
		if first != nil {
			if err := bridge.WriteFrame(conn, first, bridge.MaxRequest); err != nil {
				return
			}
		}
		for f := range in {
			if err := bridge.WriteFrame(conn, f, bridge.MaxRequest); err != nil {
				return
			}
		}
	}()
	go func() {
		defer end()
		// The app frames its messages already (≤ bridge.MaxToBrowser).
		io.Copy(h.Out, conn)
	}()
	<-done
	return 0
}

// answerNotRunning answers first (if any) and each request in in with
// app.not_running, then keeps answering until the browser closes stdin or
// nothing more came for Linger after the last answer.
func (h *Host) answerNotRunning(first []byte, in <-chan []byte, reason string) {
	answer := func(f []byte) {
		env := envelopeOf(f)
		b, _ := json.Marshal(bridge.ReplyHeader{Type: env.Type, ID: env.ID, OK: false,
			Error: bridge.NewError(bridge.CodeAppNotRunning, map[string]any{"reason": reason})})
		bridge.WriteFrame(h.Out, b, bridge.MaxToBrowser)
	}
	answered := false
	if first != nil {
		answer(first)
		answered = true
	}
	for {
		var linger <-chan time.Time
		if answered {
			linger = time.After(h.Linger)
		}
		select {
		case f, ok := <-in:
			if !ok {
				return
			}
			answer(f)
			answered = true
		case <-linger:
			return
		}
	}
}

// envelopeOf reads type and id only; anything unreadable stays empty.
func envelopeOf(f []byte) bridge.Envelope {
	var m map[string]json.RawMessage
	var env bridge.Envelope
	if json.Unmarshal(f, &m) != nil {
		return env
	}
	json.Unmarshal(m["type"], &env.Type)
	json.Unmarshal(m["id"], &env.ID)
	env.Type, env.ID = clip(env.Type, 40), clip(env.ID, 100)
	return env
}
