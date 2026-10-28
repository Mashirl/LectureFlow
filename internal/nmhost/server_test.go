// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package nmhost

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/bridge"
)

// pipeServer serves h on one end of an in-memory connection.
func pipeServer(t *testing.T, h bridge.Handler) (*client, *Server, *logBuffer) {
	t.Helper()
	log, buf := newTestLogger()
	s := NewServer(ServerOptions{Handler: h, Log: log, WriteTimeout: 2 * time.Second})
	a, b := net.Pipe()
	s.ServeConn(b, false)
	t.Cleanup(func() {
		a.Close()
		s.Close()
	})
	return &client{t: t, nc: a}, s, buf
}

func errCode(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func errParams(m map[string]any) map[string]any {
	e, _ := m["error"].(map[string]any)
	p, _ := e["params"].(map[string]any)
	return p
}

func TestServerHelloRepliesAndSubscribes(t *testing.T) {
	h := newFakeHandler()
	h.initial = []bridge.Status{{TaskID: 7, State: bridge.StateDownloading, BytesTotal: -1}}
	c, s, _ := pipeServer(t, h)

	if _, ok := s.LastHello(); ok {
		t.Fatal("hello recorded before any")
	}
	c.send(hello("h1"))
	rep := c.recv()
	if rep["type"] != "hello" || rep["id"] != "h1" || rep["ok"] != true {
		t.Fatalf("hello reply: %v", rep)
	}
	app := rep["app"].(map[string]any)
	if app["version"] != "9.9.9" || app["running"] != true {
		t.Fatalf("app: %v", app)
	}
	if rep["protocol"] != float64(bridge.ProtocolVersion) {
		t.Fatalf("protocol: %v", rep["protocol"])
	}
	courses := rep["courses"].([]any)
	if len(courses) != 1 || courses[0].(map[string]any)["id"] != "01HCOURSE" {
		t.Fatalf("courses: %v", courses)
	}
	// The status of every unfinished task follows the reply.
	st := c.recv()
	if st["type"] != "status" || st["taskId"] != float64(7) || st["state"] != "downloading" {
		t.Fatalf("first push: %v", st)
	}
	// Later changes are pushed too.
	waitFor(t, func() bool { return h.subscribers() == 1 })
	h.push(bridge.Status{TaskID: 7, State: bridge.StateDone, LectureID: "01HLEC"})
	st = c.recv()
	if st["state"] != "done" || st["lectureId"] != "01HLEC" {
		t.Fatalf("push: %v", st)
	}
	info, ok := s.LastHello()
	if !ok || info.Browser != "chrome" || info.ExtensionVersion != "0.1.0" || !info.Accepted || info.At.IsZero() {
		t.Fatalf("last hello: %+v %v", info, ok)
	}
	// A second hello does not subscribe twice.
	c.send(hello("h2"))
	if rep := c.recv(); rep["id"] != "h2" || rep["ok"] != true {
		t.Fatalf("second hello: %v", rep)
	}
	time.Sleep(50 * time.Millisecond)
	if h.subscriptions() != 1 {
		t.Fatalf("subscribed %d times", h.subCount)
	}
	// The subscription ends with the connection.
	c.nc.Close()
	waitFor(t, func() bool { return h.subscribers() == 0 })
}

func TestServerProtocolMismatch(t *testing.T) {
	h := newFakeHandler()
	c, s, _ := pipeServer(t, h)
	req := hello("p1")
	req.Protocol = 99
	req.Extension.Browser = "netscape"
	req.Extension.Version = "1.0<script>"
	c.send(req)
	rep := c.recv()
	if rep["ok"] != false || errCode(rep) != bridge.CodeProtocol || rep["id"] != "p1" || rep["type"] != "hello" {
		t.Fatalf("reply: %v", rep)
	}
	p := errParams(rep)
	if p["app"] != float64(bridge.ProtocolVersion) || p["extension"] != float64(99) {
		t.Fatalf("params: %v", p)
	}
	info, _ := s.LastHello()
	if info.Accepted || info.Protocol != 99 || info.Browser != "other" || info.ExtensionVersion != "1.0script" {
		t.Fatalf("last hello: %+v", info)
	}
	// No subscription without an accepted hello.
	time.Sleep(50 * time.Millisecond)
	if h.subscriptions() != 0 {
		t.Fatal("subscribed after a refused hello")
	}
}

func TestServerBadRequests(t *testing.T) {
	c, _, _ := pipeServer(t, newFakeHandler())

	c.sendRaw([]byte(`{"type":"check","id":"b1"`)) // broken JSON
	if rep := c.recv(); errCode(rep) != bridge.CodeBadRequest || errParams(rep)["field"] != "json" {
		t.Fatalf("broken json: %v", rep)
	}
	c.sendRaw([]byte(`{"type":5,"id":"b2"}`)) // the envelope itself is wrong
	if rep := c.recv(); errCode(rep) != bridge.CodeBadRequest || rep["id"] != "b2" {
		t.Fatalf("bad envelope: %v", rep)
	}
	c.sendRaw([]byte(`{"type":"check","id":"b3","sessionIds":"notalist"}`))
	if rep := c.recv(); errCode(rep) != bridge.CodeBadRequest || errParams(rep)["field"] != "sessionIds" || rep["type"] != "check" || rep["id"] != "b3" {
		t.Fatalf("wrong field type: %v", rep)
	}
	c.sendRaw([]byte(`{"type":"teleport","id":"b4"}`))
	if rep := c.recv(); errCode(rep) != bridge.CodeUnknownType || errParams(rep)["type"] != "teleport" || rep["id"] != "b4" {
		t.Fatalf("unknown type: %v", rep)
	}
	c.sendRaw([]byte(`{"type":"status","id":"b5"}`)) // pushes go the other way
	if rep := c.recv(); errCode(rep) != bridge.CodeUnknownType {
		t.Fatalf("status from the extension: %v", rep)
	}
	// The connection still works.
	c.send(bridge.CheckRequest{Envelope: bridge.Envelope{Type: bridge.TypeCheck, ID: "b6"}, Platform: "generic", SessionIDs: []string{"a"}})
	if rep := c.recv(); rep["ok"] != true || rep["id"] != "b6" {
		t.Fatalf("check after errors: %v", rep)
	}
}

func TestServerHandlerErrors(t *testing.T) {
	h := newFakeHandler()
	c, _, logs := pipeServer(t, h)

	c.send(enqueueReq("e1", "coded"))
	rep := c.recv()
	if errCode(rep) != bridge.CodeCourseGone || errParams(rep)["courseId"] != "01HCOURSE" || rep["type"] != "enqueue" {
		t.Fatalf("coded error: %v", rep)
	}
	c.send(enqueueReq("e2", "fail"))
	rep = c.recv()
	if errCode(rep) != bridge.CodeInternal || rep["error"].(map[string]any)["params"] != nil {
		t.Fatalf("plain error: %v", rep)
	}
	h.panicOn = bridge.TypeCheck
	c.send(bridge.CheckRequest{Envelope: bridge.Envelope{Type: bridge.TypeCheck, ID: "e3"}, SessionIDs: []string{"x"}})
	if rep := c.recv(); errCode(rep) != bridge.CodeInternal || rep["id"] != "e3" {
		t.Fatalf("panic: %v", rep)
	}
	c.send(enqueueReq("e4", "ok"))
	if rep := c.recv(); rep["ok"] != true || rep["taskId"] != float64(42) || rep["position"] != float64(1) {
		t.Fatalf("enqueue: %v", rep)
	}
	text := logs.String()
	if !strings.Contains(text, bridge.LogURL("https://media.example.edu/v/1/audio.mp4")) || strings.Contains(text, "/v/1/audio.mp4") {
		t.Errorf("the failed request's URL should be logged as host and path hash:\n%s", text)
	}
	// Credentials: neither the cookie value nor the signed query reaches
	// the log, whatever the handler's error or panic says.
	assertNoSecrets(t, "server log", text)
	if h.enqueued[0].Cookies[0].Value != secretCookie {
		t.Fatal("the handler did not get the cookie")
	}
}

func TestServerConcurrentRequests(t *testing.T) {
	h := newFakeHandler()
	c, _, _ := pipeServer(t, h)
	c.send(bridge.CheckRequest{Envelope: bridge.Envelope{Type: bridge.TypeCheck, ID: "slow"}, SessionIDs: []string{"slow"}})
	c.send(bridge.CheckRequest{Envelope: bridge.Envelope{Type: bridge.TypeCheck, ID: "fast"}, SessionIDs: []string{"a"}})
	// The fast request is answered while the slow one waits.
	if rep := c.recv(); rep["id"] != "fast" {
		t.Fatalf("first reply %v, want fast", rep)
	}
	close(h.block)
	if rep := c.recv(); rep["id"] != "slow" || rep["ok"] != true {
		t.Fatalf("second reply %v, want slow", rep)
	}
}

func TestServerSlowReaderKeepsLatestStatus(t *testing.T) {
	h := newFakeHandler()
	c, _, _ := pipeServer(t, h)
	c.send(hello("h"))
	if rep := c.recv(); rep["ok"] != true {
		t.Fatal(rep)
	}
	waitFor(t, func() bool { return h.subscribers() == 1 })
	// The reader takes nothing while 300 pushes for two tasks arrive.
	for i := 1; i <= 150; i++ {
		h.push(bridge.Status{TaskID: 1, State: bridge.StateDownloading, BytesDone: int64(i), BytesTotal: 150})
		h.push(bridge.Status{TaskID: 2, State: bridge.StateDownloading, BytesDone: int64(i), BytesTotal: 150})
	}
	h.push(bridge.Status{TaskID: 1, State: bridge.StateDone})
	h.push(bridge.Status{TaskID: 2, State: bridge.StateFailed, Error: bridge.NewError("download.http", nil)})
	time.Sleep(200 * time.Millisecond)
	// A reply queued behind the pushes still arrives.
	c.send(bridge.CheckRequest{Envelope: bridge.Envelope{Type: bridge.TypeCheck, ID: "c"}, SessionIDs: []string{"a"}})
	last := map[float64]string{}
	n, gotReply := 0, false
	for {
		m, err := c.tryRecv(500 * time.Millisecond)
		if err != nil {
			break
		}
		if m["type"] == "check" {
			gotReply = true
			continue
		}
		n++
		last[m["taskId"].(float64)] = m["state"].(string)
	}
	if !gotReply {
		t.Fatal("the reply was lost behind the pushes")
	}
	if last[1] != "done" || last[2] != "failed" {
		t.Fatalf("latest states %v, want done and failed", last)
	}
	if n >= 302 {
		t.Fatalf("%d pushes delivered: nothing was coalesced", n)
	}
	t.Logf("%d of 302 pushes delivered", n)
}

func TestServerWriteTimeoutClosesConnection(t *testing.T) {
	h := newFakeHandler()
	log, _ := newTestLogger()
	s := NewServer(ServerOptions{Handler: h, Log: log, WriteTimeout: 100 * time.Millisecond})
	a, b := net.Pipe()
	defer a.Close()
	done := make(chan struct{})
	go func() {
		s.ServeConn(b, true)
		close(done)
	}()
	c := &client{t: t, nc: a}
	c.send(hello("h"))
	// Never read: the writer gives up and the connection ends.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a reader that never reads kept the connection open")
	}
	s.Close()
}

func TestServerReplyTooLarge(t *testing.T) {
	h := newFakeHandler()
	big := make([]bridge.Course, 0, 30000)
	for i := range 30000 {
		big = append(big, bridge.Course{ID: fmt.Sprintf("%026d", i), Name: strings.Repeat("x", 20)})
	}
	c, _, _ := pipeServer(t, helloOverride{fakeHandler: h, courses: big})
	c.send(hello("big"))
	rep := c.recv()
	if errCode(rep) != bridge.CodeInternal || rep["id"] != "big" {
		t.Fatalf("over 1 MB: %v", rep)
	}
}

type helloOverride struct {
	*fakeHandler
	courses []bridge.Course
}

func (h helloOverride) Hello(ctx context.Context, req *bridge.HelloRequest) (*bridge.HelloReply, error) {
	return &bridge.HelloReply{Courses: h.courses}, nil
}

func TestServerFrameTooLargeClosesConnection(t *testing.T) {
	c, _, _ := pipeServer(t, newFakeHandler())
	// A length beyond MaxRequest: the stream cannot be trusted any more.
	c.nc.Write([]byte{0xff, 0xff, 0xff, 0x7f})
	if _, err := c.tryRecv(2 * time.Second); err == nil {
		t.Fatal("the connection stayed open")
	}
}

func TestServerCloseEndsConnections(t *testing.T) {
	h := newFakeHandler()
	ep := uniqueSocket(t)
	log, _ := newTestLogger()
	ln, err := Listen(ep)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(ServerOptions{Handler: h, Log: log})
	served := make(chan error, 1)
	go func() { served <- s.Serve(ln) }()
	nc, err := Dial(t.Context(), ep)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	c := &client{t: t, nc: nc}
	c.send(hello("h"))
	c.recv()
	s.Close()
	if err := <-served; err != nil {
		t.Fatalf("Serve after Close: %v", err)
	}
	if _, err := c.tryRecv(2 * time.Second); err == nil {
		t.Fatal("connection still open after Close")
	}
}

func TestErrTextRedactsURLs(t *testing.T) {
	got := errText(fmt.Errorf(`get "https://h.example/a/b.m3u8?` + secretQuery + `": 403`))
	if strings.Contains(got, "SECRETSIGNATURE") || strings.Contains(got, "/a/b.m3u8") || !strings.Contains(got, bridge.LogURL("https://h.example/a/b.m3u8")) {
		t.Fatalf("errText: %s", got)
	}
	// a word that starts with "://" (found by FuzzErrText): dropped whole
	if got := errText(fmt.Errorf("x ://u:p@h/x?%s", secretQuery)); got != "x (invalid url)" {
		t.Fatalf("errText: %s", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var _ = json.Marshal

// slowEnqueue holds Enqueue until released and records its context's state.
type slowEnqueue struct {
	*fakeHandler
	started, release chan struct{}
	ctxErr           chan error
}

func (h slowEnqueue) Enqueue(ctx context.Context, req *bridge.EnqueueRequest) (*bridge.EnqueueReply, error) {
	close(h.started)
	<-h.release
	h.ctxErr <- ctx.Err()
	return &bridge.EnqueueReply{TaskID: 1}, nil
}

// An enqueue the user confirmed completes even when the extension
// disconnects right after sending it; Close cancels it.
func TestServerRequestOutlivesConnection(t *testing.T) {
	h := slowEnqueue{fakeHandler: newFakeHandler(), started: make(chan struct{}), release: make(chan struct{}), ctxErr: make(chan error, 1)}
	c, s, _ := pipeServer(t, h)
	c.send(enqueueReq("e", "ok"))
	<-h.started
	c.nc.Close()
	time.Sleep(50 * time.Millisecond)
	close(h.release)
	if err := <-h.ctxErr; err != nil {
		t.Fatalf("the enqueue was cancelled with the connection: %v", err)
	}

	h2 := slowEnqueue{fakeHandler: newFakeHandler(), started: make(chan struct{}), release: make(chan struct{}), ctxErr: make(chan error, 1)}
	c2, s2, _ := pipeServer(t, h2)
	c2.send(enqueueReq("e", "ok"))
	<-h2.started
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(h2.release)
	}()
	s2.Close()
	if err := <-h2.ctxErr; err == nil {
		t.Fatal("Close did not cancel the request")
	}
	_ = s
}
