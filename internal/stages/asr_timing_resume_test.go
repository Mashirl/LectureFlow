// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// Power management real-machine verification, item 1
// (docs/benchmarks/power-checkpoint.md §6.8; asr.go asrTimingResume): after
// a resume from the checkpoint, job.stats.asr_timing covers the whole
// lecture — the transcription time of the attempts summed, the whole audio,
// the last attempt's load — while an uninterrupted run's timing stays the
// worker's, byte for byte. The fake worker (mode "ckpt") reports the timing
// of its own run (LF_FAKE_CKPT_TIMING); a killed one reports none, so the
// interrupted attempts' time is what the stage measured (asrNow, here a
// clock that advances one second per reading).

// stepClock makes asrNow advance by step on every reading.
func stepClock(t *testing.T, step time.Duration) {
	t.Helper()
	var mu sync.Mutex
	now := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	old := asrNow
	asrNow = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(step)
		return now
	}
	t.Cleanup(func() { asrNow = old })
}

// setCkptTiming sets the timing block of the fake worker's next result.
func (e *env) setCkptTiming(loadS, transcribeS float64, firstCompile bool) {
	e.t.Helper()
	e.t.Setenv(fakeCkptTimingEnv, string(marshalT(e.t, map[string]any{
		"load_s": loadS, "transcribe_s": transcribeS, "first_compile": firstCompile, "peak_working_set_mb": 0,
	})))
}

// priorMS is the asr job's stored stats.asr_prior_transcribe_ms.
func (e *env) priorMS(l model.Lecture) int64 {
	e.t.Helper()
	return e.job(model.ScopeLecture, l.ID, model.StageASR).Stats.ASRPriorTranscribeMS
}

// doneTiming is the stored asr_timing of the lecture's finished asr job as
// JSON; the stats must not keep asr_prior_transcribe_ms.
func (e *env) doneTiming(l model.Lecture) string {
	e.t.Helper()
	st := e.job(model.ScopeLecture, l.ID, model.StageASR).Stats
	b := marshalT(e.t, st)
	if bytes.Contains(b, []byte(`"asr_prior_transcribe_ms"`)) {
		e.t.Errorf("the finished job's stats keep asr_prior_transcribe_ms: %s", b)
	}
	if st.ASRTiming == nil {
		e.t.Fatalf("no asr_timing: %s", b)
	}
	return string(marshalT(e.t, st.ASRTiming))
}

// workerAudioS is timing.audio_s of the lecture's last result.json (what the
// worker itself reported).
func (e *env) workerAudioS(l model.Lecture) float64 {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.lectureDir(l), "result.json"))
	e.must(err)
	var res struct {
		Timing struct {
			AudioS float64 `json:"audio_s"`
		} `json:"timing"`
	}
	e.must(json.Unmarshal(b, &res))
	return res.Timing.AudioS
}

// cancelAndRetry moves the running asr job to cancelled and retries it (the
// user's retry: the same job, SPEC §4.6).
func (e *env) cancelAndRetry(l model.Lecture) {
	e.t.Helper()
	j := e.job(model.ScopeLecture, l.ID, model.StageASR)
	e.must(state.TransitionJob(e.ctx, e.st, j.ID, model.StatusRunning, model.StatusCancelled, db.JobFinish{}))
	e.must(state.Retry(e.ctx, e.st, model.ScopeLecture, l.ID, model.StageASR))
}

// TestASRTimingUninterrupted: without an interruption asr_timing is the
// worker's block converted to ms, exactly as before, and the stats carry no
// asr_prior_transcribe_ms (faster-whisper and OpenVINO with a first compile).
func TestASRTimingUninterrupted(t *testing.T) {
	old := ovCheckpointChunkS
	ovCheckpointChunkS = 30
	t.Cleanup(func() { ovCheckpointChunkS = old })
	stepClock(t, time.Second)
	e := newEnv(t)
	for _, c := range []struct {
		fake, device, mdl, precision string
		load, transcribe             float64
		compile                      bool
		want                         string
	}{
		{fakeASR, model.DeviceCUDA0, "large-v2", "auto", 3.17, 195.71, false, `{"load_ms":3170,"transcribe_ms":195710,"audio_ms":60000}`},
		{fakeASROV, model.DeviceOVNPU, "large-v3-turbo", "fp16", 241.5, 105.84, true, `{"load_ms":241500,"transcribe_ms":105840,"audio_ms":60000,"first_compile":true}`},
	} {
		e.useFakeWorker("ckpt", c.fake)
		e.setCkptTiming(c.load, c.transcribe, c.compile)
		l := e.lecture("L "+c.device, "m.mp4")
		e.writeWAV(l, "wav")
		e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(c.device, c.mdl, c.precision, "en"))
		r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
		if r.err != nil {
			t.Fatalf("%s: %v", c.device, r.err)
		}
		if got := e.doneTiming(l); got != c.want {
			t.Errorf("%s: asr_timing %s, want %s", c.device, got, c.want)
		}
		if a := e.workerAudioS(l); a != 60 {
			t.Errorf("%s: the worker reported audio_s %v", c.device, a)
		}
	}
}

// TestASRTimingPowerMigrate: Intel GPU → NPU (scenario 1 of the
// verification). The GPU attempt is killed by the policy; the NPU attempt
// continues the checkpoint and reports only the rest of the audio. The
// stored timing is the NPU's load and first compile, the sum of both
// attempts' transcription time and the whole lecture.
func TestASRTimingPowerMigrate(t *testing.T) {
	old := ovCheckpointChunkS
	ovCheckpointChunkS = 30
	t.Cleanup(func() { ovCheckpointChunkS = old })
	stepClock(t, time.Second)
	e := newEnv(t)
	e.useFakeWorker("ckpt", fakeASROV)
	l := e.lecture("Migrate", "m.mp4")
	e.writeWAV(l, "wav")
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceOVGPU, "large-v3-turbo", "fp16", "en"))
	if err := e.killAtFlag(l, "2", sched.ErrPowerMigrate); !sched.IsPowerInterrupt(err) {
		t.Fatalf("migrate: %v", err)
	}
	// measured from the worker's first PROGRESS line to its exit
	first := e.priorMS(l)
	if first < 1000 {
		t.Fatalf("asr_prior_transcribe_ms %d after the interrupted attempt", first)
	}
	if ck, ok := e.readCheckpoint(l); !ok || ck.ResumeMS != 8200 {
		t.Fatalf("checkpoint %+v %v", ck, ok)
	}
	j := e.job(model.ScopeLecture, l.ID, model.StageASR)
	e.powerRequeue(j.ID, model.DeviceOVGPU, sched.CodePowerSwitched, model.DeviceOVNPU)
	if got := e.priorMS(l); got != first {
		t.Fatalf("asr_prior_transcribe_ms %d after the requeue, want %d", got, first)
	}
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceOVNPU, "large-v3-turbo", "fp16", "en"))
	e.setCkptTiming(2.43, 151.22, true)
	r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if a := e.workerAudioS(l); a != 51.8 {
		t.Fatalf("the NPU worker reported audio_s %v, want 51.8 (the rest after 8.2 s)", a)
	}
	want := model.ASRTiming{LoadMS: 2430, TranscribeMS: 151220 + first, AudioMS: 60000, FirstCompile: true}
	if r.stats.ASRTiming == nil || *r.stats.ASRTiming != want {
		t.Errorf("asr_timing %+v, want %+v", r.stats.ASRTiming, want)
	}
	if got, wantJSON := e.doneTiming(l), string(marshalT(t, want)); got != wantJSON {
		t.Errorf("stored asr_timing %s, want %s", got, wantJSON)
	}
	if _, s := e.storedSnapshot(l); len(s.Parts) != 2 {
		t.Errorf("parts %+v", s.Parts)
	}
}

// TestASRTimingSameDevice: a user cancel and retry on cuda:0, then a power
// suspension and the resume on the same device — three attempts of one job.
// Each interrupted attempt adds its measured time; the last run adds the
// sum to its own transcription time and reports the whole lecture.
func TestASRTimingSameDevice(t *testing.T) {
	stepClock(t, time.Second)
	e := newEnv(t)
	e.useFakeWorker("ckpt", fakeASR)
	l := e.lecture("L", "m.mp4")
	e.writeWAV(l, "wav")
	snap := deviceSnapshot(model.DeviceCUDA0, "large-v2", "auto", "en")
	e.setSnapshot(l.ID, model.StageASR, snap)
	if err := e.killAtFlag(l, "3", nil); err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
	p1 := e.priorMS(l)
	if p1 < 1000 {
		t.Fatalf("asr_prior_transcribe_ms %d after the cancel", p1)
	}
	e.cancelAndRetry(l)
	e.setSnapshot(l.ID, model.StageASR, snap)
	if err := e.killAtFlag(l, "2", sched.ErrPowerSuspend); !sched.IsPowerInterrupt(err) {
		t.Fatalf("suspend: %v", err)
	}
	p2 := e.priorMS(l)
	if p2 < p1+1000 {
		t.Fatalf("asr_prior_transcribe_ms %d after the second attempt, first %d", p2, p1)
	}
	ck, ok := e.readCheckpoint(l)
	if !ok || ck.ResumeMS <= 12600 {
		t.Fatalf("checkpoint %+v %v", ck, ok)
	}
	j := e.job(model.ScopeLecture, l.ID, model.StageASR)
	e.powerRequeue(j.ID, model.DeviceCUDA0, sched.CodePowerSuspended, "")
	e.setCkptTiming(2.78, 120.34, false)
	r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if a, wantA := e.workerAudioS(l), float64(60000-ck.ResumeMS)/1000; a != wantA {
		t.Fatalf("the worker reported audio_s %v, want %v", a, wantA)
	}
	want := model.ASRTiming{LoadMS: 2780, TranscribeMS: 120340 + p2, AudioMS: 60000}
	if r.stats.ASRTiming == nil || *r.stats.ASRTiming != want {
		t.Errorf("asr_timing %+v, want %+v", r.stats.ASRTiming, want)
	}
	if got, wantJSON := e.doneTiming(l), string(marshalT(t, want)); got != wantJSON {
		t.Errorf("stored asr_timing %s, want %s", got, wantJSON)
	}
	if len(e.sentences(l.ID)) != 14 {
		t.Errorf("%d sentences", len(e.sentences(l.ID)))
	}
}

// TestASRTimingStartedOver: the earlier attempts' time is not added when the
// run did not continue their checkpoint — the stage discarded it (another
// model on the same device after a user cancel), or the worker started over
// although Go expected a resume (here the OpenVINO fake without chunks;
// the real faster-whisper worker does so after a version change) — and an
// unknown earlier time (0, e.g. an attempt interrupted before this field
// existed) leaves the resumed run's own pair, so the RTF stays right.
func TestASRTimingStartedOver(t *testing.T) {
	old := ovCheckpointChunkS
	t.Cleanup(func() { ovCheckpointChunkS = old })
	stepClock(t, time.Second)
	e := newEnv(t)

	// discarded by the stage
	e.useFakeWorker("ckpt", fakeASR)
	l := e.lecture("Other model", "m.mp4")
	e.writeWAV(l, "wav")
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceCUDA0, "large-v2", "auto", "en"))
	if err := e.killAtFlag(l, "3", nil); err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
	if e.priorMS(l) < 1000 {
		t.Fatalf("asr_prior_transcribe_ms %d", e.priorMS(l))
	}
	e.cancelAndRetry(l)
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceCUDA0, "large-v3-turbo", "auto", "en"))
	e.setCkptTiming(3.05, 200.49, false)
	if r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if got, want := e.doneTiming(l), `{"load_ms":3050,"transcribe_ms":200490,"audio_ms":60000}`; got != want {
		t.Errorf("discarded checkpoint: asr_timing %s, want %s", got, want)
	}

	// started over by the worker: the result covers the span itself
	ovCheckpointChunkS = 30
	e.useFakeWorker("ckpt", fakeASROV)
	l2 := e.lecture("Worker started over", "m.mp4")
	e.writeWAV(l2, "wav")
	e.setSnapshot(l2.ID, model.StageASR, deviceSnapshot(model.DeviceOVGPU, "large-v3-turbo", "fp16", "en"))
	if err := e.killAtFlag(l2, "2", sched.ErrPowerMigrate); !sched.IsPowerInterrupt(err) {
		t.Fatalf("migrate: %v", err)
	}
	if e.priorMS(l2) < 1000 {
		t.Fatalf("asr_prior_transcribe_ms %d", e.priorMS(l2))
	}
	e.powerRequeue(e.job(model.ScopeLecture, l2.ID, model.StageASR).ID, model.DeviceOVGPU, sched.CodePowerSwitched, model.DeviceOVNPU)
	e.setSnapshot(l2.ID, model.StageASR, deviceSnapshot(model.DeviceOVNPU, "large-v3-turbo", "fp16", "en"))
	ovCheckpointChunkS = 0 // the fake then deletes the checkpoint and starts over
	e.setCkptTiming(2.42, 235.29, false)
	if r := e.run(e.stage(e.deps(nil), model.StageASR), l2.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if got, want := e.doneTiming(l2), `{"load_ms":2420,"transcribe_ms":235290,"audio_ms":60000}`; got != want {
		t.Errorf("worker started over: asr_timing %s, want %s", got, want)
	}

	// resumed, earlier time unknown
	e.useFakeWorker("ckpt", fakeASR)
	l3 := e.lecture("Unknown earlier time", "m.mp4")
	e.writeWAV(l3, "wav")
	snap := deviceSnapshot(model.DeviceCUDA0, "large-v2", "auto", "en")
	e.setSnapshot(l3.ID, model.StageASR, snap)
	if err := e.killAtFlag(l3, "3", nil); err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
	j := e.job(model.ScopeLecture, l3.ID, model.StageASR)
	st := j.Stats
	st.ASRPriorTranscribeMS = 0
	e.must(e.st.SetJobStats(e.ctx, j.ID, st))
	e.cancelAndRetry(l3)
	e.setSnapshot(l3.ID, model.StageASR, snap)
	e.setCkptTiming(1.2, 30.5, false)
	if r := e.run(e.stage(e.deps(nil), model.StageASR), l3.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if got, want := e.doneTiming(l3), `{"load_ms":1200,"transcribe_ms":30500,"audio_ms":47400}`; got != want {
		t.Errorf("unknown earlier time: asr_timing %s, want %s", got, want)
	}
}

// TestAttemptClock: the measurement starts at the first PROGRESS line,
// goes into the stats at most every priorSaveEvery while the worker runs
// and always when it exits; progress values pass through. An attempt that
// starts over replaces the stored sum with 0 at once, and a worker that
// exits before its first PROGRESS line adds nothing.
func TestAttemptClock(t *testing.T) {
	var now time.Time
	old := asrNow
	asrNow = func() time.Time { return now }
	t.Cleanup(func() { asrNow = old })
	at := func(s float64) {
		now = time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC).Add(time.Duration(s * float64(time.Second)))
	}
	rec := &sched.StatsRecorder{Stats: model.JobStats{ASRPriorTranscribeMS: 9000}}
	ctx := sched.WithStatsSink(context.Background(), rec)
	got := func() int64 { return rec.Snapshot().ASRPriorTranscribeMS }

	c := newAttemptClock(ctx, 9000, 9000)
	var seen []float64
	p := c.wrap(func(v float64) { seen = append(seen, v) })
	for _, s := range []struct {
		at   float64
		p    float64
		want int64
	}{
		{10, 0.1, 9000},    // loaded: the clock starts
		{14.9, 0.2, 9000},  // not yet priorSaveEvery since the start
		{15, 0.3, 14000},   // 5 s
		{19.5, 0.4, 14000}, // 4.5 s since the last write
		{20.25, 0.5, 19250},
	} {
		at(s.at)
		p(s.p)
		if got() != s.want {
			t.Errorf("at %v s: %d, want %d", s.at, got(), s.want)
		}
	}
	at(21.5)
	c.exited()
	if got() != 20500 {
		t.Errorf("after the exit: %d, want 20500", got())
	}
	if !slices.Equal(seen, []float64{0.1, 0.2, 0.3, 0.4, 0.5}) {
		t.Errorf("progress %v", seen)
	}

	c = newAttemptClock(ctx, 0, 20500)
	if got() != 0 {
		t.Errorf("starting over: %d", got())
	}
	at(30)
	c.exited()
	if got() != 0 {
		t.Errorf("exit before any PROGRESS line: %d", got())
	}
}

// TestResumedAudioMS: the parts' span when the worker transcribed only what
// follows the resume point (within the 10 ms rounding of audio_s; one part
// when faster-whisper merged the same device's parts), nothing otherwise.
func TestResumedAudioMS(t *testing.T) {
	gpu := model.ASRPart{FromMS: 0, ToMS: 1205560, DeviceID: model.DeviceOVGPU}
	npu := model.ASRPart{FromMS: 1205560, ToMS: 3287340, DeviceID: model.DeviceOVNPU}
	whole := model.ASRPart{FromMS: 0, ToMS: 3287340, DeviceID: model.DeviceCUDA0}
	for _, c := range []struct {
		name     string
		parts    []model.ASRPart
		audio, r int64
		want     int64
		ok       bool
	}{
		{"migrated", []model.ASRPart{gpu, npu}, 2081780, 1205560, 3287340, true},
		{"same device, merged", []model.ASRPart{whole}, 2010880, 1276460, 3287340, true},
		{"rounding above", []model.ASRPart{whole}, 2010885, 1276460, 3287340, true},
		{"started over", []model.ASRPart{whole}, 3287340, 1276460, 0, false},
		{"started over, rounding", []model.ASRPart{whole}, 3287335, 1276460, 0, false},
		{"no parts", nil, 2010880, 1276460, 0, false},
		{"no resume point", []model.ASRPart{whole}, 2010880, 0, 0, false},
	} {
		if got, ok := resumedAudioMS(c.parts, c.audio, c.r); got != c.want || ok != c.ok {
			t.Errorf("%s: %d %v, want %d %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// TestContinuedFrom: the resume point of a checkpoint the worker will
// continue, 0 for one it starts over from.
func TestContinuedFrom(t *testing.T) {
	cj := devices.CheckpointJob{JobID: "j1", InputHash: "h1"}
	ok := devices.Checkpoint{Version: devices.CheckpointVersion, JobID: "j1", InputHash: "h1", Backend: model.BackendFasterWhisper,
		ResumeMS: 12600, Parts: []model.ASRPart{{FromMS: 0, ToMS: 12600, DeviceID: model.DeviceCUDA0}}}
	if got := continuedFrom(ok, cj, model.BackendFasterWhisper); got != 12600 {
		t.Errorf("continued: %d", got)
	}
	for name, mod := range map[string]func(*devices.Checkpoint){
		"version":    func(c *devices.Checkpoint) { c.Version = 1 },
		"job":        func(c *devices.Checkpoint) { c.JobID = "j0" },
		"input hash": func(c *devices.Checkpoint) { c.InputHash = "h0" },
		"backend":    func(c *devices.Checkpoint) { c.Backend = model.BackendOpenVINO },
		"no parts":   func(c *devices.Checkpoint) { c.Parts = nil },
		"no resume":  func(c *devices.Checkpoint) { c.ResumeMS = 0 },
	} {
		c := ok
		mod(&c)
		if got := continuedFrom(c, cj, model.BackendFasterWhisper); got != 0 {
			t.Errorf("%s: %d, want 0", name, got)
		}
	}
}
