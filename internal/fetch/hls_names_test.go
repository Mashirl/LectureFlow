// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"crypto/aes"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/bridge"
	lfmedia "lectureflow/internal/media"
)

// Security review 3, I1: the local playlist's whitelist admits the hls
// demuxer (media.HLSFormatWhitelist), and ffmpeg opens a segment named
// .m3u8 as a nested playlist. So a segment or init section is saved under
// a media extension only (hlsPartExt: the URL's when it is one, else the
// neutral .seg, which ffmpeg identifies by content), and a part that is a
// playlist is refused before ffmpeg runs (refusePlaylistParts).

func TestHLSPartNamesNeverPlaylists(t *testing.T) {
	base := mustURL(t, "https://media.example.edu/s/v720.m3u8?Policy=x")
	segs := []struct{ uri, name string }{
		{"seg1.m3u8", "s00001.seg"},
		{"seg2.M3U", "s00002.seg"},
		{"seg3.mpd?x=1", "s00003.seg"},
		{"seg4.jpg", "s00004.seg"},
		{"seg5.php?id=5", "s00005.seg"},
		{"seg6", "s00006.ts"}, // no extension: the default, as before
		{"seg7.ts", "s00007.ts"},
		{"seg8.m4s", "s00008.m4s"},
		{"seg9.aac", "s00009.aac"},
		{"seg10.MP4", "s00010.mp4"},
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:6\n#EXT-X-MAP:URI=\"init.m3u8\"\n")
	b.WriteString("#EXT-X-KEY:METHOD=AES-128,URI=\"key.m3u8\",IV=0x01\n")
	for _, s := range segs {
		b.WriteString("#EXTINF:6,\n" + s.uri + "\n")
	}
	b.WriteString("#EXT-X-MAP:URI=\"init2\"\n#EXTINF:6,\nlast.m3u8\n#EXT-X-ENDLIST\n")
	m, err := parseMedia([]byte(b.String()), base)
	if err != nil {
		t.Fatal(err)
	}
	// by the URL's last path element (the URLs carry the playlist's query)
	want := map[string]string{"init.m3u8": "m001.seg", "init2": "m002.mp4", "key.m3u8": "k001.key", "last.m3u8": "s00011.seg"}
	for _, s := range segs {
		want[strings.SplitN(s.uri, "?", 2)[0]] = s.name
	}
	got := map[string]string{}
	for u, name := range m.names {
		got[path.Base(mustURL(t, u).Path)] = name
	}
	for k, name := range want {
		if got[k] != name {
			t.Errorf("%s saved as %q, want %q", k, got[k], name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("names %v", m.names)
	}
	local := string(m.local())
	for _, l := range strings.Split(local, "\n") {
		if strings.HasPrefix(l, "#EXTM3U") {
			continue
		}
		if low := strings.ToLower(l); strings.Contains(low, ".m3u") || strings.Contains(low, ".mpd") {
			t.Errorf("the local playlist names a playlist: %q\n%s", l, local)
		}
	}
	// every lecture media extension is kept
	for _, e := range lfmedia.Exts {
		if got := hlsPartExt("https://x.example/a/b."+e+"?q=1", ".ts"); got != "."+e {
			t.Errorf("hlsPartExt(.%s) = %q", e, got)
		}
	}
}

// hlsDir serves a media playlist at /dir/index.m3u8 whose segments have
// URLs ending in .m3u8 (n of them, with an AES-128 key when key is set);
// content(i) is segment i's bytes before encryption.
func hlsDir(t *testing.T, e *qenv, dir string, n int, key bool, content func(i int) []byte) {
	t.Helper()
	k, iv := []byte("0123456789abcdef"), make([]byte, aes.BlockSize)
	iv[15] = 0x0A
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-PLAYLIST-TYPE:VOD\n")
	if key {
		e.media.put("/"+dir+"/key.bin", k)
		b.WriteString("#EXT-X-KEY:METHOD=AES-128,URI=\"/" + dir + "/key.bin?kt=" + testSig + "\",IV=0x0000000000000000000000000000000A\n")
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("seg%d.m3u8", i)
		body := content(i)
		if key {
			body = encryptCBC(t, k, iv, body)
		}
		e.media.put("/"+dir+"/"+name, body)
		b.WriteString("#EXTINF:1.0,\n" + name + "\n")
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	e.media.put("/"+dir+"/index.m3u8", []byte(b.String()))
}

// With the fake ffmpeg: segments whose URLs end in .m3u8 are saved as .seg
// and joined; a segment that is itself a playlist fails the task before
// ffmpeg runs (downloads.playlist_invalid), nothing imported.
func TestHLSSegmentsNamedM3U8(t *testing.T) {
	e := newQEnv(t, nil)
	hlsDir(t, e, "ok", 3, false, func(i int) []byte { return pattern(byte(i+1), 2000+i) })
	info := e.wait(e.enqueue(e.request("ok", e.hlsTrack(bridge.KindVideo, "/ok/index.m3u8"))), bridge.StateDone)
	var want []byte
	for i := 0; i < 3; i++ {
		want = append(want, pattern(byte(i+1), 2000+i)...)
	}
	if got := readAll(t, e.lecture(info.LectureID).MediaPath); string(got) != string(want) {
		t.Fatalf("joined %d bytes, want %d", len(got), len(want))
	}
	adds, _, _, _, _ := e.aria.snapshot()
	for _, a := range adds {
		if strings.Contains(a.URI, "/ok/seg") && filepath.Ext(strings.TrimSuffix(a.Out, ".part")) != ".seg" {
			t.Errorf("segment %s saved as %s", a.URI, a.Out)
		}
	}

	nested := []byte("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:1.0,\n" + filepath.Join(e.root, "private.ts") + "\n#EXT-X-ENDLIST\n")
	// (unencrypted: the first bytes of an encrypted part are not looked at;
	// its .seg name is what keeps ffmpeg from reading it as a playlist)
	hlsDir(t, e, "bad", 2, false, func(i int) []byte {
		if i == 1 {
			return nested
		}
		return pattern(5, 3000)
	})
	before := len(e.ffmpegCalls())
	info = e.wait(e.enqueue(e.request("bad", e.hlsTrack(bridge.KindVideo, "/bad/index.m3u8"))), bridge.StateFailed)
	if info.Error == nil || info.Error.Code != CodePlaylistInvalid || !strings.Contains(info.Error.Detail, "m3u8") {
		t.Fatalf("task error %+v", info.Error)
	}
	if n := len(e.ffmpegCalls()); n != before {
		t.Errorf("ffmpeg ran on a playlist segment (%d calls)", n-before)
	}
	if len(e.importsSnapshot()) != 1 {
		t.Errorf("imports %+v", e.importsSnapshot())
	}
}

// With the real ffmpeg: AES-128 encrypted MPEG-TS segments and fragmented
// MP4 parts (init section and segments) whose URLs end in .m3u8 are saved
// as .seg, identified by their content and joined.
func TestRealHLSSegmentsNamedM3U8(t *testing.T) {
	ff := realFFmpeg(t)
	gen := t.TempDir()
	if b, err := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration=3", "-f", "lavfi", "-i", realSine,
		"-c:v", "mpeg2video", "-g", "10", "-c:a", "mp2", "-f", "segment", "-segment_time", "1", "-segment_format", "mpegts",
		filepath.Join(gen, "ts%d.ts")).CombinedOutput(); err != nil {
		t.Fatalf("segments: %v\n%s", err, b)
	}
	fmp4 := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration=3", "-f", "lavfi", "-i", realSine,
		"-c:v", "mpeg4", "-g", "10", "-c:a", "aac", "-f", "hls", "-hls_time", "1", "-hls_segment_type", "fmp4",
		"-hls_fmp4_init_filename", "init.mp4", "-hls_segment_filename", "f%d.m4s", "f.m3u8")
	fmp4.Dir = gen // the init section is written relative to the working directory
	if b, err := fmp4.CombinedOutput(); err != nil {
		t.Fatalf("fmp4 segments: %v\n%s", err, b)
	}
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(gen, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	tsSegs, _ := filepath.Glob(filepath.Join(gen, "ts*.ts"))
	fSegs, _ := filepath.Glob(filepath.Join(gen, "f*.m4s"))
	if len(tsSegs) < 2 || len(fSegs) < 2 {
		t.Fatalf("segments %v %v", tsSegs, fSegs)
	}

	e := newQEnv(t, func(o *Options) { o.FFmpeg = ff })
	hlsDir(t, e, "ts", len(tsSegs), true, func(i int) []byte { return read(fmt.Sprintf("ts%d.ts", i)) })
	info := e.wait(e.enqueue(e.request("ts", e.hlsTrack(bridge.KindVideo, "/ts/index.m3u8"))), bridge.StateDone)
	checkMedia(t, ff, e.lecture(info.LectureID).MediaPath, 2)

	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:1\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-MAP:URI=\"init.m3u8\"\n")
	e.media.put("/fm/init.m3u8", read("init.mp4"))
	for i := range fSegs {
		name := fmt.Sprintf("f%d.m3u8", i)
		e.media.put("/fm/"+name, read(fmt.Sprintf("f%d.m4s", i)))
		b.WriteString("#EXTINF:1.0,\n" + name + "\n")
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	e.media.put("/fm/index.m3u8", []byte(b.String()))
	info = e.wait(e.enqueue(e.request("fm", e.hlsTrack(bridge.KindVideo, "/fm/index.m3u8"))), bridge.StateDone)
	checkMedia(t, ff, e.lecture(info.LectureID).MediaPath, 2)
	adds, _, _, _, _ := e.aria.snapshot()
	for _, a := range adds {
		if strings.Contains(a.URI, ".m3u8") && strings.HasSuffix(a.Out, ".m3u8") {
			t.Errorf("%s saved as %s", a.URI, a.Out)
		}
	}
}
