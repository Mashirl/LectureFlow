// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// Power management, task P4: the upper area's readings (system.go), the
// power fields, the policy view and the device rows' new flags.

func fptr(f float64) *float64 { return &f }

func intOrNil(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// --- pure readings ---

func TestCPUPctOf(t *testing.T) {
	cases := []struct {
		in   *float64
		want any
	}{
		{nil, nil}, // first collection of the rate counter / no counter
		{fptr(0), 0},
		{fptr(17.5), 18},
		{fptr(99.6), 100},
		{fptr(142.3), 100}, // % Processor Utility above the base frequency
		{fptr(-1), nil},
		{fptr(math.NaN()), nil},
		{fptr(math.Inf(1)), nil},
	}
	for _, c := range cases {
		if got := intOrNil(CPUPctOf(c.in)); got != c.want {
			t.Errorf("CPUPctOf(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestKelvinToC(t *testing.T) {
	cases := []struct {
		k    float64
		c    int
		ok   bool
		name string
	}{
		{318.15, 45, true, "45 °C"},
		{301.0, 28, true, "rounded"},
		{373.4, 100, true, "100 °C"},
		{0, 0, false, "0 K: no sensor"},
		{273.15, 0, false, "0 °C is not a CPU reading"},
		{274.15, 1, true, "lowest plausible"},
		{423.15, 150, true, "highest plausible"},
		{424.15, 0, false, "above 150 °C"},
		{math.NaN(), 0, false, "NaN"},
	}
	for _, c := range cases {
		got, ok := KelvinToC(c.k)
		if ok != c.ok || (ok && got != c.c) {
			t.Errorf("%s: KelvinToC(%v) = %d %v, want %d %v", c.name, c.k, got, ok, c.c, c.ok)
		}
	}
}

func TestMaxTempC(t *testing.T) {
	zones := []EngineSample{
		{`\_TZ.TZ00`, 301.15},
		{`\_TZ.THRM`, 335.15}, // the hottest: 62 °C
		{`\_TZ.TZ01`, 0},      // not connected
		{`\_TZ.TZ02`, 550},    // implausible
	}
	if got := intOrNil(MaxTempC(zones)); got != 62 {
		t.Fatalf("max = %v, want 62", got)
	}
	for _, zs := range [][]EngineSample{nil, {}, {{`\_TZ.TZ01`, 0}}} {
		if got := MaxTempC(zs); got != nil {
			t.Errorf("MaxTempC(%v) = %d, want null", zs, *got)
		}
	}
}

func TestMemOf(t *testing.T) {
	used, total := MemOf(32<<30, 18<<30)
	if intOrNil(used) != 14336 || intOrNil(total) != 32768 {
		t.Fatalf("used %v total %v", intOrNil(used), intOrNil(total))
	}
	if u, tt := MemOf(0, 0); u != nil || tt != nil {
		t.Fatal("a zero total is unknown")
	}
	if u, _ := MemOf(8<<30, 9<<30); intOrNil(u) != 0 {
		t.Fatalf("avail above total: used %v", intOrNil(u))
	}
	if got := intOrNil(SharedTotalMB(iptr(32456))); got != 16228 {
		t.Fatalf("shared total %v", got)
	}
	if SharedTotalMB(nil) != nil || SharedTotalMB(iptr(0)) != nil {
		t.Fatal("unknown memory: no shared total")
	}
}

func TestSharedByLUID(t *testing.T) {
	got := SharedByLUID([]EngineSample{
		{"luid_0x00000000_0x000120E2_phys_0", 300 << 20},
		{"luid_0x00000000_0x000120E2_phys_1", 10 << 20}, // summed per adapter
		{"luid_0x00000000_0x000126d0_phys_0", 2950.4 * (1 << 20)},
		{"no luid", 5 << 20},
		{"luid_0x00000000_0x000126D1_phys_0", -1},
	})
	want := map[string]int{"0x00000000_0x000120E2": 310, "0x00000000_0x000126D0": 2950}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %d, want %d", k, got[k], v)
		}
	}
}

func TestPowerSourceOf(t *testing.T) {
	cases := []struct {
		p    devices.Power
		want string
	}{
		{devices.Power{}, ""},                       // unknown (not Windows, failed reading)
		{devices.Power{Known: true}, PowerSourceAC}, // a desktop
		{devices.Power{Known: true, HasBattery: true}, PowerSourceAC},
		{devices.Power{Known: true, HasBattery: true, OnBattery: true}, PowerSourceBattery},
		{devices.PowerFromStatus(1, 8, 80, 0), PowerSourceAC},      // charging laptop
		{devices.PowerFromStatus(0, 0, 40, 0), PowerSourceBattery}, // unplugged
		{devices.PowerFromStatus(255, 255, 255, 0), ""},
	}
	for _, c := range cases {
		if got := PowerSourceOf(c.p); got != c.want {
			t.Errorf("%+v: %q, want %q", c.p, got, c.want)
		}
	}
	p := PowerOf(devices.PowerFromStatus(1, 8, 80, 0), true)
	if !p.Known || p.OnBattery || !p.HasBattery || !p.Charging || p.Source != PowerSourceAC || intOrNil(p.BatteryPct) != 80 || !p.BatteryAware {
		t.Fatalf("PowerOf %+v", p)
	}
}

func TestPolicyOfNeverNull(t *testing.T) {
	b, _ := json.Marshal(PolicyOf(sched.PowerStatus{}))
	for _, k := range []string{`"active":[]`, `"notices":[]`, `"mode":"auto"`, `"wait_ac":0`, `"battery_machine":false`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("%s missing in %s", k, b)
		}
	}
	for _, k := range []string{`"next"`, `"none_reason"`} {
		if strings.Contains(string(b), k) {
			t.Errorf("%s in %s", k, b)
		}
	}
}

// --- the real readers, with fakes ---

type fakeSystem struct {
	mu     sync.Mutex
	reads  []sysReading
	errs   []error
	n      int
	closed int
}

func (f *fakeSystem) Read() (sysReading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.n
	f.n++
	if i < len(f.errs) && f.errs[i] != nil {
		return sysReading{}, f.errs[i]
	}
	if i >= len(f.reads) {
		i = len(f.reads) - 1
	}
	return f.reads[i], nil
}

func (f *fakeSystem) Close() { f.mu.Lock(); f.closed++; f.mu.Unlock() }

func TestSystemReadings(t *testing.T) {
	f := newDevFixture(t)
	clk := newClock()
	d := f.deps(clk)
	d.SystemStats = nil // the readers
	s := New(d)
	s.newEngines = (&fakeEngines{reads: [][]EngineSample{nil}}).open
	sys := &fakeSystem{reads: []sysReading{
		{CPU: nil, Zones: []EngineSample{{`\_TZ.THRM`, 318.15}}}, // first collection: CPU primes
		{CPU: fptr(23.4), Zones: []EngineSample{{`\_TZ.TZ00`, 300.15}, {`\_TZ.THRM`, 321.15}}},
	}}
	opens := 0
	s.newSystem = func() (systemReader, error) { opens++; return sys, nil }
	s.memStatus = func() (uint64, uint64, error) { return 32 << 30, 20 << 30, nil }
	sampleNow(s)
	snap := s.build(context.Background())
	if v := snap.System; v == nil || v.CPUPct != nil || intOrNil(v.CPUTempC) != 45 || intOrNil(v.MemUsedMB) != 12288 || intOrNil(v.MemTotalMB) != 32768 {
		t.Fatalf("first sample %+v", snap.System)
	}
	b, _ := json.Marshal(snap.System)
	if !strings.Contains(string(b), `"cpu_pct":null`) {
		t.Fatalf("json %s", b)
	}
	clk.Advance(2 * time.Second)
	sampleNow(s)
	if v := s.build(context.Background()).System; intOrNil(v.CPUPct) != 23 || intOrNil(v.CPUTempC) != 48 {
		t.Fatalf("second sample %+v", v)
	}
	if opens != 1 {
		t.Fatalf("opened %d times", opens)
	}
	s.Stop()
	if sys.closed != 1 {
		t.Fatalf("closed %d times on Stop, want 1", sys.closed)
	}
}

// Nothing readable (not Windows, no counters, no thermal zone): every value
// is null ("--"); the PDH query is retried after a minute; a read error
// closes it. The memory is read on its own.
func TestSystemReadingsUnavailable(t *testing.T) {
	f := newDevFixture(t)
	clk := newClock()
	d := f.deps(clk)
	d.SystemStats = nil
	s := New(d)
	s.newEngines = (&fakeEngines{reads: [][]EngineSample{nil}}).open
	opens := 0
	s.newSystem = func() (systemReader, error) { opens++; return nil, errors.New("no counters") }
	s.memStatus = func() (uint64, uint64, error) { return 0, 0, errors.New("no memory status") }
	sampleNow(s)
	snap := s.build(context.Background())
	b, _ := json.Marshal(snap.System)
	if string(b) != `{"cpu_pct":null,"mem_used_mb":null,"mem_total_mb":null,"cpu_temp_c":null}` {
		t.Fatalf("system %s", b)
	}
	clk.Advance(2 * time.Second)
	sampleNow(s)
	if opens != 1 {
		t.Fatalf("opened %d times within a minute", opens)
	}
	clk.Advance(engineRetry)
	sys := &fakeSystem{reads: []sysReading{{}, {CPU: fptr(5)}}, errs: []error{nil, nil, errors.New("gone")}}
	s.newSystem = func() (systemReader, error) { opens++; return sys, nil }
	s.memStatus = func() (uint64, uint64, error) { return 16 << 30, 4 << 30, nil }
	sampleNow(s)
	sampleNow(s)
	// No thermal zone: the temperature stays null while the CPU is read.
	if v := s.build(context.Background()).System; intOrNil(v.CPUPct) != 5 || v.CPUTempC != nil || intOrNil(v.MemUsedMB) != 12288 {
		t.Fatalf("after reopening %+v", v)
	}
	sampleNow(s)
	if v := s.build(context.Background()).System; v.CPUPct != nil || intOrNil(v.MemTotalMB) != 16384 {
		t.Fatalf("after a read error %+v", v)
	}
	if sys.closed != 1 || opens != 2 {
		t.Fatalf("closed %d, opened %d", sys.closed, opens)
	}
}

// Before the device wiring (and in the other areas' tests) the snapshot has
// no system and no policy; an injected SystemStats is enough for system.
func TestSnapshotSystemOnlyWhenWired(t *testing.T) {
	s := New(Deps{RunCmd: (&fakeSMI{}).Run, Now: newClock().Now})
	s.newSystem = func() (systemReader, error) { t.Fatal("system counters opened without the wiring"); return nil, nil }
	s.memStatus = func() (uint64, uint64, error) { t.Fatal("memory read without the wiring"); return 0, 0, nil }
	b, _ := json.Marshal(s.Snapshot(context.Background()))
	if bytes.Contains(b, []byte(`"system"`)) || bytes.Contains(b, []byte(`"policy"`)) {
		t.Fatalf("system or policy in %s", b)
	}
	s2 := New(Deps{RunCmd: (&fakeSMI{}).Run, Now: newClock().Now,
		SystemStats: func() System { return System{CPUPct: iptr(7)} }})
	snap := s2.Snapshot(context.Background())
	if snap.System == nil || intOrNil(snap.System.CPUPct) != 7 || snap.Devices != nil || snap.Power != nil || snap.Policy != nil {
		t.Fatalf("injected system %+v", snap)
	}
}

// --- the policy, the power fields and the device rows ---

// fakeMemEngines is a PDH reader that also reads AdapterMemCounter.
type fakeMemEngines struct {
	*fakeEngines
	shared []EngineSample
}

func (f *fakeMemEngines) SharedUsage() []EngineSample { return f.shared }

// laptopOnBattery turns the fixture into the scene of power_resources.json:
// on battery, the NPU active with a lecture, a CUDA job suspended, fw:cpu
// disabled; the NPU never tested.
func laptopOnBattery(t *testing.T, f *devFixture, clk *fakeClock) (*Service, sched.PowerStatus) {
	t.Helper()
	bench := `{"version":1,"status":"done"}`
	f.rows[0].LastBenchmark = &bench
	f.rows[1].LastBenchmark = &bench
	f.rows = f.rows[:3] // cuda:0, ov:GPU, ov:NPU
	f.rows = append(f.rows, model.ASRDevice{ID: model.DeviceFWCPU, Backend: model.BackendFasterWhisper, Label: "CPU (faster-whisper)",
		Enabled: false, Model: "small", Precision: "auto", MaxParallel: 1, Priority: 10})
	f.stats = []sched.DeviceStatus{
		{ID: model.DeviceCUDA0, SlotsMax: 1, BatteryHold: true},
		{ID: model.DeviceOVGPU, SlotsMax: 1, BatteryHold: true},
		{ID: model.DeviceOVNPU, SlotsUsed: 1, SlotsMax: 1,
			Running: []sched.RunningASR{{JobID: "j-asr-w03l2", LectureID: f.lec.ID, Progress: 0.42}}},
	}
	pw := devices.PowerFromStatus(0, 0, 63, 0)
	st := sched.PowerStatus{
		Power: pw,
		View: devices.PolicyView{BatteryMachine: true, OnBattery: true, Mode: model.ASRDeviceModeAuto,
			Reason: devices.PolicyReasonBattery, Active: []string{model.DeviceOVNPU}},
		WaitAC: 1, HoldsSuspended: []string{model.DeviceCUDA0},
		Notices: []sched.PowerNotice{{ID: "j1:power.suspended:2026-10-24T07:12:05Z", At: "2026-10-24T07:12:05Z",
			Info:  model.ErrorInfo{Code: sched.CodePowerSuspended, Params: map[string]any{"lecture": "W03 L1"}},
			JobID: "j1", LectureID: "l1", CourseID: "c1"}},
	}
	d := f.deps(clk)
	d.PowerStatus = func() sched.PowerStatus { return st }
	d.Power = func() devices.Power { t.Error("the power area reads PowerStatus's reading"); return devices.Power{} }
	d.SystemStats = func() System {
		return System{CPUPct: iptr(18), MemUsedMB: iptr(14210), MemTotalMB: iptr(32456)}
	}
	s := New(d)
	eng := &fakeMemEngines{fakeEngines: &fakeEngines{reads: [][]EngineSample{nil, engineRead(2, 47)}},
		shared: []EngineSample{
			{"luid_0x00000000_0x000120E2_phys_0", 310 << 20},
			{"luid_0x00000000_0x000126D0_phys_0", 2950 << 20},
		}}
	s.newEngines = func() (engineReader, error) { return eng, nil }
	return s, st
}

func TestSnapshotPolicyAndDeviceRows(t *testing.T) {
	f := newDevFixture(t)
	clk := newClock()
	s, st := laptopOnBattery(t, f, clk)
	sampleNow(s)
	clk.Advance(2 * time.Second)
	sampleNow(s)
	snap := s.build(context.Background())

	// Every row, priority order, disabled ones too.
	var ids, active []string
	for _, d := range snap.Devices {
		ids = append(ids, d.ID)
		if d.Active {
			active = append(active, d.ID)
		}
	}
	if strings.Join(ids, ",") != "cuda:0,ov:GPU,ov:NPU,fw:cpu" {
		t.Fatalf("rows %v", ids)
	}
	// The cards of the middle area are the policy's active devices.
	if snap.Policy == nil || !slices.Equal(active, snap.Policy.Active) || !slices.Equal(active, []string{model.DeviceOVNPU}) {
		t.Fatalf("active cards %v, policy %+v", active, snap.Policy)
	}
	p := snap.Policy
	if !p.BatteryMachine || p.Mode != model.ASRDeviceModeAuto || p.ManualIgnored || p.Reason != devices.PolicyReasonBattery ||
		p.Next != "" || p.NoneReason != "" || p.WaitAC != 1 || p.WaitNoDevice != 0 || len(p.Notices) != 1 || p.Notices[0].ID != st.Notices[0].ID {
		t.Fatalf("policy %+v", p)
	}
	cuda, igpu, npu, cpu := cardOf(t, snap, model.DeviceCUDA0), cardOf(t, snap, model.DeviceOVGPU), cardOf(t, snap, model.DeviceOVNPU), cardOf(t, snap, model.DeviceFWCPU)
	if !cuda.HoldsSuspended || igpu.HoldsSuspended || npu.HoldsSuspended || cpu.HoldsSuspended {
		t.Errorf("holds_suspended %v %v %v %v", cuda.HoldsSuspended, igpu.HoldsSuspended, npu.HoldsSuspended, cpu.HoldsSuspended)
	}
	if cuda.Untested || igpu.Untested || !npu.Untested || !cpu.Untested {
		t.Errorf("untested %v %v %v %v", cuda.Untested, igpu.Untested, npu.Untested, cpu.Untested)
	}
	if cpu.Enabled || cpu.Active || !cuda.Enabled || !npu.Enabled {
		t.Errorf("enabled cpu %v cuda %v npu %v", cpu.Enabled, cuda.Enabled, npu.Enabled)
	}
	if !cuda.BatteryHold || !igpu.BatteryHold || npu.BatteryHold || !npu.BatteryMode {
		t.Errorf("battery flags cuda %v igpu %v npu %v/%v", cuda.BatteryHold, igpu.BatteryHold, npu.BatteryHold, npu.BatteryMode)
	}
	if len(npu.Tasks) != 1 || npu.Tasks[0].LectureTitle != f.lec.Title || intOrNil(npu.UtilPct) != 47 {
		t.Errorf("npu card %+v", npu)
	}
	// The Intel devices' shared memory (half the physical memory as the
	// total); CUDA and CPU devices have none.
	if intOrNil(igpu.MemUsedMB) != 310 || intOrNil(igpu.MemTotalMB) != 16228 || intOrNil(npu.MemUsedMB) != 2950 || intOrNil(npu.MemTotalMB) != 16228 {
		t.Errorf("intel memory igpu %v/%v npu %v/%v", intOrNil(igpu.MemUsedMB), intOrNil(igpu.MemTotalMB), intOrNil(npu.MemUsedMB), intOrNil(npu.MemTotalMB))
	}
	if cuda.MemUsedMB != nil || cuda.MemTotalMB != nil || cpu.MemUsedMB != nil {
		t.Errorf("memory on cuda / cpu: %+v %+v", cuda, cpu)
	}
	// The power area: the reading of the power status.
	if pw := snap.Power; pw == nil || !pw.Known || !pw.OnBattery || !pw.HasBattery || pw.Charging || pw.Source != PowerSourceBattery ||
		intOrNil(pw.BatteryPct) != 63 {
		t.Fatalf("power %+v", snap.Power)
	}
	if sy := snap.System; sy == nil || intOrNil(sy.CPUPct) != 18 || sy.CPUTempC != nil || intOrNil(sy.MemTotalMB) != 32456 {
		t.Fatalf("system %+v", snap.System)
	}
	b, _ := json.Marshal(cuda)
	if !strings.Contains(string(b), `"mem_used_mb":null,"mem_total_mb":null`) {
		t.Fatalf("cuda json %s", b)
	}
}

// The shared memory is unknown ("--") when the reader has no
// AdapterMemCounter or it had no data; the utilisation stays.
func TestIntelSharedMemoryUnreadable(t *testing.T) {
	f := newDevFixture(t)
	clk := newClock()
	for _, eng := range []engineReader{
		&fakeEngines{reads: [][]EngineSample{nil, engineRead(30, 40)}},                               // no counter
		&fakeMemEngines{fakeEngines: &fakeEngines{reads: [][]EngineSample{nil, engineRead(30, 40)}}}, // no data
	} {
		s := New(f.deps(clk))
		e := eng
		s.newEngines = func() (engineReader, error) { return e, nil }
		sampleNow(s)
		sampleNow(s)
		igpu := cardOf(t, s.build(context.Background()), model.DeviceOVGPU)
		if igpu.MemUsedMB != nil || igpu.MemTotalMB != nil || intOrNil(igpu.UtilPct) != 30 {
			t.Errorf("%T: igpu %+v", eng, igpu)
		}
	}
	// An adapter without an instance in a valid reading uses none (0).
	s := New(f.deps(clk))
	s.newEngines = func() (engineReader, error) {
		return &fakeMemEngines{fakeEngines: &fakeEngines{reads: [][]EngineSample{nil}}, shared: []EngineSample{}}, nil
	}
	sampleNow(s)
	if npu := cardOf(t, s.build(context.Background()), model.DeviceOVNPU); intOrNil(npu.MemUsedMB) != 0 || intOrNil(npu.MemTotalMB) != 16000 {
		t.Errorf("idle npu memory %v/%v", intOrNil(npu.MemUsedMB), intOrNil(npu.MemTotalMB))
	}
}

// Desktop and AC cases of the mapping: Next and NoneReason pass through;
// no active device → no active card.
func TestSnapshotPolicyNoActiveDevice(t *testing.T) {
	f := newDevFixture(t)
	clk := newClock()
	st := sched.PowerStatus{Power: devices.PowerFromStatus(0, 0, 40, 0),
		View: devices.PolicyView{BatteryMachine: true, OnBattery: true, Mode: model.ASRDeviceModeAuto, ManualIgnored: true,
			Reason: devices.PolicyReasonBattery, Active: []string{}, NoneReason: model.WaitPowerNoSaverDevice},
		WaitNoDevice: 2}
	d := f.deps(clk)
	d.PowerStatus = func() sched.PowerStatus { return st }
	s := New(d)
	s.newEngines = (&fakeEngines{reads: [][]EngineSample{nil}}).open
	snap := s.Snapshot(context.Background())
	for _, c := range snap.Devices {
		if c.Active {
			t.Errorf("%s active without a policy device", c.ID)
		}
	}
	if p := snap.Policy; p.NoneReason != model.WaitPowerNoSaverDevice || !p.ManualIgnored || p.WaitNoDevice != 2 || p.Active == nil {
		t.Fatalf("policy %+v", p)
	}
	b, _ := json.Marshal(snap.Policy)
	if !strings.Contains(string(b), `"none_reason":"power_no_saver_device"`) || !strings.Contains(string(b), `"active":[]`) {
		t.Fatalf("policy json %s", b)
	}
	// A pending switch on AC.
	st = sched.PowerStatus{Power: devices.PowerFromStatus(1, 8, 70, 0),
		View: devices.PolicyView{BatteryMachine: true, Mode: model.ASRDeviceModeAuto, Reason: devices.PolicyReasonACAuto,
			Active: []string{model.DeviceOVNPU}, Next: model.DeviceCUDA0}}
	clk.Advance(3 * time.Second)
	snap = s.Snapshot(context.Background())
	if p := snap.Policy; p.Next != model.DeviceCUDA0 || p.Reason != devices.PolicyReasonACAuto || !cardOf(t, snap, model.DeviceOVNPU).Active ||
		cardOf(t, snap, model.DeviceCUDA0).Active || snap.Power.Source != PowerSourceAC || !snap.Power.Charging {
		t.Fatalf("switch pending %+v power %+v", p, snap.Power)
	}
}

// testdata/api/power_resources.json (maintained by task P4) has exactly the
// fields a real snapshot of the same scene has: every object of the example
// has the keys the producer writes there, and no other.
func TestPowerResourcesExampleMatchesSnapshot(t *testing.T) {
	f := newDevFixture(t)
	clk := newClock()
	s, _ := laptopOnBattery(t, f, clk)
	sampleNow(s)
	clk.Advance(2 * time.Second)
	sampleNow(s)
	snap := s.build(context.Background())
	snap.Providers = []Provider{}
	got, _ := json.Marshal(snap)
	want, err := os.ReadFile(filepath.Join("..", "..", "testdata", "api", "power_resources.json"))
	if err != nil {
		t.Fatal(err)
	}
	gk, wk := jsonKeys(t, got), jsonKeys(t, want)
	if strings.Join(gk, "\n") != strings.Join(wk, "\n") {
		t.Fatalf("keys differ\nsnapshot:\n%s\nexample:\n%s", strings.Join(gk, "\n"), strings.Join(wk, "\n"))
	}
}

// jsonKeys lists the key paths of a JSON document ("devices[].tasks[].job_id"),
// sorted and without duplicates.
func jsonKeys(t *testing.T, b []byte) []string {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				seen[p] = true
				// params of a coded text are free-form
				if k != "params" {
					walk(p, e)
				}
			}
		case []any:
			for _, e := range x {
				walk(prefix+"[]", e)
			}
		}
	}
	walk("", v)
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
