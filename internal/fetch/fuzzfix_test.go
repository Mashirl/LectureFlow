// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"strings"
	"testing"
	"time"

	"lectureflow/internal/bridge"
	"lectureflow/internal/calendar"
)

// Regression tests of what the fuzz tests found (NOTES "fuzz").

// The local playlist that the bundled ffmpeg reads (file protocol allowed)
// names only the downloaded files, however the server wrote its lines.
func TestLocalPlaylistNamesOnlyLocalFiles(t *testing.T) {
	base := mustURL(t, "https://media.example.edu/s/abc/v.m3u8?Policy=x")
	cases := map[string]struct{ pl, want, not string }{
		"unquoted map URI": {"#EXTM3U\n#EXT-X-MAP:URI=/etc/passwd\n#EXTINF:6,\ns1.mp4\n#EXT-X-ENDLIST\n",
			"#EXT-X-MAP:URI=\"m001.mp4\"\n", "passwd"},
		"lower-case key URI": {"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,uri=\"k.bin\",IV=0X0a\n#EXTINF:6,\ns1.ts\n#EXT-X-ENDLIST\n",
			"#EXT-X-KEY:METHOD=AES-128,URI=\"k001.key\",IV=0X0a\n", "k.bin"},
		"escaped quote": {"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k.bin\",URI=\\\"/etc/passwd\"\n#EXTINF:6,\ns1.ts\n#EXT-X-ENDLIST\n",
			"#EXT-X-KEY:METHOD=AES-128,URI=\"k001.key\"\n", "passwd"},
		"key NONE with a URI": {"#EXTM3U\n#EXT-X-KEY:METHOD=NONE,URI=\"/etc/passwd\"\n#EXTINF:6,\ns1.ts\n#EXT-X-ENDLIST\n",
			"#EXT-X-KEY:METHOD=NONE\n", "passwd"},
		"rendition in a media playlist": {"#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",URI=\"/etc/passwd\"\n# a comment /etc/passwd\n#EXTINF:6,\ns1.ts\n#EXT-X-ENDLIST\n",
			"#EXTINF:6,\ns00001.ts\n", "passwd"},
		// a lone CR ends a line for ffmpeg: it does here too
		"lone CR": {"#EXTM3U\n#EXTINF:6,\r/etc/passwd\ns1.ts\n#EXT-X-ENDLIST\n", "#EXTINF:6,\ns00001.ts\ns00002.ts\n", "passwd"},
		// byte ranges are written out with explicit offsets, as parsed
		"byte ranges": {"#EXTM3U\n#EXT-X-MAP:URI=\"f.mp4\",BYTERANGE=\"10@0\"\n#EXTINF:1,\n#EXT-X-BYTERANGE:20\nf.mp4\n#EXT-X-ENDLIST\n",
			"#EXT-X-MAP:URI=\"m001.mp4\",BYTERANGE=\"10@0\"\n#EXTINF:1,\n#EXT-X-BYTERANGE:20@10\nm001.mp4\n", "f.mp4"},
	}
	for name, c := range cases {
		m, err := parseMedia([]byte(c.pl), base)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		local := string(m.local())
		if !strings.Contains(local, c.want) || strings.Contains(local, c.not) || strings.Contains(local, "\r") {
			t.Errorf("%s: local playlist\n%s", name, local)
		}
	}
	for name, pl := range map[string]string{
		"NUL":    "#EXTM3U\n#EXTINF:6,\x00/etc/passwd\ns1.ts\n#EXT-X-ENDLIST\n",
		"bad IV": "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k\",IV=0x0g\n#EXTINF:6,\ns1.ts\n#EXT-X-ENDLIST\n",
	} {
		if _, err := parseMedia([]byte(pl), base); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// "http:" alone resolves to a URL without a host: not usable.
func TestResolveNeedsHost(t *testing.T) {
	base := mustURL(t, "https://m.example.edu/a/b.m3u8")
	for _, ref := range []string{"http:", "https:", "http:x.ts", "http:///x.ts", "//::"} {
		if u, err := resolve(base, ref); err == nil {
			t.Errorf("resolve(%q) = %q", ref, u)
		}
		if u, err := dashResolve(base, ref); err == nil {
			t.Errorf("dashResolve(%q) = %q", ref, u)
		}
	}
}

// A byte range whose end does not fit an int64 is malformed.
func TestByteRangeOverflow(t *testing.T) {
	base := mustURL(t, "https://m.example.edu/a/b.m3u8")
	for _, pl := range []string{
		"#EXTM3U\n#EXT-X-BYTERANGE:9223372036854775807@1\nf.mp4\n#EXT-X-ENDLIST\n",
		"#EXTM3U\n#EXT-X-BYTERANGE:9223372036854775807@0\nf.mp4\n#EXT-X-BYTERANGE:1\nf.mp4\n#EXT-X-ENDLIST\n",
		"#EXTM3U\n#EXT-X-MAP:URI=\"i.mp4\",BYTERANGE=\"9223372036854775807@1\"\n#EXTINF:1,\ns.mp4\n#EXT-X-ENDLIST\n",
	} {
		if _, err := parseMedia([]byte(pl), base); err == nil {
			t.Errorf("accepted %q", pl)
		}
	}
	if r, err := parseRange("9223372036854775807@0", -1); err != nil || r.length != 9223372036854775807 {
		t.Fatalf("the largest range: %+v %v", r, err)
	}
}

// Relative URIs take the playlist's query: a long query times many
// segments is refused before it takes gigabytes; so is an MPD template
// that expands a long Representation id into many segments.
func TestPlanBounds(t *testing.T) {
	long := mustURL(t, "https://m.example.edu/a/b.m3u8?sig="+strings.Repeat("x", 1<<20))
	pl := "#EXTM3U\n" + strings.Repeat("#EXTINF:1,\ns.ts\n", 40) + "#EXT-X-ENDLIST\n"
	if _, err := parseMedia([]byte(pl), long); err == nil || !strings.Contains(err.Error(), "too many URLs") {
		t.Fatalf("media: %v", err)
	}
	master := "#EXTM3U\n" + strings.Repeat("#EXT-X-STREAM-INF:BANDWIDTH=1\nv.m3u8\n", 40)
	if _, err := parseMaster([]byte(master), long); err == nil || !strings.Contains(err.Error(), "too many URLs") {
		t.Fatalf("master: %v", err)
	}
	many := "#EXTM3U\n" + strings.Repeat("#EXTINF:1,\ns.ts\n", maxHLSSegments+1) + "#EXT-X-ENDLIST\n"
	if _, err := parseMedia([]byte(many), mustURL(t, "https://m.example.edu/a/b.m3u8")); err == nil || !strings.Contains(err.Error(), "too many segments") {
		t.Fatalf("segments: %v", err)
	}
	mpdBody := `<MPD mediaPresentationDuration="PT100000S"><Period><AdaptationSet contentType="video"><SegmentTemplate duration="1" media="$RepresentationID$/$Number$"/><Representation id="` +
		strings.Repeat("i", 2000) + `"/></AdaptationSet></Period></MPD>`
	m := mustMPD(t, mpdBody, "https://m.example.edu/x.mpd")
	s, _ := m.find(bridge.KindVideo, "")
	if _, err := m.source(s); err == nil || !strings.Contains(err.Error(), "too many URLs") {
		t.Fatalf("MPD: %v", err)
	}
	if _, err := parseMPD([]byte("<MPD>"+strings.Repeat("<a/>", maxMPDElements)+"</MPD>"), mustURL(t, "https://m.example.edu/x.mpd")); err == nil ||
		!strings.Contains(err.Error(), "too large") {
		t.Fatalf("elements: %v", err)
	}
}

// A domain cookie never goes to an IP address that merely ends like it.
func TestCookieNotToIPSuffix(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cs := []bridge.Cookie{{Name: "a", Value: "1", Domain: "0.0.1", Path: "/"}, {Name: "b", Value: "2", Domain: "127.0.0.1", Path: "/"}}
	if got := cookieHeader("https://127.0.0.1/x", cs, now); got != "b=2" {
		t.Fatalf("%q", got)
	}
	if got := cookieHeader("https://[::1]/x", []bridge.Cookie{{Name: "c", Value: "3", Domain: "::1"}}, now); got != "c=3" {
		t.Fatalf("IPv6 host itself: %q", got)
	}
}

// A URL in a message is redacted whatever the case of its scheme.
func TestRedactTextAnyCase(t *testing.T) {
	got := redactText(`Get "HTTPS://Cdn.Example.edu/a/b.mp4?Signature=` + testSig + `": EOF`)
	if strings.Contains(got, testSig) || strings.Contains(got, "/a/b.mp4") || !strings.Contains(got, "(path ") {
		t.Fatalf("%s", got)
	}
}

// A device name is one before an extension or trailing spaces too; the
// "_" goes right after it.
func TestFileStemDeviceNames(t *testing.T) {
	for in, want := range map[string]string{
		"CON.mp4": "CON_.mp4", "aux .x": "aux_.x", "COM¹": "COM¹_", "lpt0.tar.gz": "lpt0_.tar.gz", "CONIN$": "CONIN$_",
		"Console.mp4": "Console.mp4", "NULL": "NULL", "com10": "com10",
	} {
		if got := fileStem(in); got != want {
			t.Errorf("fileStem(%q) = %q, want %q", in, got, want)
		}
	}
}

// The stated clock is kept (tz-fix), so an offset can no longer push a time
// past what lecture.recorded_at holds: the two values that did so when
// they were converted to the computer's zone (fuzz) keep their own clock
// and read back.
func TestRecordedAtOutOfRange(t *testing.T) {
	for s, want := range map[string]string{
		"9999-12-31T23:59:59-23:59": "9999-12-31T23:59",
		"0000-01-01T00:00:00+01:00": "0000-01-01T00:00",
		"2026-10-20T10:00:00+01:00": "2026-10-20T10:00",
	} {
		v := recordedAt(s)
		if v == nil || *v != want {
			t.Fatalf("recordedAt(%q) = %v, want %s", s, v, want)
		}
		if p, err := calendar.ParseRecordedAt(*v, time.UTC); err != nil || calendar.FormatRecordedAt(p) != want {
			t.Errorf("%s reads back as %v, %v", want, p, err)
		}
	}
}
