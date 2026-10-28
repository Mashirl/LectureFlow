// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/bridge"
	"lectureflow/internal/db"
)

// The layout follows the order the extension sent: the first track that is
// not audio is the main picture, the second the second angle.
func TestLayoutOf(t *testing.T) {
	rec := func(kinds ...string) []trackRecord {
		out := make([]trackRecord, len(kinds))
		for i, k := range kinds {
			out[i].Kind = k
		}
		return out
	}
	for _, c := range []struct {
		kinds []string
		want  trackLayout
	}{
		{[]string{bridge.KindAudio}, trackLayout{0, -1, -1}},
		{[]string{bridge.KindVideo}, trackLayout{-1, 0, -1}},
		{[]string{bridge.KindVideo, bridge.KindAudio}, trackLayout{1, 0, -1}},
		{[]string{bridge.KindAudio, bridge.KindVideo, bridge.KindVideo}, trackLayout{0, 1, 2}},
		{[]string{bridge.KindVideo, bridge.KindAudio, bridge.KindUnknown}, trackLayout{1, 0, 2}},
		{[]string{bridge.KindVideo, bridge.KindVideo}, trackLayout{-1, 0, 1}},
	} {
		if got := layoutOf(rec(c.kinds...)); got != c.want {
			t.Errorf("%v: %+v, want %+v", c.kinds, got, c.want)
		}
	}
}

// Audio, the main picture and a second angle: the main picture and the
// audio become the lecture's media as before; the second angle is remuxed
// alone (first video stream, -c copy, no audio) into angle2.mp4, which the
// import receives with the media and moves into the lecture's directory.
func TestDualAngleFiles(t *testing.T) {
	e := newQEnv(t, nil)
	audio, video, video2 := pattern(1, 40_000), pattern(2, 120_000), pattern(3, 90_000)
	e.media.put("/files/a.m4a", audio)
	e.media.put("/files/v.mp4", video)
	e.media.put("/files/v2.mp4", video2)
	v2 := e.fileTrack(bridge.KindVideo, "/files/v2.mp4")
	v2.Role = "secondary-video"
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindAudio, "/files/a.m4a"), e.fileTrack(bridge.KindVideo, "/files/v.mp4"), v2))
	info := e.wait(id, bridge.StateDone)
	if len(info.Tracks) != 3 || info.Tracks[2].Role != "secondary-video" {
		t.Fatalf("tracks %+v", info.Tracks)
	}
	l := e.lecture(info.LectureID)
	if !bytes.Equal(readAll(t, l.MediaPath), append(append([]byte{}, video...), audio...)) {
		t.Fatal("the main media is not the main picture with the audio")
	}
	angle := filepath.Join(filepath.Dir(l.MediaPath), AngleFile)
	if !bytes.Equal(readAll(t, angle), video2) {
		t.Fatal("angle2.mp4 is not the second video")
	}
	imps := e.importsSnapshot()
	if len(imps) != 1 || len(imps[0].Angles) != 1 || imps[0].Angles[0].Name != AngleName {
		t.Fatalf("import %+v", imps)
	}
	calls := e.ffmpegCalls()
	if len(calls) != 2 {
		t.Fatalf("ffmpeg calls %v", calls)
	}
	remux, combine := strings.Join(calls[0], " "), strings.Join(calls[1], " ")
	for _, want := range []string{"-map 0:v:0 -c copy -an -f mp4", "t2.mp4"} {
		if !strings.Contains(remux, want) {
			t.Errorf("angle remux lacks %q: %s", want, remux)
		}
	}
	if strings.Count(remux, "-i ") != 1 {
		t.Errorf("the angle remux has another input: %s", remux)
	}
	if !strings.Contains(combine, "t1.mp4") || !strings.Contains(combine, "t0.m4a") || strings.Contains(combine, "t2.mp4") {
		t.Errorf("combine %s", combine)
	}
	e.waitFor("the task directory removed", func() bool {
		_, err := os.Stat(e.q.taskDir(id))
		return os.IsNotExist(err)
	})
}

// Two videos without an audio track: the main picture brings the audio of
// its manifest (HLS separate rendition), the second angle never does.
func TestDualAngleHLSWithoutAudioTrack(t *testing.T) {
	e := newQEnv(t, nil)
	hlsSegments(e, "v", 2, false)
	hlsSegments(e, "a", 2, false)
	hlsSegments(e, "w", 3, false)
	master := "#EXTM3U\n" +
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"g\",NAME=\"en\",DEFAULT=YES,URI=\"/a/index.m3u8\"\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1000,CODECS=\"avc1.4d401e\",AUDIO=\"g\"\n"
	e.media.put("/m/master.m3u8", []byte(master+"/v/index.m3u8\n"))
	e.media.put("/m2/master.m3u8", []byte(master+"/w/index.m3u8\n"))
	id := e.enqueue(e.request("s1", e.hlsTrack(bridge.KindVideo, "/m/master.m3u8"), e.hlsTrack(bridge.KindVideo, "/m2/master.m3u8")))
	info := e.wait(id, bridge.StateDone)
	if n := len(e.media.requests("/a/index.m3u8")); n != 1 {
		t.Fatalf("the audio rendition was read %d times, want once (for the main picture only)", n)
	}
	var joins, remux int
	for _, c := range e.ffmpegCalls() {
		j := strings.Join(c, " ")
		switch {
		case slices.Contains(c, "-an"):
			remux++
			if !strings.Contains(j, "t1.mp4") {
				t.Errorf("the remux is not of the second track: %s", j)
			}
		case strings.Contains(j, "index.m3u8"):
			joins++
			if strings.Contains(j, filepath.Join("t1", "a")) {
				t.Errorf("the second angle was joined with audio: %s", j)
			}
		}
	}
	if joins != 2 || remux != 1 {
		t.Fatalf("joins %d, remux %d: %v", joins, remux, e.ffmpegCalls())
	}
	l := e.lecture(info.LectureID)
	if _, err := os.Stat(filepath.Join(filepath.Dir(l.MediaPath), AngleFile)); err != nil {
		t.Fatal(err)
	}
}

// The second angle is remuxed before the main media is made: when the
// combine fails, the retry neither downloads nor remuxes the angle again.
func TestDualAngleRetryAfterCombineFailed(t *testing.T) {
	e := newQEnv(t, nil)
	t.Setenv("LF_FAKE_FFMPEG", "failcombine")
	e.media.put("/files/a.m4a", pattern(1, 4000))
	e.media.put("/files/v.mp4", pattern(2, 9000))
	e.media.put("/files/v2.mp4", pattern(3, 7000))
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindAudio, "/files/a.m4a"), e.fileTrack(bridge.KindVideo, "/files/v.mp4"),
		e.fileTrack(bridge.KindVideo, "/files/v2.mp4")))
	info := e.wait(id, bridge.StateFailed)
	if info.Error == nil || info.Error.Code != CodeMergeFailed {
		t.Fatalf("%+v", info.Error)
	}
	dir := e.q.taskDir(id)
	if !angleDone(dir) || findFinal(dir, "t2") != "" {
		t.Fatal("the angle is not remuxed (or its track file is left) before the combine")
	}
	// after a restart (no credentials) everything is on disk: the retry
	// goes on without the extension
	e.restart()
	t.Setenv("LF_FAKE_FFMPEG", "ok")
	adds, _, _, _, _ := e.aria.snapshot()
	before, remuxes := len(adds), 0
	if _, err := e.q.Retry(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	info = e.wait(id, bridge.StateDone)
	if adds, _, _, _, _ := e.aria.snapshot(); len(adds) != before {
		t.Fatalf("the retry downloaded again: %d → %d", before, len(adds))
	}
	for _, c := range e.ffmpegCalls() {
		if slices.Contains(c, "-an") {
			remuxes++
		}
	}
	if remuxes != 1 {
		t.Fatalf("the angle was remuxed %d times", remuxes)
	}
	l := e.lecture(info.LectureID)
	if !bytes.Equal(readAll(t, filepath.Join(filepath.Dir(l.MediaPath), AngleFile)), pattern(3, 7000)) {
		t.Fatal("angle2.mp4 differs")
	}
}

// A second "video" without a video stream fails with its own code.
func TestDualAngleNoVideo(t *testing.T) {
	e := newQEnv(t, nil)
	t.Setenv("LF_FAKE_FFMPEG", "novideo")
	e.media.put("/files/v.mp4", pattern(2, 9000))
	e.media.put("/files/v2.mp4", pattern(3, 7000))
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindVideo, "/files/v.mp4"), e.fileTrack(bridge.KindUnknown, "/files/v2.mp4")))
	info := e.wait(id, bridge.StateFailed)
	if info.Error == nil || info.Error.Code != CodeAngleNoVideo || !strings.Contains(info.Error.Detail, "matches no streams") {
		t.Fatalf("%+v", info.Error)
	}
}

// A refresh matches two videos without roles by their order. (The 403 of
// the second stops the first while its add is in flight: an add answered
// after the stop must not leave its download running in aria2c, which
// would complete the partial file behind the queue's back; the resume then
// asked for the bytes after its end and failed with 416.)
func TestDualAngleRefreshKeepsOrder(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	e.media.put("/files/v.mp4", pattern(2, 9000))
	e.media.put("/files/v2.mp4", pattern(3, 7000))
	e.media.setStatus("/files/v2.mp4", 403)
	v, v2 := e.fileTrack(bridge.KindVideo, "/files/v.mp4"), e.fileTrack(bridge.KindVideo, "/files/v2.mp4")
	v.Role, v2.Role = "", ""
	id := e.enqueue(e.request("s1", v, v2))
	e.wait(id, bridge.StateNeedsRefresh)
	e.media.setStatus("/files/v2.mp4", 0)
	if _, err := e.q.Refresh(ctx, &bridge.RefreshRequest{Platform: "panopto", SessionID: "s1",
		Tracks: []bridge.Track{v, v2}, Referer: testReferer, Cookies: e.cookies()}); err != nil {
		t.Fatal(err)
	}
	info := e.wait(id, bridge.StateDone)
	l := e.lecture(info.LectureID)
	if !bytes.Equal(readAll(t, l.MediaPath), pattern(2, 9000)) ||
		!bytes.Equal(readAll(t, filepath.Join(filepath.Dir(l.MediaPath), AngleFile)), pattern(3, 7000)) {
		t.Fatal("the refresh swapped the main picture and the second angle")
	}
}

// ---- the progress of a resumed task (M1 acceptance) ----

// progressStore records the progress every write of the queue stores.
type progressStore struct {
	Store
	mu     sync.Mutex
	writes [][2]int64 // done, total
}

func (p *progressStore) UpdateDownloadTask(ctx context.Context, t db.DownloadTask) error {
	p.mu.Lock()
	p.writes = append(p.writes, [2]int64{t.BytesDone, t.BytesTotal})
	p.mu.Unlock()
	return p.Store.UpdateDownloadTask(ctx, t)
}

func (p *progressStore) SetDownloadProgress(ctx context.Context, id, done, total int64, updatedAt string) error {
	p.mu.Lock()
	p.writes = append(p.writes, [2]int64{done, total})
	p.mu.Unlock()
	return p.Store.SetDownloadProgress(ctx, id, done, total, updatedAt)
}

func (p *progressStore) reset() []([2]int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.writes
	p.writes = nil
	return out
}

// A task stopped half way (the app closed) keeps the bytes it reached, and
// its resume never shows less: not while aria2c has not yet reported (it
// says 0 of unknown until it has read its control file), not in the
// database, not in the status pushes.
func TestResumeKeepsProgress(t *testing.T) {
	var ps *progressStore
	e := newQEnv(t, func(o *Options) {
		ps = &progressStore{Store: o.Store}
		o.Store = ps
	})
	ctx := context.Background()
	video := pattern(4, 400_000)
	half := int64(len(video) / 2)
	e.media.put("/files/v.mp4", video)
	e.media.hold("/files/v.mp4")
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindVideo, "/files/v.mp4")))
	e.waitFor("half of the file reported", func() bool {
		info, err := e.q.Get(ctx, id)
		return err == nil && info.BytesDone >= half
	})
	e.restart() // well within the 5 s of the progress writes: Close stores it
	row := e.row(id)
	if row.State != bridge.StateNeedsRefresh || row.BytesDone != half || row.BytesTotal != int64(len(video)) {
		t.Fatalf("after the restart: state %s, %d of %d bytes", row.State, row.BytesDone, row.BytesTotal)
	}
	e.media.release()
	ps.reset()
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var pushes []bridge.Status
	var pmu sync.Mutex
	ch := e.q.Subscribe(sctx)
	go func() {
		for st := range ch {
			pmu.Lock()
			pushes = append(pushes, st)
			pmu.Unlock()
		}
	}()
	if _, err := e.q.Refresh(ctx, &bridge.RefreshRequest{Platform: "panopto", SessionID: "s1",
		Tracks: []bridge.Track{e.fileTrack(bridge.KindVideo, "/files/v.mp4")}, Referer: testReferer, Cookies: e.cookies()}); err != nil {
		t.Fatal(err)
	}
	e.wait(id, bridge.StateDone)
	time.Sleep(100 * time.Millisecond) // the last push
	for _, w := range ps.reset() {
		if w[0] < half || w[1] != int64(len(video)) {
			t.Errorf("stored progress %d of %d after resuming from %d of %d", w[0], w[1], half, len(video))
		}
	}
	pmu.Lock()
	defer pmu.Unlock()
	if len(pushes) == 0 {
		t.Fatal("no status push")
	}
	for _, st := range pushes {
		if st.TaskID == id && (st.BytesDone < half || st.BytesTotal != int64(len(video))) {
			t.Errorf("pushed %s %d of %d after resuming from %d", st.State, st.BytesDone, st.BytesTotal, half)
		}
	}
}

// Sent again with the same tracks after a failure: the partial files stay
// and so does the progress (a fresh row otherwise).
func TestReenqueueKeepsProgress(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	e.media.put("/files/v.mp4", pattern(4, 50_000))
	e.media.setStatus("/files/v.mp4", 500)
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindVideo, "/files/v.mp4")))
	e.wait(id, bridge.StateFailed)
	rec := e.row(id)
	rec.BytesDone, rec.BytesTotal = 1234, 50_000
	if err := e.opts.Store.UpdateDownloadTask(ctx, rec); err != nil {
		t.Fatal(err)
	}
	e.media.hold("/files/v.mp4")
	e.media.setStatus("/files/v.mp4", 0)
	e.q.mu.Lock()
	delete(e.q.tasks, id) // as after a restart: only the row is left
	e.q.mu.Unlock()
	e.enqueue(e.request("s1", e.fileTrack(bridge.KindVideo, "/files/v.mp4")))
	if row := e.row(id); row.BytesDone < 1234 || row.BytesTotal != 50_000 {
		t.Fatalf("progress after sending again: %d of %d", row.BytesDone, row.BytesTotal)
	}
	e.media.release()
	e.wait(id, bridge.StateDone)
}

// ---- closing with aria2c running ----

// aria2c answers forceShutdown but takes seconds to exit (1.37.0: 4.3–4.5
// s): the close kills it after forceExitWait instead of waiting.
func TestCloseDoesNotWaitForSlowAria2c(t *testing.T) {
	e := newQEnv(t, nil)
	e.aria.mu.Lock()
	e.aria.slowExit = true
	e.aria.mu.Unlock()
	if _, err := e.q.eng.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.q.eng.release()
	start := time.Now()
	e.q.Close(5 * time.Second)
	if d := time.Since(start); d > forceExitWait+1500*time.Millisecond {
		t.Fatalf("the close took %s", d)
	}
	if e.aria.running() {
		t.Fatal("aria2c still runs after the close")
	}
	if _, _, _, _, shutdowns := e.aria.snapshot(); shutdowns != 1 {
		t.Fatalf("%d shutdown calls", shutdowns)
	}
}
