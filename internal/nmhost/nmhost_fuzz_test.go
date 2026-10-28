// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package nmhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/bridge"
)

// Native fuzz tests (go test -fuzz=FuzzXxx ./internal/nmhost). Under a plain
// go test they run their seeds only.

// fuzzHandler answers from the request's own fields, so that the fuzzer
// reaches every reply path: coded errors, plain errors quoting the track
// URLs, panics, replies too large for the browser.
type fuzzHandler struct{}

func (fuzzHandler) Hello(ctx context.Context, req *bridge.HelloRequest) (*bridge.HelloReply, error) {
	if req.Extension.Locale == "fail" {
		return nil, errors.New("hello failed")
	}
	return &bridge.HelloReply{App: bridge.AppInfo{Version: req.Extension.Version}, Courses: []bridge.Course{{ID: "c", Name: req.Extension.Locale}}}, nil
}

func (fuzzHandler) Check(ctx context.Context, req *bridge.CheckRequest) (*bridge.CheckReply, error) {
	out := &bridge.CheckReply{}
	for _, s := range req.SessionIDs {
		out.Items = append(out.Items, bridge.CheckItem{SessionID: s, State: bridge.StateQueued})
	}
	return out, nil
}

func (fuzzHandler) Enqueue(ctx context.Context, req *bridge.EnqueueRequest) (*bridge.EnqueueReply, error) {
	switch {
	case strings.HasPrefix(req.SessionID, "coded"):
		return nil, bridge.NewError(bridge.CodeCourseGone, map[string]any{"title": req.Lecture.Title})
	case strings.HasPrefix(req.SessionID, "plain"):
		var urls []string
		for _, t := range req.Tracks {
			urls = append(urls, t.URL)
		}
		return nil, fmt.Errorf("download %s refused", strings.Join(urls, " "))
	case strings.HasPrefix(req.SessionID, "panic"):
		panic("enqueue " + req.Lecture.Title)
	}
	return &bridge.EnqueueReply{TaskID: int64(len(req.Tracks)), Position: len(req.Cookies)}, nil
}

func (fuzzHandler) Refresh(ctx context.Context, req *bridge.RefreshRequest) (*bridge.RefreshReply, error) {
	return &bridge.RefreshReply{State: req.SessionID}, nil
}

func (fuzzHandler) Open(ctx context.Context, req *bridge.OpenRequest) (*bridge.OpenReply, error) {
	if req.LectureID == "" && req.TaskID == 0 {
		return nil, bridge.NewError(bridge.CodeNotFound, nil)
	}
	return nil, nil // a handler that forgets its reply
}

func (fuzzHandler) Subscribe(ctx context.Context) <-chan bridge.Status { return nil }

// replyOf decodes one reply frame and checks its shape.
func replyOf(t *testing.T, b []byte) (typ, id string, ok bool) {
	t.Helper()
	if len(b) > bridge.MaxToBrowser {
		t.Fatalf("reply of %d bytes (max %d)", len(b), bridge.MaxToBrowser)
	}
	var r struct {
		Type  *string `json:"type"`
		ID    *string `json:"id"`
		OK    *bool   `json:"ok"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("reply is not JSON: %v\n%.300s", err, b)
	}
	if r.Type == nil || r.ID == nil || r.OK == nil {
		t.Fatalf("reply without type, id or ok: %.300s", b)
	}
	if !*r.OK && (r.Error == nil || r.Error.Code == "") {
		t.Fatalf("failed reply without an error code: %.300s", b)
	}
	return *r.Type, *r.ID, *r.OK
}

// FuzzServerDispatch: every frame the extension may send gets exactly one
// well-formed reply of at most bridge.MaxToBrowser bytes with the request's
// id; the log never shows the path or the query of a URL a handler error
// quotes.
func FuzzServerDispatch(f *testing.F) {
	for _, s := range []string{
		`{"type":"hello","id":"1","protocol":1,"extension":{"version":"0.1.0+x y","browser":"chrome","locale":"zh-CN"}}`,
		`{"type":"hello","id":"1","protocol":2}`,
		`{"type":"hello","id":"1","protocol":1,"extension":{"locale":"fail"}}`,
		`{"type":"check","id":"2","platform":"p","sessionIds":["a","b"]}`,
		`{"type":"check","id":"2","sessionIds":"a"}`,
		`{"type":"enqueue","id":"3","sessionId":"coded","lecture":{"title":"t"}}`,
		`{"type":"enqueue","id":"3","sessionId":"plain","tracks":[{"url":"https://h/p?sig=SECRETQUERY1"},{"url":"://u:p@h/x?sig=SECRETQUERY2"}]}`,
		`{"type":"enqueue","id":"3","sessionId":"panic","lecture":{"title":"boom"}}`,
		`{"type":"enqueue","id":"3","tracks":[{"sizeBytes":"x"}]}`,
		`{"type":"refresh","id":"4","sessionId":"queued"}`,
		`{"type":"open","id":"5","lectureId":"01J"}`,
		`{"type":"open","id":"5"}`,
		`{"type":"nope","id":"6"}`,
		`{"type":7,"id":"7"}`,
		`{"id":"8"}`,
		`[1,2]`,
		`not json`,
		``,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, frame []byte) {
		var logs logBuffer
		s := NewServer(ServerOptions{Handler: fuzzHandler{}, Log: slog.New(slog.NewTextHandler(&logs, nil)),
			Now: func() time.Time { return time.Unix(0, 0) }})
		defer s.Close()
		c := newConn(s, nil)
		defer c.cancel()
		c.dispatch(frame)
		c.mu.Lock()
		replies := c.replies
		c.mu.Unlock()
		if len(replies) != 1 {
			t.Fatalf("%d replies to one request", len(replies))
		}
		typ, id, _ := replyOf(t, replies[0])

		var env bridge.Envelope
		wantID := lenientID(frame)
		wantType := ""
		if json.Unmarshal(frame, &env) == nil {
			wantID, wantType = env.ID, env.Type
		}
		if len(wantID) <= 100 && len(wantType) <= 40 && (id != wantID || typ != wantType) {
			t.Fatalf("reply type %q id %q, request type %q id %q", typ, id, wantType, wantID)
		}
		if h, ok := s.LastHello(); ok {
			switch h.Browser {
			case "", BrowserChrome, BrowserEdge, BrowserFirefox, "other":
			default:
				t.Fatalf("hello browser %q", h.Browser)
			}
			if len(h.ExtensionVersion) > 32 || strings.ContainsAny(h.ExtensionVersion, " \t\r\n\"<>") {
				t.Fatalf("hello version %q", h.ExtensionVersion)
			}
		}
		// a handler error quoting the track URLs: their queries stay out
		var req bridge.EnqueueRequest
		if json.Unmarshal(frame, &req) == nil && strings.HasPrefix(req.SessionID, "plain") {
			log := logs.String()
			// only requests whose every URL is one absolute URL word (text
			// redaction finds URLs by "://"; another word may hold the
			// same text)
			urls := []*url.URL{}
			for _, tr := range req.Tracks {
				u, err := url.Parse(tr.URL)
				if err != nil || u.Host == "" || u.Scheme == "" || strings.ContainsAny(tr.URL, " \t\r\n\v\f\u0085\u00a0\"'(),;") {
					urls = nil
					break
				}
				urls = append(urls, u)
			}
			for _, u := range urls {
				for _, secret := range []string{u.RawQuery, u.EscapedPath()} {
					if len(secret) >= 8 && !strings.Contains(env.Type+u.Host, secret) && strings.Contains(log, secret) {
						t.Fatalf("the log shows %q:\n%s", secret, log)
					}
				}
			}
		}
	})
}

// FuzzErrText: every URL left in a log line by errText is reduced to scheme
// and host (and the path's hash).
func FuzzErrText(f *testing.F) {
	for _, s := range []string{
		`download https://u:p@cdn.example.edu/a.mp4?sig=SECRET refused`,
		`Get "https://h/p?x=1": dial tcp: lookup h: no such host`,
		`(https://h/p?q), [https://h/p?q] <https://h/p?q> url=https://h/p?q`,
		`://u:p@h/x?sig=SECRET`, `HTTPS://H/P?Q`, `a+b://user:pw@h/p?q#f`, "x\thttps://h/?q\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, msg string) {
		out := errText(errors.New(msg))
		if len(out) > 500 {
			t.Fatalf("%d bytes", len(out))
		}
		for _, field := range strings.Fields(out) {
			_, rest, ok := strings.Cut(field, "://")
			if !ok {
				continue
			}
			if strings.ContainsAny(rest, "?#@/") {
				t.Fatalf("errText(%q) = %q keeps %q", msg, out, field)
			}
		}
	})
}

// FuzzAnswerNotRunning: the host's own answer to any frame is one
// well-formed app.not_running reply with the request's type and id (cut).
func FuzzAnswerNotRunning(f *testing.F) {
	for _, s := range []string{
		`{"type":"hello","id":"1","noLaunch":true}`, `{"type":"enqueue","id":"x"}`, `{"type":1,"id":2}`,
		`{"type":"` + strings.Repeat("t", 100) + `","id":"` + strings.Repeat("i", 300) + `"}`, `[]`, `"`, ``,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, frame []byte) {
		var out bytes.Buffer
		h := &Host{Out: &out, Linger: time.Millisecond}
		in := make(chan []byte)
		close(in)
		h.answerNotRunning(frame, in, ReasonTimeout)
		b, err := bridge.ReadFrame(&out, bridge.MaxToBrowser)
		if err != nil {
			t.Fatalf("no answer: %v", err)
		}
		if out.Len() != 0 {
			t.Fatalf("%d bytes after the answer", out.Len())
		}
		typ, id, ok := replyOf(t, b)
		env := envelopeOf(frame)
		if ok || typ != env.Type || id != env.ID || len(typ) > 40 || len(id) > 100 {
			t.Fatalf("answer %s to %q", b, frame)
		}
		var r struct {
			Error bridge.Error `json:"error"`
		}
		if json.Unmarshal(b, &r); r.Error.Code != bridge.CodeAppNotRunning || r.Error.Params["reason"] != ReasonTimeout {
			t.Fatalf("answer %s", b)
		}
		if noLaunch(frame) && env.Type != bridge.TypeHello {
			t.Fatalf("noLaunch for a %q frame", env.Type)
		}
	})
}
