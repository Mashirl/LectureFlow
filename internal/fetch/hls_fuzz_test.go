// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"math"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"lectureflow/internal/bridge"
)

// Native fuzz tests of the HLS parser (go test -fuzz=FuzzXxx
// ./internal/fetch). Under a plain go test they run their seeds only.

// fuzzBases are the playlist URLs the fuzzers pick from (a fuzzed base
// would rarely parse).
var fuzzBases = []string{
	"https://media.example.edu/s/abc/v720.m3u8?Policy=x&Signature=y",
	"https://d1abc.cloudfront.net/sessions/x/y.hls/index.m3u8",
	"http://u:p@127.0.0.1:8080/a/b/c.m3u8?",
}

func fuzzBase(t *testing.T, i uint8) *url.URL {
	u, err := url.Parse(fuzzBases[int(i)%len(fuzzBases)])
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// checkPlanURL: a URL the parser hands to aria2c is absolute http(s)
// without a fragment or a control character.
func checkPlanURL(t *testing.T, what, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Fragment != "" || hasCTL(raw) {
		t.Fatalf("%s: unusable URL %q (%v)", what, raw, err)
	}
	return u
}

// FuzzResolve: a URI of a playlist resolves to an http(s) URL, and the
// playlist's query (its signature) never travels to another host.
func FuzzResolve(f *testing.F) {
	for _, s := range []string{"seg1.ts", "/abs/seg.ts", "https://other.example.net/x.ts", "//other.example.net/x",
		"?q=1", "", "#f", "ftp://x/y", "seg.ts?own=1", "HTTPS://MEDIA.EXAMPLE.EDU/s/x.ts", " seg.ts ", "%zz",
		"//user@media.example.edu/x", "https://media.example.edu:443/x"} {
		f.Add(uint8(0), s)
	}
	f.Fuzz(func(t *testing.T, bi uint8, ref string) {
		base := fuzzBase(t, bi)
		out, err := resolve(base, ref)
		if err != nil {
			return
		}
		u := checkPlanURL(t, "resolve", out)
		if base.RawQuery != "" && strings.Contains(u.RawQuery, base.RawQuery) &&
			(!strings.EqualFold(u.Host, base.Host) || !strings.EqualFold(u.Scheme, base.Scheme)) {
			r, _ := url.Parse(strings.TrimSpace(ref))
			if !strings.Contains(r.RawQuery, base.RawQuery) {
				t.Fatalf("resolve(%q, %q) = %q: the playlist's query went to another host", base, ref, out)
			}
		}
	})
}

// FuzzParseMaster: a master playlist parses without panicking into
// variants and renditions with usable URLs, and pick always chooses one
// of them.
func FuzzParseMaster(f *testing.F) {
	for _, s := range []string{testMaster,
		"#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=112000,CODECS=\"mp4a.40.2\"\nindex.m3u8\n",
		"#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"x\",DEFAULT=YES\n#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"a\"\nv.m3u8\n",
		"#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=9223372036854775807,RESOLUTION=99999999999x99999999999\nv.m3u8\n#EXT-X-STREAM-INF:\nw.m3u8\n",
		"#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"g\nv.m3u8\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"g\",URI=\"a.m3u8\"\n",
		"\xef\xbb\xbf#EXTM3U\r\n#EXT-X-STREAM-INF:BANDWIDTH=1\r\nv.m3u8\r\n", "<html>login</html>", "",
	} {
		f.Add(uint8(0), []byte(s))
	}
	f.Fuzz(func(t *testing.T, bi uint8, body []byte) {
		base := fuzzBase(t, bi)
		m, err := parseMaster(body, base)
		if err != nil {
			return
		}
		if len(m.variants) == 0 {
			t.Fatal("no variants")
		}
		uris := map[string]bool{}
		planned := 0
		for _, v := range m.variants {
			checkPlanURL(t, "variant", v.uri)
			uris[v.uri] = true
			planned += len(v.uri)
		}
		for _, r := range m.renditions {
			if r.uri != "" {
				checkPlanURL(t, "rendition", r.uri)
				uris[r.uri] = true
				planned += len(r.uri)
			}
		}
		if planned > maxPlanBytes {
			t.Fatalf("%d bytes of URLs planned (max %d)", planned, maxPlanBytes)
		}
		for _, kind := range []string{bridge.KindAudio, bridge.KindVideo, bridge.KindUnknown} {
			media, extra := m.pick(kind)
			if !uris[media] || (extra != "" && !uris[extra]) {
				t.Fatalf("pick(%s) = %q, %q: not from the playlist", kind, media, extra)
			}
		}
	})
}

// ffLines splits a playlist as ffmpeg's HLS demuxer reads it: a line ends at
// "\n", "\r" or NUL, trailing white space is dropped.
func ffLines(s string) []string {
	lines := strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' || r == 0 })
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\v\f")
	}
	return lines
}

func ffSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}

// ffAttrs reads an attribute list as ffmpeg's ff_parse_key_value does:
// keys keep their "=" and their case, quoted values take backslash
// escapes, an unquoted value ends at white space or a comma, and the last
// value of a key wins.
func ffAttrs(s string) map[string]string {
	out := map[string]string{}
	i := 0
	for {
		for i < len(s) && (ffSpace(s[i]) || s[i] == ',') {
			i++
		}
		if i >= len(s) {
			return out
		}
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			return out
		}
		key := s[i : i+eq+1]
		i += eq + 1
		var v strings.Builder
		if i < len(s) && s[i] == '"' {
			for i++; i < len(s) && s[i] != '"'; {
				if s[i] == '\\' {
					if i+1 >= len(s) {
						break
					}
					v.WriteByte(s[i+1])
					i += 2
					continue
				}
				v.WriteByte(s[i])
				i++
			}
			if i < len(s) && s[i] == '"' {
				i++
			}
		} else {
			for ; i < len(s) && !ffSpace(s[i]) && s[i] != ','; i++ {
				v.WriteByte(s[i])
			}
		}
		out[key] = v.String()
	}
}

var localNameRe = regexp.MustCompile(`^(s\d{5,}|k\d{3,}|m\d{3,})\.[a-z0-9]{1,5}$`)

// FuzzParseMedia: a media playlist parses without panicking; every part has
// a usable URL and a plain local file name; the single-file fast path has
// consistent ranges; and the local playlist that the bundled ffmpeg reads
// (with the file protocol allowed) names only the downloaded files.
func FuzzParseMedia(f *testing.F) {
	pan, _ := panoptoMedia(3, 1000, false)
	for _, s := range []string{pan,
		"\xef\xbb\xbf#EXTM3U\r\n#EXT-X-VERSION:3\r\n#EXT-X-TARGETDURATION:6\r\n#EXT-X-KEY:METHOD=AES-128,URI=\"https://keys.example.edu/k?token=abc\",IV=0x01\r\n#EXTINF:6.0,\r\nseg1.ts\r\n#EXTINF:6.0,\r\nseg2.ts?x=1\r\n#EXT-X-KEY:METHOD=NONE\r\n#EXTINF:3.0,\r\n/abs/seg3.aac\r\n#EXT-X-ENDLIST\r\n",
		"#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:6,\ns1.ts\n",
		"#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"skd://x\"\n#EXTINF:6,\ns1.ts\n#EXT-X-ENDLIST\n",
		"#EXTM3U\n#EXT-X-BYTERANGE:9223372036854775807@0\nf.mp4\n#EXT-X-BYTERANGE:9223372036854775807\nf.mp4\n#EXT-X-ENDLIST\n",
		"#EXTM3U\n#EXT-X-MAP:URI=\"i.mp4\",BYTERANGE=\"9223372036854775807@1\"\n#EXT-X-BYTERANGE:1\ni.mp4\n#EXT-X-ENDLIST\n",
		// what ffmpeg reads differently from the parser
		"#EXTM3U\n#EXT-X-MAP:URI=/etc/passwd\n#EXTINF:6,\ns1.mp4\n#EXT-X-ENDLIST\n",
		"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,uri=\"k.bin\"\n#EXTINF:6,\ns1.ts\n#EXT-X-ENDLIST\n",
		"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k.bin\",URI=\\\"/etc/passwd\"\n#EXTINF:6,\ns1.ts\n#EXT-X-ENDLIST\n",
		"#EXTM3U\n#EXTINF:6,\r/etc/passwd\ns1.ts\n#EXT-X-ENDLIST\n",
		"#EXTM3U\n#EXTINF:6,\x00/etc/passwd\ns1.ts\n#EXT-X-ENDLIST\n",
		"#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",URI=\"/etc/passwd\"\n#EXTINF:6,\ns1.ts\n#EXT-X-ENDLIST\n",
		"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k\",IV=0x01,KEYFORMAT=\"identity\"\n#EXTINF:6,\ns1.ts\n#EXT-X-ENDLIST\n",
		"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k\",IV=\"x\"\n#EXTINF:6,\ns1.ts\n#EXT-X-ENDLIST\n",
		"#EXTM3U\n#EXTINF:6,\ns1.ts\n#EXT-X-BYTERANGE:10@5\n#EXTINF:6,\ns1.ts\n#EXT-X-ENDLIST\n",
	} {
		f.Add(uint8(0), []byte(s))
	}
	f.Fuzz(func(t *testing.T, bi uint8, body []byte) {
		base := fuzzBase(t, bi)
		m, err := parseMedia(body, base)
		if err != nil {
			return
		}
		if m.segments == 0 || m.segments > maxHLSSegments || len(m.ranges) != len(m.rangeURIs) || len(m.names) != len(m.resources) {
			t.Fatalf("segments %d, ranges %d/%d, names %d, resources %d", m.segments, len(m.ranges), len(m.rangeURIs), len(m.names), len(m.resources))
		}
		local := map[string]bool{}
		planned := 0
		for _, r := range m.resources {
			checkPlanURL(t, "resource", r)
			name := m.names[r]
			if !localNameRe.MatchString(name) || local[name] {
				t.Fatalf("local name %q of %q", name, r)
			}
			local[name] = true
		}
		for _, l := range m.lines {
			planned += len(l.uri)
		}
		if planned > maxPlanBytes {
			t.Fatalf("%d bytes of URLs planned (max %d)", planned, maxPlanBytes)
		}
		for _, r := range m.ranges {
			if r.ok && (r.off < 0 || r.length < 0 || r.off > math.MaxInt64-r.length) {
				t.Fatalf("range %+v", r)
			}
		}
		if uri, minSize, ok := m.singleFile(); ok {
			if uri != m.resources[0] || minSize <= 0 {
				t.Fatalf("single file %q of at least %d bytes", uri, minSize)
			}
			for _, r := range m.ranges {
				if r.off+r.length > minSize {
					t.Fatalf("range %+v beyond %d", r, minSize)
				}
			}
		}
		for _, line := range ffLines(string(m.local())) {
			switch {
			case line == "":
			case strings.HasPrefix(line, "#EXT-X-KEY:"), strings.HasPrefix(line, "#EXT-X-MAP:"):
				a := ffAttrs(line[strings.IndexByte(line, ':')+1:])
				if u, ok := a["URI="]; ok && !local[u] {
					t.Fatalf("ffmpeg reads %q from the local playlist line %q", u, line)
				}
			case strings.HasPrefix(line, "#EXT-X-MEDIA:"), strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
				t.Fatalf("ffmpeg opens other playlists of %q", line)
			case strings.HasPrefix(line, "#"):
			default:
				if !local[line] {
					t.Fatalf("ffmpeg reads the segment %q from the local playlist", line)
				}
			}
		}
	})
}
