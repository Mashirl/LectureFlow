// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package bridge

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// Native fuzz tests (go test -fuzz=FuzzXxx ./internal/bridge). Under a plain
// go test they run their seeds only.

// frame returns msg with its length prefix.
func frame(msg string) []byte {
	b := make([]byte, 4+len(msg))
	binary.LittleEndian.PutUint32(b, uint32(len(msg)))
	copy(b[4:], msg)
	return b
}

// FuzzReadFrame feeds ReadFrame arbitrary streams: it never panics, never
// returns more than max bytes, reports io.EOF only for an empty stream, and
// what it returns is exactly the bytes after the prefix.
func FuzzReadFrame(f *testing.F) {
	f.Add(frame(`{"type":"hello","id":"1"}`), uint16(100))
	f.Add(frame(``), uint16(0))
	f.Add(frame(`{}`), uint16(1))
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, '{'}, uint16(4096))
	f.Add([]byte{2, 0}, uint16(10))
	f.Add([]byte{5, 0, 0, 0, 'a', 'b'}, uint16(10))
	f.Add(append(frame(`{"type":"check"}`), frame(`x`)...), uint16(64))
	f.Fuzz(func(t *testing.T, in []byte, max16 uint16) {
		max := int(max16)
		got, err := ReadFrame(bytes.NewReader(in), max)
		switch {
		case len(in) == 0:
			if err != io.EOF {
				t.Fatalf("empty stream: %v, want io.EOF", err)
			}
		case len(in) < 4:
			if err == nil || err == io.EOF || !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("short prefix: %v", err)
			}
		default:
			n := binary.LittleEndian.Uint32(in)
			switch {
			case int64(n) > int64(max):
				if !errors.Is(err, ErrFrameTooLarge) {
					t.Fatalf("length %d over %d: %v", n, max, err)
				}
			case int64(len(in)-4) < int64(n):
				if err == nil || err == io.EOF || !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("truncated body: %v", err)
				}
			default:
				if err != nil {
					t.Fatalf("whole frame: %v", err)
				}
				if !bytes.Equal(got, in[4:4+n]) {
					t.Fatal("frame body differs from the stream")
				}
			}
		}
		if len(got) > max {
			t.Fatalf("%d bytes returned, max %d", len(got), max)
		}
		if err == nil {
			// WriteFrame writes the same bytes back.
			var buf bytes.Buffer
			if err := WriteFrame(&buf, got, max); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(buf.Bytes(), in[:4+len(got)]) {
				t.Fatal("WriteFrame(ReadFrame(x)) != x")
			}
		}
	})
}

// requestTypes are the decoders of every request the app reads.
func requestTypes() []any {
	return []any{&Envelope{}, &HelloRequest{}, &CheckRequest{}, &EnqueueRequest{}, &RefreshRequest{}, &OpenRequest{}}
}

// FuzzDecodeRequest decodes arbitrary JSON into every request type: no
// panic, and a decoded request survives an encode / decode round trip.
func FuzzDecodeRequest(f *testing.F) {
	f.Add([]byte(`{"type":"hello","id":"1","protocol":1,"extension":{"version":"0.1.0","browser":"chrome","locale":"zh-CN"},"noLaunch":true}`))
	f.Add([]byte(`{"type":"check","id":"2","platform":"panopto","sessionIds":["a","b"],"titles":["x",""],"courseId":"01J"}`))
	f.Add([]byte(`{"type":"enqueue","id":"4","profile":{"id":"panopto","version":"1.0.0","reviewed":true},"platform":"panopto","sessionId":"s1","lecture":{"title":"L1","recordedAt":"2026-10-20T10:00","durationSec":3000},"courseId":"01JC0URSE","tracks":[{"url":"https://x/a.mp4","kind":"audio","format":"file","role":"primary-audio","label":"Lecture audio","sizeBytes":-1,"selector":"audio:en:0"}],"referer":"https://x/v","cookies":[{"name":"a","value":"b","domain":"x","path":"/","expires":0,"secure":true,"httpOnly":true,"hostOnly":false}],"maxConnections":4,"overrideDisabled":false}`))
	f.Add([]byte(`{"type":"refresh","id":"5","platform":"p","sessionId":"s","tracks":[],"referer":"","cookies":null}`))
	f.Add([]byte(`{"type":"open","id":"6","lectureId":"01JL","taskId":7}`))
	f.Add([]byte(`{"type":7,"id":"x"}`))
	f.Add([]byte(`{"type":"enqueue","tracks":[{"sizeBytes":1e400}]}`))
	f.Add([]byte(`{"type":"check","sessionIds":"\ud800"}`))
	f.Fuzz(func(t *testing.T, in []byte) {
		for _, v := range requestTypes() {
			if json.Unmarshal(in, v) != nil {
				continue
			}
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("%T: re-encode: %v", v, err)
			}
			again := reflect.New(reflect.TypeOf(v).Elem()).Interface()
			if err := json.Unmarshal(b, again); err != nil {
				t.Fatalf("%T: decode of own encoding: %v\n%s", v, err, b)
			}
			b2, _ := json.Marshal(again)
			if !bytes.Equal(b, b2) {
				t.Fatalf("%T: round trip differs:\n%s\n%s", v, b, b2)
			}
		}
	})
}

// FuzzRedactURL: the only form of a URL a log may show keeps scheme, host
// and path; nothing of the query, the fragment or the user info.
func FuzzRedactURL(f *testing.F) {
	for _, s := range []string{
		"https://u:p@x.panopto.eu/Panopto/Podcast/a.mp4?token=t#f",
		"https://cdn.example.edu/a%20b/c.m3u8",
		"not a url", "/relative/path", "https://h/p%3Fq?x#y", "https://[fe80::1%25en0]:8443/a?b",
		"HTTPS://User@Host/A?b", "https://h/a?b?c#d#e", "a+b://user:pw@h/p?q", "https://h/%zz?q",
		"https://h/p;jsessionid=x?y", "https://user@h", "//h/p?q",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		out := RedactURL(raw)
		if out == "(invalid url)" {
			return
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			t.Fatalf("RedactURL(%q) = %q for a URL that does not parse", raw, out)
		}
		if strings.ContainsAny(out, "?#") {
			t.Fatalf("RedactURL(%q) = %q keeps a query or fragment", raw, out)
		}
		rest, ok := strings.CutPrefix(out, u.Scheme+"://")
		if !ok {
			t.Fatalf("RedactURL(%q) = %q lost the scheme", raw, out)
		}
		authority, _, _ := strings.Cut(rest, "/")
		if strings.Contains(authority, "@") || authority != u.Host {
			t.Fatalf("RedactURL(%q) = %q: authority %q, host %q", raw, out, authority, u.Host)
		}
		// (a zone or an escaped host may not parse again; when it does,
		// nothing but scheme, host and path is left)
		if r, err := url.Parse(out); err == nil && (r.User != nil || r.RawQuery != "" || r.Fragment != "" || r.ForceQuery) {
			t.Fatalf("RedactURL(%q) = %q keeps user info, query or fragment", raw, out)
		}
		if pw, ok := u.User.Password(); ok && len(pw) >= 4 && !strings.Contains(u.Host+u.EscapedPath()+u.Scheme, pw) &&
			strings.Contains(out, pw) {
			t.Fatalf("RedactURL(%q) = %q keeps the password", raw, out)
		}
	})
}

// FuzzFormatNoSecrets: whatever a request holds, %v, %+v, %#v and %s of it
// never show a cookie value, nor the path or the query of a track URL.
func FuzzFormatNoSecrets(f *testing.F) {
	f.Add([]byte(`{"type":"enqueue","id":"7","tracks":[{"url":"https://cdn.example.edu/s/abc/master.m3u8","kind":"video","format":"hls"}],"cookies":[{"name":"sid","value":"x","domain":"example.edu"}]}`), "https://cdn.example.edu/s/a.m3u8")
	f.Add([]byte(`{"type":"refresh","tracks":[{"url":"x"},{"url":"y"}],"cookies":[{},{}]}`), "https://u:p@h/p#f")
	f.Add([]byte(`{}`), "%")
	f.Fuzz(func(t *testing.T, in []byte, trackURL string) {
		const cookieSecret = "C00KIESECRET9a7e"
		const querySecret = "SIGSECRET5c1d"
		const pathSecret = "P4THSECRET0b2e"
		var req EnqueueRequest
		if json.Unmarshal(in, &req) != nil {
			return
		}
		if strings.Contains(string(in), cookieSecret) || strings.Contains(string(in), querySecret) ||
			strings.Contains(trackURL, cookieSecret) || strings.Contains(trackURL, querySecret) ||
			strings.Contains(string(in), pathSecret) || strings.Contains(trackURL, pathSecret) {
			return
		}
		for i := range req.Cookies {
			req.Cookies[i].Value = cookieSecret
		}
		if len(req.Tracks) == 0 {
			req.Tracks = []Track{{}}
		}
		full := trackURL + "/" + pathSecret + "?Signature=" + querySecret
		for i := range req.Tracks {
			req.Tracks[i].URL = full
		}
		secrets := []string{cookieSecret, querySecret, pathSecret}
		if u, err := url.Parse(full); err == nil && strings.Contains(u.Host, pathSecret) {
			secrets = secrets[:2] // ("//" before it: the host, which may be shown)
		}
		ref := RefreshRequest{Envelope: req.Envelope, Tracks: req.Tracks, Cookies: req.Cookies}
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			for _, v := range []any{req, &req, ref, &ref, req.Tracks, req.Cookies, req.Tracks[0]} {
				out := fmt.Sprintf(verb, v)
				if strings.Contains(out, secrets[0]) || strings.Contains(out, secrets[1]) || (len(secrets) > 2 && strings.Contains(out, secrets[2])) {
					t.Fatalf("%s of %T leaks a secret: %s", verb, v, out)
				}
			}
		}
	})
}
