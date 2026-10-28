// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"lectureflow/internal/bridge"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

const testMaster = `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",DEFAULT=YES,AUTOSELECT=YES,URI="audio/en.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="Other",DEFAULT=NO,URI="audio/other.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS="avc1.4d401e,mp4a.40.2",AUDIO="aud"
v360.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2400000,RESOLUTION=1920x1080,CODECS="avc1.640028,mp4a.40.2",AUDIO="aud"
https://cdn.example.edu/v1080/index.m3u8?sig=1
#EXT-X-STREAM-INF:BANDWIDTH=2400000,RESOLUTION=1280x720,CODECS="avc1.640028,mp4a.40.2",AUDIO="aud"
v720.m3u8
`

func TestMasterPick(t *testing.T) {
	base := mustURL(t, "https://media.example.edu/s/abc/master.m3u8?Policy=x")
	m, err := parseMaster([]byte(testMaster), base)
	if err != nil {
		t.Fatal(err)
	}
	// (ruling 6: a URI without a query on the master's host takes the
	// master's query; another host or a query of its own keeps its own)
	media, extra := m.pick(bridge.KindVideo)
	if media != "https://cdn.example.edu/v1080/index.m3u8?sig=1" || extra != "https://media.example.edu/s/abc/audio/en.m3u8?Policy=x" {
		t.Fatalf("video pick %q %q", media, extra)
	}
	if media, extra = m.pick(bridge.KindAudio); media != "https://media.example.edu/s/abc/audio/en.m3u8?Policy=x" || extra != "" {
		t.Fatalf("audio pick %q %q", media, extra)
	}
	// Panopto (M0): one variant per master, an audio-only one for the
	// audio track
	pan := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=112000,CODECS=\"mp4a.40.2\"\nindex.m3u8\n"
	m, err = parseMaster([]byte(pan), base)
	if err != nil {
		t.Fatal(err)
	}
	if media, extra = m.pick(bridge.KindAudio); media != "https://media.example.edu/s/abc/index.m3u8?Policy=x" || extra != "" {
		t.Fatalf("panopto audio %q %q", media, extra)
	}
	// muxed audio (no URI in the rendition): nothing extra
	muxed := "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"x\",DEFAULT=YES\n#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"a\"\nv.m3u8\n"
	m, _ = parseMaster([]byte(muxed), base)
	if _, extra = m.pick(bridge.KindVideo); extra != "" {
		t.Fatalf("muxed audio extra %q", extra)
	}
	if _, err := parseMaster([]byte("#EXTM3U\n#EXT-X-VERSION:3\n"), base); err == nil {
		t.Fatal("master without variants accepted")
	}
	if _, err := parseMaster([]byte("<html>login</html>"), base); err == nil {
		t.Fatal("an HTML page accepted as playlist")
	}
}

func TestMediaPlaylistAESAndRewrite(t *testing.T) {
	base := mustURL(t, "https://media.example.edu/s/abc/v720.m3u8?Policy=x")
	pl := "\xef\xbb\xbf#EXTM3U\r\n#EXT-X-VERSION:3\r\n#EXT-X-TARGETDURATION:6\r\n" +
		"#EXT-X-KEY:METHOD=AES-128,URI=\"https://keys.example.edu/k?token=abc\",IV=0x01\r\n" +
		"#EXTINF:6.0,\r\nseg1.ts\r\n#EXTINF:6.0,\r\nseg2.ts?x=1\r\n" +
		"#EXT-X-KEY:METHOD=NONE\r\n#EXTINF:3.0,\r\n/abs/seg3.aac\r\n#EXT-X-ENDLIST\r\n"
	m, err := parseMedia([]byte(pl), base)
	if err != nil {
		t.Fatal(err)
	}
	if m.segments != 3 || len(m.resources) != 4 || !m.encrypted {
		t.Fatalf("segments %d resources %v encrypted %v", m.segments, m.resources, m.encrypted)
	}
	if _, _, ok := m.singleFile(); ok {
		t.Fatal("an encrypted segment list taken for a single file")
	}
	local := string(m.local())
	for _, want := range []string{`URI="k001.key",IV=0x01`, "\ns00001.ts\n", "\ns00002.ts\n", "\ns00003.aac\n", "#EXT-X-ENDLIST"} {
		if !strings.Contains(local, want) {
			t.Errorf("local playlist lacks %q:\n%s", want, local)
		}
	}
	if strings.Contains(local, "http") || strings.Contains(local, "token") {
		t.Fatalf("local playlist keeps a URL:\n%s", local)
	}
	if m.names["https://media.example.edu/s/abc/seg1.ts?Policy=x"] != "s00001.ts" || m.names["https://media.example.edu/s/abc/seg2.ts?x=1"] != "s00002.ts" ||
		m.names["https://media.example.edu/abs/seg3.aac?Policy=x"] != "s00003.aac" || m.names["https://keys.example.edu/k?token=abc"] != "k001.key" {
		t.Fatalf("names %v", m.names)
	}
}

func TestMediaPlaylistRefusals(t *testing.T) {
	base := mustURL(t, "https://m.example.edu/a/b.m3u8")
	cases := map[string]string{
		"live":       "#EXTM3U\n#EXTINF:6,\ns1.ts\n",
		"sample-aes": "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"skd://x\"\n#EXTINF:6,\ns1.ts\n#EXT-X-ENDLIST\n",
		"drm":        "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k\",KEYFORMAT=\"com.apple.streamingkeydelivery\"\n#EXTINF:6,\ns1.ts\n#EXT-X-ENDLIST\n",
	}
	for feature, pl := range cases {
		_, err := parseMedia([]byte(pl), base)
		var pe *errPlaylist
		if !errors.As(err, &pe) || pe.feature != feature {
			t.Errorf("%s: %v", feature, err)
		}
		f := playlistFailure(err).(*failure)
		switch feature {
		case "drm", "sample-aes": // DRM has its own code (the wording the user chose)
			if f.code != CodeDRMProtected || f.params != nil || !strings.Contains(f.detail, feature) {
				t.Errorf("%s: failure %+v", feature, f)
			}
		default:
			if f.code != CodePlaylistUnsupported || f.params["feature"] != feature {
				t.Errorf("%s: failure %+v", feature, f)
			}
		}
	}
	// VOD without ENDLIST is fine
	if _, err := parseMedia([]byte("#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:6,\ns1.ts\n"), base); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"#EXTM3U\n#EXT-X-ENDLIST\n", "#EXTM3U\n#EXTINF:1,\nftp://x/y.ts\n#EXT-X-ENDLIST\n",
		"#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nv.m3u8\n"} {
		if _, err := parseMedia([]byte(bad), base); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// panoptoMedia is the shape M0 found on Panopto: fMP4 in one file, the init
// section and every segment consecutive byte ranges of it.
func panoptoMedia(segs int, segLen int64, explicit bool) (string, int64) {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:10\n#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString("#EXT-X-MAP:URI=\"fragmented.mp4\",BYTERANGE=\"762@0\"\n")
	off := int64(762)
	for i := 0; i < segs; i++ {
		b.WriteString("#EXTINF:9.0,\n")
		if explicit || i == 0 {
			b.WriteString("#EXT-X-BYTERANGE:" + itoa(segLen) + "@" + itoa(off) + "\n")
		} else {
			b.WriteString("#EXT-X-BYTERANGE:" + itoa(segLen) + "\n")
		}
		b.WriteString("fragmented.mp4\n")
		off += segLen
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String(), off
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestSingleFileFastPath(t *testing.T) {
	base := mustURL(t, "https://d1abc.cloudfront.net/sessions/x/y.hls/index.m3u8")
	for _, explicit := range []bool{true, false} {
		pl, end := panoptoMedia(367, 1000, explicit)
		m, err := parseMedia([]byte(pl), base)
		if err != nil {
			t.Fatal(err)
		}
		uri, minSize, ok := m.singleFile()
		if !ok || uri != "https://d1abc.cloudfront.net/sessions/x/y.hls/fragmented.mp4" || minSize != end || end != 762+367*1000 {
			t.Fatalf("explicit=%v: %q %d %v", explicit, uri, minSize, ok)
		}
	}
	// a gap, a range not starting at 0, two files: segments as usual
	gap := strings.Replace(func() string { s, _ := panoptoMedia(3, 1000, true); return s }(), "1000@1762", "1000@1800", 1)
	notZero := strings.Replace(func() string { s, _ := panoptoMedia(3, 1000, true); return s }(), "762@0", "762@10", 1)
	two := strings.Replace(func() string { s, _ := panoptoMedia(3, 1000, true); return s }(), "#EXT-X-ENDLIST", "#EXTINF:1,\nother.mp4\n#EXT-X-ENDLIST", 1)
	noRange := "#EXTM3U\n#EXTINF:1,\na.mp4\n#EXTINF:1,\na.mp4\n#EXT-X-ENDLIST\n"
	for name, pl := range map[string]string{"gap": gap, "not at 0": notZero, "two files": two, "no ranges": noRange} {
		m, err := parseMedia([]byte(pl), base)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, _, ok := m.singleFile(); ok {
			t.Errorf("%s taken for a single file", name)
		}
	}
}

func TestAttrs(t *testing.T) {
	a := attrs(`BANDWIDTH=1,CODECS="avc1.4d401e,mp4a.40.2",NAME="a, b",URI="x?y=1"`)
	if a["BANDWIDTH"] != "1" || a["CODECS"] != "avc1.4d401e,mp4a.40.2" || a["NAME"] != "a, b" || a["URI"] != "x?y=1" {
		t.Fatalf("%v", a)
	}
	if !audioOnly("mp4a.40.2") || audioOnly("avc1.4d401e,mp4a.40.2") || audioOnly("") {
		t.Fatal("audioOnly")
	}
	if extOf("https://x/a/b.TS?q=1", ".ts") != ".ts" || extOf("https://x/a/b", ".mp4") != ".mp4" || extOf("https://x/a.weird-ext", ".ts") != ".ts" {
		t.Fatal("extOf")
	}
}
