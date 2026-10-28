// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build realnet

package fetch

// test-media I1 with the real aria2c and ffmpeg: the joined file's audio
// and video start together (within 50 ms) and last as long as the source,
// for fragmented-MP4 HLS with segments longer than ffmpeg's 10 s
// discontinuity threshold (clear and AES-128; the audio was one segment
// late before the fix) and, unchanged, for MPEG-TS with AES-128, separate
// audio renditions, audio-only playlists, playlists with an
// EXT-X-DISCONTINUITY and DASH. Run with
//
//	go test -tags realnet ./internal/fetch -run RealFMP4 -v

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/bridge"
)

// probeTimes are the start and duration (s) of the first video and audio
// stream of a file (-1 = no such stream).
type probeTimes struct{ vStart, vDur, aStart, aDur float64 }

func probeStarts(t *testing.T, ffprobe, path string) probeTimes {
	t.Helper()
	out, err := exec.Command(ffprobe, "-v", "error", "-show_entries", "stream=codec_type,start_time,duration", "-of", "csv=p=0", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe %s: %v\n%s", path, err, out)
	}
	p := probeTimes{-1, -1, -1, -1}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(strings.TrimSpace(l), ",")
		if len(f) < 3 {
			continue
		}
		start, _ := strconv.ParseFloat(f[1], 64)
		dur, err := strconv.ParseFloat(f[2], 64)
		if err != nil {
			dur = -1
		}
		switch {
		case f[0] == "video" && p.vStart < 0:
			p.vStart, p.vDur = start, dur
		case f[0] == "audio" && p.aStart < 0:
			p.aStart, p.aDur = start, dur
		}
	}
	return p
}

// encryptSegments encrypts the fMP4 segments of the playlist in dir with
// AES-128-CBC (PKCS#7) and adds the key tag after EXT-X-MAP (ffmpeg's hls
// muxer cannot write encrypted fMP4); the init section stays clear.
func encryptSegments(t *testing.T, dir string) {
	t.Helper()
	key := []byte("0123456789abcdef")
	iv := make([]byte, 16)
	iv[15] = 0xAB
	blk, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(string(pl), "\r", ""), "\n") {
		out = append(out, l)
		if strings.HasPrefix(l, "#EXT-X-MAP") {
			out = append(out, "#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin?kt="+testSig+"\",IV=0x000000000000000000000000000000AB")
		}
		if strings.HasSuffix(l, ".m4s") {
			b, err := os.ReadFile(filepath.Join(dir, l))
			if err != nil {
				t.Fatal(err)
			}
			pad := 16 - len(b)%16
			b = append(b, bytes.Repeat([]byte{byte(pad)}, pad)...)
			enc := make([]byte, len(b))
			cipher.NewCBCEncrypter(blk, iv).CryptBlocks(enc, b)
			if err := os.WriteFile(filepath.Join(dir, l), enc, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte(strings.Join(out, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.bin"), key, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRealFMP4Timing(t *testing.T) {
	aria := realBinary(t, "LF_REAL_ARIA2C", "aria2c")
	ff := realBinary(t, "LF_REAL_FFMPEG", "ffmpeg")
	ffprobe := filepath.Join(filepath.Dir(ff), strings.Replace(filepath.Base(ff), "ffmpeg", "ffprobe", 1))
	if _, err := os.Stat(ffprobe); err != nil {
		if ffprobe, err = exec.LookPath("ffprobe"); err != nil {
			t.Skip("ffprobe not found next to ffmpeg nor in PATH")
		}
	}
	e := newQEnv(t, func(o *Options) {
		o.Launcher, o.ariaVersion = nil, nil // the real aria2c and its --version
		o.Aria2c, o.FFmpeg = aria, ff
		o.PollInterval = 100 * time.Millisecond
	})
	src := t.TempDir()
	run := func(dir string, args ...string) {
		if err := os.MkdirAll(filepath.Join(src, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		ffrun(t, ff, filepath.Join(src, dir), args...)
	}
	// 40 s, a keyframe every second: the HLS muxer cuts where -hls_time says
	av := filepath.Join(src, "av.mp4")
	ffrun(t, ff, src, "-f", "lavfi", "-i", "testsrc=d=40:s=160x120:r=10", "-f", "lavfi", "-i", "sine=frequency=440:duration=40",
		"-c:v", "mpeg4", "-g", "10", "-c:a", "aac", "-shortest", "av.mp4")
	hls := func(segs string, extra ...string) []string {
		args := []string{"-i", av}
		args = append(args, extra...)
		return append(args, "-f", "hls", "-hls_time", segs, "-hls_playlist_type", "vod")
	}
	fmp4 := []string{"-hls_segment_type", "fmp4", "-hls_segment_filename", "seg%03d.m4s"}
	run("fmp4_long", append(hls("15", "-c", "copy"), append(fmp4, "index.m3u8")...)...)
	run("fmp4_long_aes", append(hls("15", "-c", "copy"), append(fmp4, "index.m3u8")...)...)
	encryptSegments(t, filepath.Join(src, "fmp4_long_aes"))
	run("fmp4_one", append(hls("60", "-c", "copy"), append(fmp4, "index.m3u8")...)...) // one 40 s segment
	encryptSegments(t, filepath.Join(src, "fmp4_one"))
	run("fmp4_short", append(hls("4", "-c", "copy"), append(fmp4, "index.m3u8")...)...)
	run("fmp4_aonly", append(hls("15", "-map", "0:a", "-c", "copy"), append(fmp4, "index.m3u8")...)...)
	run("fmp4_sep/v", append(hls("15", "-map", "0:v", "-c", "copy"), append(fmp4, "index.m3u8")...)...)
	run("fmp4_sep/a", append(hls("15", "-map", "0:a", "-c", "copy"), append(fmp4, "index.m3u8")...)...)
	// MPEG-TS (MPEG-2 video: MPEG-4 part 2 keeps its headers out of the
	// segments when the HLS muxer asks for global headers)
	if err := os.WriteFile(filepath.Join(src, "enc.key"), []byte("0123456789abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	keyinfo := filepath.Join(src, "keyinfo")
	if err := os.WriteFile(keyinfo, []byte("key.bin?kt="+testSig+"\n"+filepath.ToSlash(filepath.Join(src, "enc.key"))+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := []string{"-c:v", "mpeg2video", "-g", "10", "-c:a", "aac"}
	run("ts_aes", append(hls("15", ts...), "-hls_key_info_file", keyinfo, "-hls_segment_filename", "seg%03d.ts", "index.m3u8")...)
	if err := os.WriteFile(filepath.Join(src, "ts_aes", "key.bin"), []byte("0123456789abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("ts_aonly", append(hls("15", "-map", "0:a", "-c", "copy"), "-hls_segment_filename", "seg%03d.ts", "index.m3u8")...)
	run("ts_sep/v", append(hls("15", "-map", "0:v", "-c:v", "mpeg2video", "-g", "10"), "-hls_segment_filename", "seg%03d.ts", "index.m3u8")...)
	run("ts_sep/a", append(hls("15", "-map", "0:a", "-c", "copy"), "-hls_segment_filename", "seg%03d.ts", "index.m3u8")...)
	master := "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"g\",NAME=\"en\",DEFAULT=YES,URI=\"a/index.m3u8\"\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=500000,CODECS=\"mp4v.20.9\",AUDIO=\"g\"\nv/index.m3u8\n"
	for _, d := range []string{"fmp4_sep", "ts_sep"} {
		if err := os.WriteFile(filepath.Join(src, d, "master.m3u8"), []byte(master), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// two encodes whose timestamps start over after an EXT-X-DISCONTINUITY
	discontinuity := func(dir string, part func(i int, from string) []string, ext string) {
		var body strings.Builder
		body.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:5\n#EXT-X-PLAYLIST-TYPE:VOD\n")
		for i, from := range []string{"0", "20"} {
			run(dir, part(i, from)...)
			pl, err := os.ReadFile(filepath.Join(src, dir, "p"+strconv.Itoa(i)+".m3u8"))
			if err != nil {
				t.Fatal(err)
			}
			if i > 0 {
				body.WriteString("#EXT-X-DISCONTINUITY\n")
			}
			for _, l := range strings.Split(strings.ReplaceAll(string(pl), "\r", ""), "\n") {
				if strings.HasPrefix(l, "#EXTINF") || strings.HasPrefix(l, "#EXT-X-MAP") || strings.HasSuffix(l, ext) {
					body.WriteString(l + "\n")
				}
			}
		}
		body.WriteString("#EXT-X-ENDLIST\n")
		if err := os.WriteFile(filepath.Join(src, dir, "index.m3u8"), []byte(body.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	discontinuity("ts_disc", func(i int, from string) []string {
		p := "p" + strconv.Itoa(i)
		return []string{"-ss", from, "-t", "20", "-i", av, "-c:v", "mpeg2video", "-g", "10", "-c:a", "aac", "-f", "hls", "-hls_time", "4",
			"-hls_playlist_type", "vod", "-hls_segment_filename", p + "_%03d.ts", p + ".m3u8"}
	}, ".ts")
	discontinuity("fmp4_disc", func(i int, from string) []string {
		p := "p" + strconv.Itoa(i)
		return []string{"-ss", from, "-t", "20", "-i", av, "-c", "copy", "-f", "hls", "-hls_time", "4", "-hls_playlist_type", "vod",
			"-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", p + "_init.mp4", "-hls_segment_filename", p + "_%03d.m4s", p + ".m3u8"}
	}, ".m4s")
	// DASH: segments of 15 s, joined by bytes before the remux
	run("dash", "-i", av, "-map", "0:v", "-map", "0:a", "-c", "copy", "-f", "dash", "-seg_duration", "15",
		"-use_template", "1", "-use_timeline", "1", "-adaptation_sets", "id=0,streams=v id=1,streams=a", "m.mpd")

	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		e.media.put("/s/"+filepath.ToSlash(rel), b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		tracks []bridge.Track
		video  bool
		dur    float64
	}{
		{"fmp4_long", []bridge.Track{e.hlsTrack(bridge.KindVideo, "/s/fmp4_long/index.m3u8")}, true, 40},
		{"fmp4_long_aes", []bridge.Track{e.hlsTrack(bridge.KindVideo, "/s/fmp4_long_aes/index.m3u8")}, true, 40},
		{"fmp4_one", []bridge.Track{e.hlsTrack(bridge.KindVideo, "/s/fmp4_one/index.m3u8")}, true, 40},
		{"fmp4_short", []bridge.Track{e.hlsTrack(bridge.KindVideo, "/s/fmp4_short/index.m3u8")}, true, 40},
		{"fmp4_aonly", []bridge.Track{e.hlsTrack(bridge.KindAudio, "/s/fmp4_aonly/index.m3u8")}, false, 40},
		{"fmp4_sep", []bridge.Track{e.hlsTrack(bridge.KindVideo, "/s/fmp4_sep/master.m3u8")}, true, 40},
		{"fmp4_disc", []bridge.Track{e.hlsTrack(bridge.KindVideo, "/s/fmp4_disc/index.m3u8")}, true, 40},
		{"ts_aes", []bridge.Track{e.hlsTrack(bridge.KindVideo, "/s/ts_aes/index.m3u8")}, true, 40},
		{"ts_aonly", []bridge.Track{e.hlsTrack(bridge.KindAudio, "/s/ts_aonly/index.m3u8")}, false, 40},
		{"ts_sep", []bridge.Track{e.hlsTrack(bridge.KindVideo, "/s/ts_sep/master.m3u8")}, true, 40},
		{"ts_disc", []bridge.Track{e.hlsTrack(bridge.KindVideo, "/s/ts_disc/index.m3u8")}, true, 40},
		{"dash", []bridge.Track{e.dashTrack(bridge.KindVideo, "/s/dash/m.mpd", "video:und:0"), e.dashTrack(bridge.KindAudio, "/s/dash/m.mpd", "audio:und:0")}, true, 40},
	}
	ids := make([]int64, len(cases))
	for i, c := range cases {
		ids[i] = e.enqueue(e.request(c.name, c.tracks...))
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			info := e.wait(ids[i], bridge.StateDone)
			media := e.lecture(info.LectureID).MediaPath
			p := probeStarts(t, ffprobe, media)
			t.Logf("%s: video start %.3f dur %.3f, audio start %.3f dur %.3f", c.name, p.vStart, p.vDur, p.aStart, p.aDur)
			if p.aStart < 0 || math.Abs(p.aStart) > 0.05 {
				t.Errorf("audio starts at %.3f s", p.aStart)
			}
			if math.Abs(p.aDur-c.dur) > 0.5 {
				t.Errorf("audio lasts %.3f s, want %.0f s", p.aDur, c.dur)
			}
			if !c.video {
				if p.vStart >= 0 {
					t.Errorf("a video stream in an audio-only download")
				}
				return
			}
			if math.Abs(p.vStart-p.aStart) > 0.05 {
				t.Errorf("video starts at %.3f s, audio at %.3f s", p.vStart, p.aStart)
			}
			if math.Abs(p.vDur-c.dur) > 0.5 {
				t.Errorf("video lasts %.3f s, want %.0f s", p.vDur, c.dur)
			}
		})
	}
	e.q.Close(5 * time.Second)
}
