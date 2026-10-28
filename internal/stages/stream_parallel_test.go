// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/llm"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// stream-parallel (user decision 2026-10-27): a source that must be
// transcoded no longer holds up prep. prep writes audio.wav and queues the
// stream task, which makes stream.mp4 in the background — after prep is
// done, in a CPU slot, in parallel with asr and the later stages.

// prepThenStream runs prep with fm and checks that it is done, made no
// playback file itself (no stream.mp4 call, no kv state) and queued the
// stream task; then runs that task with fm. It returns both runs.
func (e *env) prepThenStream(fm *fakeMedia, lectureID string) (prep, stream runResult) {
	e.t.Helper()
	prep = e.run(&prepStage{d: e.deps(nil), media: fm}, lectureID)
	if prep.err != nil || prep.job.Status != model.StatusDone {
		e.t.Fatalf("prep %v %s", prep.err, prep.job.Status)
	}
	if sc := fm.streamCalls(); len(sc) != 0 {
		e.t.Fatalf("prep transcoded itself: %v", sc)
	}
	if st, ok, _ := StreamStateOf(e.ctx, e.st, lectureID); ok {
		e.t.Fatalf("prep recorded a playback file state %+v for a transcode", st)
	}
	if j := e.job(model.ScopeLecture, lectureID, model.StageStream); j.Status != model.StatusPending {
		e.t.Fatalf("stream task %s after prep, want pending", j.Status)
	}
	return prep, e.run(&streamStage{d: e.deps(nil), media: fm}, lectureID)
}

// hasCurrentJob reports whether the lecture has a current job of stage.
func (e *env) hasCurrentJob(lectureID, stage string) bool {
	e.t.Helper()
	_, err := e.st.GetCurrentJob(e.ctx, model.ScopeLecture, lectureID, stage)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		e.t.Fatal(err)
	}
	return err == nil
}

// drive moves a job through the given statuses (state.TransitionJob).
func (e *env) drive(jobID string, from string, to ...string) {
	e.t.Helper()
	for _, s := range to {
		e.must(state.TransitionJob(e.ctx, e.st, jobID, from, s, db.JobFinish{}))
		from = s
	}
}

// TestPrepQueuesStreamForTranscodeOnly: a transcode is left to the stream
// task (queued, with a job event for the UI; prep's progress is audio.wav
// only; no kv state; under keep_original_media = false the original stays,
// without a note); a remux, an audio-only source and a failed probe are
// settled by prep as before and queue nothing.
func TestPrepQueuesStreamForTranscodeOnly(t *testing.T) {
	e := newEnv(t)
	e.cfg.Storage.KeepOriginalMedia = boolp(false)
	l, dir := e.mediaLecture("transcode")
	fm := &fakeMedia{probe: probeHEVC}
	r := e.run(&prepStage{d: e.deps(nil), media: fm}, l.ID)
	if r.err != nil || r.job.Status != model.StatusDone {
		t.Fatalf("prep %v %s", r.err, r.job.Status)
	}
	if calls := fm.ffmpegCalls(); len(calls) != 1 || isStreamCall(calls[0]) || fm.encCalls != 0 {
		t.Errorf("prep ffmpeg calls %v, encoder checks %d: audio.wav only", calls, fm.encCalls)
	}
	if vals := r.progress.vals; !monotone(vals) || !slices.Contains(vals, 0.5) || r.progress.last() != 1 {
		t.Errorf("progress %v: audio.wav is all of prep", vals)
	}
	if _, ok, _ := StreamStateOf(e.ctx, e.st, l.ID); ok {
		t.Error("prep recorded a stream state for a transcode")
	}
	j := e.job(model.ScopeLecture, l.ID, model.StageStream)
	if j.Status != model.StatusPending || !model.IsEmptySnapshot(j.ConfigSnapshot) || j.Attempt != 0 {
		t.Errorf("stream task %+v", j)
	}
	if evs := e.ev.jobEvents(l.ID, model.StageStream); len(evs) != 1 || evs[0].ID != j.ID || evs[0].Status != model.StatusPending {
		t.Errorf("stream job events %+v", evs)
	}
	if _, err := os.Stat(filepath.Join(dir, "m.mp4")); err != nil {
		t.Errorf("keep_original_media=false: prep deleted the original the stream task needs: %v", err)
	}
	if r.job.Stats.NoteInfo != nil || r.job.Stats.Note != "" {
		t.Errorf("prep note %q %+v", r.job.Stats.Note, r.job.Stats.NoteInfo)
	}
	if _, err := os.Stat(filepath.Join(dir, model.AudioFile)); err != nil {
		t.Errorf("audio.wav: %v", err)
	}

	for _, c := range []struct {
		name   string
		fm     *fakeMedia
		status string
	}{
		{"remux", &fakeMedia{probe: probeMP4}, StreamReady},
		{"audio only", &fakeMedia{probe: probeM4A}, StreamAudioOnly},
		{"probe failed", &fakeMedia{probeErr: errors.New("找不到 ffprobe")}, StreamFailed},
	} {
		l, _ := e.mediaLecture(c.name)
		if r := e.run(&prepStage{d: e.deps(nil), media: c.fm}, l.ID); r.err != nil {
			t.Fatalf("%s: %v", c.name, r.err)
		}
		if e.hasCurrentJob(l.ID, model.StageStream) {
			t.Errorf("%s: stream task queued", c.name)
		}
		if st := e.streamState(l.ID); st.Status != c.status {
			t.Errorf("%s: state %+v", c.name, st)
		}
	}
}

// TestPrepRequeuesStream: each prep of a transcode source asks for one run
// of the stream task (state.EnqueueAux): after a done or failed run a new
// one; while one waits or runs, none (that one makes the file).
func TestPrepRequeuesStream(t *testing.T) {
	e := newEnv(t)
	for _, prev := range []string{model.StatusDone, model.StatusFailed} {
		l, _ := e.mediaLecture("after " + prev)
		e.enqueueStream(l.ID)
		old := e.job(model.ScopeLecture, l.ID, model.StageStream)
		e.drive(old.ID, model.StatusPending, model.StatusQueued, model.StatusRunning, prev)
		if r := e.run(&prepStage{d: e.deps(nil), media: &fakeMedia{probe: probeHEVC}}, l.ID); r.err != nil {
			t.Fatal(r.err)
		}
		if j := e.job(model.ScopeLecture, l.ID, model.StageStream); j.ID == old.ID || j.Status != model.StatusPending {
			t.Errorf("after a %s run: stream task %s %s (old %s)", prev, j.ID, j.Status, old.ID)
		}
	}
	for _, cur := range []string{model.StatusPending, model.StatusRunning} {
		l, _ := e.mediaLecture("while " + cur)
		e.enqueueStream(l.ID)
		old := e.job(model.ScopeLecture, l.ID, model.StageStream)
		if cur == model.StatusRunning {
			e.drive(old.ID, model.StatusPending, model.StatusQueued, model.StatusRunning)
		}
		r := e.run(&prepStage{d: e.deps(nil), media: &fakeMedia{probe: probeHEVC}}, l.ID)
		if r.err != nil || r.job.Status != model.StatusDone {
			t.Fatalf("prep %v %s", r.err, r.job.Status)
		}
		if j := e.job(model.ScopeLecture, l.ID, model.StageStream); j.ID != old.ID || j.Status != cur {
			t.Errorf("while %s: stream task %s %s, want the same job", cur, j.ID, j.Status)
		}
	}
}

// TestStreamTaskDeletesOriginalLast (keep_original_media = false, media-p2
// rule kept): the original goes only when the stream task made stream.mp4
// (ready); it stays (with the task's note) when the task failed or found
// too little space, and when prep is being run again meanwhile (a rerun
// reads it).
func TestStreamTaskDeletesOriginalLast(t *testing.T) {
	e := newEnv(t)
	e.cfg.Storage.KeepOriginalMedia = boolp(false)
	original := func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, "m.mp4"))
		return err == nil
	}

	l, dir := e.mediaLecture("ready")
	if _, r := e.prepThenStream(&fakeMedia{probe: probeHEVC}, l.ID); r.err != nil || r.job.Status != model.StatusDone {
		t.Fatalf("stream task %v %s", r.err, r.job.Status)
	}
	if original(dir) {
		t.Error("the original must go once the stream task made stream.mp4")
	}
	if _, err := os.Stat(filepath.Join(dir, model.StreamFile)); err != nil {
		t.Errorf("stream.mp4: %v", err)
	}

	unsupported := func(args []string) (string, error) {
		if isStreamCall(args) {
			return "[vist#0:1] Decoder (codec hevc) not found for input stream #0:1", errors.New("exit status 1")
		}
		return "", nil
	}
	for _, c := range []struct {
		name, status string
		fm           *fakeMedia
		failed       bool
	}{
		{"failed", StreamFailed, &fakeMedia{probe: probeHEVC, fail: unsupported}, true},
		{"no space", StreamNoSpace, &fakeMedia{probe: probeHEVC, free: 1 << 30}, false},
	} {
		l, dir := e.mediaLecture(c.name)
		_, r := e.prepThenStream(c.fm, l.ID)
		if (r.job.Status == model.StatusFailed) != c.failed {
			t.Errorf("%s: stream task %v %s", c.name, r.err, r.job.Status)
		}
		if !original(dir) {
			t.Errorf("%s: the original was deleted without a playback file", c.name)
		}
		if st := e.streamState(l.ID); st.Status != c.status {
			t.Errorf("%s: state %+v", c.name, st)
		}
		if n := r.job.Stats.NoteInfo; infoCode(n) != NotePrepKeptOriginal || !paramsMatch(n, "stream_status", c.status) {
			t.Errorf("%s: stream task note %+v", c.name, n)
		}
		// the user's retry (a new run) makes it, and then the original goes
		e.enqueueStream(l.ID)
		if r := e.run(&streamStage{d: e.deps(nil), media: &fakeMedia{probe: probeHEVC}}, l.ID); r.err != nil {
			t.Fatal(r.err)
		}
		if original(dir) {
			t.Errorf("%s: the original stayed after the retry made stream.mp4", c.name)
		}
	}

	// prep is run again while the stream task runs: the original stays
	l, dir = e.mediaLecture("prep rerun")
	block := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	fm := &fakeMedia{probe: probeHEVC, fail: func(args []string) (string, error) {
		if isStreamCall(args) {
			once.Do(func() { close(entered) })
			<-block
		}
		return "", nil
	}}
	if r := e.run(&prepStage{d: e.deps(nil), media: fm}, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	done := make(chan runResult, 1)
	go func() { done <- e.run(&streamStage{d: e.deps(nil), media: fm}, l.ID) }()
	<-entered
	e.must(state.Rerun(e.ctx, e.st, e.cfg.DataDir, model.ScopeLecture, l.ID, model.StagePrep))
	close(block)
	if r := <-done; r.err != nil || r.job.Status != model.StatusDone {
		t.Fatalf("stream task %v %s", r.err, r.job.Status)
	}
	if !original(dir) {
		t.Error("the stream task deleted the original while prep runs again")
	}
	if st := e.streamState(l.ID); st.Status != StreamReady {
		t.Errorf("state %+v", st)
	}
}

// ---------- through the scheduler ----------

// gatedStage runs inner after wait returns (nil: at once).
type gatedStage struct {
	sched.Stage
	wait func(ctx context.Context, id string) error
}

func (g *gatedStage) Run(ctx context.Context, id string, progress func(float64)) error {
	if g.wait != nil {
		if err := g.wait(ctx, id); err != nil {
			return err
		}
	}
	return g.Stage.Run(ctx, id, progress)
}

// streamSched is a scheduler with every stage, the fake ASR, the fake LLM
// and media tools fm for prep and the stream task; asrWait (optional) gates
// asr. Not started.
func (e *env) streamSched(fm mediaTools, asrWait func(ctx context.Context, id string) error) *sched.Scheduler {
	e.t.Helper()
	abs, _ := filepath.Abs(fakeASR)
	e.cfg.ASR.FakeResult = abs
	var s *sched.Scheduler
	d := e.deps(nil)
	d.Invalidate = func(ctx context.Context, courseID string, cause state.StaleCause) error {
		return s.InvalidateCourseWith(ctx, courseID, cause)
	}
	s, err := sched.New(sched.Options{
		Store: e.st, Config: e.cfg, Events: e.ev, DataDir: e.cfg.DataDir,
		GPUQuery: func(context.Context) (sched.GPUMem, error) {
			return sched.GPUMem{FreeMB: 20000, TotalMB: 24000, UsedMB: 1000}, nil
		},
		ScanInterval: 5 * time.Millisecond, ProgressMinInterval: 5 * time.Millisecond, IncrementalDebounce: 20 * time.Millisecond,
		RetryBackoff: []time.Duration{20 * time.Millisecond, 40 * time.Millisecond},
		// a transcode holds its CPU slot for as long as it runs: room for
		// prep and export of the other lectures meanwhile on any machine
		CPUSlots: 4,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	d.GPUTotalMB = func(ctx context.Context) int { return s.GPUStatus(ctx).TotalMB }
	client := llm.New(e.cfg, s.Pool(), "", nil)
	client.Backoff = []time.Duration{time.Millisecond}
	d.LLM = client
	for _, stg := range All(d) {
		switch stg.Name() {
		case model.StagePrep:
			stg = &prepStage{d: d, media: fm}
		case model.StageStream:
			stg = &streamStage{d: d, media: fm}
		case model.StageASR:
			stg = &gatedStage{Stage: stg, wait: asrWait}
		}
		s.Register(stg)
	}
	return s
}

// waitFor polls cond for up to timeout (failing the test after it).
func (e *env) waitFor(what string, timeout time.Duration, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *env) pipelineDone(lectureID string) bool {
	for _, st := range model.LectureStages {
		if e.job(model.ScopeLecture, lectureID, st).Status != model.StatusDone {
			return false
		}
	}
	return true
}

// TestStreamParallelWithPipeline: with the real scheduler, a lecture whose
// source must be transcoded starts asr as soon as audio.wav exists, while
// its stream task transcodes (asr here only proceeds once that task is
// running); the whole pipeline finishes with the transcode still going,
// and the playback file is ready afterwards. A second lecture's transcode
// fails: its pipeline finishes all the same, the task stays failed (no
// automatic retry) with the D6 code in kv.
func TestStreamParallelWithPipeline(t *testing.T) {
	e := newEnv(t)
	lA, dirA := e.mediaLecture("A")
	lB, _ := e.mediaLecture("B")
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	fm := &fakeMedia{probe: probeHEVC, fail: func(args []string) (string, error) {
		if !isStreamCall(args) {
			return "", nil
		}
		switch out := args[len(args)-1]; {
		case strings.Contains(out, lA.ID):
			once.Do(func() { close(started) })
			<-release
			return "", nil
		case strings.Contains(out, lB.ID):
			return "[vist#0:1] Decoder (codec hevc) not found for input stream #0:1", errors.New("exit status 1")
		}
		return "", nil
	}}
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	s := e.streamSched(fm, func(ctx context.Context, id string) error {
		if id != lA.ID {
			return nil
		}
		select { // asr of A runs only alongside A's transcode
		case <-started:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Second):
			return errors.New("the stream task did not run in parallel with asr")
		}
	})
	e.must(state.Recover(e.ctx, e.st, e.cfg.DataDir))
	e.must(s.Start(e.ctx))
	t.Cleanup(s.Stop)

	e.waitFor("both pipelines", 60*time.Second, func() bool { return e.pipelineDone(lA.ID) && e.pipelineDone(lB.ID) })
	if j := e.job(model.ScopeLecture, lA.ID, model.StageStream); j.Status != model.StatusRunning {
		t.Errorf("A's stream task %s: the transcode still runs after export", j.Status)
	}
	if _, err := os.Stat(filepath.Join(dirA, model.StreamFile)); !os.IsNotExist(err) {
		t.Error("A's stream.mp4 exists before the transcode ended")
	}
	if p := e.job(model.ScopeLecture, lA.ID, model.StagePrep); p.Progress != 1 {
		t.Errorf("A's prep progress %v", p.Progress)
	}
	jb := e.job(model.ScopeLecture, lB.ID, model.StageStream)
	if jb.Status != model.StatusFailed || jb.Attempt != 0 {
		t.Errorf("B's stream task %s attempt %d", jb.Status, jb.Attempt)
	}
	if info := streamError(t, e.streamState(lB.ID)); info.Code != MediaUnsupported {
		t.Errorf("B's stream state %+v", info)
	}

	close(release)
	e.waitFor("A's stream task", 10*time.Second, func() bool {
		return e.job(model.ScopeLecture, lA.ID, model.StageStream).Status == model.StatusDone
	})
	if st := e.streamState(lA.ID); st.Status != StreamReady || !st.Transcoded {
		t.Errorf("A's stream state %+v", st)
	}
	// B's failure is not retried by itself (only the user's retry)
	time.Sleep(100 * time.Millisecond)
	if j := e.job(model.ScopeLecture, lB.ID, model.StageStream); j.ID != jb.ID || j.Status != model.StatusFailed || j.Attempt != 0 {
		t.Errorf("B's stream task %s %s attempt %d: retried automatically", j.ID, j.Status, j.Attempt)
	}
	if !e.pipelineDone(lA.ID) || !e.pipelineDone(lB.ID) {
		t.Error("the playback file changed the pipeline")
	}
}

// TestStreamTaskIgnoresTranscriptionDevices: the stream task needs a CPU
// slot only — with every transcription device paused (asr waits), the
// transcode still runs and finishes; asr goes on when the devices resume.
func TestStreamTaskIgnoresTranscriptionDevices(t *testing.T) {
	e := newEnv(t)
	l, _ := e.mediaLecture("paused")
	s := e.streamSched(&fakeMedia{probe: probeHEVC}, nil)
	e.must(s.SetGPUPaused(true, false))
	e.must(s.Start(e.ctx))
	t.Cleanup(s.Stop)
	e.waitFor("the stream task", 20*time.Second, func() bool {
		return e.hasCurrentJob(l.ID, model.StageStream) && e.job(model.ScopeLecture, l.ID, model.StageStream).Status == model.StatusDone
	})
	if j := e.job(model.ScopeLecture, l.ID, model.StageASR); j.Status != model.StatusQueued && j.Status != model.StatusPending {
		t.Errorf("asr %s while every device is paused", j.Status)
	}
	if st := e.streamState(l.ID); st.Status != StreamReady || !st.Transcoded {
		t.Errorf("stream state %+v", st)
	}
	e.must(s.SetGPUPaused(false, false))
	e.waitFor("the pipeline", 60*time.Second, func() bool { return e.pipelineDone(l.ID) })
}

// TestStreamTaskRestart (§4.7): a stream task cut off by a restart is
// requeued like a pipeline stage (lead ruling 2026-10-27 14:30: the playback
// file is a regular product since stream-parallel; the other auxiliary
// tasks stay failed, state.TestRecoverDoesNotRetryAux), its .tmp removed, no
// kv result, the original kept (keep_original_media = false); prep stays
// done; the scheduler then runs it to the end. Once its retries are used up
// it stays failed, and the player asking for the media (EnsureStream)
// starts it again; that run makes the file and removes the original. A real
// failure is restarted neither by a restart nor by the player.
func TestStreamTaskRestart(t *testing.T) {
	e := newEnv(t)
	e.cfg.Storage.KeepOriginalMedia = boolp(false)
	l, dir := e.mediaLecture("restart")
	if r := e.run(&prepStage{d: e.deps(nil), media: &fakeMedia{probe: probeHEVC}}, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	j := e.job(model.ScopeLecture, l.ID, model.StageStream)
	e.drive(j.ID, model.StatusPending, model.StatusQueued, model.StatusRunning)
	e.must(os.WriteFile(filepath.Join(dir, model.StreamFile+".tmp"), []byte("partial"), 0o644))

	e.must(state.Recover(e.ctx, e.st, e.cfg.DataDir))
	// §4.7 (lead ruling 2026-10-27 14:30): the interrupted stream task is
	// queued again like a pipeline stage, the same job
	n := e.job(model.ScopeLecture, l.ID, model.StageStream)
	if n.ID != j.ID || n.Status != model.StatusQueued || n.Attempt != 1 || !state.IsInterrupted(n.Error) {
		t.Fatalf("after the restart: stream task %s %s attempt %d error %v, want the same job queued (attempt 1)", n.ID, n.Status, n.Attempt, n.Error)
	}
	if p := e.job(model.ScopeLecture, l.ID, model.StagePrep); p.Status != model.StatusDone {
		t.Errorf("prep %s", p.Status)
	}
	if _, err := os.Stat(filepath.Join(dir, model.StreamFile+".tmp")); !os.IsNotExist(err) {
		t.Error("stream.mp4.tmp survived the restart")
	}
	if _, ok, _ := StreamStateOf(e.ctx, e.st, l.ID); ok {
		t.Error("an interrupted run recorded a result")
	}
	if _, err := os.Stat(filepath.Join(dir, "m.mp4")); err != nil {
		t.Errorf("the original: %v", err)
	}
	s := e.streamSched(&fakeMedia{probe: probeHEVC}, nil)
	// the player finds it generating and queues nothing more
	if generating, err := s.EnsureStream(e.ctx, l.ID); err != nil || !generating {
		t.Fatalf("EnsureStream after the restart: %v %v", generating, err)
	}
	if n := e.job(model.ScopeLecture, l.ID, model.StageStream); n.ID != j.ID {
		t.Errorf("EnsureStream replaced the requeued stream task: %s", n.ID)
	}

	// Interrupted until its retries are used up, it stays failed (no result
	// in kv): then opening the player queues a new run (EnsureStream).
	for i := 0; i < model.MaxAutoRetries; i++ {
		e.drive(j.ID, model.StatusQueued, model.StatusRunning)
		e.must(state.Recover(e.ctx, e.st, e.cfg.DataDir))
	}
	j = e.job(model.ScopeLecture, l.ID, model.StageStream)
	if j.Status != model.StatusFailed || j.Attempt != model.MaxAutoRetries || !state.IsInterrupted(j.Error) {
		t.Fatalf("after %d more restarts: stream task %s attempt %d error %v", model.MaxAutoRetries, j.Status, j.Attempt, j.Error)
	}
	generating, err := s.EnsureStream(e.ctx, l.ID)
	if err != nil || !generating {
		t.Fatalf("EnsureStream after the interruptions: %v %v", generating, err)
	}
	if n := e.job(model.ScopeLecture, l.ID, model.StageStream); n.ID == j.ID || n.Status != model.StatusPending {
		t.Errorf("stream task %s %s, want a new pending run", n.ID, n.Status)
	}
	// a real failure (kv failed, job failed with its code) is not restarted
	l2, _ := e.mediaLecture("failed")
	if r := e.run(&prepStage{d: e.deps(nil), media: &fakeMedia{probe: probeHEVC}}, l2.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if r := e.run(&streamStage{d: e.deps(nil), media: &fakeMedia{probe: probeHEVC, fail: func([]string) (string, error) {
		return "Conversion failed!", errors.New("exit status 1")
	}}}, l2.ID); r.job.Status != model.StatusFailed {
		t.Fatalf("stream task %s", r.job.Status)
	}
	failed := e.job(model.ScopeLecture, l2.ID, model.StageStream)
	e.must(state.Recover(e.ctx, e.st, e.cfg.DataDir))
	if n := e.job(model.ScopeLecture, l2.ID, model.StageStream); n.ID != failed.ID || n.Status != model.StatusFailed || n.Attempt != failed.Attempt {
		t.Errorf("a restart touched a failed stream task: %s %s attempt %d", n.ID, n.Status, n.Attempt)
	}
	if generating, err := s.EnsureStream(e.ctx, l2.ID); err != nil || generating {
		t.Errorf("EnsureStream after a failure: %v %v", generating, err)
	}
	if n := e.job(model.ScopeLecture, l2.ID, model.StageStream); n.ID != failed.ID || n.Status != model.StatusFailed {
		t.Errorf("a failed stream task was replaced: %s %s", n.ID, n.Status)
	}

	// interrupted once: requeued by the restart, run by the scheduler
	l3, _ := e.mediaLecture("requeued")
	if r := e.run(&prepStage{d: e.deps(nil), media: &fakeMedia{probe: probeHEVC}}, l3.ID); r.err != nil {
		t.Fatal(r.err)
	}
	j3 := e.job(model.ScopeLecture, l3.ID, model.StageStream)
	e.drive(j3.ID, model.StatusPending, model.StatusQueued, model.StatusRunning)
	e.must(state.Recover(e.ctx, e.st, e.cfg.DataDir))
	if n := e.job(model.ScopeLecture, l3.ID, model.StageStream); n.ID != j3.ID || n.Status != model.StatusQueued {
		t.Fatalf("interrupted stream task %s %s, want queued", n.ID, n.Status)
	}

	e.must(s.Start(e.ctx))
	t.Cleanup(s.Stop)
	for _, id := range []string{l.ID, l3.ID} {
		e.waitFor("the stream task", 20*time.Second, func() bool {
			return e.job(model.ScopeLecture, id, model.StageStream).Status == model.StatusDone
		})
		if st := e.streamState(id); st.Status != StreamReady || !st.Transcoded {
			t.Errorf("stream state %+v", st)
		}
	}
	if j := e.job(model.ScopeLecture, l3.ID, model.StageStream); j.ID != j3.ID || j.Attempt != 1 {
		t.Errorf("the requeued run: %s attempt %d, want the same job (attempt 1)", j.ID, j.Attempt)
	}
	if _, err := os.Stat(filepath.Join(dir, "m.mp4")); !os.IsNotExist(err) {
		t.Error("keep_original_media=false: the original stayed after stream.mp4 was made")
	}
}
