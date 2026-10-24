// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// --- PDH parsing (lead ruling 16) ---

func TestParseEngineInstance(t *testing.T) {
	cases := []struct {
		in, luid, engine string
		ok               bool
	}{
		{"pid_1234_luid_0x00000000_0x000120E2_phys_0_eng_0_engtype_3D", "0x00000000_0x000120E2", "phys_0_eng_0", true},
		{"pid_9_luid_0x00000000_0x000126d0_phys_0_eng_0_engtype_Neural", "0x00000000_0x000126D0", "phys_0_eng_0", true},
		{"luid_0x00000001_0x0000D1F2_phys_1_eng_13_engtype_", "0x00000001_0x0000D1F2", "phys_1_eng_13", true},
		{"pid_1_luid_0x00000000_0x0027D9F7_phys_0_eng_6_engtype_OFA_0", "0x00000000_0x0027D9F7", "phys_0_eng_6", true},
		{"pid_1_phys_0_eng_0", "", "", false},
		{"pid_1_luid_00000000_0x0001", "", "", false},
		{"pid_1_luid_0x_0x1", "", "", false},
		{"pid_1_luid_0xZZ_0x1_phys_0", "", "", false},
	}
	for _, c := range cases {
		luid, eng, ok := ParseEngineInstance(c.in)
		if luid != c.luid || eng != c.engine || ok != c.ok {
			t.Errorf("%q: got %q %q %v, want %q %q %v", c.in, luid, eng, ok, c.luid, c.engine, c.ok)
		}
	}
}

// The OpenVINO DEVICE_LUID of this machine's devices (worker probe) and the
// PDH instance names of the same adapters: "e220010000000000" is the LUID
// struct's bytes, LowPart 0x000120E2 first.
func TestLUIDKey(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"e220010000000000", "0x00000000_0x000120E2", true},
		{"D026010000000000", "0x00000000_0x000126D0", true},
		{"f7d9270001000000", "0x00000001_0x0027D9F7", true},
		{"luid_0x00000000_0x000120E2", "0x00000000_0x000120E2", true},
		{"0x00000000_0x000120e2", "0x00000000_0x000120E2", true},
		{"", "", false},
		{"e2200100", "", false},
		{"not hex at all!!", "", false},
	}
	for _, c := range cases {
		got, ok := LUIDKey(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("LUIDKey(%q) = %q %v, want %q %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// Task Manager's rule: per engine the sum over processes, per adapter the
// busiest engine, capped at 100.
func TestUtilByLUID(t *testing.T) {
	got := UtilByLUID([]EngineSample{
		{"pid_1_luid_0x00000000_0x000120E2_phys_0_eng_0_engtype_3D", 20.4},
		{"pid_2_luid_0x00000000_0x000120E2_phys_0_eng_0_engtype_3D", 15.3}, // same engine: 35.7
		{"pid_2_luid_0x00000000_0x000120E2_phys_0_eng_5_engtype_Compute", 30},
		{"pid_3_luid_0x00000000_0x000126D0_phys_0_eng_0_engtype_Neural", 70},
		{"pid_4_luid_0x00000000_0x000126D0_phys_0_eng_0_engtype_Neural", 60}, // 130 → 100
		{"pid_5_luid_0x00000000_0x0027D9F7_phys_0_eng_0_engtype_3D", -1},     // ignored (no entry)
		{"pid_6_phys_0_eng_0_engtype_3D", 50},                                // no LUID
	})
	want := map[string]int{"0x00000000_0x000120E2": 36, "0x00000000_0x000126D0": 100}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d (all %v)", k, got[k], v, got)
		}
	}
}

// --- device cards (I10) ---

type fakeEngines struct {
	mu     sync.Mutex
	reads  [][]EngineSample // returned in order; the last one repeats
	errs   []error
	n      int
	opened int
	closed int
}

func (f *fakeEngines) open() (engineReader, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened++
	return f, nil
}

func (f *fakeEngines) Read() ([]EngineSample, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.n
	f.n++
	if i < len(f.errs) && f.errs[i] != nil {
		return nil, f.errs[i]
	}
	if i >= len(f.reads) {
		i = len(f.reads) - 1
	}
	return f.reads[i], nil
}

func (f *fakeEngines) Close() {
	f.mu.Lock()
	f.closed++
	f.mu.Unlock()
}

func sptr(s string) *string { return &s }
func iptr(i int) *int       { return &i }

// devFixture is a laptop with all four devices (ov:CPU disabled), a lecture
// transcribing on the Intel GPU, a self-test on the NPU and the CUDA card
// paused.
type devFixture struct {
	st    db.Store
	lec   model.Lecture
	rows  []model.ASRDevice
	stats []sched.DeviceStatus
	det   devices.Detection
	power devices.Power
	asr   model.ASRSettings
	mu    sync.Mutex
}

func newDevFixture(t *testing.T) *devFixture {
	t.Helper()
	st, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	c := model.Course{ID: db.NewID(), Name: "CSC1003"}
	if err := st.CreateCourse(ctx, c); err != nil {
		t.Fatal(err)
	}
	l := model.Lecture{ID: db.NewID(), CourseID: c.ID, Title: "Week 3 Ciphers", MediaPath: "m.mp4"}
	if err := st.CreateLecture(ctx, l); err != nil {
		t.Fatal(err)
	}
	at := "2026-10-24T08:00:00Z"
	f := &devFixture{st: st, lec: l,
		rows: []model.ASRDevice{
			{ID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, Label: "NVIDIA GeForce RTX 5070 Ti Laptop GPU", Enabled: true,
				Model: "large-v3", Precision: "auto", MaxParallel: 1, Priority: 300, DetectedAt: &at},
			{ID: model.DeviceOVGPU, Backend: model.BackendOpenVINO, Label: "Intel(R) Graphics (iGPU)", Enabled: true,
				Model: "large-v3-turbo", Precision: "fp16", MaxParallel: 1, Priority: 200, DetectedAt: &at},
			{ID: model.DeviceOVNPU, Backend: model.BackendOpenVINO, Label: "Intel(R) AI Boost", Enabled: true,
				Model: "large-v3-turbo", Precision: "fp16", MaxParallel: 2, Priority: 100, DetectedAt: &at},
			{ID: model.DeviceOVCPU, Backend: model.BackendOpenVINO, Label: "Intel(R) Core(TM) Ultra 9 386H", Enabled: false,
				Model: "large-v3-turbo", Precision: "int8", MaxParallel: 1, Priority: 0, DetectedAt: &at},
		},
		stats: []sched.DeviceStatus{
			{ID: model.DeviceCUDA0, SlotsMax: 1, Paused: true},
			{ID: model.DeviceOVGPU, SlotsUsed: 1, SlotsMax: 1,
				Running: []sched.RunningASR{{JobID: "job-1", LectureID: l.ID, Progress: 0.42}}},
			{ID: model.DeviceOVNPU, SlotsUsed: 1, SlotsMax: 2,
				SelfTest: &sched.DeviceTestState{DeviceID: model.DeviceOVNPU, State: sched.DeviceTestRunning, Progress: 0.25}},
		},
		det: devices.Detection{At: time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC), CUDAName: "NVIDIA GeForce RTX 5070 Ti Laptop GPU",
			OV: &devices.OVProbe{Devices: []devices.OVDevice{
				{Name: "GPU.0", Kind: model.DeviceKindIGPU, FullName: "Intel(R) Graphics (iGPU)", LUID: "e220010000000000"},
				{Name: "NPU", Kind: model.DeviceKindNPU, FullName: "Intel(R) AI Boost", LUID: "d026010000000000"},
				{Name: "CPU", Kind: model.DeviceKindCPU, FullName: "Intel(R) Core(TM) Ultra 9 386H"},
			}}},
		asr: model.ASRSettings{BatteryAware: true, ComputeType: "auto", Device: "cuda"},
	}
	return f
}

func (f *devFixture) deps(clk *fakeClock) Deps {
	return Deps{
		Store: f.st, Now: clk.Now, RunCmd: (&fakeSMI{}).Run,
		ASR: func() model.ASRSettings { f.mu.Lock(); defer f.mu.Unlock(); return f.asr },
		ASRDevices: func() []model.ASRDevice {
			f.mu.Lock()
			defer f.mu.Unlock()
			return append([]model.ASRDevice(nil), f.rows...)
		},
		DeviceStatuses: func() []sched.DeviceStatus {
			f.mu.Lock()
			defer f.mu.Unlock()
			return append([]sched.DeviceStatus(nil), f.stats...)
		},
		Detection: func() devices.Detection { f.mu.Lock(); defer f.mu.Unlock(); return f.det },
		Power:     func() devices.Power { f.mu.Lock(); defer f.mu.Unlock(); return f.power },
		// the upper area's readings (power management): fixed, the real
		// readers are tested with fakes in power_test.go
		SystemStats: func() System { return System{MemUsedMB: iptr(14000), MemTotalMB: iptr(32000)} },
	}
}

func cardOf(t *testing.T, snap Snapshot, id string) Device {
	t.Helper()
	for _, d := range snap.Devices {
		if d.ID == id {
			return d
		}
	}
	t.Fatalf("no card %s in %+v", id, snap.Devices)
	return Device{}
}

func engineRead(igpu, npu float64) []EngineSample {
	return []EngineSample{
		{"pid_10_luid_0x00000000_0x000120E2_phys_0_eng_0_engtype_3D", igpu},
		{"pid_10_luid_0x00000000_0x000126D0_phys_0_eng_0_engtype_Neural", npu},
	}
}

func TestDeviceCards(t *testing.T) {
	f := newDevFixture(t)
	clk := newClock()
	eng := &fakeEngines{reads: [][]EngineSample{nil, engineRead(41.6, 12), engineRead(55, 0)}}
	s := New(f.deps(clk))
	s.newEngines = eng.open
	sampleNow(s) // primes the rate counter: values unknown
	snap := s.build(context.Background())
	if g := cardOf(t, snap, model.DeviceOVGPU); g.UtilPct != nil || len(g.UtilHistory) != 0 {
		t.Fatalf("igpu while priming: util %v history %v, want null and []", g.UtilPct, g.UtilHistory)
	}
	clk.Advance(2 * time.Second)
	sampleNow(s)
	clk.Advance(2 * time.Second)
	sampleNow(s)
	snap = s.build(context.Background())

	// Power management (task P4): one entry per row, disabled ones too,
	// priority order; ov:CPU is disabled. Without a power status the
	// enabled rows are the active cards (the 3b cards).
	var ids []string
	for _, d := range snap.Devices {
		ids = append(ids, d.ID)
		if d.Enabled != (d.ID != model.DeviceOVCPU) || d.Active != d.Enabled {
			t.Errorf("%s: enabled %v active %v", d.ID, d.Enabled, d.Active)
		}
	}
	if strings.Join(ids, ",") != "cuda:0,ov:GPU,ov:NPU,ov:CPU" {
		t.Fatalf("cards %v", ids)
	}
	cuda := cardOf(t, snap, model.DeviceCUDA0)
	if !cuda.Paused || cuda.SlotsUsed != 0 || cuda.SlotsMax != 1 || cuda.Kind != model.DeviceKindCUDA ||
		cuda.Backend != model.BackendFasterWhisper || cuda.Label != "NVIDIA GeForce RTX 5070 Ti Laptop GPU" || !cuda.Present {
		t.Fatalf("cuda card %+v", cuda)
	}
	// CUDA keeps the nvidia-smi metrics (I10).
	if cuda.UtilPct == nil || *cuda.UtilPct != 10 || len(cuda.UtilHistory) != len(snap.GPU.UtilHistory) || len(cuda.UtilHistory) == 0 {
		t.Fatalf("cuda util %v %v (gpu %v)", cuda.UtilPct, cuda.UtilHistory, snap.GPU.UtilHistory)
	}
	igpu := cardOf(t, snap, model.DeviceOVGPU)
	if igpu.SlotsUsed != 1 || igpu.SlotsMax != 1 || igpu.Model != "large-v3-turbo" || igpu.Precision != "fp16" || igpu.Kind != model.DeviceKindIGPU {
		t.Fatalf("igpu card %+v", igpu)
	}
	if len(igpu.Tasks) != 1 || igpu.Tasks[0] != (DeviceTask{JobID: "job-1", LectureID: f.lec.ID, LectureTitle: "Week 3 Ciphers",
		CourseID: f.lec.CourseID, Progress: 0.42}) {
		t.Fatalf("igpu tasks %+v", igpu.Tasks)
	}
	if igpu.UtilPct == nil || *igpu.UtilPct != 55 || fmt.Sprint(igpu.UtilHistory) != "[42 55]" {
		t.Fatalf("igpu util %v history %v", igpu.UtilPct, igpu.UtilHistory)
	}
	npu := cardOf(t, snap, model.DeviceOVNPU)
	if npu.SlotsUsed != 1 || npu.SlotsMax != 2 || len(npu.Tasks) != 1 || !npu.Tasks[0].SelfTest || npu.Tasks[0].Progress != 0.25 || npu.Tasks[0].JobID != "" {
		t.Fatalf("npu card %+v", npu)
	}
	// An adapter with no instance in a valid reading is idle (0), not unknown.
	if npu.UtilPct == nil || *npu.UtilPct != 0 {
		t.Fatalf("npu util %v", npu.UtilPct)
	}
	if snap.Power == nil || snap.Power.Known || snap.Power.OnBattery || !snap.Power.BatteryAware || snap.Power.BatteryPct != nil {
		t.Fatalf("power %+v", snap.Power)
	}
	for _, d := range snap.Devices {
		if d.BatteryMode || d.BatteryHold {
			t.Fatalf("%s battery flags on AC power: %+v", d.ID, d)
		}
	}
	if eng.opened != 1 {
		t.Fatalf("PDH reader opened %d times, want 1", eng.opened)
	}
	// GPU.Backend comes from the cuda:0 row (precision auto, 16 GB card → float16).
	if snap.GPU.Backend != "CUDA · float16" {
		t.Fatalf("gpu backend %q", snap.GPU.Backend)
	}
	s.Stop()
	if eng.closed != 1 {
		t.Fatalf("PDH reader closed %d times on Stop, want 1", eng.closed)
	}
}

// Battery mode (I8, I10): every card says so; the scheduler's battery hold
// is shown on the device it applies to.
func TestDeviceCardsOnBattery(t *testing.T) {
	f := newDevFixture(t)
	f.power = devices.Power{Known: true, OnBattery: true, Saver: true, BatteryPct: iptr(57)}
	f.stats[0].BatteryHold = true
	f.asr.BatteryAware = true
	clk := newClock()
	s := New(f.deps(clk))
	s.newEngines = (&fakeEngines{reads: [][]EngineSample{nil}}).open
	snap := s.Snapshot(context.Background())
	if snap.Power == nil || !snap.Power.Known || !snap.Power.OnBattery || !snap.Power.Saver || snap.Power.BatteryPct == nil ||
		*snap.Power.BatteryPct != 57 || !snap.Power.BatteryAware {
		t.Fatalf("power %+v", snap.Power)
	}
	for _, d := range snap.Devices {
		if !d.BatteryMode {
			t.Fatalf("%s: battery_mode false on battery", d.ID)
		}
		if d.BatteryHold != (d.ID == model.DeviceCUDA0) {
			t.Fatalf("%s: battery_hold %v", d.ID, d.BatteryHold)
		}
	}
	// Unknown power state: the scheduler treats it as AC, so does the card.
	f.mu.Lock()
	f.power = devices.Power{Known: false, OnBattery: true}
	f.mu.Unlock()
	clk.Advance(3 * time.Second)
	if d := s.Snapshot(context.Background()).Devices[0]; d.BatteryMode {
		t.Fatalf("battery_mode with an unknown power state: %+v", d)
	}
}

// PDH not readable (not Windows, no counter set, a failing read): the Intel
// cards carry util_pct null and util_history [] ("--"), slots and tasks
// still shown; the reader is retried after a minute, not on every sample.
func TestDeviceCardsUtilUnreadable(t *testing.T) {
	f := newDevFixture(t)
	clk := newClock()
	s := New(f.deps(clk))
	opens := 0
	s.newEngines = func() (engineReader, error) { opens++; return nil, errors.New("no counters") }
	sampleNow(s)
	clk.Advance(2 * time.Second)
	sampleNow(s)
	snap := s.build(context.Background())
	igpu := cardOf(t, snap, model.DeviceOVGPU)
	if igpu.UtilPct != nil || igpu.UtilHistory == nil || len(igpu.UtilHistory) != 0 || len(igpu.Tasks) != 1 || igpu.SlotsUsed != 1 {
		t.Fatalf("igpu without counters %+v", igpu)
	}
	b, _ := json.Marshal(igpu)
	if !strings.Contains(string(b), `"util_pct":null`) || !strings.Contains(string(b), `"util_history":[]`) {
		t.Fatalf("json %s", b)
	}
	if opens != 1 {
		t.Fatalf("opened %d times within a minute, want 1", opens)
	}
	clk.Advance(engineRetry)
	sampleNow(s)
	if opens != 2 {
		t.Fatalf("opened %d times after a minute, want 2", opens)
	}

	// A read error closes the reader and shows "--" again.
	eng := &fakeEngines{reads: [][]EngineSample{nil, engineRead(30, 30)}, errs: []error{nil, nil, errors.New("gone")}}
	s2 := New(f.deps(clk))
	s2.newEngines = eng.open
	sampleNow(s2)
	sampleNow(s2)
	if v := cardOf(t, s2.build(context.Background()), model.DeviceOVNPU).UtilPct; v == nil || *v != 30 {
		t.Fatalf("npu util %v", v)
	}
	sampleNow(s2)
	if v := cardOf(t, s2.build(context.Background()), model.DeviceOVNPU).UtilPct; v != nil {
		t.Fatalf("npu util after a read error %v, want null", *v)
	}
	if eng.closed != 1 {
		t.Fatalf("closed %d, want 1", eng.closed)
	}

	// Without a LUID from the probe there is nothing to match: "--", and
	// the counters are not even opened.
	f.mu.Lock()
	for i := range f.det.OV.Devices {
		f.det.OV.Devices[i].LUID = ""
	}
	f.mu.Unlock()
	s3 := New(f.deps(clk))
	opened := 0
	s3.newEngines = func() (engineReader, error) { opened++; return &fakeEngines{reads: [][]EngineSample{nil}}, nil }
	sampleNow(s3)
	if v := cardOf(t, s3.build(context.Background()), model.DeviceOVGPU).UtilPct; v != nil || opened != 0 {
		t.Fatalf("util %v, opened %d", v, opened)
	}
}

// Present (I10 "未检测到"): before the first detection a row seen earlier
// counts as present; afterwards what the detection saw; fw:cpu always.
func TestDeviceCardsPresent(t *testing.T) {
	f := newDevFixture(t)
	f.rows = append(f.rows, model.ASRDevice{ID: model.DeviceFWCPU, Backend: model.BackendFasterWhisper, Label: "CPU",
		Enabled: true, Model: "small", Precision: "auto", MaxParallel: 1, Priority: 10})
	f.rows[2].DetectedAt = nil  // ov:NPU never seen
	f.det = devices.Detection{} // no detection yet
	clk := newClock()
	s := New(f.deps(clk))
	s.newEngines = (&fakeEngines{reads: [][]EngineSample{nil}}).open
	snap := s.Snapshot(context.Background())
	want := map[string]bool{model.DeviceCUDA0: true, model.DeviceOVGPU: true, model.DeviceOVNPU: false, model.DeviceFWCPU: true}
	for id, w := range want {
		if got := cardOf(t, snap, id).Present; got != w {
			t.Errorf("before detection %s present %v, want %v", id, got, w)
		}
	}
	// The detection: nvidia-smi failed this time, the probe saw the NPU only.
	f.mu.Lock()
	f.det = devices.Detection{At: clk.Now(), CUDAErr: "nvidia-smi failed",
		OV: &devices.OVProbe{Devices: []devices.OVDevice{{Name: "NPU", Kind: model.DeviceKindNPU}}}}
	f.mu.Unlock()
	clk.Advance(3 * time.Second)
	snap = s.Snapshot(context.Background())
	want = map[string]bool{model.DeviceCUDA0: false, model.DeviceOVGPU: false, model.DeviceOVNPU: true, model.DeviceFWCPU: true}
	for id, w := range want {
		if got := cardOf(t, snap, id).Present; got != w {
			t.Errorf("after detection %s present %v, want %v", id, got, w)
		}
	}
}

// Without the device dependencies (before 3b) the snapshot has no device
// cards and no power, exactly as before.
func TestSnapshotWithoutDevices(t *testing.T) {
	s := New(Deps{RunCmd: (&fakeSMI{}).Run, Now: newClock().Now,
		ASR: func() model.ASRSettings { return model.ASRSettings{ComputeType: "int8_float16", Device: "cuda"} }})
	s.newEngines = func() (engineReader, error) { t.Fatal("PDH opened without devices"); return nil, nil }
	snap := s.Snapshot(context.Background())
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"devices"`) || strings.Contains(string(b), `"power"`) {
		t.Fatalf("devices or power in %s", b)
	}
	if snap.GPU.Backend != "CUDA · int8_float16" {
		t.Fatalf("legacy backend %q", snap.GPU.Backend)
	}
	_ = sptr
}
