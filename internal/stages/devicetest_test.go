// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/wav"
)

// Phase 3b task D (SPEC-v2 I9): the device self-test, with the test binary
// as both workers (mode "selftest", scripted per candidate by
// LF_FAKE_SELFTEST).

// writePCM writes a PCM WAV of sec seconds (8-bit mono at 1000 Hz: small,
// but a real WAV that wav.Cut accepts).
func writePCM(t *testing.T, path string, sec int) {
	t.Helper()
	data := make([]byte, sec*1000)
	for i := range data {
		data[i] = byte(128 + 50*math.Sin(float64(i)/7))
	}
	b := []byte("RIFF")
	b = binary.LittleEndian.AppendUint32(b, uint32(36+len(data)))
	b = append(b, "WAVEfmt "...)
	b = binary.LittleEndian.AppendUint32(b, 16)
	b = binary.LittleEndian.AppendUint16(b, 1)
	b = binary.LittleEndian.AppendUint16(b, 1)
	b = binary.LittleEndian.AppendUint32(b, 1000)
	b = binary.LittleEndian.AppendUint32(b, 1000)
	b = binary.LittleEndian.AppendUint16(b, 1)
	b = binary.LittleEndian.AppendUint16(b, 8)
	b = append(b, "data"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(data)))
	b = append(b, data...)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// repairedLecture creates a lecture of 10 minutes whose repair is done: a
// real PCM audio.wav, 100 sentences (5 s each, 0.5 s pauses, a human edit
// in sentence 55), an asr job in German that used the prompt "cache, TLB".
func (e *env) repairedLecture() (model.Lecture, []model.Sentence) {
	e.t.Helper()
	l := e.lecture("Clip source", "m.mp4")
	writePCM(e.t, filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), model.AudioFile), 600)
	ss := make([]model.Sentence, 100)
	for i := range ss {
		start := int64(i) * 5500
		txt := fmt.Sprintf("Sentence %d is about caches and memory.", i)
		ss[i] = model.Sentence{LectureID: l.ID, Idx: i, StartMS: start, EndMS: start + 5000, ASRText: txt,
			RepairedText: strings.Replace(txt, "caches", "the caches", 1), ZhStatus: model.ZhNone}
	}
	e.insertSentences(l.ID, ss)
	human := "A human edit of sentence 55."
	e.must(e.st.SetHumanText(e.ctx, l.ID, 55, &human))
	e.must(e.st.RecomputeText(e.ctx, l.ID, []int{55}))
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceCUDA0, "large-v3", "auto", "de"))
	e.markDone(model.ScopeLecture, l.ID, model.StageASR)
	j := e.job(model.ScopeLecture, l.ID, model.StageASR)
	e.must(e.st.SetJobStats(e.ctx, j.ID, model.JobStats{ASRPrompt: "cache, TLB", HashVersion: ASRHashVersion}))
	e.markDone(model.ScopeLecture, l.ID, model.StageRepair)
	return l, e.sentences(l.ID)
}

// selfTestScript sets LF_FAKE_SELFTEST.
func (e *env) selfTestScript(m map[string]fakeCandidate) {
	e.t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Setenv(fakeSelfTestEnv, string(b))
}

// testEnv records the reports and BeforeCandidate calls of a self-test.
type testEnv struct {
	mu      sync.Mutex
	reports []sched.DeviceTestProgress
	befores int
	before  func(ctx context.Context, n int) error // nil = return ctx.Err()
}

func (te *testEnv) env() sched.DeviceTestEnv {
	return sched.DeviceTestEnv{
		Report: func(p sched.DeviceTestProgress) {
			te.mu.Lock()
			te.reports = append(te.reports, p)
			te.mu.Unlock()
		},
		BeforeCandidate: func(ctx context.Context) error {
			te.mu.Lock()
			te.befores++
			n := te.befores
			te.mu.Unlock()
			if te.before != nil {
				return te.before(ctx, n)
			}
			return ctx.Err()
		},
	}
}

func (te *testEnv) phases() []string {
	te.mu.Lock()
	defer te.mu.Unlock()
	var out []string
	for _, r := range te.reports {
		s := fmt.Sprintf("%s#%d/%d", r.Phase, r.Step, r.Steps)
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}

var gpuDevice = model.ASRDevice{ID: model.DeviceOVGPU, Backend: model.BackendOpenVINO, Label: "Intel(R) Arc(TM) Graphics",
	Enabled: true, Model: "large-v3-turbo", Precision: "fp16", MaxParallel: 1, Priority: 200}

// dropWords removes the first n words of s.
func dropWords(s string, n int) string { return strings.Join(strings.Fields(s)[n:], " ") }

// TestDeviceTest: the clip is cut from the middle of the repaired lecture
// into the self-test directory, every candidate runs on the device's
// worker with the lecture's language and prompt, RTF and WER are measured
// against the repaired text, the fallback candidate is skipped once an
// earlier one reached RTF <= 0.5, and Pick chooses the most accurate.
func TestDeviceTest(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("selftest", "")
	log := e.jobLog()
	l, ss := e.repairedLecture()
	start, end, ref, ok := devices.PlanClip(ss, devices.SelfTestClip)
	if !ok || !strings.Contains(ref, "the caches") {
		t.Fatalf("plan %d–%d %q %v", start, end, ref, ok)
	}
	e.selfTestScript(map[string]fakeCandidate{
		"large-v3-turbo/fp16": {Compile: true, Text: ref, Words: true, TranscribeS: 30, PeakMB: 2683.4},
		"large-v3-turbo/int8": {Text: dropWords(ref, 3), TranscribeS: 24, PeakMB: 1500},
		"small/int8":          {Text: ref, TranscribeS: 6},
	})
	te := &testEnv{}
	d := e.deps(nil)
	b, err := d.DeviceTest(e.ctx, gpuDevice, te.env())
	if err != nil {
		t.Fatal(err)
	}
	clipMS := end - start
	wantAudio := model.BenchmarkAudio{Source: model.BenchmarkSourceLec, LectureID: l.ID, StartMS: start, EndMS: end,
		ReferenceWords: len(strings.Fields(ref))}
	if b.Version != 1 || b.Status != model.BenchmarkDone || b.Audio != wantAudio || b.Error != nil || b.StartedAt == "" || b.FinishedAt == "" {
		t.Errorf("benchmark %+v", b)
	}
	if len(b.Results) != 2 {
		t.Fatalf("%d results (the fallback must be skipped): %+v", len(b.Results), b.Results)
	}
	r0, r1 := b.Results[0], b.Results[1]
	wer1, _ := devices.WER(ref, dropWords(ref, 3))
	if !r0.OK || r0.Model != "large-v3-turbo" || r0.Precision != "fp16" || r0.WER == nil || *r0.WER != 0 ||
		math.Abs(r0.RTF-30/(float64(clipMS)/1000)) > 1e-9 || r0.LoadS != 2.5 || r0.TranscribeS != 30 ||
		!r0.FirstCompile || r0.Prepared || r0.PeakMemMB != 2683 || !r0.Words || r0.Error != nil {
		t.Errorf("result 0 %+v", r0)
	}
	if !r1.OK || r1.Precision != "int8" || r1.WER == nil || *r1.WER != wer1 || r1.FirstCompile || r1.Words || r1.PeakMemMB != 1500 {
		t.Errorf("result 1 %+v (wer %v, want %v)", r1, r1.WER, wer1)
	}
	if b.Pick == nil || *b.Pick != (model.BenchmarkPick{Model: "large-v3-turbo", Precision: "fp16", Rule: model.PickRuleBest}) {
		t.Errorf("pick %+v", b.Pick)
	}
	jobs := fakeJobs(t, log)
	data := e.absDataDir()
	clip := filepath.Join(data, "selftest", "ov_GPU", model.AudioFile)
	if len(jobs) != 2 {
		t.Fatalf("%d worker runs", len(jobs))
	}
	for i, j := range jobs {
		c := devices.Candidates(model.DeviceOVGPU)[i]
		if j.Script != "asr_openvino.py" || j.Job["audio"] != clip || j.Job["device"] != "GPU" || j.Job["model"] != c.Model ||
			j.Job["precision"] != c.Precision || j.Job["language"] != "de" || j.Job["initial_prompt"] != "cache, TLB" ||
			j.Job["model_dir"] != devices.ModelDir(data, c.Model, c.Precision) || j.Job["cache_dir"] != devices.CacheDir(data, "GPU", c.Model, c.Precision) {
			t.Errorf("run %d: %s %v", i, j.Script, j.Job)
		}
		if _, ok := j.Job["checkpoint"]; ok { // power management: pipeline runs only
			t.Errorf("run %d: the self-test passed a checkpoint %v", i, j.Job["checkpoint"])
		}
	}
	if _, err := os.Stat(clip); !os.IsNotExist(err) {
		t.Error("the clip was not removed")
	}
	if te.befores != 2 {
		t.Errorf("BeforeCandidate called %d times", te.befores)
	}
	want := []string{"clip#0/3", "transcribing#1/3", sched.DeviceTestPhaseCompiling + "#1/3", "transcribing#1/3", "transcribing#2/3"}
	if got := te.phases(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("phases %v, want %v", got, want)
	}
	// the self-test never writes the database
	if rows, _ := e.st.ListASRDevices(e.ctx); len(rows) != 0 {
		t.Errorf("device rows %v", rows)
	}
	if _, ok, _ := e.st.GetKV(e.ctx, model.MemObservedKey(model.DeviceOVGPU, "large-v3-turbo", "fp16")); ok {
		t.Error("mem_observed written by the self-test")
	}
	if j := e.job(model.ScopeLecture, l.ID, model.StageRepair); j.Status != model.StatusDone {
		t.Errorf("repair job %s", j.Status)
	}
}

// TestDeviceTestClip: the clip in the self-test directory is the planned
// part of the lecture's audio (kept here by a worker that copies it).
func TestDeviceTestClip(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("selftest", "")
	l, ss := e.repairedLecture()
	start, end, _, _ := devices.PlanClip(ss, devices.SelfTestClip)
	e.selfTestScript(map[string]fakeCandidate{"large-v3-turbo/fp16": {Text: "x", TranscribeS: 1}, "large-v3-turbo/int8": {Text: "x", TranscribeS: 1}})
	clip := filepath.Join(e.absDataDir(), "selftest", "ov_GPU", model.AudioFile)
	var got []byte
	te := &testEnv{before: func(ctx context.Context, n int) error {
		if n == 2 { // the clip exists while the test runs
			got, _ = os.ReadFile(clip)
		}
		return ctx.Err()
	}}
	if b, err := e.deps(nil).DeviceTest(e.ctx, gpuDevice, te.env()); err != nil || b.Status != model.BenchmarkDone {
		t.Fatalf("%+v %v", b, err)
	}
	ref := filepath.Join(t.TempDir(), "ref.wav")
	if _, err := wav.Cut(filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), model.AudioFile), ref, start, end); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(ref)
	if len(got) == 0 || string(got) != string(want) {
		t.Errorf("clip of %d bytes, want %d", len(got), len(want))
	}
	if ms, ok := wav.DurationMS(ref); !ok || ms != end-start {
		t.Errorf("clip duration %d", ms)
	}
}

// TestDeviceTestFallbackAndPick: the fallback runs when no earlier
// candidate reached RTF <= 0.5; failed candidates are recorded with their
// coded error; when nothing reaches the limit the lowest RTF wins.
func TestDeviceTestFallbackAndPick(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("selftest", "")
	_, ss := e.repairedLecture()
	s, en, _, _ := devices.PlanClip(ss, devices.SelfTestClip)
	clipS := float64(en-s) / 1000
	d := e.deps(nil)

	// fp16 too slow, int8 fails: the fallback runs and is the pick
	e.selfTestScript(map[string]fakeCandidate{
		"large-v3-turbo/fp16": {Text: "a", TranscribeS: clipS * 0.8},
		"large-v3-turbo/int8": {Fail: "device"},
		"small/int8":          {Text: "b", TranscribeS: clipS * 0.2},
	})
	b, err := d.DeviceTest(e.ctx, gpuDevice, (&testEnv{}).env())
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Results) != 3 || b.Status != model.BenchmarkDone {
		t.Fatalf("results %+v status %s", b.Results, b.Status)
	}
	if r := b.Results[1]; r.OK || r.WER != nil || infoCode(r.Error) != CodeASROVDeviceUnavailable || !paramsMatch(r.Error, "device", model.DeviceOVGPU) ||
		!strings.Contains(r.Error.Detail, "device lost") {
		t.Errorf("failed candidate %+v %+v", r, r.Error)
	}
	if b.Pick == nil || *b.Pick != (model.BenchmarkPick{Model: "small", Precision: "int8", Rule: model.PickRuleBest}) {
		t.Errorf("pick %+v", b.Pick)
	}

	// nothing under the limit: lowest RTF
	e.selfTestScript(map[string]fakeCandidate{
		"large-v3-turbo/fp16": {Text: "a", TranscribeS: clipS * 0.9},
		"large-v3-turbo/int8": {Text: "a", TranscribeS: clipS * 0.7},
		"small/int8":          {Text: "b", TranscribeS: clipS * 0.6},
	})
	b, _ = d.DeviceTest(e.ctx, gpuDevice, (&testEnv{}).env())
	if len(b.Results) != 3 || b.Pick == nil || *b.Pick != (model.BenchmarkPick{Model: "small", Precision: "int8", Rule: model.PickRuleLowestRTF}) {
		t.Errorf("results %d pick %+v", len(b.Results), b.Pick)
	}

	// every candidate fails: status failed with the first candidate's error
	e.selfTestScript(map[string]fakeCandidate{
		"large-v3-turbo/fp16": {Fail: "exit1"},
		"large-v3-turbo/int8": {Fail: "device"},
		"small/int8":          {Fail: "exit1"},
	})
	b, _ = d.DeviceTest(e.ctx, gpuDevice, (&testEnv{}).env())
	if b.Status != model.BenchmarkFailed || len(b.Results) != 3 || b.Pick != nil || infoCode(b.Error) != workerASROV+workerFailed ||
		!paramsMatch(b.Error, "exit_code", 1) {
		t.Errorf("all failed: %+v", b)
	}

	// no timing block: Go measures the times itself
	e.selfTestScript(map[string]fakeCandidate{
		"large-v3-turbo/fp16": {Text: "a", NoTiming: true, LoadMS: 200},
		"large-v3-turbo/int8": {Text: "a", NoTiming: true},
	})
	b, _ = d.DeviceTest(e.ctx, gpuDevice, (&testEnv{}).env())
	if r := b.Results[0]; !r.OK || r.LoadS < 0.2 || r.TranscribeS < 0 || r.RTF != r.TranscribeS/clipS || r.PeakMemMB != 0 {
		t.Errorf("measured %+v", r)
	}
}

// TestDeviceTestPickTie (power management, lead addition after I12): the
// self-test picks with devices.PickFor — the I12 NPU case (same WER, int8
// 0.8 % faster) keeps the recommended fp16; a clearly faster int8 wins.
func TestDeviceTestPickTie(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("selftest", "")
	_, ss := e.repairedLecture()
	_, _, ref, _ := devices.PlanClip(ss, devices.SelfTestClip)
	npu := model.ASRDevice{ID: model.DeviceOVNPU, Backend: model.BackendOpenVINO, Label: "Intel(R) AI Boost", Enabled: true,
		Model: "large-v3-turbo", Precision: "fp16", MaxParallel: 1}
	hyp := dropWords(ref, 2)
	for _, c := range []struct {
		int8S float64
		want  string
	}{{9.77, "fp16"}, {8.0, "int8"}} {
		e.selfTestScript(map[string]fakeCandidate{
			"large-v3-turbo/fp16": {Text: hyp, TranscribeS: 9.85},
			"large-v3-turbo/int8": {Text: hyp, TranscribeS: c.int8S},
		})
		b, err := e.deps(nil).DeviceTest(e.ctx, npu, (&testEnv{}).env())
		if err != nil || b.Status != model.BenchmarkDone || len(b.Results) != 2 {
			t.Fatalf("%+v %v", b, err)
		}
		if p, _ := devices.Pick(b.Results); p.Precision != "int8" {
			t.Fatalf("Pick %+v: the test does not exercise the tie rule", p)
		}
		if b.Pick == nil || *b.Pick != (model.BenchmarkPick{Model: "large-v3-turbo", Precision: c.want, Rule: model.PickRuleBest}) {
			t.Errorf("int8 %.2f s: pick %+v, want %s", c.int8S, b.Pick, c.want)
		}
	}
}

// TestDeviceTestFasterWhisper: cuda:0 and fw:cpu run worker/asr.py with
// config python, the resolved compute type and timing requested.
func TestDeviceTestFasterWhisper(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("selftest", "")
	log := e.jobLog()
	e.cfg.PythonOpenVINO = filepath.Join(e.root, "missing", "python.exe")
	_, ss := e.repairedLecture()
	_, _, ref, _ := devices.PlanClip(ss, devices.SelfTestClip)
	e.selfTestScript(map[string]fakeCandidate{
		"large-v3/float16":       {Text: ref, TranscribeS: 10},
		"large-v3-turbo/float16": {Text: ref, TranscribeS: 5},
		"small/int8":             {Text: ref, TranscribeS: 80},
		"base/int8":              {Text: ref, TranscribeS: 30},
	})
	cuda := model.ASRDevice{ID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, Label: "NVIDIA GeForce RTX", Enabled: true,
		Model: "large-v3", Precision: "auto", MaxParallel: 1}
	b, err := e.deps(nil).DeviceTest(e.ctx, cuda, (&testEnv{}).env())
	if err != nil || b.Status != model.BenchmarkDone || len(b.Results) != 2 {
		t.Fatalf("%+v %v", b, err)
	}
	// equal WER (0): the lower RTF
	if b.Pick == nil || b.Pick.Model != "large-v3-turbo" || b.Pick.Precision != "auto" {
		t.Errorf("pick %+v", b.Pick)
	}
	for _, j := range fakeJobs(t, log) {
		if j.Script != "asr.py" || j.Job["device"] != "cuda" || j.Job["compute_type"] != "float16" || j.Job["timing"] != true ||
			!strings.HasSuffix(fmt.Sprint(j.Job["audio"]), filepath.Join("selftest", "cuda_0", model.AudioFile)) || j.Job["checkpoint"] != nil {
			t.Errorf("%s %v", j.Script, j.Job)
		}
	}
	// fw:cpu: small too slow (RTF > 0.5), so the fallback base runs too
	cpu := model.ASRDevice{ID: model.DeviceFWCPU, Backend: model.BackendFasterWhisper, Label: "CPU", Enabled: true, Model: "small", Precision: "auto", MaxParallel: 1}
	b, _ = e.deps(nil).DeviceTest(e.ctx, cpu, (&testEnv{}).env())
	if len(b.Results) != 2 || b.Results[1].Model != "base" || b.Results[0].RTF <= 0.5 || b.Pick == nil ||
		*b.Pick != (model.BenchmarkPick{Model: "base", Precision: "auto", Rule: model.PickRuleBest}) {
		t.Errorf("fw:cpu %+v pick %+v", b.Results, b.Pick)
	}
	jobs := fakeJobs(t, log)
	if j := jobs[len(jobs)-1]; j.Job["device"] != "cpu" || j.Job["compute_type"] != "int8" || j.Job["cpu_threads"] != nil {
		t.Errorf("fw:cpu job %v", j.Job)
	}
}

// TestDeviceTestCompileTimeout (lead ruling 3): a candidate that does not
// finish loading within the limit is killed and fails with
// selftest.compile_timeout; preparing the model does not count as loading.
func TestDeviceTestCompileTimeout(t *testing.T) {
	old := selfTestCompileLimit
	selfTestCompileLimit = 1500 * time.Millisecond
	t.Cleanup(func() { selfTestCompileLimit = old })
	e := newEnv(t)
	e.useFakeWorker("selftest", "")
	e.repairedLecture()
	e.selfTestScript(map[string]fakeCandidate{
		"large-v3-turbo/fp16": {Compile: true, LoadMS: 20000, Text: "never"},
		"large-v3-turbo/int8": {Prepare: true, PrepareMS: 2500, Text: "prepared", TranscribeS: 1},
	})
	started := time.Now()
	b, err := e.deps(nil).DeviceTest(e.ctx, gpuDevice, (&testEnv{}).env())
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > 12*time.Second {
		t.Errorf("took %v: the compiling worker was not killed", took)
	}
	if len(b.Results) != 2 {
		t.Fatalf("results %+v", b.Results)
	}
	if r := b.Results[0]; r.OK || infoCode(r.Error) != CodeSelfTestCompileTimeout || !paramsMatch(r.Error, "minutes", 1) || !r.FirstCompile {
		t.Errorf("timed out candidate %+v %+v", r, r.Error)
	}
	if r := b.Results[1]; !r.OK || !r.Prepared || r.Error != nil {
		t.Errorf("prepared candidate %+v %+v", r, r.Error)
	}
	if b.Status != model.BenchmarkDone || b.Pick == nil || b.Pick.Precision != "int8" {
		t.Errorf("status %s pick %+v", b.Status, b.Pick)
	}
	if mins := int(math.Ceil(devices.SelfTestCompileLimit.Minutes())); mins != 15 {
		t.Errorf("limit %d minutes", mins)
	}
}

// TestDeviceTestCancel: cancelling kills the running worker within seconds;
// the benchmark comes back cancelled with the results of the candidates
// that finished (the interrupted one is dropped). A cancel while waiting in
// BeforeCandidate ends the test the same way.
func TestDeviceTestCancel(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("selftest", "")
	e.repairedLecture()
	e.selfTestScript(map[string]fakeCandidate{
		"large-v3-turbo/fp16": {Text: "done", TranscribeS: 10},
		"large-v3-turbo/int8": {Text: "never", Hang: true},
	})
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	te := &testEnv{}
	env := te.env()
	at := make(chan time.Time, 1)
	report := env.Report
	var once sync.Once
	env.Report = func(p sched.DeviceTestProgress) {
		report(p)
		if p.Step == 2 && p.Phase == sched.DeviceTestPhaseTranscribing && p.Progress > 0 {
			once.Do(func() {
				at <- time.Now()
				cancel()
			})
		}
	}
	b, err := e.deps(nil).DeviceTest(ctx, gpuDevice, env)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(<-at); took > 3*time.Second {
		t.Errorf("cancel took %v", took)
	}
	if b.Status != model.BenchmarkCancelled || len(b.Results) != 1 || !b.Results[0].OK || b.Pick == nil || b.Pick.Precision != "fp16" || b.FinishedAt == "" {
		t.Errorf("cancelled benchmark %+v", b)
	}
	if _, err := os.Stat(filepath.Join(e.absDataDir(), "selftest", "ov_GPU", "result.json.tmp")); !os.IsNotExist(err) {
		t.Error("result.json.tmp left behind")
	}

	// cancelled while yielding to a pipeline job
	ctx2, cancel2 := context.WithCancel(e.ctx)
	te2 := &testEnv{before: func(ctx context.Context, n int) error {
		if n == 2 {
			cancel2()
		}
		return ctx.Err()
	}}
	e.selfTestScript(map[string]fakeCandidate{"large-v3-turbo/fp16": {Text: "done", TranscribeS: 10}})
	b, err = e.deps(nil).DeviceTest(ctx2, gpuDevice, te2.env())
	if err != nil || b.Status != model.BenchmarkCancelled || len(b.Results) != 1 {
		t.Errorf("cancel in BeforeCandidate: %+v %v", b, err)
	}
}

// TestDeviceTestNoAudio (lead ruling 7): without a repaired lecture long
// enough the test fails at once with selftest.no_audio and runs no worker.
func TestDeviceTestNoAudio(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("selftest", "")
	log := e.jobLog()
	d := e.deps(nil)
	b, err := d.DeviceTest(e.ctx, gpuDevice, (&testEnv{}).env())
	if err != nil || b.Status != model.BenchmarkFailed || infoCode(b.Error) != CodeSelfTestNoAudio || b.Results == nil ||
		len(b.Results) != 0 || b.Pick != nil || b.FinishedAt == "" {
		t.Errorf("no lecture: %+v %v", b, err)
	}
	// a repaired lecture without any pause in its sentences cannot give a clip
	l := e.lecture("One sentence", "m.mp4")
	writePCM(t, filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), model.AudioFile), 600)
	e.insertSentences(l.ID, []model.Sentence{{LectureID: l.ID, Idx: 0, StartMS: 0, EndMS: 600000, ASRText: "x", RepairedText: "x", ZhStatus: model.ZhNone}})
	e.markDone(model.ScopeLecture, l.ID, model.StageRepair)
	b, _ = d.DeviceTest(e.ctx, gpuDevice, (&testEnv{}).env())
	if b.Status != model.BenchmarkFailed || infoCode(b.Error) != CodeSelfTestNoAudio {
		t.Errorf("no usable clip: %+v", b)
	}
	if n := len(fakeJobs(t, log)); n != 0 {
		t.Errorf("%d worker runs", n)
	}
	// a device this build cannot test
	b, _ = d.DeviceTest(e.ctx, model.ASRDevice{ID: "ov:VPU", Backend: model.BackendOpenVINO}, (&testEnv{}).env())
	if b.Status != model.BenchmarkFailed || infoCode(b.Error) != CodeASRDeviceMissing {
		t.Errorf("unknown device: %+v", b)
	}
}

func TestSelfTestDir(t *testing.T) {
	for id, want := range map[string]string{"cuda:0": "cuda_0", "ov:GPU": "ov_GPU", "fw:cpu": "fw_cpu", `a/b\c..d`: "a_b_c..d"} {
		if got := selfTestDir("D", id); got != filepath.Join("D", "selftest", want) {
			t.Errorf("%s: %s", id, got)
		}
	}
}
