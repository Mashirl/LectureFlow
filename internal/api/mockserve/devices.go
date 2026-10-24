// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// Transcription devices of the mock (SPEC-v2 I6-I10, phase 3b; the types of
// internal/api/types_intel.go): the device rows of the settings page, their
// self-test (simulated in time: the state follows the wall clock, like the
// running stages), the device cards and the power state of the resource
// panel, the OpenVINO part of the diagnostics and the workbench prompt.
//
// Every store has its own devices. Three scenarios have their own store:
//
//	battery  the computer runs on battery (battery awareness holds the
//	         NVIDIA card; in auto mode only the NPU takes new work)
//	noov     no OpenVINO environment (.venv-ov missing): cuda:0 and the
//	         turned-off fw:cpu, the diagnostics' OpenVINO checks warn
//	cpuonly  no NVIDIA card and no OpenVINO environment: only fw:cpu, turned
//	         on (the fallback of gate round 1)
//
// In the default data cuda:0 was migrated from the legacy settings (no
// prompt), the Intel GPU was tested two days ago, the NPU is new and never
// tested (the workbench prompt; its model is converted but not compiled for
// it yet), the OpenVINO CPU is turned off and fw:cpu (CPU faster-whisper,
// always there since gate round 1) is turned off because faster devices
// exist. The "empty" scenario has no repaired lecture, so the self-test
// uses the bundled sample (SPEC-v3 V4.3); with -no-sample (a build without
// the sample) it is blocked (selftest.no_audio).

import (
	"flag"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/resources"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
	"lectureflow/internal/stages"
)

// selftestScale multiplies the simulated self-test durations (tests make it
// small; 1 = a test of about half a minute).
var selftestScale = 1.0

const (
	labelCUDA  = "NVIDIA GeForce RTX 5070 Ti Laptop GPU"
	labelIGPU  = "Intel(R) Graphics (iGPU)"
	labelNPU   = "Intel(R) AI Boost"
	labelOVCPU = "Intel(R) Core(TM) Ultra 9 386H"
	labelFWCPU = "CPU (faster-whisper)" // devices.LabelFWCPU
	mockOVPy   = ".venv-ov\\Scripts\\python.exe"
)

type mockDevice struct {
	row       model.ASRDevice
	present   bool
	paused    bool
	dismissed bool // the workbench prompt was closed
	migrated  bool // created from the legacy settings (no prompt)
	bench     *model.DeviceBenchmark
	test      *mockTest
}

// mockTest is a running or finished self-test.
type mockTest struct {
	// waitFor: the power-aware reason of the waiting phase (a laptop with
	// another active device, devices.SelfTest Start false); "" otherwise
	waitFor   string
	started   time.Time
	plan      []testSeg
	steps     int
	results   []model.BenchmarkResult // by step, filled when the test ends
	cancelled *time.Time
	ended     *time.Time
	audio     model.BenchmarkAudio
	// result: the test's own benchmark when it was not stored (cancelled
	// before any candidate finished: the device keeps its last benchmark)
	result *model.DeviceBenchmark
}

// testSeg is one phase of the simulated test.
type testSeg struct {
	phase string // waiting | clip | preparing_model | compiling_model | transcribing
	step  int    // 1-based candidate, 0 before the first
	cand  devices.Candidate
	dur   float64 // seconds (before selftestScale)
	end   float64 // cumulative end, seconds (scaled)
}

// devState is the device world of a store.
type devState struct {
	devs     []*mockDevice
	prepared map[string]bool // "model-precision" converted for OpenVINO
	compiled map[string]bool // "id|model|precision" compiled for an Intel GPU / NPU
	power    resources.Power // Known, OnBattery, BatteryPct, HasBattery, Charging (BatteryAware: settings; Source: powerView)
	noOV     bool            // the OpenVINO environment is missing
	cpuOnly  bool            // no NVIDIA card either: only fw:cpu (scenario cpuonly)
	noCUDA   bool            // no NVIDIA card (scenario power_ac_intel, power.go)
	npuUtil  bool            // the NPU's usage is readable (the power scenarios on battery)
	assigned map[string]string
	// power management (power.go): the upper area's readings, the lectures
	// waiting power_wait_ac with the device they continue on, the notices
	sys       sysReadings
	suspended map[string]string
	notices   []resources.PowerNotice
}

func initDevices(s *store, now time.Time) {
	at := func(d time.Duration) *string { return rfcPtr(now.Add(-d)) }
	// a laptop on AC power, charging (power management: a machine with a
	// battery, power.go)
	ds := &devState{
		prepared:  map[string]bool{"large-v3-turbo-fp16": true, "large-v3-turbo-int8": true},
		compiled:  map[string]bool{"ov:GPU|large-v3-turbo|fp16": true, "ov:GPU|large-v3-turbo|int8": true},
		power:     powerOf(false, true, true, intPtr(87)),
		assigned:  map[string]string{},
		sys:       sysReadings{cpuBase: 24, tempC: intPtr(66)},
		suspended: map[string]string{},
	}
	ds.devs = []*mockDevice{
		{row: model.ASRDevice{ID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, Label: labelCUDA, Enabled: true,
			Model: "large-v2", Precision: model.PrecisionAuto, MaxParallel: 1, Priority: 300, DetectedAt: at(2 * time.Minute)}, present: true, migrated: true},
		{row: model.ASRDevice{ID: model.DeviceOVGPU, Backend: model.BackendOpenVINO, Label: labelIGPU, Enabled: true,
			Model: "large-v3-turbo", Precision: model.PrecisionFP16, MaxParallel: 1, Priority: 200, DetectedAt: at(2 * time.Minute)}, present: true,
			bench: igpuBenchmark(s, now)},
		{row: model.ASRDevice{ID: model.DeviceOVNPU, Backend: model.BackendOpenVINO, Label: labelNPU, Enabled: true,
			Model: "large-v3-turbo", Precision: model.PrecisionFP16, MaxParallel: 1, Priority: 100, DetectedAt: at(2 * time.Minute)}, present: true},
		{row: model.ASRDevice{ID: model.DeviceOVCPU, Backend: model.BackendOpenVINO, Label: labelOVCPU, Enabled: false,
			Model: "large-v3-turbo", Precision: model.PrecisionFP16, MaxParallel: 1, Priority: 0, DetectedAt: at(2 * time.Minute)}, present: true},
		// gate round 1: fw:cpu always has a row, off while something faster exists
		{row: model.ASRDevice{ID: model.DeviceFWCPU, Backend: model.BackendFasterWhisper, Label: labelFWCPU, Enabled: false,
			Model: "small", Precision: model.PrecisionAuto, MaxParallel: 1, Priority: 10, DetectedAt: at(2 * time.Minute)}, present: true, dismissed: true},
	}
	s.dev = ds
}

// setupCPUOnly is scenario cpuonly: neither NVIDIA nor OpenVINO, fw:cpu on;
// a desktop (no battery: the 3b rules keep the CPU fallback, Q1).
func setupCPUOnly(s *store) {
	s.dev.noOV, s.dev.cpuOnly = true, true
	s.dev.power = powerOf(false, false, false, nil)
	s.dev.sys = sysReadings{cpuBase: 38, tempC: nil}
	for _, d := range s.dev.devs {
		if d.row.ID == model.DeviceFWCPU {
			d.row.Enabled = true
		}
	}
}

// igpuBenchmark is the Intel GPU's test of two days ago (applied).
func igpuBenchmark(s *store, now time.Time) *model.DeviceBenchmark {
	started := now.Add(-50*time.Hour - 7*time.Minute)
	audio, _ := s.clipSource()
	b := &model.DeviceBenchmark{Version: model.BenchmarkVersion, Status: model.BenchmarkDone,
		StartedAt: started.UTC().Format(time.RFC3339), FinishedAt: started.Add(3 * time.Minute).UTC().Format(time.RFC3339), Audio: audio}
	for _, c := range devices.Candidates(model.DeviceOVGPU) {
		if c.Fallback {
			continue
		}
		b.Results = append(b.Results, simulatedResult(model.DeviceOVGPU, model.BackendOpenVINO, c, true, false, audio.ReferenceWords > 0))
	}
	b.Pick = pickOf(model.DeviceOVGPU, audio.ReferenceWords, b.Results)
	applied := started.Add(4 * time.Minute).UTC().Format(time.RFC3339)
	b.AppliedAt = &applied
	return b
}

// noSampleFlag simulates a build without the bundled sample lecture.
var noSampleFlag = flag.Bool("no-sample", false, "no bundled sample lecture: without a repaired lecture the device self-test is blocked (devices.go)")

// The bundled sample as the self-test records it (assets/sample: 65.5 s,
// 174 reference words).
var bundledAudio = model.BenchmarkAudio{Source: model.BenchmarkSourceBundled, StartMS: 0, EndMS: 65500, ReferenceWords: 174}

// clipSource is the middle two minutes of the first repaired lecture
// (devices.FindClipSource / PlanClip), else the bundled sample (SPEC-v3
// V4.3) unless -no-sample; ok is false without either.
func (s *store) clipSource() (model.BenchmarkAudio, bool) {
	for _, l := range s.lectures {
		if stageStatus(&l, model.StageRepair) != model.StatusDone {
			continue
		}
		mid := int64(30 * 60000)
		if l.DurationMS != nil && *l.DurationMS > 4*60000 {
			mid = *l.DurationMS / 2
		}
		start := (mid - 60000) / 1000 * 1000
		return model.BenchmarkAudio{Source: model.BenchmarkSourceLec, LectureID: l.ID, StartMS: start + 1400, EndMS: start + 121800, ReferenceWords: 312}, true
	}
	if !*noSampleFlag {
		return bundledAudio, true
	}
	return model.BenchmarkAudio{}, false
}

// the simulated figures of a combination (roughly the 3a measurements)
var simFigures = map[string]struct {
	rtf, wer, load, firstLoad float64
	mem                       int
}{
	"ov:GPU|large-v3-turbo|fp16": {0.19, 0.071, 6.1, 41.2, 3120},
	"ov:GPU|large-v3-turbo|int8": {0.14, 0.079, 4.8, 22.5, 2240},
	"ov:GPU|small|int8":          {0.06, 0.118, 1.2, 6.4, 880},
	"ov:NPU|large-v3-turbo|fp16": {0.31, 0.072, 9.4, 236.0, 2650},
	"ov:NPU|large-v3-turbo|int8": {0.27, 0.083, 8.1, 181.0, 1930},
	"ov:NPU|small|fp16":          {0.12, 0.117, 2.3, 48.0, 820},
	"ov:CPU|large-v3-turbo|int8": {0.62, 0.080, 7.0, 7.0, 2610},
	"ov:CPU|small|int8":          {0.21, 0.118, 1.6, 1.6, 900},
	"cuda:0|large-v3|auto":       {0.05, 0.058, 8.2, 8.2, 0},
	"cuda:0|large-v3-turbo|auto": {0.03, 0.066, 5.1, 5.1, 0},
	"fw:cpu|small|auto":          {0.44, 0.112, 3.0, 3.0, 0},
	"fw:cpu|base|auto":           {0.21, 0.161, 1.4, 1.4, 0},
}

func simulatedResult(id, backend string, c devices.Candidate, firstCompile, prepared, withRef bool) model.BenchmarkResult {
	f, ok := simFigures[id+"|"+c.Model+"|"+c.Precision]
	if !ok {
		f.rtf, f.wer, f.load, f.firstLoad = 0.4, 0.1, 5, 5
	}
	r := model.BenchmarkResult{Model: c.Model, Precision: c.Precision, OK: true, LoadS: f.load, RTF: f.rtf,
		TranscribeS: math.Round(f.rtf*120.4*10) / 10, FirstCompile: firstCompile, Prepared: prepared, PeakMemMB: f.mem,
		Words: backend == model.BackendFasterWhisper}
	if firstCompile {
		r.LoadS = f.firstLoad
	}
	if withRef {
		w := f.wer
		r.WER = &w
	}
	return r
}

// pickOf is the self-test's pick (I9 with the tie rule of power
// management): devices.PickFor, the rule the real DeviceTest uses — the
// lowest WER with RTF <= 0.5 (else the lowest RTF), and the recommended
// combination of the device when it ties with the winner. nil without any
// OK result.
func pickOf(id string, refWords int, rs []model.BenchmarkResult) *model.BenchmarkPick {
	p, ok := devices.PickFor(id, refWords, rs)
	if !ok {
		return nil
	}
	return &p
}

// ---------- lookups ----------

// visible are the devices the scenario shows, priority order.
func (s *store) visibleDevices() []*mockDevice {
	var out []*mockDevice
	for _, d := range s.dev.devs {
		if s.dev.noOV && d.row.Backend == model.BackendOpenVINO {
			continue
		}
		if (s.dev.cpuOnly || s.dev.noCUDA) && model.DeviceKind(d.row.ID) == model.DeviceKindCUDA {
			continue
		}
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].row.Priority > out[j].row.Priority })
	return out
}

func (s *store) device(id string) (*mockDevice, error) {
	for _, d := range s.visibleDevices() {
		if d.row.ID == id {
			return d, nil
		}
	}
	return nil, codedErr(http.StatusNotFound, devices.CodeNotFound, map[string]any{"device": id}, "找不到转写设备「"+id+"」")
}

// preferred is the device a launch in the current mode would name (the
// snapshot of settings.CurrentSnapshots; nil when there is none). Power
// management (power-brief §3.2, lead ruling 9.2): manual names the
// highest-priority enabled and present device of the manual list, else
// the auto choice (the first enabled and present device).
func (s *store) preferredDevice() *mockDevice {
	a := model.NormalizeASRDeviceMode(s.settings.ASR)
	if a.DeviceMode == model.ASRDeviceModeManual {
		for _, d := range s.visibleDevices() {
			if s.usable(d) && slices.Contains(a.ManualDevices, d.row.ID) {
				return d
			}
		}
	}
	for _, d := range s.visibleDevices() {
		if s.usable(d) {
			return d
		}
	}
	return nil
}

// asrLabelOf is internal/api's asr label "设备名 · 模型 · 精度 · 语言".
func asrLabelOf(label, mdl, precision, language string) string {
	return fmt.Sprintf("%s · %s · %s · %s", label, mdl, precision, language)
}

// runningByDevice are the running asr jobs by the device they run on
// (assignRuns, power.go: devices.Place on the store's state). s.mu held.
func (s *store) runningByDevice(now time.Time) map[string][]api.DeviceTaskView {
	out := map[string][]api.DeviceTaskView{}
	runs := s.assignRuns(now)
	for _, l := range s.lectures {
		id, ok := runs[l.ID]
		if !ok {
			continue
		}
		j := stageIndex(&l, model.StageASR)
		out[id] = append(out[id], api.DeviceTaskView{JobID: asrJobID(l.ID), LectureID: l.ID, LectureTitle: l.Title, CourseID: l.CourseID, Progress: l.Stages[j].Progress})
	}
	return out
}

// batteryHold (power management, power-brief §3.4: the new meaning of
// DeviceStatus.BatteryHold): a machine with a battery runs on battery and
// the device is enabled, present and not the battery device P_batt
// (devices.BatteryDevice; the manual list and battery_aware do not apply
// on battery).
func (s *store) batteryHold(d *mockDevice, in devices.PolicyInput) bool {
	if !in.Power.OnBatteryPower() || !s.usable(d) {
		return false
	}
	pb, ok := devices.BatteryDevice(in)
	return !ok || pb != d.row.ID
}

// ---------- self-test simulation ----------

func (s *store) planTest(d *mockDevice, now time.Time, hasRunning bool) *mockTest {
	audio, _ := s.clipSource()
	t := &mockTest{started: now, audio: audio}
	add := func(seg testSeg) {
		end := 0.0
		if n := len(t.plan); n > 0 {
			end = t.plan[n-1].end
		}
		seg.end = end + seg.dur*selftestScale
		t.plan = append(t.plan, seg)
	}
	if hasRunning {
		add(testSeg{phase: sched.DeviceTestWaiting, dur: 3})
	}
	add(testSeg{phase: sched.DeviceTestPhaseClip, dur: 1.5})
	prepared := map[string]bool{}
	compiled := map[string]bool{}
	ok := false
	step := 0
	for _, c := range devices.Candidates(d.row.ID) {
		if c.Fallback && ok {
			continue
		}
		step++
		key := c.Model + "-" + c.Precision
		ckey := d.row.ID + "|" + c.Model + "|" + c.Precision
		prep := d.row.Backend == model.BackendOpenVINO && !s.dev.prepared[key] && !prepared[key]
		kind := model.DeviceKind(d.row.ID)
		comp := (kind == model.DeviceKindIGPU || kind == model.DeviceKindNPU) && !s.dev.compiled[ckey] && !compiled[ckey]
		if prep {
			add(testSeg{phase: sched.DeviceTestPhasePreparing, step: step, cand: c, dur: 4})
			prepared[key] = true
		}
		if comp {
			dur := 3.0
			if kind == model.DeviceKindNPU {
				dur = 8
			}
			add(testSeg{phase: sched.DeviceTestPhaseCompiling, step: step, cand: c, dur: dur})
			compiled[ckey] = true
		}
		add(testSeg{phase: sched.DeviceTestPhaseTranscribing, step: step, cand: c, dur: 6})
		r := simulatedResult(d.row.ID, d.row.Backend, c, comp, prep, audio.ReferenceWords > 0)
		t.results = append(t.results, r)
		ok = ok || r.RTF <= model.PickMaxRTF
	}
	t.steps = step
	return t
}

// advanceDevices ends the tests whose time is up (and writes their
// benchmark); must be called with s.mu held. Reports whether anything ended.
func (s *store) advanceDevices(now time.Time) bool {
	changed := false
	for _, d := range s.dev.devs {
		t := d.test
		if t == nil || t.ended != nil {
			continue
		}
		total := t.plan[len(t.plan)-1].end
		el := now.Sub(t.started).Seconds()
		if t.cancelled == nil && el < total {
			continue
		}
		end := now
		status := model.BenchmarkDone
		results := t.results
		if t.cancelled != nil {
			end, status = *t.cancelled, model.BenchmarkCancelled
			el = t.cancelled.Sub(t.started).Seconds()
			// only the candidates whose transcription finished
			done := 0
			for _, seg := range t.plan {
				if seg.phase == sched.DeviceTestPhaseTranscribing && seg.end <= el {
					done = seg.step
				}
			}
			results = append([]model.BenchmarkResult{}, results[:done]...)
		} else {
			end = t.started.Add(time.Duration(total * float64(time.Second)))
		}
		for _, r := range results {
			s.dev.prepared[r.Model+"-"+r.Precision] = s.dev.prepared[r.Model+"-"+r.Precision] || d.row.Backend == model.BackendOpenVINO
			if k := model.DeviceKind(d.row.ID); k == model.DeviceKindIGPU || k == model.DeviceKindNPU {
				s.dev.compiled[d.row.ID+"|"+r.Model+"|"+r.Precision] = true
			}
		}
		if results == nil {
			results = []model.BenchmarkResult{}
		}
		t.ended = &end
		b := &model.DeviceBenchmark{Version: model.BenchmarkVersion, Status: status,
			StartedAt: t.started.UTC().Format(time.RFC3339), FinishedAt: end.UTC().Format(time.RFC3339),
			Audio: t.audio, Results: results, Pick: pickOf(d.row.ID, t.audio.ReferenceWords, results)}
		if status == model.BenchmarkCancelled && len(results) == 0 {
			t.result = b // gate round 1: nothing finished, last_benchmark stays
		} else {
			d.bench = b
		}
		changed = true
	}
	if changed {
		s.notify(api.ChangedEvent{Kind: "devices"})
	}
	s.syncPowerWaits(now) // a test of the active device started or ended
	return changed
}

func (s *store) selfTestView(d *mockDevice, now time.Time) api.SelfTestView {
	v := api.SelfTestView{DeviceID: d.row.ID, State: sched.DeviceTestIdle}
	t := d.test
	if t == nil {
		return v
	}
	started := t.started.UTC().Format(time.RFC3339)
	v.StartedAt, v.Steps = &started, t.steps
	if t.ended != nil {
		fin := t.ended.UTC().Format(time.RFC3339)
		v.FinishedAt = &fin
		v.State = sched.DeviceTestDone
		if t.cancelled != nil {
			v.State = sched.DeviceTestCancelled
		}
		v.Result = d.bench
		if t.result != nil {
			v.Result = t.result
		}
		return v
	}
	el := now.Sub(t.started).Seconds()
	begin := 0.0
	for _, seg := range t.plan {
		if el < seg.end {
			if seg.phase == sched.DeviceTestWaiting {
				// the power-aware reason (power_queue:{active device}) of a
				// laptop, decided when the test was asked for (the real
				// back end reads it on every request; the mock's wait is a
				// fixed few seconds)
				v.State, v.WaitingFor = sched.DeviceTestWaiting, t.waitFor
				return v
			}
			v.State, v.Phase, v.Step = sched.DeviceTestRunning, seg.phase, seg.step
			if seg.step > 0 {
				c := api.ModelPrecision{Model: seg.cand.Model, Precision: seg.cand.Precision}
				v.Candidate = &c
			}
			if seg.phase == sched.DeviceTestPhaseTranscribing && seg.end > begin {
				// the first quarter is the model load: progress 0 (task D's
				// reports have no loading phase)
				f := (el-begin)/(seg.end-begin)*4/3 - 1.0/3
				v.Progress = math.Round(math.Max(0, math.Min(1, f))*100) / 100
			}
			return v
		}
		begin = seg.end
	}
	v.State = sched.DeviceTestRunning
	return v
}

func (d *mockDevice) testActive() bool {
	return d.test != nil && d.test.ended == nil
}

// ---------- views ----------

func (s *store) deviceView(d *mockDevice, now time.Time, running map[string][]api.DeviceTaskView, in devices.PolicyInput) api.ASRDeviceView {
	r := d.row
	v := api.ASRDeviceView{ID: r.ID, Backend: r.Backend, Kind: model.DeviceKind(r.ID), Label: r.Label, Enabled: r.Enabled,
		Model: r.Model, Precision: r.Precision, MaxParallel: r.MaxParallel, Priority: r.Priority, DetectedAt: r.DetectedAt,
		Present: d.present && !(s.dev.noOV && r.Backend == model.BackendOpenVINO), Paused: d.paused,
		New:       d.present && r.Enabled && d.bench == nil && !d.dismissed && !d.migrated,
		Benchmark: d.bench, SelfTest: s.selfTestView(d, now), Running: running[r.ID], BatteryHold: s.batteryHold(d, in),
		Options: api.DeviceOptions{Models: devices.ModelChoices(r.Backend), Precisions: devices.PrecisionChoices(r.Backend)}}
	if v.Running == nil {
		v.Running = []api.DeviceTaskView{}
	}
	v.SlotsUsed = len(v.Running)
	if v.SelfTest.State == sched.DeviceTestRunning {
		v.SlotsUsed++
	}
	if c, ok := devices.Recommended(r.ID); ok {
		v.Options.Recommended = &api.ModelPrecision{Model: c.Model, Precision: c.Precision}
	}
	if r.Backend == model.BackendOpenVINO {
		ready := s.dev.prepared[r.Model+"-"+r.Precision]
		k := model.DeviceKind(r.ID)
		cached := k == model.DeviceKindCPU || s.dev.compiled[r.ID+"|"+r.Model+"|"+r.Precision]
		v.ModelReady, v.CompileCached = &ready, &cached
	}
	if _, ok := s.clipSource(); !ok {
		v.SelfTestBlocked = &model.ErrorInfo{Code: stages.CodeSelfTestNoAudio, Detail: "还没有可用于测试的录音：请先完成一节讲座的转写与修复"}
	}
	return v
}

// deviceViews is GET /api/asr/devices (priority order). s.mu held.
func (s *store) deviceViews(now time.Time) []api.ASRDeviceView {
	s.advanceDevices(now)
	running := s.runningByDevice(now)
	in := s.policyInput(now, s.assignRuns(now))
	out := []api.ASRDeviceView{}
	for _, d := range s.visibleDevices() {
		out = append(out, s.deviceView(d, now, running, in))
	}
	return out
}

// devicePrompts are the open workbench prompts (devices.Prompts).
func (s *store) devicePrompts() []api.DevicePrompt {
	var out []api.DevicePrompt
	for _, d := range s.visibleDevices() {
		if d.present && d.row.Enabled && d.bench == nil && !d.dismissed && !d.migrated {
			out = append(out, api.DevicePrompt{DeviceID: d.row.ID, Label: d.row.Label, Kind: model.DeviceKind(d.row.ID)})
		}
	}
	return out
}

func (s *store) powerView() resources.Power {
	p := s.dev.power
	p.BatteryAware = s.settings.ASR.BatteryAware // retired by power management, still sent
	p.Source = ""
	if p.Known {
		p.Source = resources.PowerSourceAC
		if p.OnBattery {
			p.Source = resources.PowerSourceBattery
		}
	}
	return p
}

// deviceCards are the resource panel's device rows (power management:
// every row, priority order; Active = the middle area's cards, the policy's
// active devices). s.mu held.
func (s *store) deviceCards(base resources.Snapshot, now time.Time, pol *resources.Policy) []resources.Device {
	views := s.deviceViews(now)
	p := s.dev.power
	t := float64(now.Unix())
	var out []resources.Device
	for _, v := range views {
		d, _ := s.device(v.ID)
		c := resources.Device{ID: v.ID, Label: v.Label, Backend: v.Backend, Kind: v.Kind, Model: v.Model, Precision: v.Precision,
			SlotsUsed: v.SlotsUsed, SlotsMax: v.MaxParallel, Present: v.Present, Paused: v.Paused, Tasks: []resources.DeviceTask{},
			UtilHistory: []int{}, BatteryMode: p.Known && p.OnBattery, BatteryHold: v.BatteryHold,
			Enabled: v.Enabled, Active: slices.Contains(pol.Active, v.ID), Untested: d != nil && d.bench == nil, HoldsSuspended: s.holdsSuspended(v.ID)}
		for _, r := range v.Running {
			c.Tasks = append(c.Tasks, resources.DeviceTask{JobID: r.JobID, LectureID: r.LectureID, LectureTitle: r.LectureTitle, CourseID: r.CourseID, Progress: r.Progress})
		}
		if v.SelfTest.State == sched.DeviceTestRunning {
			c.Tasks = append(c.Tasks, resources.DeviceTask{SelfTest: true, Progress: v.SelfTest.Progress})
		}
		kind := v.Kind
		if kind == model.DeviceKindNPU && s.dev.npuUtil {
			kind = model.DeviceKindIGPU // read like the Intel GPU (power scenarios)
		}
		switch kind {
		case model.DeviceKindCUDA:
			if base.GPU.Available {
				c.UtilPct, c.UtilHistory = base.GPU.UtilPct, base.GPU.UtilHistory
			}
		case model.DeviceKindIGPU: // PDH readable in the mock; the NPU is not ("--") outside the power scenarios
			busy := len(c.Tasks) > 0
			util := func(ts float64) int {
				if busy {
					return int(math.Round(71 + 8*math.Sin(ts/9) + 3*math.Sin(ts/2.3)))
				}
				return int(math.Round(3 + 2*math.Sin(ts/13)))
			}
			for k := 0; k < 30; k++ {
				c.UtilHistory = append(c.UtilHistory, util(t-float64(2*(29-k))))
			}
			u := util(t)
			c.UtilPct = &u
		}
		// an Intel device's shared GPU memory (PDH by LUID in the real
		// back end; readable in the mock)
		if v.Kind == model.DeviceKindIGPU || v.Kind == model.DeviceKindNPU {
			used := 310
			if len(c.Tasks) > 0 {
				used = 2950 + int(math.Round(60*math.Sin(t/19)))
			}
			c.MemUsedMB, c.MemTotalMB = &used, intPtr(16228) // half the physical memory (power_resources.json)
		}
		out = append(out, c)
	}
	return out
}

// ---------- system (diagnostics) ----------

func (s *store) openVINOInfo(now time.Time) *api.OpenVINOInfo {
	info := &api.OpenVINOInfo{Python: mockOVPy, Devices: []api.OVDeviceInfo{}, Ignored: []api.OVDeviceInfo{}}
	if s.dev.noOV {
		info.ErrorInfo = &model.ErrorInfo{Code: devices.CodeOVPythonMissing, Params: map[string]any{"path": mockOVPy},
			Detail: "找不到 OpenVINO 环境的 Python 解释器 " + mockOVPy}
		return info
	}
	// the form of testdata/api/openvino.json (task E's example)
	info.Available, info.Version, info.GenAI, info.CanConvert = true, "2026.4.1-22982-e213a147257-releases/2026/4", "2026.4.1.0-3408-ab01d1771e4", true
	info.DetectedAt = rfcPtr(now.Add(-2 * time.Minute))
	info.Devices = []api.OVDeviceInfo{
		{Name: "GPU.0", Kind: model.DeviceKindIGPU, FullName: labelIGPU, DriverVersion: "32.0.101.8724", DeviceID: model.DeviceOVGPU},
		{Name: "NPU", Kind: model.DeviceKindNPU, FullName: labelNPU, DriverVersion: "32.0.100.4841", DeviceID: model.DeviceOVNPU},
		{Name: "CPU", Kind: model.DeviceKindCPU, FullName: labelOVCPU, DeviceID: model.DeviceOVCPU},
	}
	info.Ignored = []api.OVDeviceInfo{{Name: "GPU.1", FullName: labelCUDA + " (dGPU)", DriverVersion: "32.0.15.6614", Reason: "not_intel"}}
	return info
}

// openVINOChecks are the four diagnostics rows of I10 (at most warn), as
// internal/api/system_devices.go writes them.
func (s *store) openVINOChecks() []api.SystemCheck {
	ci := func(code string, params map[string]any) *model.ErrorInfo {
		return &model.ErrorInfo{Code: code, Params: params}
	}
	if s.dev.noOV { // no .venv-ov: the other three are not checked
		return []api.SystemCheck{
			{Name: "openvino_venv", Status: "warn", DetailInfo: ci("check.openvino_venv_missing", map[string]any{"path": mockOVPy}), HintInfo: ci("check.hint_openvino_venv", nil)},
			{Name: "openvino", Status: "warn", DetailInfo: ci("check.api_skipped", nil)},
			{Name: "openvino_devices", Status: "warn", DetailInfo: ci("check.api_skipped", nil)},
			{Name: "npu_driver", Status: "warn", DetailInfo: ci("check.api_skipped", nil)},
		}
	}
	names := labelIGPU + " (GPU.0), " + labelNPU + " (NPU), " + labelOVCPU + " (CPU)"
	return []api.SystemCheck{
		{Name: "openvino_venv", Status: "ok", DetailInfo: ci("check.openvino_venv_ok", map[string]any{"path": mockOVPy})},
		{Name: "openvino", Status: "ok", DetailInfo: ci("check.import_ok", map[string]any{"module": "openvino"})},
		{Name: "openvino_devices", Status: "ok", DetailInfo: ci("check.openvino_devices", map[string]any{"devices": names})},
		{Name: "npu_driver", Status: "ok", DetailInfo: ci("check.npu_driver_ok", map[string]any{"version": "32.0.100.4841"})},
	}
}

// withDeviceChecks inserts the OpenVINO checks after the cuda check (and
// keeps the gpu / cuda checks at warn when cuda:0 is turned off, lead
// ruling 13).
func (s *store) withDeviceChecks(checks []api.SystemCheck) []api.SystemCheck {
	cudaOn := false
	for _, d := range s.visibleDevices() {
		if d.row.ID == model.DeviceCUDA0 && d.row.Enabled {
			cudaOn = true
		}
	}
	out := make([]api.SystemCheck, 0, len(checks)+4)
	inserted := false
	for _, c := range checks {
		if s.dev.cpuOnly { // scenario cpuonly: no NVIDIA card
			switch c.Name {
			case "gpu":
				c.Status, c.Hint, c.HintInfo = "warn", "", nil
				c.DetailInfo = &model.ErrorInfo{Code: "check.gpu_unreadable", Params: map[string]any{"error": "exec: \"nvidia-smi\": executable file not found in %PATH%"}}
				c.Detail = ""
			case "cuda":
				c.Status, c.Hint, c.HintInfo, c.Detail = "warn", "", nil, ""
				c.DetailInfo = &model.ErrorInfo{Code: "check.api_skipped"}
			}
		}
		if !cudaOn && (c.Name == "gpu" || c.Name == "cuda") && c.Status == "error" {
			c.Status = "warn"
		}
		out = append(out, c)
		if c.Name == "cuda" {
			out = append(out, s.openVINOChecks()...)
			inserted = true
		}
	}
	if !inserted {
		out = append(out, s.openVINOChecks()...)
	}
	return out
}

// ---------- endpoints ----------

func (m *mock) registerDevices(mux *http.ServeMux) {
	a := m.api
	mux.HandleFunc("GET /api/asr/devices", a(m.listDevices))
	mux.HandleFunc("POST /api/asr/devices/detect", a(m.detectDevices))
	mux.HandleFunc("PUT /api/asr/devices/order", a(m.orderDevices))
	mux.HandleFunc("PATCH /api/asr/devices/{id}", a(m.patchDevice))
	mux.HandleFunc("POST /api/asr/devices/{id}/pause", a(m.pauseDevice))
	mux.HandleFunc("POST /api/asr/devices/{id}/selftest", a(m.startSelfTest))
	mux.HandleFunc("GET /api/asr/devices/{id}/selftest", a(m.getSelfTest))
	mux.HandleFunc("DELETE /api/asr/devices/{id}/selftest", a(m.cancelSelfTest))
	mux.HandleFunc("POST /api/asr/devices/{id}/selftest/apply", a(m.applySelfTest))
	mux.HandleFunc("POST /api/asr/devices/{id}/prompt/dismiss", a(m.dismissPrompt))
}

// withStore runs fn with s.mu held and writes its result.
func withStore(w http.ResponseWriter, s *store, fn func(now time.Time) (int, any, error)) {
	s.mu.Lock()
	status, v, err := fn(time.Now())
	s.mu.Unlock()
	if err != nil {
		writeErr(w, err)
		return
	}
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	writeJSON(w, status, v)
}

func (s *store) devicesChanged() {
	s.notify(api.ChangedEvent{Kind: "devices"})
}

func (s *store) oneView(d *mockDevice, now time.Time) api.ASRDeviceView {
	s.advanceDevices(now)
	return s.deviceView(d, now, s.runningByDevice(now), s.policyInput(now, s.assignRuns(now)))
}

func (m *mock) listDevices(w http.ResponseWriter, r *http.Request, s *store) {
	withStore(w, s, func(now time.Time) (int, any, error) {
		return http.StatusOK, m.obStagedDevices(r, s.deviceViews(now)), nil
	})
}

func (m *mock) detectDevices(w http.ResponseWriter, r *http.Request, s *store) {
	if !sleepCtx(r, settingsLatency) {
		return
	}
	withStore(w, s, func(now time.Time) (int, any, error) {
		for _, d := range s.dev.devs {
			if d.present {
				d.row.DetectedAt = rfcPtr(now)
			}
		}
		s.devicesChanged()
		return http.StatusOK, s.deviceViews(now), nil
	})
}

func (m *mock) orderDevices(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.DeviceOrderInput
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	withStore(w, s, func(now time.Time) (int, any, error) {
		vis := s.visibleDevices()
		bad := codedErr(http.StatusBadRequest, devices.CodeOrderInvalid, nil, "设备顺序无效：需要列出每台设备各一次")
		if len(in.IDs) != len(vis) {
			return 0, nil, bad
		}
		seen := map[string]bool{}
		var order []*mockDevice
		for _, id := range in.IDs {
			d, err := s.device(id)
			if err != nil || seen[id] {
				return 0, nil, bad
			}
			seen[id] = true
			order = append(order, d)
		}
		// the hidden devices (scenario noov) keep their place below
		for _, d := range s.dev.devs {
			if !seen[d.row.ID] {
				order = append(order, d)
			}
		}
		for i, d := range order {
			d.row.Priority = 10 * (len(order) - i)
		}
		s.devicesChanged()
		s.settingsChanged() // the current asr model of the model-choice diffs
		return http.StatusOK, s.deviceViews(now), nil
	})
}

// patchDevice mirrors devices.Registry.Update.
func (m *mock) patchDevice(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.ASRDevicePatchInput
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	withStore(w, s, func(now time.Time) (int, any, error) {
		d, err := s.device(r.PathValue("id"))
		if err != nil {
			return 0, nil, err
		}
		row := d.row
		if in.Enabled != nil {
			row.Enabled = *in.Enabled
		}
		if in.Model != nil {
			row.Model = strings.TrimSpace(*in.Model)
		}
		if in.Precision != nil {
			row.Precision = strings.TrimSpace(*in.Precision)
		}
		if in.MaxParallel != nil {
			row.MaxParallel = *in.MaxParallel
		}
		switch {
		case row.Model == "":
			return 0, nil, codedErr(http.StatusBadRequest, devices.CodeModelMissing, nil, "请为这台转写设备选择模型")
		case row.Backend == model.BackendOpenVINO && !devices.ValidOVModel(row.Model):
			return 0, nil, codedErr(http.StatusBadRequest, devices.CodeModelInvalid, map[string]any{"value": row.Model}, "Intel 设备不支持模型「"+row.Model+"」")
		case !model.ValidPrecision(row.Backend, row.Precision):
			return 0, nil, codedErr(http.StatusBadRequest, devices.CodePrecisionInvalid, map[string]any{"value": row.Precision}, "精度「"+row.Precision+"」不适用于这台设备")
		case row.MaxParallel < 1 || row.MaxParallel > 8:
			return 0, nil, codedErr(http.StatusBadRequest, devices.CodeMaxParallelInvalid, map[string]any{"min": 1, "max": 8}, "并发数必须在 1 到 8 之间")
		}
		d.row = row
		s.devicesChanged()
		s.settingsChanged()
		return http.StatusOK, s.oneView(d, now), nil
	})
}

func (m *mock) pauseDevice(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.DevicePauseInput
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	withStore(w, s, func(now time.Time) (int, any, error) {
		d, err := s.device(r.PathValue("id"))
		if err != nil {
			return 0, nil, err
		}
		d.paused = in.Paused
		if in.Paused && in.CancelRunning {
			for id, dev := range s.dev.assigned {
				if dev != d.row.ID {
					continue
				}
				if l, err := s.findLecture(id); err == nil {
					if j := stageIndex(l, model.StageASR); j >= 0 && l.Stages[j].Status == model.StatusRunning {
						l.Stages[j].Status = model.StatusCancelled
						s.changed(l)
					}
				}
			}
		}
		s.devicesChanged()
		return http.StatusOK, s.oneView(d, now), nil
	})
}

func (m *mock) startSelfTest(w http.ResponseWriter, r *http.Request, s *store) {
	withStore(w, s, func(now time.Time) (int, any, error) {
		d, err := s.device(r.PathValue("id"))
		if err != nil {
			return 0, nil, err
		}
		s.advanceDevices(now)
		if d.testActive() {
			return 0, nil, codedErr(http.StatusConflict, sched.CodeSelfTestRunning, nil, "这台设备的测试已在进行")
		}
		if _, ok := s.clipSource(); !ok {
			return 0, nil, codedErr(http.StatusConflict, stages.CodeSelfTestNoAudio, nil, "还没有可用于测试的录音：请先完成一节讲座的转写与修复")
		}
		// power management (power-brief §3.5, devices.SelfTest): on battery
		// only the battery device may be tested; on a machine with a
		// battery a test starts only while no other device is active (it
		// waits with the power-aware reason, power_queue:{active device})
		in := s.policyInput(now, s.assignRuns(now))
		dec := devices.SelfTest(in, d.row.ID)
		switch {
		case !dec.Allowed && model.DeviceKind(d.row.ID) == model.DeviceKindCPU:
			// a laptop never tests a CPU device, in any power state (sched testRefusal)
			return 0, nil, codedErr(http.StatusConflict, sched.CodeSelfTestPowerCPU, map[string]any{"device": d.row.Label},
				"电源感知：笔记本不使用 CPU 转写，不需要测试「"+d.row.Label+"」")
		case !dec.Allowed:
			return 0, nil, codedErr(http.StatusConflict, sched.CodeSelfTestPowerBattery, map[string]any{"device": d.row.Label},
				"电源感知：使用电池时只能测试省电转写设备，接通电源后再测试「"+d.row.Label+"」")
		}
		own := len(s.runningByDevice(now)[d.row.ID]) > 0 // 3b: the device's own transcriptions first
		d.test = s.planTest(d, now, own || !dec.Start)
		if !dec.Start {
			d.test.waitFor = model.WaitPowerQueue(otherActive(in, d.row.ID))
		}
		s.devicesChanged()
		return http.StatusAccepted, s.selfTestView(d, now), nil
	})
}

func (m *mock) getSelfTest(w http.ResponseWriter, r *http.Request, s *store) {
	withStore(w, s, func(now time.Time) (int, any, error) {
		d, err := s.device(r.PathValue("id"))
		if err != nil {
			return 0, nil, err
		}
		s.advanceDevices(now)
		return http.StatusOK, s.selfTestView(d, now), nil
	})
}

func (m *mock) cancelSelfTest(w http.ResponseWriter, r *http.Request, s *store) {
	withStore(w, s, func(now time.Time) (int, any, error) {
		d, err := s.device(r.PathValue("id"))
		if err != nil {
			return 0, nil, err
		}
		s.advanceDevices(now)
		if d.testActive() {
			d.test.cancelled = &now
			s.advanceDevices(now)
		}
		return http.StatusOK, s.selfTestView(d, now), nil
	})
}

// applySelfTest mirrors devices.Registry.ApplyBenchmark: only an OK result
// of the last test.
func (m *mock) applySelfTest(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.SelfTestApplyInput
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	withStore(w, s, func(now time.Time) (int, any, error) {
		d, err := s.device(r.PathValue("id"))
		if err != nil {
			return 0, nil, err
		}
		s.advanceDevices(now)
		ok := false
		if d.bench != nil {
			for _, res := range d.bench.Results {
				ok = ok || (res.OK && res.Model == in.Model && res.Precision == in.Precision)
			}
		}
		if !ok {
			return 0, nil, codedErr(http.StatusBadRequest, devices.CodeApplyNotTested, map[string]any{"model": in.Model, "precision": in.Precision},
				in.Model+" · "+in.Precision+" 没有在最近一次测试中成功运行，不能应用")
		}
		d.row.Model, d.row.Precision = in.Model, in.Precision
		b := *d.bench
		at := now.UTC().Format(time.RFC3339)
		b.AppliedAt = &at
		d.bench = &b
		s.devicesChanged()
		s.settingsChanged()
		return http.StatusOK, s.oneView(d, now), nil
	})
}

func (m *mock) dismissPrompt(w http.ResponseWriter, r *http.Request, s *store) {
	withStore(w, s, func(now time.Time) (int, any, error) {
		d, err := s.device(r.PathValue("id"))
		if err != nil {
			return 0, nil, err
		}
		d.dismissed = true
		s.devicesChanged()
		return http.StatusNoContent, nil, nil
	})
}

// checkDeviceMode is settings.SetASR's device_mode check (power-brief §3.2,
// validated before NormalizeASRDeviceMode): auto; manual with a non-empty
// list of existing, non-CPU devices; or a 3b single device id (read as
// manual [id], a CPU device refused like in a list).
func (s *store) checkDeviceMode(a model.ASRSettings) error {
	mode := strings.TrimSpace(a.DeviceMode)
	var list []string
	switch mode {
	case "", model.ASRDeviceModeAuto:
		return nil
	case model.ASRDeviceModeManual:
		list = a.ManualDevices
	default:
		if _, err := s.device(mode); err != nil {
			return codedErr(http.StatusBadRequest, settings.CodeDeviceModeInvalid, map[string]any{"value": mode},
				fmt.Sprintf("转写设备「%s」不存在，请选择“自动”或列表中的设备", mode))
		}
		list = []string{mode}
	}
	n := 0
	for _, id := range list {
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		// the order of settings.SetASR: blank entries skipped, then a CPU
		// device (named by its label, or the id without a row), then a
		// device that does not exist
		n++
		label := id
		d, err := s.device(id)
		if err == nil && strings.TrimSpace(d.row.Label) != "" {
			label = d.row.Label
		}
		if model.DeviceKind(id) == model.DeviceKindCPU {
			return codedErr(http.StatusBadRequest, settings.CodeManualDevicesCPU, map[string]any{"device": label},
				fmt.Sprintf("手动指定不能选择 CPU 转写设备「%s」", label))
		}
		if err != nil {
			return codedErr(http.StatusBadRequest, settings.CodeManualDevicesUnknown, map[string]any{"device": label},
				fmt.Sprintf("转写设备「%s」不存在，请从列表中选择", label))
		}
	}
	if n == 0 {
		return codedErr(http.StatusBadRequest, settings.CodeManualDevicesEmpty, nil, "手动指定转写设备时至少要选择一台设备")
	}
	return nil
}

func rfcPtr(t time.Time) *string {
	s := t.UTC().Format(time.RFC3339)
	return &s
}
