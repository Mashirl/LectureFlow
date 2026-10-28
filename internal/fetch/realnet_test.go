// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build realnet

package fetch

// The download queue with the real aria2c and ffmpeg (ext-brief §0: real
// binaries only under the realnet tag). The media come from a local
// httptest server (no internet): media made by ffmpeg, served with the same
// cookie, referer and signature checks as the fake server. Run with
//
//	go test -tags realnet ./internal/fetch -run Real -v
//
// LF_REAL_ARIA2C / LF_REAL_FFMPEG name the binaries; by default the staged
// desktop/build/resources/bin ones, else those on PATH.

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/bridge"
	"lectureflow/internal/selfupdate"
)

func realBinary(t *testing.T, env, name string) string {
	t.Helper()
	if p := os.Getenv(env); p != "" {
		return p
	}
	exe := name
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if p, err := filepath.Abs(filepath.Join("..", "..", "desktop", "build", "resources", "bin", exe)); err == nil {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not found (set %s)", name, env)
	}
	return p
}

func ffrun(t *testing.T, ff, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(ff, append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

// playable decodes the whole file with ffmpeg and returns its stream kinds.
func playable(t *testing.T, ff, path string) string {
	t.Helper()
	out, err := exec.Command(ff, "-hide_banner", "-v", "error", "-i", path, "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("%s does not decode: %v\n%s", path, err, out)
	}
	info, _ := exec.Command(ff, "-hide_banner", "-i", path).CombinedOutput()
	kinds := ""
	if bytes.Contains(info, []byte("Video:")) {
		kinds += "v"
	}
	if bytes.Contains(info, []byte("Audio:")) {
		kinds += "a"
	}
	return kinds
}

func TestRealAria2cAndFFmpeg(t *testing.T) {
	aria := realBinary(t, "LF_REAL_ARIA2C", "aria2c")
	ff := realBinary(t, "LF_REAL_FFMPEG", "ffmpeg")
	e := newQEnv(t, func(o *Options) {
		o.Launcher, o.ariaVersion = nil, nil // the real aria2c and its --version
		o.Aria2c, o.FFmpeg = aria, ff
		o.PollInterval = 100 * time.Millisecond
		o.IdleTimeout = 500 * time.Millisecond
	})
	ctx := context.Background()
	src := t.TempDir()
	ffrun(t, ff, src, "-f", "lavfi", "-i", "testsrc=d=6:s=320x240:r=25", "-f", "lavfi", "-i", "sine=d=6",
		"-c:v", "mpeg4", "-c:a", "aac", "-shortest", "av.mp4")
	ffrun(t, ff, src, "-i", "av.mp4", "-map", "0:v", "-c", "copy", "video.mp4")
	ffrun(t, ff, src, "-i", "av.mp4", "-map", "0:a", "-c", "copy", "audio.m4a")
	// HLS: MPEG-TS segments with an AES-128 key
	if err := os.WriteFile(filepath.Join(src, "enc.key"), []byte("0123456789abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "keyinfo"), []byte("key.bin?kt="+testSig+"\nenc.key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// (MPEG-2 video: MPEG-4 part 2 keeps its headers out of the TS
	// segments when the HLS muxer asks for global headers)
	ffrun(t, ff, src, "-f", "lavfi", "-i", "testsrc=d=6:s=320x240:r=25", "-f", "lavfi", "-i", "sine=d=6",
		"-c:v", "mpeg2video", "-c:a", "aac", "-shortest", "-f", "hls", "-hls_time", "1", "-hls_playlist_type", "vod",
		"-hls_key_info_file", "keyinfo", "-hls_segment_filename", "seg%03d.ts", "enc.m3u8")
	// HLS: one fragmented MP4 in byte ranges (Panopto, M0)
	ffrun(t, ff, src, "-i", "av.mp4", "-c", "copy", "-f", "hls", "-hls_time", "1", "-hls_playlist_type", "vod",
		"-hls_segment_type", "fmp4", "-hls_flags", "single_file", "single.m3u8")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		name := filepath.Base(p)
		switch name {
		case "enc.key":
			e.media.put("/s/key.bin", b)
		case "video.mp4", "audio.m4a":
			e.media.put("/files/"+name, b)
		default:
			e.media.put("/s/"+name, b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.media.put("/s/master.m3u8", []byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=500000,CODECS=\"mp4v.20.9,mp4a.40.2\"\nsingle.m3u8\n"))

	// audio + video files, combined
	id1 := e.enqueue(e.request("av", e.fileTrack(bridge.KindVideo, "/files/video.mp4"), e.fileTrack(bridge.KindAudio, "/files/audio.m4a")))
	// encrypted HLS
	id2 := e.enqueue(e.request("enc", e.hlsTrack(bridge.KindVideo, "/s/enc.m3u8")))
	// the single-file path
	id3 := e.enqueue(e.request("single", e.hlsTrack(bridge.KindVideo, "/s/master.m3u8")))
	for _, id := range []int64{id1, id2, id3} {
		info := e.wait(id, bridge.StateDone)
		if k := playable(t, ff, e.lecture(info.LectureID).MediaPath); k != "va" {
			t.Errorf("task %d: streams %q", id, k)
		}
	}
	for _, s := range e.media.requests("") {
		if strings.HasPrefix(s.Path, "/s/") && strings.Contains(s.Path, "seg") {
			continue
		}
		if s.Cookie != testCookie || s.Referer != testRefOrigin {
			t.Errorf("request without the session: %+v", s)
		}
	}
	single := e.media.requests("/s/single.m4s")
	if len(single) == 0 {
		t.Fatal("the single file was not downloaded")
	}
	// refused by the server: needs a refresh
	e.media.setStatus("/files/audio.m4a", 403)
	id4 := e.enqueue(e.request("refused", e.fileTrack(bridge.KindAudio, "/files/audio.m4a")))
	if info := e.wait(id4, bridge.StateNeedsRefresh); info.Error.Code != CodeCredentialsRejected || info.Error.Params["status"] != float64(403) {
		t.Fatalf("403: %+v", info.Error)
	}
	// aria2c stops when idle
	e.waitFor("aria2c stopped", func() bool { return !e.q.eng.running() })
	e.q.Close(5 * time.Second)

	for _, s := range []string{testCookie, testSig, testRefTok} {
		_ = filepath.WalkDir(e.root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(s)) {
				t.Errorf("%s contains %s", p, s)
			}
			return nil
		})
		if strings.Contains(e.logs.String(), s) {
			t.Errorf("the log contains %s", s)
		}
	}
	_ = ctx
}

// DASH from the bundled ffmpeg's own DASH muxer (SegmentTemplate with and
// without a SegmentTimeline; one file per stream in byte ranges), a video
// track alone (its audio comes with it), and a cross-host redirect: the
// real aria2c gets the final URL, the other host never sees the cookie.
func TestRealDASHAndRedirect(t *testing.T) {
	aria := realBinary(t, "LF_REAL_ARIA2C", "aria2c")
	ff := realBinary(t, "LF_REAL_FFMPEG", "ffmpeg")
	e := newQEnv(t, func(o *Options) {
		o.Launcher, o.ariaVersion = nil, nil // the real aria2c and its --version
		o.Aria2c, o.FFmpeg = aria, ff
		o.PollInterval = 100 * time.Millisecond
	})
	src := t.TempDir()
	ffrun(t, ff, src, "-f", "lavfi", "-i", "testsrc=d=6:s=320x240:r=25", "-f", "lavfi", "-i", "sine=d=6",
		"-c:v", "mpeg4", "-c:a", "aac", "-shortest", "av.mp4")
	sets := []string{"-adaptation_sets", "id=0,streams=v id=1,streams=a"}
	for dir, opts := range map[string][]string{
		"timeline": {"-use_template", "1", "-use_timeline", "1"},
		"number":   {"-use_template", "1", "-use_timeline", "0"},
		"single":   {"-single_file", "1"},
	} {
		if err := os.MkdirAll(filepath.Join(src, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		args := append([]string{"-i", "av.mp4", "-map", "0:v", "-map", "0:a", "-c", "copy", "-f", "dash", "-seg_duration", "1"}, opts...)
		args = append(append(args, sets...), dir+"/m.mpd")
		ffrun(t, ff, src, args...)
	}
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		e.media.put("/s/"+filepath.ToSlash(rel), b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// the cross-host redirect: video on "another host" (localhost)
	other := newMediaServer(t)
	other.mu.Lock()
	other.cookie, other.sigFor = "", ""
	other.mu.Unlock()
	otherURL := strings.Replace(other.srv.URL, "127.0.0.1", "localhost", 1)
	av, _ := os.ReadFile(filepath.Join(src, "av.mp4"))
	other.put("/cdn/av.mp4", av)
	e.media.mu.Lock()
	e.media.redir["/files/r/av.mp4"] = otherURL + "/cdn/av.mp4?Expires=1&Sig=cdn"
	e.media.mu.Unlock()

	var ids []int64
	for _, dir := range []string{"timeline", "number", "single"} {
		ids = append(ids, e.enqueue(e.request("dash-"+dir, e.dashTrack(bridge.KindVideo, "/s/"+dir+"/m.mpd", "video:und:0"),
			e.dashTrack(bridge.KindAudio, "/s/"+dir+"/m.mpd", "audio:und:0"))))
	}
	ids = append(ids, e.enqueue(e.request("dash-video-only", e.dashTrack(bridge.KindVideo, "/s/timeline/m.mpd", "video:und:0"))))
	ids = append(ids, e.enqueue(e.request("redirect", e.fileTrack(bridge.KindVideo, "/files/r/av.mp4"))))
	for _, id := range ids {
		info := e.wait(id, bridge.StateDone)
		if k := playable(t, ff, e.lecture(info.LectureID).MediaPath); k != "va" {
			t.Errorf("task %d: streams %q", id, k)
		}
	}
	seen := other.requests("")
	if len(seen) < 2 {
		t.Fatalf("the other host saw %+v", seen)
	}
	for _, r := range seen {
		if r.CookieHeader != "" {
			t.Fatalf("a cookie reached the other host: %+v", r)
		}
	}
	e.q.Close(5 * time.Second)
	for _, s := range []string{testCookie, testSig, testRefTok} {
		_ = filepath.WalkDir(e.root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(s)) {
				t.Errorf("%s contains %s", p, s)
			}
			return nil
		})
	}
}

// The in-app update's downloader on the real aria2c: only our User-Agent
// among the identifying headers, no helper file left.
func TestRealUpdateDownloader(t *testing.T) {
	aria := realBinary(t, "LF_REAL_ARIA2C", "aria2c")
	e := newQEnv(t, func(o *Options) {
		o.Launcher, o.ariaVersion = nil, nil // the real aria2c and its --version
		o.Aria2c = aria
		o.PollInterval = 100 * time.Millisecond
	})
	srv := newUpdateServer(t)
	pkg := pattern(3, 3_000_000)
	srv.files["/releases/download/v3.2.0/LectureFlow-Setup-3.2.0.exe"] = pkg
	path := updatePath(t)
	err := e.q.UpdateDownloader().Download(context.Background(), selfupdate.DownloadRequest{
		URLs: []string{srv.srv.URL + "/missing.exe", srv.srv.URL + "/releases/download/v3.2.0/LectureFlow-Setup-3.2.0.exe"},
		Path: path, Size: int64(len(pkg))})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readAll(t, path), pkg) {
		t.Fatal("package differs")
	}
	if _, err := os.Stat(path + ".aria2"); !os.IsNotExist(err) {
		t.Fatalf("aria2c's control file is left: %v", err)
	}
	names := map[string]bool{}
	for _, h := range srv.headers() {
		if h.Get("User-Agent") != selfupdate.UserAgent || h.Get("Cookie") != "" || h.Get("Referer") != "" {
			t.Fatalf("request headers %v", h)
		}
		for k := range h {
			names[k] = true
		}
	}
	t.Logf("aria2c's request headers: %v", names)
}
