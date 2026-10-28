// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// Redecode (worker-builder, 2026-10-27): worker/asr.py reports the windows it
// decoded again in result.json timing.postprocess.redecode; the stage keeps
// them in job.stats.asr_timing.postprocess (seconds → ms), where the API's
// job view shows them with the rest of asr_timing.

func TestWorkerTimingPostprocess(t *testing.T) {
	for _, c := range []struct {
		name, timing, want string
	}{
		{"none", `{"load_s":1,"transcribe_s":2,"audio_s":3,"first_compile":false,"peak_working_set_mb":0}`,
			`{"load_ms":1000,"transcribe_ms":2000,"audio_ms":3000}`},
		{"redecode", `{"load_s":1,"transcribe_s":2,"audio_s":3,"first_compile":false,"peak_working_set_mb":0,` +
			`"postprocess":{"redecode":{"windows":110,"unpunctuated":38,"uppercase":4,"redecoded":41,"accepted":39,"redecode_s":61.2345}}}`,
			`{"load_ms":1000,"transcribe_ms":2000,"audio_ms":3000,"postprocess":{"redecode":` +
				`{"windows":110,"unpunctuated":38,"uppercase":4,"redecoded":41,"accepted":39,"redecode_ms":61235}}}`},
		// all zero: the step ran and found nothing (shown, not dropped)
		{"zero", `{"load_s":1,"transcribe_s":2,"audio_s":3,"postprocess":{"redecode":{"windows":0,"unpunctuated":0,` +
			`"uppercase":0,"redecoded":0,"accepted":0,"redecode_s":0}}}`,
			`{"load_ms":1000,"transcribe_ms":2000,"audio_ms":3000,"postprocess":{"redecode":` +
				`{"windows":0,"unpunctuated":0,"uppercase":0,"redecoded":0,"accepted":0,"redecode_ms":0}}}`},
		// the switch was off: counted, none decoded again
		{"skipped", `{"load_s":1,"transcribe_s":2,"audio_s":3,"postprocess":{"redecode":{"skipped":true,"windows":100,` +
			`"unpunctuated":54,"uppercase":6,"redecoded":0,"accepted":0,"redecode_s":0}}}`,
			`{"load_ms":1000,"transcribe_ms":2000,"audio_ms":3000,"postprocess":{"redecode":` +
				`{"skipped":true,"windows":100,"unpunctuated":54,"uppercase":6,"redecoded":0,"accepted":0,"redecode_ms":0}}}`},
		// a postprocess object without a step this build knows: nothing to show
		{"unknown step", `{"load_s":1,"transcribe_s":2,"audio_s":3,"postprocess":{}}`,
			`{"load_ms":1000,"transcribe_ms":2000,"audio_ms":3000}`},
	} {
		var wt workerTiming
		if err := json.Unmarshal([]byte(c.timing), &wt); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := string(marshalT(t, wt.stats())); got != c.want {
			t.Errorf("%s: asr_timing %s, want %s", c.name, got, c.want)
		}
	}
}

// TestASRTimingRedecodeStored: a worker run's redecode counters end up in
// the finished job's stats.
func TestASRTimingRedecodeStored(t *testing.T) {
	stepClock(t, time.Second)
	e := newEnv(t)
	e.useFakeWorker("ckpt", fakeASR)
	e.t.Setenv(fakeCkptTimingEnv, string(marshalT(t, map[string]any{
		"load_s": 3.17, "transcribe_s": 195.71, "first_compile": false, "peak_working_set_mb": 0,
		"postprocess": map[string]any{"redecode": map[string]any{
			"windows": 110, "unpunctuated": 38, "uppercase": 4, "redecoded": 41, "accepted": 39, "redecode_s": 61.23,
		}},
	})))
	l := e.lecture("L redecode", "m.mp4")
	e.writeWAV(l, "wav")
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceCUDA0, "large-v3", "auto", "en"))
	if r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	want := `{"load_ms":3170,"transcribe_ms":195710,"audio_ms":60000,"postprocess":{"redecode":` +
		`{"windows":110,"unpunctuated":38,"uppercase":4,"redecoded":41,"accepted":39,"redecode_ms":61230}}}`
	if got := e.doneTiming(l); got != want {
		t.Errorf("asr_timing %s, want %s", got, want)
	}
}

// TestASRJobRedecodeSwitch: job.json of asr.py carries the switch
// "修复退化的转写窗口" (kv asr.redecode): on when the key was never written,
// for "on" and for any value but "off" (in any case); off for "off". The
// Colab job carries it as colab.Job.NoRedecode. The switch is no part of the
// input hash.
func TestASRJobRedecodeSwitch(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("ok", fakeASR)
	ptr := func(s string) *string { return &s }
	for _, c := range []struct {
		kv   *string
		want bool
	}{
		{nil, true}, {ptr(model.ASRRedecodeOn), true}, {ptr(""), true}, {ptr("yes"), true},
		{ptr(model.ASRRedecodeOff), false}, {ptr(" OFF\n"), false},
	} {
		if c.kv != nil {
			e.must(e.st.SetKV(e.ctx, model.KVASRRedecode, *c.kv))
		}
		l := e.lecture("L redecode", "m.mp4")
		e.writeWAV(l, "wav")
		e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceCUDA0, "large-v3", "auto", "en"))
		r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
		if r.err != nil {
			t.Fatal(r.err)
		}
		b, err := os.ReadFile(filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), "job.json"))
		e.must(err)
		var job map[string]any
		e.must(json.Unmarshal(b, &job))
		if job["redecode"] != c.want {
			t.Errorf("kv %v: job.json redecode %v, want %v", c.kv, job["redecode"], c.want)
		}
		// the same input hash either way (v3: wav + language)
		if h := e.job(model.ScopeLecture, l.ID, model.StageASR).InputHash; h == nil || *h == "" {
			t.Errorf("kv %v: no input hash", c.kv)
		}
	}

	// Colab: the same switch, as colab.Job.NoRedecode
	e.cfg.ASR.FakeResult = fakeASR
	for _, c := range []struct {
		kv   string
		want bool
	}{{model.ASRRedecodeOff, true}, {model.ASRRedecodeOn, false}} {
		e.must(e.st.SetKV(e.ctx, model.KVASRRedecode, c.kv))
		l := e.colabLecture("en")
		fr := &fakeColabASR{result: fakeASR}
		if r := e.runCtx(sched.WithRemote(e.ctx, fr), e.stage(e.deps(nil), model.StageASR), l.ID); r.err != nil {
			t.Fatal(r.err)
		}
		if len(fr.jobs) != 1 || fr.jobs[0].NoRedecode != c.want {
			t.Errorf("kv %q: remote jobs %+v", c.kv, fr.jobs)
		}
	}
}

// TestASRInputHashIgnoresTheRedecodeSwitch: switching it never makes a
// transcribed lecture stale.
func TestASRInputHashIgnoresTheRedecodeSwitch(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("ok", fakeASR)
	l := e.lecture("L hash", "m.mp4")
	e.writeWAV(l, "wav")
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceCUDA0, "large-v3", "auto", "en"))
	if r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	st := e.stage(e.deps(nil), model.StageASR).(*asrStage)
	before, err := st.InputHash(e.ctx, l.ID)
	e.must(err)
	e.must(e.st.SetKV(e.ctx, model.KVASRRedecode, model.ASRRedecodeOff))
	after, err := st.InputHash(e.ctx, l.ID)
	e.must(err)
	if before == "" || before != after {
		t.Errorf("input hash %q, after switching redecode off %q", before, after)
	}
}
