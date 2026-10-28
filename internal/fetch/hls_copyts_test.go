// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/bridge"
)

// test-media I1: an HLS playlist of fragmented MP4 without
// discontinuities is joined with its own timestamps (-copyts
// -start_at_zero, fmp4Continuous); TS playlists, playlists with an
// EXT-X-DISCONTINUITY and DASH keep ffmpeg's correction of timestamp
// jumps. The timing itself is checked with the real ffmpeg
// (realnet_fmp4_test.go).

// servePlaylist serves a media playlist of n segments at /dir/index.m3u8:
// fragmented MP4 (an init section) or TS, with an EXT-X-DISCONTINUITY
// before the last segment when disc.
func servePlaylist(e *qenv, dir string, n int, fmp4, disc bool) {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:15\n#EXT-X-DISCONTINUITY-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n")
	ext := ".ts"
	if fmp4 {
		ext = ".m4s"
		e.media.put("/"+dir+"/init.mp4", pattern(77, 900))
		b.WriteString("#EXT-X-MAP:URI=\"init.mp4\"\n")
	}
	for i := 1; i <= n; i++ {
		if disc && i == n {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		name := "seg" + itoa(int64(i)) + ext
		e.media.put("/"+dir+"/"+name, pattern(byte(i), 5000+i))
		b.WriteString("#EXTINF:15.0,\n" + name + "\n")
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	e.media.put("/"+dir+"/index.m3u8", []byte(b.String()))
}

// copyTSCall reports whether ffmpeg call c reads with -copyts
// -start_at_zero, given before the first input.
func copyTSCall(t *testing.T, c []string) bool {
	t.Helper()
	i, first := slices.Index(c, "-copyts"), slices.Index(c, "-i")
	if i < 0 {
		if slices.Contains(c, "-start_at_zero") {
			t.Errorf("-start_at_zero without -copyts: %v", c)
		}
		return false
	}
	if first < 0 || i > first || i+1 >= len(c) || c[i+1] != "-start_at_zero" {
		t.Errorf("-copyts not as \"-copyts -start_at_zero\" before the inputs: %v", c)
	}
	return true
}

func TestHLSJoinCopyTS(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *qenv) bridge.Track
		want  bool
	}{
		{"fmp4", func(e *qenv) bridge.Track {
			servePlaylist(e, "f", 3, true, false)
			return e.hlsTrack(bridge.KindVideo, "/f/index.m3u8")
		}, true},
		{"fmp4 audio only", func(e *qenv) bridge.Track {
			servePlaylist(e, "fa", 2, true, false)
			return e.hlsTrack(bridge.KindAudio, "/fa/index.m3u8")
		}, true},
		{"ts", func(e *qenv) bridge.Track {
			servePlaylist(e, "t", 3, false, false)
			return e.hlsTrack(bridge.KindVideo, "/t/index.m3u8")
		}, false},
		{"fmp4 with a discontinuity", func(e *qenv) bridge.Track {
			servePlaylist(e, "fd", 3, true, true)
			return e.hlsTrack(bridge.KindVideo, "/fd/index.m3u8")
		}, false},
		{"ts with a discontinuity", func(e *qenv) bridge.Track {
			servePlaylist(e, "td", 3, false, true)
			return e.hlsTrack(bridge.KindVideo, "/td/index.m3u8")
		}, false},
		{"fmp4 video, fmp4 audio rendition", func(e *qenv) bridge.Track {
			servePlaylist(e, "m1/v", 2, true, false)
			servePlaylist(e, "m1/a", 2, true, false)
			e.media.put("/m1/master.m3u8", []byte("#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"g\",NAME=\"en\",DEFAULT=YES,URI=\"a/index.m3u8\"\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=500000,CODECS=\"avc1.64001e\",AUDIO=\"g\"\nv/index.m3u8\n"))
			return e.hlsTrack(bridge.KindVideo, "/m1/master.m3u8")
		}, true},
		{"fmp4 video, ts audio rendition", func(e *qenv) bridge.Track {
			servePlaylist(e, "m2/v", 2, true, false)
			servePlaylist(e, "m2/a", 2, false, false)
			e.media.put("/m2/master.m3u8", []byte("#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"g\",NAME=\"en\",DEFAULT=YES,URI=\"a/index.m3u8\"\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=500000,CODECS=\"avc1.64001e\",AUDIO=\"g\"\nv/index.m3u8\n"))
			return e.hlsTrack(bridge.KindVideo, "/m2/master.m3u8")
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newQEnv(t, nil)
			e.wait(e.enqueue(e.request("s1", c.setup(e))), bridge.StateDone)
			calls := e.ffmpegCalls()
			if len(calls) != 1 {
				t.Fatalf("ffmpeg %v", calls)
			}
			if got := copyTSCall(t, calls[0]); got != c.want {
				t.Fatalf("-copyts %v, want %v: %v", got, c.want, calls[0])
			}
		})
	}
	// an older ffmpeg (the join repeated without the 7.1 options) keeps it
	t.Run("old ffmpeg", func(t *testing.T) {
		e := newQEnv(t, nil)
		t.Setenv("LF_FAKE_FFMPEG", "oldopts")
		servePlaylist(e, "f", 2, true, false)
		e.wait(e.enqueue(e.request("s1", e.hlsTrack(bridge.KindVideo, "/f/index.m3u8"))), bridge.StateDone)
		calls := e.ffmpegCalls()
		if len(calls) != 2 || !copyTSCall(t, calls[0]) || !copyTSCall(t, calls[1]) {
			t.Fatalf("calls %v", calls)
		}
	})
	// DASH parts are joined into one MP4 first: nothing to change
	t.Run("dash", func(t *testing.T) {
		e := newQEnv(t, nil)
		dashFixture(e, "d", "")
		e.wait(e.enqueue(e.request("s1", e.dashTrack(bridge.KindVideo, "/d/manifest.mpd", "video:-:0"))), bridge.StateDone)
		calls := e.ffmpegCalls()
		if len(calls) == 0 {
			t.Fatal("no ffmpeg call")
		}
		for _, c := range calls {
			if copyTSCall(t, c) {
				t.Fatalf("-copyts on a DASH join: %v", c)
			}
		}
	})
}

func TestFMP4Continuous(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		name, body string
		want       bool
	}{
		{"fmp4", "#EXTM3U\n#EXT-X-MAP:URI=\"m001.mp4\"\n#EXTINF:4,\ns00001.m4s\n#EXT-X-ENDLIST\n", true},
		{"fmp4, discontinuity sequence only", "#EXTM3U\n#EXT-X-DISCONTINUITY-SEQUENCE:3\n#EXT-X-MAP:URI=\"m001.mp4\"\n#EXTINF:4,\ns00001.m4s\n#EXT-X-ENDLIST\n", true},
		{"fmp4, crlf", "#EXTM3U\r\n#EXT-X-MAP:URI=\"m001.mp4\",BYTERANGE=\"10@0\"\r\n#EXTINF:4,\r\ns00001.m4s\r\n#EXT-X-ENDLIST\r\n", true},
		{"fmp4, discontinuity", "#EXTM3U\n#EXT-X-MAP:URI=\"m001.mp4\"\n#EXTINF:4,\ns00001.m4s\n#EXT-X-DISCONTINUITY\n#EXT-X-MAP:URI=\"m002.mp4\"\n#EXTINF:4,\ns00002.m4s\n#EXT-X-ENDLIST\n", false},
		{"ts", "#EXTM3U\n#EXTINF:4,\ns00001.ts\n#EXT-X-ENDLIST\n", false},
		{"ts, discontinuity", "#EXTM3U\n#EXTINF:4,\ns00001.ts\n#EXT-X-DISCONTINUITY\n#EXTINF:4,\ns00002.ts\n#EXT-X-ENDLIST\n", false},
	} {
		p := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "_")+".m3u8")
		if err := os.WriteFile(p, []byte(c.body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := fmp4Continuous(p); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if fmp4Continuous(filepath.Join(dir, "missing.m3u8")) {
		t.Error("a missing playlist counted as fMP4")
	}
}
