// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package nmhost

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"lectureflow/internal/bridge"
)

// Server serves the bridge protocol (internal/bridge) on the IPC endpoint:
// one native host process = one connection = one extension port.
//
// Per connection, frames are read in order and each request is handled in
// its own goroutine (a slow enqueue does not hold up a check); replies and
// status pushes go out through one writer, so frames never interleave.
// After a successful hello the connection subscribes to the handler's
// status pushes (bridge.Handler.Subscribe, which starts with one status per
// unfinished task). Pushes waiting for a slow reader are coalesced per task:
// an intermediate state may be skipped, the latest never is. Replies are
// never dropped and go before pushes.
//
// Nothing of a message's content is logged: requests carry cookies and
// signed URLs.
type Server struct {
	h            bridge.Handler
	log          *slog.Logger
	writeTimeout time.Duration
	maxInFlight  int
	now          func() time.Time

	// ctx is the handlers' context: cancelled by Close, not when the
	// extension disconnects (an enqueue the user confirmed still completes).
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	lns    map[net.Listener]struct{}
	conns  map[*conn]struct{}
	closed bool
	wg     sync.WaitGroup

	helloMu   sync.Mutex
	lastHello *HelloInfo
}

// ServerOptions configure a Server; only Handler is required.
type ServerOptions struct {
	Handler bridge.Handler
	// Log receives connection events and handler failures (never message
	// contents); nil = slog.Default().
	Log *slog.Logger
	// WriteTimeout ends a connection whose reader has not taken a frame for
	// this long (default 30 s).
	WriteTimeout time.Duration
	// MaxInFlight bounds the requests handled at once per connection
	// (default 8); the reader waits beyond it.
	MaxInFlight int
	// Now is the clock of HelloInfo.At (tests).
	Now func() time.Time
}

// HelloInfo describes the latest hello for the diagnostics page.
type HelloInfo struct {
	Browser          string `json:"browser"`
	ExtensionVersion string `json:"extension_version"`
	Protocol         int    `json:"protocol"`
	// Accepted is false when the extension spoke another protocol version.
	Accepted bool      `json:"accepted"`
	At       time.Time `json:"at"`
}

// NewServer builds a server; Serve starts it on a listener.
func NewServer(o ServerOptions) *Server {
	s := &Server{
		h:            o.Handler,
		log:          o.Log,
		writeTimeout: o.WriteTimeout,
		maxInFlight:  o.MaxInFlight,
		now:          o.Now,
		lns:          map[net.Listener]struct{}{},
		conns:        map[*conn]struct{}{},
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.writeTimeout <= 0 {
		s.writeTimeout = 30 * time.Second
	}
	if s.maxInFlight <= 0 {
		s.maxInFlight = 8
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s
}

// LastHello returns the latest hello any extension sent since the start.
func (s *Server) LastHello() (HelloInfo, bool) {
	s.helloMu.Lock()
	defer s.helloMu.Unlock()
	if s.lastHello == nil {
		return HelloInfo{}, false
	}
	return *s.lastHello, true
}

func (s *Server) recordHello(req *bridge.HelloRequest, accepted bool) {
	info := &HelloInfo{
		Browser:          cleanBrowser(req.Extension.Browser),
		ExtensionVersion: cleanVersion(req.Extension.Version),
		Protocol:         req.Protocol,
		Accepted:         accepted,
		At:               s.now(),
	}
	s.helloMu.Lock()
	s.lastHello = info
	s.helloMu.Unlock()
}

// Serve accepts connections on ln until Close; it returns nil after Close.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ln.Close()
		return net.ErrClosed
	}
	s.lns[ln] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.lns, ln)
		s.mu.Unlock()
	}()
	var delay time.Duration
	for {
		nc, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return nil
			}
			// A transient failure (out of handles, a client that vanished):
			// back off a little and go on.
			delay = min(max(delay*2, 10*time.Millisecond), time.Second)
			s.log.Warn("nm: accept failed", "err", err)
			time.Sleep(delay)
			continue
		}
		delay = 0
		if !s.ServeConn(nc, false) {
			return nil
		}
	}
}

// ServeConn serves one connection. With wait it returns when the
// connection has ended; otherwise at once. It reports false (and closes
// nc) when the server is closed.
func (s *Server) ServeConn(nc net.Conn, wait bool) bool {
	c := newConn(s, nc)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		nc.Close()
		return false
	}
	s.conns[c] = struct{}{}
	s.wg.Add(1)
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer s.wg.Done()
		defer close(done)
		c.serve()
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
	}()
	if wait {
		<-done
	}
	return true
}

// Close stops accepting, ends every connection and waits for them.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.wg.Wait()
		return nil
	}
	s.closed = true
	s.cancel()
	for ln := range s.lns {
		ln.Close()
	}
	for c := range s.conns {
		c.shutdown()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return nil
}

// ---- one connection ----

type conn struct {
	s  *Server
	nc net.Conn
	// ctx ends with the connection (the subscription, the writer).
	ctx    context.Context
	cancel context.CancelFunc

	mu         sync.Mutex
	replies    [][]byte
	pending    []bridge.Status // coalesced pushes, oldest first
	pendingIdx map[int64]int   // task id → index in pending
	wake       chan struct{}
	subscribed bool
	closeOnce  sync.Once
}

func newConn(s *Server, nc net.Conn) *conn {
	ctx, cancel := context.WithCancel(s.ctx)
	return &conn{s: s, nc: nc, ctx: ctx, cancel: cancel, pendingIdx: map[int64]int{}, wake: make(chan struct{}, 1)}
}

// shutdown ends the connection: the reader and writer stop, handlers and
// the subscription see a cancelled context.
func (c *conn) shutdown() {
	c.closeOnce.Do(func() {
		c.cancel()
		c.nc.Close()
	})
}

func (c *conn) serve() {
	c.s.log.Debug("nm: connection opened")
	var handlers sync.WaitGroup
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		c.writeLoop()
	}()
	sem := make(chan struct{}, c.s.maxInFlight)
	for {
		frame, err := bridge.ReadFrame(c.nc, bridge.MaxRequest)
		if err != nil {
			if errors.Is(err, bridge.ErrFrameTooLarge) {
				c.s.log.Warn("nm: request frame too large; closing the connection")
			}
			break
		}
		select {
		case sem <- struct{}{}:
		case <-c.ctx.Done():
		}
		if c.ctx.Err() != nil {
			break
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			defer func() { <-sem }()
			c.dispatch(frame)
		}()
	}
	c.shutdown()
	handlers.Wait()
	<-writerDone
	c.s.log.Debug("nm: connection closed")
}

func (c *conn) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// enqueueReply queues an encoded reply (never dropped).
func (c *conn) enqueueReply(b []byte) {
	c.mu.Lock()
	c.replies = append(c.replies, b)
	c.mu.Unlock()
	c.signal()
}

// enqueueStatus queues a push, replacing a queued one of the same task.
func (c *conn) enqueueStatus(st bridge.Status) {
	st.Type = bridge.TypeStatus
	c.mu.Lock()
	if i, ok := c.pendingIdx[st.TaskID]; ok {
		c.pending[i] = st
	} else {
		c.pendingIdx[st.TaskID] = len(c.pending)
		c.pending = append(c.pending, st)
	}
	c.mu.Unlock()
	c.signal()
}

// next takes the next frame to write: replies first.
func (c *conn) next() ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.replies) > 0 {
		b := c.replies[0]
		c.replies[0] = nil
		c.replies = c.replies[1:]
		return b, true
	}
	for len(c.pending) > 0 {
		st := c.pending[0]
		c.pending = c.pending[1:]
		delete(c.pendingIdx, st.TaskID)
		for id, i := range c.pendingIdx {
			c.pendingIdx[id] = i - 1
		}
		b, err := json.Marshal(st)
		if err != nil || len(b) > bridge.MaxToBrowser {
			c.s.log.Warn("nm: status push not sent", "task", st.TaskID, "bytes", len(b))
			continue
		}
		return b, true
	}
	return nil, false
}

func (c *conn) writeLoop() {
	for {
		for {
			b, ok := c.next()
			if !ok {
				break
			}
			c.nc.SetWriteDeadline(time.Now().Add(c.s.writeTimeout))
			if err := bridge.WriteFrame(c.nc, b, bridge.MaxToBrowser); err != nil {
				if c.ctx.Err() == nil {
					c.s.log.Warn("nm: write failed; closing the connection", "err", errText(err))
				}
				c.shutdown()
				return
			}
		}
		select {
		case <-c.wake:
		case <-c.ctx.Done():
			return
		}
	}
}

// subscribe forwards the handler's pushes until the connection ends.
func (c *conn) subscribe() {
	c.mu.Lock()
	if c.subscribed {
		c.mu.Unlock()
		return
	}
	c.subscribed = true
	c.mu.Unlock()
	ch := c.s.h.Subscribe(c.ctx)
	if ch == nil {
		return
	}
	go func() {
		for st := range ch {
			if c.ctx.Err() != nil {
				continue // drain until the handler closes the channel
			}
			c.enqueueStatus(st)
		}
	}()
}

// ---- dispatch ----

func (c *conn) dispatch(frame []byte) {
	var env bridge.Envelope
	if err := json.Unmarshal(frame, &env); err != nil {
		c.replyError(bridge.Envelope{ID: lenientID(frame)}, bridge.NewError(bridge.CodeBadRequest, map[string]any{"field": "json"}))
		return
	}
	defer func() {
		if p := recover(); p != nil {
			c.s.log.Error("nm: request handler panicked", "type", env.Type, "stack", string(debug.Stack()))
			c.replyError(env, bridge.NewError(bridge.CodeInternal, nil))
		}
	}()
	h := c.s.h
	switch env.Type {
	case bridge.TypeHello:
		req, ok := decode[bridge.HelloRequest](c, env, frame)
		if !ok {
			return
		}
		if req.Protocol != bridge.ProtocolVersion {
			c.s.recordHello(req, false)
			c.replyError(env, bridge.NewError(bridge.CodeProtocol, map[string]any{"app": bridge.ProtocolVersion, "extension": req.Protocol}))
			return
		}
		c.s.recordHello(req, true)
		rep, err := h.Hello(c.s.ctx, req)
		if rep == nil {
			rep = &bridge.HelloReply{}
		}
		if rep.Protocol == 0 {
			rep.Protocol = bridge.ProtocolVersion
		}
		rep.App.Running = true // it is answering
		if rep.Courses == nil {
			rep.Courses = []bridge.Course{}
		}
		if c.finish(env, err, &rep.ReplyHeader, rep) {
			c.subscribe() // after the reply is queued: pushes follow it
		}
	case bridge.TypeCheck:
		req, ok := decode[bridge.CheckRequest](c, env, frame)
		if !ok {
			return
		}
		rep, err := h.Check(c.s.ctx, req)
		if rep == nil {
			rep = &bridge.CheckReply{}
		}
		if rep.Items == nil {
			rep.Items = []bridge.CheckItem{}
		}
		c.finish(env, err, &rep.ReplyHeader, rep)
	case bridge.TypeEnqueue:
		req, ok := decode[bridge.EnqueueRequest](c, env, frame)
		if !ok {
			return
		}
		rep, err := h.Enqueue(c.s.ctx, req)
		if rep == nil {
			rep = &bridge.EnqueueReply{}
		}
		c.finish(env, err, &rep.ReplyHeader, rep)
	case bridge.TypeRefresh:
		req, ok := decode[bridge.RefreshRequest](c, env, frame)
		if !ok {
			return
		}
		rep, err := h.Refresh(c.s.ctx, req)
		if rep == nil {
			rep = &bridge.RefreshReply{}
		}
		c.finish(env, err, &rep.ReplyHeader, rep)
	case bridge.TypeOpen:
		req, ok := decode[bridge.OpenRequest](c, env, frame)
		if !ok {
			return
		}
		rep, err := h.Open(c.s.ctx, req)
		if rep == nil {
			rep = &bridge.OpenReply{}
		}
		c.finish(env, err, &rep.ReplyHeader, rep)
	default:
		c.replyError(env, bridge.NewError(bridge.CodeUnknownType, map[string]any{"type": clip(env.Type, 40)}))
	}
}

// decode parses a request; a field of the wrong type is answered with
// request.invalid {field}.
func decode[T any](c *conn, env bridge.Envelope, frame []byte) (*T, bool) {
	req := new(T)
	if err := json.Unmarshal(frame, req); err != nil {
		field := "json"
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && te.Field != "" {
			field = te.Field
		}
		c.replyError(env, bridge.NewError(bridge.CodeBadRequest, map[string]any{"field": field}))
		return nil, false
	}
	return req, true
}

// finish sends the handler's reply (rep, whose header is hdr) or its
// error. It reports whether an ok reply was queued.
func (c *conn) finish(env bridge.Envelope, err error, hdr *bridge.ReplyHeader, rep any) bool {
	if err != nil {
		var be *bridge.Error
		if errors.As(err, &be) {
			c.replyError(env, be)
		} else {
			if c.s.ctx.Err() == nil {
				c.s.log.Warn("nm: request failed", "type", env.Type, "err", errText(err))
			}
			c.replyError(env, bridge.NewError(bridge.CodeInternal, nil))
		}
		return false
	}
	*hdr = bridge.ReplyHeader{Type: env.Type, ID: env.ID, OK: true}
	b, err := json.Marshal(rep)
	if err != nil || len(b) > bridge.MaxToBrowser {
		c.s.log.Warn("nm: reply not sent", "type", env.Type, "bytes", len(b))
		c.replyError(env, bridge.NewError(bridge.CodeInternal, nil))
		return false
	}
	c.enqueueReply(b)
	return true
}

func (c *conn) replyError(env bridge.Envelope, e *bridge.Error) {
	b, err := json.Marshal(bridge.ReplyHeader{Type: env.Type, ID: env.ID, OK: false, Error: e})
	if err != nil || len(b) > bridge.MaxToBrowser {
		b, _ = json.Marshal(bridge.ReplyHeader{Type: clip(env.Type, 40), ID: clip(env.ID, 100), Error: bridge.NewError(bridge.CodeInternal, nil)})
	}
	c.enqueueReply(b)
}

// lenientID salvages the id of a request whose envelope did not parse
// (e.g. a numeric type), so the extension can match the error.
func lenientID(frame []byte) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(frame, &m) != nil {
		return ""
	}
	var id string
	if json.Unmarshal(m["id"], &id) != nil {
		return ""
	}
	return clip(id, 100)
}

// clip cuts s to at most n bytes, at the start of a character (a cut
// inside one would come back from JSON as more bytes than n; fuzz).
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// errText is an error message for the log with every URL reduced to
// bridge.LogURL, host and path hash (a handler error might quote a signed
// track URL, whose path is a credential too). A word with "://" anywhere,
// even first, counts as a URL: one that does not parse is dropped whole.
func errText(err error) string {
	fields := strings.Fields(err.Error())
	for i, f := range fields {
		if strings.Contains(f, "://") {
			fields[i] = bridge.LogURL(strings.Trim(f, `"'(),;`))
		}
	}
	return clip(strings.Join(fields, " "), 500)
}

func cleanBrowser(b string) string {
	switch b {
	case BrowserChrome, BrowserEdge, BrowserFirefox:
		return b
	case "":
		return ""
	}
	return "other"
}

func cleanVersion(v string) string {
	out := make([]byte, 0, len(v))
	for i := 0; i < len(v) && len(out) < 32; i++ {
		ch := v[i]
		if ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch == '.' || ch == '-' || ch == '+' {
			out = append(out, ch)
		}
	}
	return string(out)
}
