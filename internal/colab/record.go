// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// Recorder captures a real session (cmd/record, run only with the user's
// consent) in the replay format, redacted: tokens, secrets and the XSRF
// tokens become placeholders, the runtime URL {{runtime_url}}, uploaded
// file contents {{any}}, the downloaded result a sanitised copy (every
// text replaced) whose hash the done event refers to, e-mail addresses
// user@example.com. Recorded files replay like the constructed ones
// (TestRecorderRoundTrip).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
)

// Recorder records HTTP exchanges and kernel channel messages.
type Recorder struct {
	mu       sync.Mutex
	colab    string
	token    string
	revoke   string
	runtimes []string // runtime proxy base URLs seen in assignments
	steps    []ReplayStep
	vars     map[string]string
	secrets  secretSet
	n        int
}

// NewRecorder records against these service URLs (the Backend's).
func NewRecorder(colabURL, tokenURL, revokeURL string) *Recorder {
	return &Recorder{colab: strings.TrimRight(colabURL, "/"), token: tokenURL, revoke: revokeURL, vars: map[string]string{}}
}

var (
	emailRe     = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	doneShaRe   = regexp.MustCompile(`"sha256":"[0-9a-f]{64}"`)
	doneBytesRe = regexp.MustCompile(`"bytes":[0-9]+`)
)

// pointDoneAtResult makes the done event of stdout text refer to the
// sanitised result (var result_json).
func pointDoneAtResult(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if strings.Contains(l, `"type":"done"`) {
			l = doneShaRe.ReplaceAllString(l, `"sha256":"{{sha256:result_json}}"`)
			lines[i] = doneBytesRe.ReplaceAllString(l, `"bytes":{{len:result_json}}`)
		}
	}
	return strings.Join(lines, "\n")
}

// secretKeys are JSON keys whose values are secrets in answers.
var secretKeys = map[string]bool{"access_token": true, "refresh_token": true, "id_token": true, "token": true, "client_secret": true}

// secretParams are query and form keys whose values are secrets or
// per-run identifiers.
var secretParams = map[string]bool{"colab-runtime-proxy-token": true, "token": true, "session_id": true, "nbh": true,
	"client_secret": true, "refresh_token": true, "client_id": true}

func (r *Recorder) hostPath(u *url.URL) (string, string) {
	full := u.Scheme + "://" + u.Host + u.EscapedPath()
	switch {
	case full == r.token:
		return "oauth", "/token"
	case full == r.revoke:
		return "oauth", "/revoke"
	case strings.HasPrefix(full, r.colab+"/"):
		return "colab", strings.TrimPrefix(full, r.colab)
	}
	for _, rt := range r.runtimes {
		if strings.HasPrefix(full, rt+"/") {
			return "runtime", strings.TrimPrefix(full, rt)
		}
	}
	return "?", u.EscapedPath()
}

// redact applies the secret list and the e-mail rule.
func (r *Recorder) redact(s string) string {
	return emailRe.ReplaceAllString(r.secrets.scrub(s), "user@example.com")
}

// sanitizeJSON replaces secret values (remembering them) and learns
// runtime URLs.
func (r *Recorder) sanitizeJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			if s, ok := e.(string); ok && secretKeys[k] && s != "" {
				r.secrets.add(s)
				r.n++
				out[k] = "redacted-" + k
				continue
			}
			if s, ok := e.(string); ok && k == "url" {
				if u, err := url.Parse(s); err == nil && u.Host != "" {
					base := strings.TrimRight(s, "/")
					if !containsStr(r.runtimes, base) {
						r.runtimes = append(r.runtimes, base)
					}
					out[k] = "{{runtime_url}}"
					continue
				}
			}
			out[k] = r.sanitizeJSON(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = r.sanitizeJSON(e)
		}
		return out
	case string:
		return r.redact(x)
	}
	return v
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// sanitizeResult keeps result.json's structure and replaces every text.
func sanitizeResult(data []byte) string {
	var res map[string]any
	if json.Unmarshal(data, &res) != nil {
		return `{"prompt_terms_used":0,"sentences":[]}`
	}
	if ss, ok := res["sentences"].([]any); ok {
		for _, s := range ss {
			m, ok := s.(map[string]any)
			if !ok {
				continue
			}
			m["text"] = "[redacted]"
			for _, key := range []string{"words", "low_conf_words"} {
				if ws, ok := m[key].([]any); ok {
					for _, w := range ws {
						if wm, ok := w.(map[string]any); ok {
							wm["w"] = "x"
						}
					}
				}
			}
		}
	}
	b, _ := json.Marshal(res)
	return string(b)
}

type recTransport struct {
	r    *Recorder
	base http.RoundTripper
}

// Transport wraps base (nil = http.DefaultTransport).
func (r *Recorder) Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &recTransport{r: r, base: base}
}

func (t *recTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(rb))
	t.r.add(req, body, resp, rb)
	return resp, nil
}

func (r *Recorder) add(req *http.Request, body []byte, resp *http.Response, rb []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Request-side secrets first, so that answers echoing them are caught.
	for _, h := range []string{"Authorization", "X-Colab-Runtime-Proxy-Token", "X-Goog-Colab-Token"} {
		if v := req.Header.Get(h); v != "" {
			r.secrets.add(strings.TrimPrefix(v, "Bearer "))
		}
	}
	q := req.URL.Query()
	form, _ := url.ParseQuery(string(body))
	for k := range secretParams {
		r.secrets.add(q.Get(k))
		if strings.Contains(req.Header.Get("Content-Type"), "form") {
			r.secrets.add(form.Get(k))
		}
	}
	host, path := r.hostPath(req.URL)
	st := ReplayStep{Name: req.Method + " " + host + " " + endpointLabel(path)}
	rq := &ReplayRequest{Method: req.Method, Host: host, Path: r.redact(path)}
	if len(q) > 0 {
		rq.Query = map[string]string{}
		for k := range q {
			if secretParams[k] {
				rq.Query[k] = "{{any}}"
			} else {
				rq.Query[k] = r.redact(q.Get(k))
			}
		}
	}
	for _, h := range []string{"Authorization", "X-Colab-Runtime-Proxy-Token", "X-Goog-Colab-Token", "X-Colab-Client-Agent"} {
		if v := req.Header.Get(h); v != "" {
			if rq.Headers == nil {
				rq.Headers = map[string]string{}
			}
			if h == "X-Colab-Client-Agent" {
				rq.Headers[h] = v
			} else if h == "Authorization" {
				rq.Headers[h] = "Bearer {{any}}"
			} else {
				rq.Headers[h] = "{{any}}"
			}
		}
	}
	if strings.Contains(req.Header.Get("Content-Type"), "form") {
		rq.Form = map[string]string{}
		for k := range form {
			if secretParams[k] {
				rq.Form[k] = "{{any}}"
			} else {
				rq.Form[k] = r.redact(form.Get(k))
			}
		}
	} else if len(body) > 0 {
		var m map[string]any
		if json.Unmarshal(body, &m) == nil {
			if _, ok := m["content"]; ok {
				m["content"] = "{{any}}" // uploaded file data
			}
			rq.JSON, _ = r.sanitizeJSON(m).(map[string]any)
		}
	}
	st.Request = rq
	rs := &ReplayResponse{Status: resp.StatusCode}
	trimmed := bytes.TrimPrefix(rb, []byte(xssiPrefix))
	var v any
	switch {
	case host == "runtime" && req.Method == http.MethodGet && strings.Contains(path, "/api/contents/") && json.Unmarshal(rb, &v) == nil:
		if m, ok := v.(map[string]any); ok {
			if c, ok := m["content"].(string); ok && m["format"] == "base64" {
				data, _ := base64.StdEncoding.DecodeString(c)
				r.vars["result_json"] = sanitizeResult(data)
				m["content"] = "{{b64:result_json}}"
				v = m
			}
		}
		rs.JSON = r.sanitizeJSON(v)
	case len(trimmed) > 0 && json.Unmarshal(trimmed, &v) == nil:
		rs.JSON = r.sanitizeJSON(v)
		rs.XSSI = len(trimmed) != len(rb)
	default:
		rs.Body = r.redact(string(rb))
	}
	st.Response = rs
	r.steps = append(r.steps, st)
}

func endpointLabel(path string) string {
	for _, p := range []string{"/tun/m/assignments", "/tun/m/assign", "/tun/m/unassign", "/tun/m/ccu-info", "/api/contents", "/api/kernels", "/token", "/revoke"} {
		if strings.HasPrefix(path, p) {
			return p
		}
	}
	return "?"
}

// Dialer wraps base so that kernel channel messages are recorded.
func (r *Recorder) Dialer(base Dialer) Dialer {
	return func(ctx context.Context, rawURL string, header http.Header) (MessageConn, error) {
		conn, err := base(ctx, rawURL, header)
		if err != nil {
			return nil, err
		}
		u, _ := url.Parse(strings.Replace(rawURL, "ws", "http", 1))
		r.mu.Lock()
		for _, k := range []string{"token", "session_id"} {
			r.secrets.add(u.Query().Get(k))
		}
		_, path := r.hostPath(u)
		ws := &ReplayWS{Path: path, Query: map[string]string{}, Headers: map[string]string{}}
		for k := range u.Query() {
			ws.Query[k] = "{{any}}"
		}
		if header.Get("X-Colab-Runtime-Proxy-Token") != "" {
			ws.Headers["X-Colab-Runtime-Proxy-Token"] = "{{any}}"
		}
		r.steps = append(r.steps, ReplayStep{Name: "kernel channels", WebSocket: ws})
		r.mu.Unlock()
		return &recConn{r: r, ws: ws, MessageConn: conn}, nil
	}
}

type recConn struct {
	MessageConn
	r      *Recorder
	ws     *ReplayWS
	parent string
}

func (c *recConn) WriteText(ctx context.Context, data []byte) error {
	if m, err := decodeKernelMsg(false, data); err == nil {
		c.r.mu.Lock()
		c.parent, _ = m.Header["msg_id"].(string)
		exp := &ReplayKernelMsg{Channel: m.Channel, MsgType: m.msgType()}
		var content map[string]any
		if json.Unmarshal(m.Content, &content) == nil {
			if code, _ := content["code"].(string); strings.Contains(code, "colab_remote.py") {
				exp.ContentContains = map[string]string{"code": "colab_remote.py"}
			}
		}
		c.ws.Script = append(c.ws.Script, ReplayWSAction{Expect: exp})
		c.r.mu.Unlock()
	}
	return c.MessageConn.WriteText(ctx, data)
}

func (c *recConn) ReadMessage(ctx context.Context) (bool, []byte, error) {
	bin, data, err := c.MessageConn.ReadMessage(ctx)
	if err != nil {
		return bin, data, err
	}
	if m, derr := decodeKernelMsg(bin, data); derr == nil {
		c.r.mu.Lock()
		var content map[string]any
		_ = json.Unmarshal(m.Content, &content)
		act := ReplayWSAction{}
		parent := ""
		if m.parentID() != c.parent {
			parent = "other"
		}
		if m.msgType() == "stream" && parent == "" {
			if name, _ := content["name"].(string); name == "stdout" {
				text, _ := content["text"].(string)
				raw := c.r.redact(text) // events only (the contract), redacted anyway
				act.StdoutRaw = &raw
			} else {
				content = map[string]any{"name": name, "text": "(stderr)\n"}
			}
		}
		if act.StdoutRaw == nil {
			if m.msgType() == "stream" && parent == "other" {
				content = map[string]any{"name": content["name"], "text": "(other output)\n"}
			}
			if m.msgType() == "execute_input" {
				content = map[string]any{"code": "(the cell)", "execution_count": content["execution_count"]}
			}
			if m.msgType() == "error" || m.msgType() == "execute_reply" {
				if _, ok := content["traceback"]; ok {
					content["traceback"] = []any{}
				}
				if ev, ok := content["evalue"].(string); ok {
					content["evalue"] = c.r.redact(ev)
				}
			}
			sc, _ := c.r.sanitizeJSON(content).(map[string]any)
			act.Send = &ReplayKernelMsg{Channel: m.Channel, MsgType: m.msgType(), Content: sc, Parent: parent}
		}
		c.ws.Script = append(c.ws.Script, act)
		c.r.mu.Unlock()
	}
	return bin, data, err
}

// File returns the recording. The done event's hash and size refer to the
// sanitised result (var result_json).
func (r *Recorder) File(source string, upstream ReplayUpstream) ReplayFile {
	r.mu.Lock()
	defer r.mu.Unlock()
	steps := make([]ReplayStep, len(r.steps))
	copy(steps, r.steps)
	for i := range steps {
		ws := steps[i].WebSocket
		if ws == nil {
			continue
		}
		for j := range ws.Script {
			if raw := ws.Script[j].StdoutRaw; raw != nil && r.vars["result_json"] != "" {
				s := pointDoneAtResult(*raw)
				ws.Script[j].StdoutRaw = &s
			}
		}
	}
	vars := map[string]string{}
	for k, v := range r.vars {
		vars[k] = v
	}
	return ReplayFile{Source: source, Upstream: upstream, Recorded: true, Vars: vars, Steps: steps}
}
