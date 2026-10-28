// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package media

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Checks with a real ffmpeg / ffprobe on small files made on the spot in a
// temporary directory: the bundled build when the desktop build has staged
// it (desktop/build/resources/bin), else the one in PATH; skipped without
// one (SPEC §14). Nothing outside the temporary directory is read.

func realFFmpeg(t *testing.T) (ffmpeg, ffprobe string) {
	t.Helper()
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	bundled, _ := filepath.Abs(filepath.Join("..", "..", "desktop", "build", "resources", "bin", "ffmpeg"+exe))
	if fi, err := os.Stat(bundled); err == nil && !fi.IsDir() {
		ffmpeg = bundled
	} else if p, err := exec.LookPath("ffmpeg"); err == nil {
		ffmpeg = p
	} else {
		t.Skip("ffmpeg not found: the real ffmpeg checks are skipped (SPEC §14)")
	}
	p, err := FFprobeFor(ffmpeg)
	if err != nil {
		t.Skipf("ffprobe not found (%v): the real ffmpeg checks are skipped (SPEC §14)", err)
	}
	return ffmpeg, p
}

func execRun(ctx context.Context, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	return out.String(), errb.String(), err
}

func makeMedia(t *testing.T, ffmpeg, out string, args ...string) {
	t.Helper()
	all := append([]string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}, args...)
	all = append(all, out)
	if b, err := exec.Command(ffmpeg, all...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", all, err, b)
	}
}

const sine = "sine=frequency=440:duration=1"

// The playlists of review M3 reach a file outside the lecture directory
// without the whitelists and are refused with them, by ffprobe and by
// ffmpeg (the -i form prep and stream use), before anything is read.
func TestRealPlaylistsRefused(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	lec := filepath.Join(root, "lecture")
	os.MkdirAll(outside, 0o755)
	os.MkdirAll(lec, 0o755)
	seg := filepath.Join(outside, "seg.ts")
	makeMedia(t, ffmpeg, seg, "-f", "lavfi", "-i", sine, "-c:a", "mp2", "-f", "mpegts")
	hls := filepath.Join(lec, "media.m3u8")
	os.WriteFile(hls, []byte(fmt.Sprintf("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:1.0,\n%s\n#EXT-X-ENDLIST\n", seg)), 0o644)
	os.WriteFile(filepath.Join(lec, "part.wav"), nil, 0o644)
	makeMedia(t, ffmpeg, filepath.Join(lec, "part.wav"), "-f", "lavfi", "-i", sine)
	concat := filepath.Join(lec, "media.mp3") // a whitelisted extension: only the content tells
	os.WriteFile(concat, []byte("ffconcat version 1.0\nfile 'part.wav'\n"), 0o644)
	ctx := context.Background()

	// the attack works without the whitelists (so the checks below mean something)
	if out, _, err := execRun(ctx, ffprobe, "-v", "error", "-show_entries", "format=format_name", "-of", "default=nw=1:nk=1", hls); err != nil || strings.TrimSpace(out) != "hls" {
		t.Fatalf("plain ffprobe on the HLS playlist: %q %v", out, err)
	}
	if out, _, err := execRun(ctx, ffprobe, "-v", "error", "-show_entries", "format=format_name", "-of", "default=nw=1:nk=1", concat); err != nil || strings.TrimSpace(out) != "concat" {
		t.Fatalf("plain ffprobe on the ffconcat script: %q %v", out, err)
	}

	for path, want := range map[string]string{hls: "hls", concat: "concat"} {
		format, refused, err := ProbeFormat(ctx, execRun, ffprobe, path)
		if err != nil || !refused || format != want {
			t.Errorf("ProbeFormat(%s) = %q refused %v err %v, want %q refused", filepath.Base(path), format, refused, err, want)
		}
		out := filepath.Join(lec, "audio.wav")
		args := append([]string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}, InputArgs()...)
		args = append(args, "-i", path, "-vn", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-f", "wav", out)
		_, stderr, err := execRun(ctx, ffmpeg, args...)
		if err == nil || !strings.Contains(stderr, "not on whitelist") {
			t.Errorf("ffmpeg %s with InputArgs: err %v, stderr %q", filepath.Base(path), err, stderr)
		}
		os.Remove(out)
	}
	// Check: a playlist is caught by its first bytes already
	if pr, err := Check(ctx, execRun, ffmpeg, concat); err != nil || pr == nil || pr.Kind != ProblemPlaylist || pr.Format != "ffconcat" {
		t.Errorf("Check(ffconcat) = %+v, %v", pr, err)
	}
	// a file ffprobe cannot read passes Check (prep reports it)
	junk := filepath.Join(lec, "junk.mp4")
	os.WriteFile(junk, []byte("not a recording at all"), 0o644)
	if _, _, err := ProbeFormat(ctx, execRun, ffprobe, junk); err == nil {
		t.Error("ProbeFormat(junk): no error")
	}
	if pr, err := Check(ctx, execRun, ffmpeg, junk); pr != nil || err != nil {
		t.Errorf("Check(junk) = %+v, %v", pr, err)
	}
}

// Recordings of the accepted types pass, and ffmpeg converts them with
// InputArgs the way prep does. Only encoders every ffmpeg build has (the
// bundled LGPL one included) are used.
func TestRealMediaAccepted(t *testing.T) {
	ffmpeg, ffprobe := realFFmpeg(t)
	dir := t.TempDir()
	video := []string{"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration=1", "-f", "lavfi", "-i", sine}
	cases := []struct {
		name   string
		args   []string
		format string // a demuxer of ffprobe's format_name
	}{
		{"a.wav", []string{"-f", "lavfi", "-i", sine}, "wav"},
		{"a.flac", []string{"-f", "lavfi", "-i", sine}, "flac"},
		{"a.m4a", []string{"-f", "lavfi", "-i", sine, "-c:a", "aac"}, "mov"},
		{"a.aiff", []string{"-f", "lavfi", "-i", sine}, "aiff"},
		{"a.ogg", []string{"-f", "lavfi", "-i", sine, "-c:a", "flac"}, "ogg"},
		{"a.wma", []string{"-f", "lavfi", "-i", sine, "-c:a", "wmav2"}, "asf"},
		{"v.mp4", append(append([]string{}, video...), "-c:v", "mpeg4", "-c:a", "aac"), "mov"},
		{"v.mkv", append(append([]string{}, video...), "-c:v", "mpeg4", "-c:a", "flac"), "matroska"},
		{"v.avi", append(append([]string{}, video...), "-c:v", "mpeg4", "-c:a", "pcm_s16le"), "avi"},
		{"v.ts", append(append([]string{}, video...), "-c:v", "mpeg2video", "-c:a", "mp2"), "mpegts"},
		{"v.mpg", append(append([]string{}, video...), "-c:v", "mpeg2video", "-c:a", "mp2", "-f", "mpeg"), "mpeg"},
		{"v.wmv", append(append([]string{}, video...), "-c:v", "wmv2", "-c:a", "wmav2"), "asf"},
		{"v.flv", append(append([]string{}, video...), "-c:v", "flv1", "-c:a", "aac", "-ar", "44100"), "flv"},
	}
	ctx := context.Background()
	for _, tc := range cases {
		p := filepath.Join(dir, tc.name)
		makeMedia(t, ffmpeg, p, tc.args...)
		format, refused, err := ProbeFormat(ctx, execRun, ffprobe, p)
		if err != nil || refused || !strings.Contains(","+format+",", ","+tc.format+",") {
			t.Errorf("%s: format %q refused %v err %v", tc.name, format, refused, err)
			continue
		}
		if pr, err := Check(ctx, execRun, ffmpeg, p); pr != nil || err != nil {
			t.Errorf("%s: Check = %+v, %v", tc.name, pr, err)
		}
		out := filepath.Join(dir, "audio.wav")
		args := append([]string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}, InputArgs()...)
		args = append(args, "-i", p, "-vn", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-f", "wav", out)
		if _, stderr, err := execRun(ctx, ffmpeg, args...); err != nil {
			t.Errorf("%s: ffmpeg with InputArgs: %v %s", tc.name, err, stderr)
		}
		os.Remove(out)
	}
}
