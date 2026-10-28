// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package bridge

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	msgs := []string{`{"type":"hello","id":"1"}`, `{}`, ``}
	for _, m := range msgs {
		if err := WriteFrame(&buf, []byte(m), MaxToBrowser); err != nil {
			t.Fatal(err)
		}
	}
	// The length prefix is little-endian, as Chrome and Firefox send it.
	if got := buf.Bytes()[:4]; !bytes.Equal(got, []byte{25, 0, 0, 0}) {
		t.Fatalf("prefix = %v", got)
	}
	for _, want := range msgs {
		got, err := ReadFrame(&buf, MaxRequest)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("got %q want %q", got, want)
		}
	}
	if _, err := ReadFrame(&buf, MaxRequest); err != io.EOF {
		t.Fatalf("clean end: err = %v, want io.EOF", err)
	}
}

func TestFrameLimitsAndTruncation(t *testing.T) {
	if err := WriteFrame(io.Discard, make([]byte, 11), 10); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("write over max: %v", err)
	}
	var buf bytes.Buffer
	_ = WriteFrame(&buf, make([]byte, 11), 100)
	if _, err := ReadFrame(bytes.NewReader(buf.Bytes()), 10); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("read over max: %v", err)
	}
	for _, cut := range []int{2, 4, 9} {
		_, err := ReadFrame(bytes.NewReader(buf.Bytes()[:cut]), 100)
		if err == nil || err == io.EOF || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("cut at %d: err = %v, want unexpected EOF", cut, err)
		}
	}
}

// Cookie values and signed query strings must not reach logs through %v,
// %+v or %#v, including when nested in a request.
func TestNoSecretsInFormatting(t *testing.T) {
	req := EnqueueRequest{
		Envelope: Envelope{Type: TypeEnqueue, ID: "7"},
		Tracks:   []Track{{URL: "https://cdn.example.edu/s/abc/master.m3u8?Signature=SIGSECRET&Key-Pair-Id=K", Kind: KindVideo, Format: FormatHLS}},
		Cookies:  []Cookie{{Name: "sid", Value: "COOKIESECRET", Domain: "example.edu"}},
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		out := fmt.Sprintf(verb, req.Cookies) + fmt.Sprintf(verb, req.Tracks) + fmt.Sprintf(verb, req.Cookies[0])
		if strings.Contains(out, "COOKIESECRET") || strings.Contains(out, "SIGSECRET") {
			t.Fatalf("%s leaks a secret: %s", verb, out)
		}
	}
	// the path is a credential too (lead's ruling 3): host and path hash
	got := fmt.Sprintf("%v", req.Tracks[0])
	if strings.Contains(got, "/s/abc") || got != "video/hls https://cdn.example.edu (path "+pathHash16("/s/abc/master.m3u8")+")" {
		t.Fatalf("track string: %s", got)
	}
}

func pathHash16(p string) string {
	sum := sha256.Sum256([]byte(p))
	return hex.EncodeToString(sum[:8])
}

func TestLogURL(t *testing.T) {
	cases := map[string]string{
		"https://u:p@x.panopto.eu/Panopto/Podcast/a.mp4?token=t#f": "https://x.panopto.eu (path " + pathHash16("/Panopto/Podcast/a.mp4") + ")",
		"https://cdn.example.edu":                                  "https://cdn.example.edu (path " + pathHash16("/") + ")",
		"HTTPS://CDN.example.edu/A%20b":                            "https://CDN.example.edu (path " + pathHash16("/A%20b") + ")",
		"not a url":                                                "(invalid url)",
		"://u:p@h/x?sig=1":                                         "(invalid url)",
	}
	for in, want := range cases {
		if got := LogURL(in); got != want {
			t.Errorf("LogURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactURL(t *testing.T) {
	cases := map[string]string{
		"https://u:p@x.panopto.eu/Panopto/Podcast/a.mp4?token=t#f": "https://x.panopto.eu/Panopto/Podcast/a.mp4",
		"https://cdn.example.edu/a%20b/c.m3u8":                     "https://cdn.example.edu/a%20b/c.m3u8",
		"not a url":                                                "(invalid url)",
		"/relative/path":                                           "(invalid url)",
	}
	for in, want := range cases {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// The JSON names are the contract with the extension (ext-brief §3).
func TestWireNames(t *testing.T) {
	b, _ := json.Marshal(EnqueueReply{ReplyHeader: ReplyHeader{Type: TypeEnqueue, ID: "3", OK: true}, TaskID: 9, Position: 2})
	if got, want := string(b), `{"type":"enqueue","id":"3","ok":true,"taskId":9,"position":2}`; got != want {
		t.Fatalf("enqueue reply:\n got %s\nwant %s", got, want)
	}
	var req EnqueueRequest
	in := `{"type":"enqueue","id":"4","profile":{"id":"panopto","version":"1.0.0","reviewed":true},"platform":"panopto","sessionId":"s1","lecture":{"title":"L1","recordedAt":"2026-10-20T10:00","durationSec":3000},"courseId":"01JC0URSE","tracks":[{"url":"https://x/a.mp4","kind":"audio","format":"file","role":"primary-audio","label":"Lecture audio","sizeBytes":-1}],"referer":"https://x/v","cookies":[{"name":"a","value":"b","domain":"x","path":"/","expires":0,"secure":true,"httpOnly":true,"hostOnly":false}],"maxConnections":4,"overrideDisabled":false}`
	if err := json.Unmarshal([]byte(in), &req); err != nil {
		t.Fatal(err)
	}
	if req.ID != "4" || req.Profile.ID != "panopto" || req.SessionID != "s1" || req.CourseID != "01JC0URSE" ||
		req.Lecture.DurationSec != 3000 || len(req.Tracks) != 1 || req.Tracks[0].Role != "primary-audio" ||
		req.Tracks[0].SizeBytes != -1 || len(req.Cookies) != 1 || !req.Cookies[0].HTTPOnly || req.MaxConnections != 4 {
		t.Fatalf("decoded %+v", req)
	}
	b, _ = json.Marshal(ReplyHeader{Type: TypeCheck, ID: "5", Error: NewError(CodeDuplicate, map[string]any{"state": StateImported})})
	if got, want := string(b), `{"type":"check","id":"5","ok":false,"error":{"code":"queue.duplicate","params":{"state":"imported"}}}`; got != want {
		t.Fatalf("error reply:\n got %s\nwant %s", got, want)
	}
}
