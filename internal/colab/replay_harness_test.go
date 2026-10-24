// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// The replay server: serves the steps of a testdata/replay sample in order
// and fails the test on any request that does not match the next step, and
// on steps left unused.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type replayServer struct {
	t    *testing.T
	name string
	srv  *httptest.Server

	mu       sync.Mutex
	steps    []ReplayStep
	pos      int
	vars     map[string]string
	finished bool
	failures []string
	wsDone   sync.WaitGroup
}

// loadReplay reads a sample of testdata/replay, resolving "extends".
func loadReplay(t *testing.T, name string) (ReplayFile, []ReplayStep, map[string]string) {
	t.Helper()
	return loadReplayPath(t, filepath.Join("testdata", "replay", name))
}

// loadReplayPath reads a sample file ("extends" relative to its directory).
func loadReplayPath(t *testing.T, path string) (ReplayFile, []ReplayStep, map[string]string) {
	t.Helper()
	name := filepath.Base(path)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f ReplayFile
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if f.Source == "" || f.Upstream.Version == "" || len(f.Upstream.Files) == 0 {
		t.Fatalf("%s: _source and upstream (version, files) are required", name)
	}
	vars := map[string]string{}
	var steps []ReplayStep
	if f.Extends != "" {
		_, parent, pv := loadReplayPath(t, filepath.Join(filepath.Dir(path), f.Extends))
		for k, v := range pv {
			vars[k] = v
		}
		found := false
		for _, s := range parent {
			if s.Name == f.Until {
				found = true
				break
			}
			steps = append(steps, s)
		}
		if !found {
			t.Fatalf("%s: step %q not found in %s", name, f.Until, f.Extends)
		}
	}
	for k, v := range f.Vars {
		vars[k] = v
	}
	return f, append(steps, f.Steps...), vars
}

func newReplay(t *testing.T, name string) *replayServer {
	t.Helper()
	return newReplayPath(t, filepath.Join("testdata", "replay", name))
}

func newReplayPath(t *testing.T, path string) *replayServer {
	t.Helper()
	_, steps, vars := loadReplayPath(t, path)
	name := filepath.Base(path)
	rs := &replayServer{t: t, name: name, steps: steps, vars: vars}
	rs.srv = httptest.NewServer(http.HandlerFunc(rs.serve))
	t.Cleanup(func() {
		rs.srv.CloseClientConnections()
		rs.srv.Close()
	})
	return rs
}

func (rs *replayServer) colabURL() string   { return rs.srv.URL + "/colab" }
func (rs *replayServer) tokenURL() string   { return rs.srv.URL + "/oauth/token" }
func (rs *replayServer) revokeURL() string  { return rs.srv.URL + "/oauth/revoke" }
func (rs *replayServer) runtimeURL() string { return rs.srv.URL + "/rt" }

// finish fails the test when steps are left or a request did not match.
func (rs *replayServer) finish() {
	rs.t.Helper()
	rs.wsDone.Wait()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.finished = true
	for _, f := range rs.failures {
		rs.t.Errorf("%s: %s", rs.name, f)
	}
	if rs.pos < len(rs.steps) {
		var left []string
		for _, s := range rs.steps[rs.pos:] {
			left = append(left, s.Name)
		}
		rs.t.Errorf("%s: %d steps not used: %s", rs.name, len(left), strings.Join(left, "; "))
	}
}

var placeholderRe = regexp.MustCompile(`\{\{([a-z0-9]+:)?([A-Za-z0-9_]+)\}\}`)

// sub replaces the placeholders (except {{any}}).
func (rs *replayServer) sub(s string) string {
	return placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
		g := placeholderRe.FindStringSubmatch(m)
		fn, name := strings.TrimSuffix(g[1], ":"), g[2]
		if fn == "" && name == "any" {
			return m
		}
		if fn == "" && name == "runtime_url" {
			return rs.runtimeURL()
		}
		v, ok := rs.vars[name]
		if !ok {
			rs.failures = append(rs.failures, "unknown var "+name)
			return m
		}
		switch fn {
		case "b64":
			return base64.StdEncoding.EncodeToString([]byte(v))
		case "sha256":
			h := sha256.Sum256([]byte(v))
			return hex.EncodeToString(h[:])
		case "len":
			return strconv.Itoa(len(v))
		}
		return v
	})
}

func (rs *replayServer) matchStr(want, got string) bool {
	want = rs.sub(want)
	if !strings.Contains(want, "{{any}}") {
		return want == got
	}
	parts := strings.Split(want, "{{any}}")
	if !strings.HasPrefix(got, parts[0]) {
		return false
	}
	rest := got[len(parts[0]):]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(rest, p)
		if i < 0 {
			return false
		}
		rest = rest[i+len(p):]
	}
	return strings.HasSuffix(rest, parts[len(parts)-1])
}

func (rs *replayServer) matchJSON(want, got any) bool {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for k, wv := range w {
			gv, ok := g[k]
			if !ok || !rs.matchJSON(wv, gv) {
				return false
			}
		}
		return true
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !rs.matchJSON(w[i], g[i]) {
				return false
			}
		}
		return true
	case string:
		g, ok := got.(string)
		return ok && rs.matchStr(w, g)
	case nil:
		return got == nil
	default:
		return fmt.Sprint(w) == fmt.Sprint(got)
	}
}

func hostOf(path string) (string, string) {
	for _, h := range []string{"colab", "oauth", "rt"} {
		if strings.HasPrefix(path, "/"+h+"/") {
			name := h
			if h == "rt" {
				name = "runtime"
			}
			return name, strings.TrimPrefix(path, "/"+h)
		}
	}
	return "?", path
}

func (rs *replayServer) fail(w http.ResponseWriter, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	rs.failures = append(rs.failures, msg)
	rs.mu.Unlock()
	http.Error(w, "replay mismatch", 418) // an unexpected status: the backend reports it
}

func (rs *replayServer) serve(w http.ResponseWriter, r *http.Request) {
	host, path := hostOf(r.URL.EscapedPath())
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	rs.mu.Lock()
	if rs.pos >= len(rs.steps) {
		rs.fail(w, "unexpected request after the last step: %s %s %s", r.Method, host, path)
		return
	}
	st := rs.steps[rs.pos]
	where := fmt.Sprintf("step %d %q", rs.pos+1, st.Name)
	if ws := st.WebSocket; ws != nil {
		if host != "runtime" || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !rs.matchStr(ws.Path, path) {
			rs.fail(w, "%s: expected a WebSocket to %s, got %s %s %s", where, ws.Path, r.Method, host, path)
			return
		}
		if msg := rs.checkQueryHeaders(ws.Query, ws.Headers, r); msg != "" {
			rs.fail(w, "%s: %s", where, msg)
			return
		}
		rs.pos++
		rs.wsDone.Add(1)
		rs.mu.Unlock()
		defer rs.wsDone.Done()
		rs.runWS(w, r, ws, where)
		return
	}
	req := st.Request
	if req == nil {
		rs.fail(w, "%s: neither request nor websocket", where)
		return
	}
	if req.Method != r.Method || req.Host != host || !rs.matchStr(req.Path, path) {
		rs.fail(w, "%s: expected %s %s %s, got %s %s %s", where, req.Method, req.Host, req.Path, r.Method, host, path)
		return
	}
	if msg := rs.checkQueryHeaders(req.Query, req.Headers, r); msg != "" {
		rs.fail(w, "%s: %s", where, msg)
		return
	}
	if req.Form != nil {
		form, err := url.ParseQuery(string(body))
		if err != nil {
			rs.fail(w, "%s: body is not a form", where)
			return
		}
		for k, v := range req.Form {
			if !rs.matchStr(v, form.Get(k)) {
				rs.fail(w, "%s: form field %s does not match", where, k)
				return
			}
		}
	}
	if req.JSON != nil {
		var got any
		if json.Unmarshal(body, &got) != nil || !rs.matchJSON(map[string]any(req.JSON), got) {
			rs.fail(w, "%s: JSON body does not match", where)
			return
		}
	}
	rs.pos++
	resp := st.Response
	if resp == nil {
		resp = &ReplayResponse{Status: 200}
	}
	out := rs.sub(resp.Body)
	if resp.Body == "" && resp.JSON != nil {
		b, _ := json.Marshal(resp.JSON)
		out = rs.sub(string(b))
		if resp.XSSI {
			out = xssiPrefix + out
		}
		w.Header().Set("Content-Type", "application/json")
	}
	for k, v := range resp.Headers {
		w.Header().Set(k, rs.sub(v))
	}
	rs.mu.Unlock()
	w.WriteHeader(resp.Status)
	_, _ = io.WriteString(w, out)
}

// checkQueryHeaders: query keys must be exactly the expected ones; headers
// a subset. Called with rs.mu held.
func (rs *replayServer) checkQueryHeaders(query, headers map[string]string, r *http.Request) string {
	q := r.URL.Query()
	if query != nil {
		for k := range q {
			if _, ok := query[k]; !ok {
				return "unexpected query parameter " + k
			}
		}
		for k, v := range query {
			if !q.Has(k) || !rs.matchStr(v, q.Get(k)) {
				return "query parameter " + k + " does not match"
			}
		}
	}
	for k, v := range headers {
		if !rs.matchStr(v, r.Header.Get(k)) {
			return "header " + k + " does not match"
		}
	}
	return ""
}

// runWS answers the upgrade and plays the script.
func (rs *replayServer) runWS(w http.ResponseWriter, r *http.Request, ws *ReplayWS, where string) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		rs.addFailure("%s: cannot hijack", where)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		rs.addFailure("%s: hijack: %v", where, err)
		return
	}
	fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
		acceptKey(r.Header.Get("Sec-WebSocket-Key")))
	if err := brw.Flush(); err != nil {
		conn.Close()
		return
	}
	c := newWSConn(conn, brw.Reader, false)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var parent, session string
	send := func(channel, msgType string, content map[string]any, par string) bool {
		if par == "other" {
			par = "unrelated-" + newUUID()
		} else {
			par = parent
		}
		cb, _ := json.Marshal(rs.subAny(content))
		if content == nil {
			cb = []byte("{}")
		}
		msg, _ := json.Marshal(kernelMsg{
			Header:       map[string]any{"msg_id": newUUID(), "msg_type": msgType, "session": session, "username": "replay", "version": "5.3"},
			ParentHeader: map[string]any{"msg_id": par, "msg_type": "execute_request", "session": session},
			Metadata:     map[string]any{}, Content: cb, Channel: channel, Buffers: []any{},
		})
		return c.WriteText(ctx, msg) == nil
	}
	for i, a := range ws.Script {
		switch {
		case a.Expect != nil:
			_, data, err := c.ReadMessage(ctx)
			if err != nil {
				rs.addFailure("%s action %d: expected a %s message, read failed: %v", where, i+1, a.Expect.MsgType, err)
				return
			}
			m, err := decodeKernelMsg(false, data)
			if err != nil || m.Channel != a.Expect.Channel || m.msgType() != a.Expect.MsgType {
				rs.addFailure("%s action %d: expected %s/%s, got %s/%s", where, i+1, a.Expect.Channel, a.Expect.MsgType, m.Channel, m.msgType())
				return
			}
			var content map[string]any
			_ = json.Unmarshal(m.Content, &content)
			for k, sub := range a.Expect.ContentContains {
				if s, _ := content[k].(string); !strings.Contains(s, sub) {
					rs.addFailure("%s action %d: content.%s does not contain %q", where, i+1, k, sub)
				}
			}
			for k, v := range a.Expect.Content {
				if !rs.matchJSON(v, content[k]) {
					rs.addFailure("%s action %d: content.%s does not match", where, i+1, k)
				}
			}
			parent, _ = m.Header["msg_id"].(string)
			session, _ = m.Header["session"].(string)
		case a.Send != nil:
			if !send(a.Send.Channel, a.Send.MsgType, a.Send.Content, a.Send.Parent) {
				return
			}
		case a.StdoutLines != nil:
			var sb strings.Builder
			for _, line := range a.StdoutLines {
				ev := map[string]any{}
				for k, v := range line {
					if s, ok := v.(string); ok {
						if m := regexp.MustCompile(`^\{\{len:([A-Za-z0-9_]+)\}\}$`).FindStringSubmatch(s); m != nil {
							ev[k] = len(rs.vars[m[1]])
							continue
						}
						v = rs.subL(s)
					}
					ev[k] = v
				}
				b, _ := json.Marshal(ev)
				sb.Write(b)
				sb.WriteByte('\n')
			}
			if !send("iopub", "stream", map[string]any{"name": "stdout", "text": sb.String()}, "") {
				return
			}
		case a.StdoutRaw != nil:
			if !send("iopub", "stream", map[string]any{"name": "stdout", "text": rs.subL(*a.StdoutRaw)}, "") {
				return
			}
		case a.Close:
			return
		}
	}
	// Wait for the client to hang up.
	for {
		if _, _, err := c.ReadMessage(ctx); err != nil {
			return
		}
	}
}

func (rs *replayServer) addFailure(format string, args ...any) {
	rs.mu.Lock()
	rs.failures = append(rs.failures, fmt.Sprintf(format, args...))
	rs.mu.Unlock()
}

func (rs *replayServer) subL(s string) string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.sub(s)
}

func (rs *replayServer) subAny(v any) any {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.subAnyLocked(v)
}

func (rs *replayServer) subAnyLocked(v any) any {
	switch x := v.(type) {
	case string:
		return rs.sub(x)
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = rs.subAnyLocked(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = rs.subAnyLocked(e)
		}
		return out
	}
	return v
}
