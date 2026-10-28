// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/bridge"
)

func TestCookieHeader(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cs := []bridge.Cookie{
		{Name: "host", Value: "1", Domain: "media.example.edu", Path: "/", HostOnly: true},
		{Name: "dom", Value: "2", Domain: ".example.edu", Path: "/"},
		{Name: "deep", Value: "3", Domain: "example.edu", Path: "/sessions"},
		{Name: "sec", Value: "4", Domain: "example.edu", Path: "/", Secure: true},
		{Name: "old", Value: "5", Domain: "example.edu", Path: "/", Expires: now.Unix() - 1},
		{Name: "fresh", Value: "6", Domain: "example.edu", Path: "/", Expires: now.Unix() + 60},
		{Name: "foreign", Value: "7", Domain: "cloudfront.net", Path: "/"},
		{Name: "prefix", Value: "8", Domain: "example.edu", Path: "/sess"},
	}
	cases := map[string]string{
		"https://media.example.edu/sessions/a.mp4": "deep=3; host=1; dom=2; sec=4; fresh=6",
		"http://media.example.edu/sessions/a.mp4":  "deep=3; host=1; dom=2; fresh=6",
		"https://cdn.example.edu/other":            "dom=2; sec=4; fresh=6",
		"https://example.edu/sessions":             "deep=3; dom=2; sec=4; fresh=6",
		"https://notexample.edu/":                  "",
		"https://d1.cloudfront.net/x":              "foreign=7",
	}
	for u, want := range cases {
		if got := cookieHeader(u, cs, now); got != want {
			t.Errorf("%s: %q, want %q", u, got, want)
		}
	}
	h := requestHeaders("https://d1.cloudfront.net/x", nil, "https://lms.example.edu/v?id=1", now)
	if len(h) != 1 || h[0] != "Referer: https://lms.example.edu/" {
		t.Fatalf("no cookies (Panopto on CloudFront): %v", h)
	}
	if !allExpired(cs[4:5], now) || allExpired(cs, now) || allExpired(nil, now) {
		t.Fatal("allExpired")
	}
}

func TestValidCookieAndURL(t *testing.T) {
	good := bridge.Cookie{Name: "sid", Value: "a=b%20c", Domain: "x.edu", Path: "/"}
	if !validCookie(good) {
		t.Fatal("good cookie refused")
	}
	for _, c := range []bridge.Cookie{
		{Name: "", Value: "x", Domain: "x"},
		{Name: "a;b", Value: "x", Domain: "x"},
		{Name: "a", Value: "x\r\nX-Evil: 1", Domain: "x"},
		{Name: "a", Value: "x; other=1", Domain: "x"},
		{Name: "a b", Value: "x", Domain: "x"},
	} {
		if validCookie(c) {
			t.Errorf("accepted %q=%q", c.Name, c.Value)
		}
	}
	for u, ok := range map[string]bool{
		"https://x.edu/a?b=1": true, "http://127.0.0.1:8/a": true, "ftp://x/a": false, "/relative": false,
		"https://x.edu/a\r\nX: y": false, "https://x.edu/a b": false, "": false,
	} {
		if validHTTPURL(u) != ok {
			t.Errorf("%q: %v", u, !ok)
		}
	}
}

// The credentials never show through formatting or redaction helpers.
func TestRedactText(t *testing.T) {
	msg := "The response status is not successful. status=403 URI=https://d1.cloudfront.net/a/b.mp4?Signature=" + testSig + "&Key-Pair-Id=K"
	got := redactText(msg)
	// ruling 3: the path is a credential too; host and path hash only
	if strings.Contains(got, testSig) || strings.Contains(got, "/a/b.mp4") ||
		!strings.Contains(got, "https://d1.cloudfront.net (path "+pathHash("/a/b.mp4")+")") {
		t.Fatalf("%s", got)
	}
	tk := &task{cookies: []bridge.Cookie{{Name: "sid", Value: testCookie}}, tracks: []liveTrack{{url: "https://x/a?Signature=" + testSig}}}
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		if s := fmt.Sprintf(verb, tk.cookies); strings.Contains(s, testCookie) {
			t.Fatalf("%s leaks the cookie: %s", verb, s)
		}
	}
	tk.forget()
	if tk.hasCreds() || tk.cookies != nil || tk.referer != "" {
		t.Fatal("forget kept credentials")
	}
}

func TestFileStemAndTitle(t *testing.T) {
	cases := map[string]string{
		"Lecture 3: Graphs / Trees": "Lecture 3 Graphs Trees",
		"  CON ":                    "CON_",
		"a<b>c|d?e*f\"g":            "a b c d e f g",
		"...":                       "lecture",
		"":                          "lecture",
		"Week 1.":                   "Week 1",
		"讲座 第三讲：图论":                 "讲座 第三讲：图论",
	}
	for in, want := range cases {
		if got := fileStem(in); got != want {
			t.Errorf("fileStem(%q) = %q, want %q", in, got, want)
		}
	}
	if got := cleanTitle("  a\tb\n c\x00 "); got != "a b c" {
		t.Fatalf("cleanTitle %q", got)
	}
	long := strings.Repeat("长", 300)
	if n := len([]rune(fileStem(long))); n != 120 {
		t.Fatalf("stem length %d", n)
	}
}
