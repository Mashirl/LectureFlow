// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build realnet

package fetch

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/bridge"
)

// Two camera angles with the real aria2c and ffmpeg: audio + main picture +
// second picture (a file, and HLS segments that carry audio of their own):
// the lecture's media has video and audio, angle2.mp4 video only and
// decodes. Then the close with aria2c busy (no 4-second wait for aria2c's
// exit) and the resume of the interrupted task, whose progress never goes
// below what it had reached.
func TestRealDualAngleCloseAndResume(t *testing.T) {
	aria := realBinary(t, "LF_REAL_ARIA2C", "aria2c")
	ff := realBinary(t, "LF_REAL_FFMPEG", "ffmpeg")
	var ps *progressStore
	e := newQEnv(t, func(o *Options) {
		o.Launcher, o.ariaVersion = nil, nil // the real aria2c and its --version
		o.Aria2c, o.FFmpeg = aria, ff
		o.PollInterval = 100 * time.Millisecond
		o.IdleTimeout = time.Minute
		ps = &progressStore{Store: o.Store}
		o.Store = ps
	})
	ctx := context.Background()
	src := t.TempDir()
	ffrun(t, ff, src, "-f", "lavfi", "-i", "testsrc=d=6:s=320x240:r=25", "-f", "lavfi", "-i", "sine=d=6",
		"-c:v", "mpeg4", "-c:a", "aac", "-shortest", "av.mp4")
	ffrun(t, ff, src, "-i", "av.mp4", "-map", "0:v", "-c", "copy", "video.mp4")
	ffrun(t, ff, src, "-i", "av.mp4", "-map", "0:a", "-c", "copy", "audio.m4a")
	// the second camera: another picture, with a microphone of its own
	ffrun(t, ff, src, "-f", "lavfi", "-i", "testsrc2=d=6:s=320x240:r=25", "-f", "lavfi", "-i", "sine=f=880:d=6",
		"-c:v", "mpeg4", "-c:a", "aac", "-shortest", "cam2.mp4")
	ffrun(t, ff, src, "-f", "lavfi", "-i", "testsrc2=d=6:s=320x240:r=25", "-f", "lavfi", "-i", "sine=f=880:d=6",
		"-c:v", "mpeg2video", "-c:a", "aac", "-shortest", "-f", "hls", "-hls_time", "1", "-hls_playlist_type", "vod",
		"-hls_segment_filename", "cam%03d.ts", "cam2.m3u8")
	// a long file for the close and the resume
	ffrun(t, ff, src, "-f", "lavfi", "-i", "testsrc=d=60:s=640x480:r=25", "-c:v", "mpeg4", "-q:v", "2", "long.mp4")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		switch name := filepath.Base(p); filepath.Ext(name) {
		case ".mp4", ".m4a":
			e.media.put("/files/"+name, b)
		default:
			e.media.put("/s/"+name, b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bridge.Track{
		"file": e.fileTrack(bridge.KindVideo, "/files/cam2.mp4"),
		"hls":  e.hlsTrack(bridge.KindVideo, "/s/cam2.m3u8"),
	}
	for name, cam := range cases {
		id := e.enqueue(e.request("dual-"+name, e.fileTrack(bridge.KindAudio, "/files/audio.m4a"),
			e.fileTrack(bridge.KindVideo, "/files/video.mp4"), cam))
		info := e.wait(id, bridge.StateDone)
		media := e.lecture(info.LectureID).MediaPath
		if k := playable(t, ff, media); k != "va" {
			t.Errorf("%s: media streams %q", name, k)
		}
		if k := playable(t, ff, filepath.Join(filepath.Dir(media), AngleFile)); k != "v" {
			t.Errorf("%s: angle2.mp4 streams %q, want video only", name, k)
		}
	}

	// the close while aria2c downloads
	long := int64(len(readAll(t, filepath.Join(src, "long.mp4"))))
	e.media.stall("/files/long.mp4")
	id := e.enqueue(e.request("long", e.fileTrack(bridge.KindVideo, "/files/long.mp4")))
	e.waitFor("the first bytes of the long file reported", func() bool {
		info, err := e.q.Get(ctx, id)
		return err == nil && info.BytesDone >= 64<<10
	})
	start := time.Now()
	e.q.Close(5 * time.Second)
	closeTook := time.Since(start)
	t.Logf("the close took %s", closeTook)
	if closeTook > 3*time.Second {
		t.Errorf("the close took %s (aria2c's exit was waited for)", closeTook)
	}
	if e.q.eng.running() {
		t.Error("aria2c still runs after the close")
	}
	e.media.release()
	e.start()
	row := e.row(id)
	if row.State != bridge.StateNeedsRefresh || row.BytesDone < 64<<10 || row.BytesTotal != long {
		t.Fatalf("after the restart: %s, %d of %d", row.State, row.BytesDone, row.BytesTotal)
	}
	stored := row.BytesDone
	ps.reset()
	var mu sync.Mutex
	var pushed []bridge.Status
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := e.q.Subscribe(sctx)
	go func() {
		for st := range ch {
			mu.Lock()
			pushed = append(pushed, st)
			mu.Unlock()
		}
	}()
	if _, err := e.q.Refresh(ctx, &bridge.RefreshRequest{Platform: "panopto", SessionID: "long",
		Tracks: []bridge.Track{e.fileTrack(bridge.KindVideo, "/files/long.mp4")}, Referer: testReferer, Cookies: e.cookies()}); err != nil {
		t.Fatal(err)
	}
	info := e.wait(id, bridge.StateDone)
	if k := playable(t, ff, e.lecture(info.LectureID).MediaPath); k != "v" {
		t.Errorf("resumed file streams %q", k)
	}
	time.Sleep(200 * time.Millisecond)
	// aria2c's control file records whole pieces: the pieces in flight at
	// the stop (up to one per connection, 1 MiB each) are fetched again,
	// so the resume may count a little less than the stop had shown. Never
	// back to 0, never unknown.
	slack := int64(DefaultConnections) << 20
	for _, w := range ps.reset() {
		if w[0] < stored-slack || w[0] <= 0 || w[1] != long {
			t.Errorf("stored progress %d of %d after resuming from %d", w[0], w[1], stored)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	t.Logf("long %d, stored %d of %d", long, stored, row.BytesTotal)
	for _, st := range pushed {
		t.Logf("push %s %d of %d", st.State, st.BytesDone, st.BytesTotal)
	}
	for _, st := range pushed {
		if st.TaskID == id && (st.BytesDone < stored-slack || st.BytesDone <= 0 || st.BytesTotal != long) {
			t.Errorf("pushed %s %d of %d after resuming from %d", st.State, st.BytesDone, st.BytesTotal, stored)
		}
	}
	reqs := e.media.requests("/files/long.mp4")
	if last := reqs[len(reqs)-1]; last.Range == "" || last.Range == "bytes=0-" {
		t.Errorf("not resumed: %+v", last)
	}
}
