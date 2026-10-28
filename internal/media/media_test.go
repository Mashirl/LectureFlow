// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAllowedName(t *testing.T) {
	for name, want := range map[string]bool{
		"rec.mp4": true, "REC.MP4": true, "a.b.mkv": true, "x.Opus": true, "x.aif": true, "x.ts": true, "x.3gp": true,
		"x.m2ts": true, "X.MKA": true, "x.ogv": true, "x.mka.m3u8": false,
		"list.m3u8": false, "list.m3u": false, "x.ffconcat": false, "x.txt": false, "x.concat": false, "x.sdp": false,
		"x.mpd": false, "x.pls": false, "x.xspf": false, "x.mp4.m3u8": false, "x.m3u8.mp4": true,
		"noext": false, "x.mp4.": false, "x.mp4 ": false, "": false,
	} {
		if got := AllowedName(name); got != want {
			t.Errorf("AllowedName(%q) = %v, want %v", name, got, want)
		}
	}
	if len(Exts) != 28 || ExtList() != strings.Join(Exts, ", ") {
		t.Errorf("Exts %v", Exts)
	}
}

func TestSniffPlaylist(t *testing.T) {
	for head, want := range map[string]string{
		"#EXTM3U\n#EXT-X-TARGETDURATION:2\n":  "m3u8",
		"\xef\xbb\xbf#EXTM3U\r\n":             "m3u8",
		"  \r\n#extm3u\n":                     "m3u8",
		"ffconcat version 1.0\nfile 'a.wav'":  "ffconcat",
		"FFCONCAT version 1.0":                "ffconcat",
		"[playlist]\nFile1=x.mp3":             "pls",
		"<?xml version=\"1.0\"?><MPD>":        "xml",
		"<MPD xmlns=\"urn:mpeg:dash\">":       "mpd",
		"<smil>":                              "smil",
		"<ASX version=\"3.0\">":               "asx",
		"<playlist version=\"1\">":            "xspf",
		"v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\n": "sdp",
		"#!AMR\n\x3c\x00\x01\x02":             "", // a real AMR header
		"RIFF\x24\x00\x00\x00WAVEfmt ":        "",
		"\x00\x00\x00\x18ftypmp42":            "",
		"ID3\x04\x00\x00":                     "",
		"#EXTM":                               "", // shorter than the sign
		"":                                    "",
		"plain text file":                     "",
	} {
		if got := SniffPlaylist([]byte(head)); got != want {
			t.Errorf("SniffPlaylist(%q) = %q, want %q", head, got, want)
		}
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "x.mp4")
	if err := os.WriteFile(p, []byte("#EXTM3U\n"+strings.Repeat("x", 10000)), 0o644); err != nil {
		t.Fatal(err)
	}
	if k, err := SniffFile(p); err != nil || k != "m3u8" {
		t.Errorf("SniffFile = %q, %v", k, err)
	}
	empty := filepath.Join(dir, "empty.mp4")
	os.WriteFile(empty, nil, 0o644)
	if k, err := SniffFile(empty); err != nil || k != "" {
		t.Errorf("SniffFile(empty) = %q, %v", k, err)
	}
	if _, err := SniffFile(filepath.Join(dir, "missing.mp4")); err == nil {
		t.Error("SniffFile of a missing file: no error")
	}
}

func TestFormatAllowed(t *testing.T) {
	for f, want := range map[string]bool{
		"mov,mp4,m4a,3gp,3g2,mj2": true, "matroska,webm": true, "wav": true, "mpegts": true, "asf": true, "amr": true,
		"hls": false, "concat": false, "sdp": false, "rtsp": false, "dash": false, "image2": false, "webm_dash_manifest": false,
		"imf": false, "": false, "lavfi": false,
	} {
		if got := FormatAllowed(f); got != want {
			t.Errorf("FormatAllowed(%q) = %v, want %v", f, got, want)
		}
	}
	args := InputArgs()
	if len(args) != 4 || args[0] != "-format_whitelist" || args[1] != FormatWhitelist || args[2] != "-protocol_whitelist" || args[3] != "file" {
		t.Errorf("InputArgs %v", args)
	}
	for _, bad := range []string{"hls", "concat", "sdp", "dash", "image2", "rtsp", "imf"} {
		for _, d := range strings.Split(FormatWhitelist, ",") {
			if d == bad {
				t.Errorf("%s is in the format whitelist", bad)
			}
		}
	}
}

func TestUnsafeStoredPath(t *testing.T) {
	for p, want := range map[string]bool{
		`\\server\share\x.mp4`:              true,
		`//server/share/x.mp4`:              true,
		`\\?\UNC\server\share\x.mp4`:        true,
		`//?/UNC/server/share/x.mp4`:        true,
		`\??\UNC\server\share\x.mp4`:        true,
		`\\.\UNC\server\share\x.mp4`:        true,
		`\\.\C:\x.mp4`:                      true,
		`\\?\GLOBALROOT\Device\Mup\s\x.mp4`: true,
		`\\localhost\c$\x.mp4`:              true,
		`  \\server\share\x.mp4`:            true,
		`file://server/share/x.mp4`:         true,
		`file:///C:/x.mp4`:                  true,
		`smb://server/x.mp4`:                true,
		`concat:a.mp4|b.mp4`:                true,
		`\\?\C:\Lectures\x.mp4`:             false,
		`C:\Users\me\LectureFlow\x.mp4`:     false,
		`C:/Users/me/x.mp4`:                 false,
		`/Users/me/LectureFlow/x.mp4`:       false,
		`/Users/me/a:b/x.mp4`:               false,
		`media.mp4`:                         false,
		`\Users\x.mp4`:                      false,
		``:                                  false,
	} {
		if got := UnsafeStoredPath(p); got != want {
			t.Errorf("UnsafeStoredPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestWithinDir(t *testing.T) {
	type c struct {
		p, dir string
		want   bool
	}
	cases := []c{
		{`\\nas\share\LF\courses\a.mp4`, `\\nas\share\LF`, true},
		{`//nas/share/LF/courses/a.mp4`, `\\nas\share\LF\`, true},
		{`\\nas\share\LF`, `\\nas\share\LF`, true},
		{`\\nas\share\LF2\a.mp4`, `\\nas\share\LF`, false},
		{`\\evil\share\a.mp4`, `C:\data`, false},
		{`C:\data\x`, ``, false},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases, c{`\\NAS\Share\lf\a.mp4`, `\\nas\share\LF`, true})
	}
	for _, tc := range cases {
		if got := WithinDir(tc.p, tc.dir); got != tc.want {
			t.Errorf("WithinDir(%q, %q) = %v, want %v", tc.p, tc.dir, got, tc.want)
		}
	}
}

// fakeRun answers like ffprobe would and records the arguments.
type fakeRun struct {
	stdout, stderr string
	err            error
	args           []string
}

func (f *fakeRun) run(_ context.Context, _ string, args ...string) (string, string, error) {
	f.args = args
	return f.stdout, f.stderr, f.err
}

func TestProbeFormatFake(t *testing.T) {
	exit := errors.New("exit status 1")
	cases := []struct {
		name           string
		f              fakeRun
		format         string
		refused, isErr bool
	}{
		{"mp4", fakeRun{stdout: "mov,mp4,m4a,3gp,3g2,mj2\n"}, "mov,mp4,m4a,3gp,3g2,mj2", false, false},
		{"hls refused by the whitelist", fakeRun{stderr: "[hls @ 000001f0c5b38100] Format not on whitelist 'mov,mp4'\nx.m3u8: Invalid argument\n", err: exit}, "hls", true, false},
		{"refused, no demuxer named", fakeRun{stderr: "Format not on whitelist 'mov'\n", err: exit}, "", true, false},
		{"whitelist ignored", fakeRun{stdout: "concat\n"}, "concat", true, false},
		{"not media", fakeRun{stderr: "x.mp4: Invalid data found when processing input\n", err: exit}, "", false, true},
		{"empty output", fakeRun{stdout: "\n"}, "", false, true},
	}
	for _, tc := range cases {
		f := tc.f
		format, refused, err := ProbeFormat(context.Background(), f.run, "ffprobe", `C:\x\media.mp4`)
		if format != tc.format || refused != tc.refused || (err != nil) != tc.isErr {
			t.Errorf("%s: format %q refused %v err %v", tc.name, format, refused, err)
		}
		// the whitelists come before the input, which is last
		joined := strings.Join(f.args, " ")
		if !strings.Contains(joined, strings.Join(InputArgs(), " ")) || f.args[len(f.args)-1] != `C:\x\media.mp4` {
			t.Errorf("%s: args %v", tc.name, f.args)
		}
	}
}

func TestCheckFake(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// a playlist never reaches ffprobe
	called := false
	run := func(context.Context, string, ...string) (string, string, error) {
		called = true
		return "wav\n", "", nil
	}
	pr, err := Check(context.Background(), run, "ffmpeg", write("a.mp4", "#EXTM3U\n#EXTINF:1,\n\\\\evil\\s\\x.ts\n"))
	if err != nil || pr == nil || pr.Kind != ProblemPlaylist || pr.Format != "m3u8" || called {
		t.Fatalf("playlist: %+v %v (ffprobe called %v)", pr, err, called)
	}
	// no runner: only the sniffing
	if pr, err := Check(context.Background(), nil, "ffmpeg", write("b.wav", "RIFF....WAVE")); pr != nil || err != nil {
		t.Errorf("no runner: %+v %v", pr, err)
	}
	if _, err := Check(context.Background(), nil, "ffmpeg", filepath.Join(dir, "missing.wav")); err == nil {
		t.Error("missing file: no error")
	}
}
