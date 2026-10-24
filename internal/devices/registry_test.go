// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// Task B tests of the Registry (brief section 7 B): the sync rules of
// Detect, the one-time migration of the legacy settings, presence,
// Usable, CurrentASRSnapshot, Update / Reorder / ApplyBenchmark with their
// codes, the workbench prompts and the OnChange hooks. The outside world
// (nvidia-smi, the OpenVINO probe, the clock) is injected.

func tctx() context.Context { return context.Background() }

func regStore(t *testing.T) db.Store {
	t.Helper()
	st, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// world is the injected outside world of a Registry.
type world struct {
	mu       sync.Mutex
	now      time.Time
	cuda     string // "" = nvidia-smi fails
	probe    *OVProbe
	probeErr error
	probes   atomic.Int32
}

func (w *world) opts() RegistryOptions {
	return RegistryOptions{
		Now: func() time.Time { w.mu.Lock(); defer w.mu.Unlock(); return w.now },
		CUDAName: func(context.Context) (string, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			if w.cuda == "" {
				return "", errors.New("nvidia-smi: exit status 9")
			}
			return w.cuda, nil
		},
		Probe: func(context.Context) (*OVProbe, error) {
			w.probes.Add(1)
			w.mu.Lock()
			defer w.mu.Unlock()
			return w.probe, w.probeErr
		},
	}
}

func (w *world) set(fn func(w *world)) { w.mu.Lock(); fn(w); w.mu.Unlock() }

func (w *world) stamp() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.now.UTC().Format(time.RFC3339)
}

// intelProbe is the probe of the reference laptop: Intel GPU, NPU and CPU,
// the NVIDIA card ignored.
func intelProbe() *OVProbe {
	return &OVProbe{OpenVINO: "2026.4.1", GenAI: "2026.4.1.0", CanConvert: true,
		Devices: []OVDevice{
			{Name: "GPU.0", Kind: model.DeviceKindIGPU, FullName: "Intel(R) Arc(TM) 140T GPU (16GB) (iGPU)", Vendor: "0x8086", LUID: "a1b2"},
			{Name: "NPU", Kind: model.DeviceKindNPU, FullName: "Intel(R) AI Boost", Vendor: "0x8086", DriverVersion: "32.0.100.4297"},
			{Name: "CPU", Kind: model.DeviceKindCPU, FullName: "Intel(R) Core(TM) Ultra 9 285H", Vendor: "0x8086"},
		},
		Ignored: []OVDevice{{Name: "GPU.1", FullName: "NVIDIA GeForce RTX 5070 Ti Laptop GPU", Vendor: "0x10de", Reason: ProbeReasonNotIntel}},
	}
}

const rtx = "NVIDIA GeForce RTX 5070 Ti Laptop GPU"

func newWorld() *world {
	return &world{now: time.Date(2026, 10, 24, 1, 2, 3, 0, time.UTC), cuda: rtx, probe: intelProbe()}
}

// newReg returns a loaded registry on st with w's outside world and a
// counter of its OnChange calls.
func newReg(t *testing.T, st db.Store, w *world) (*Registry, *atomic.Int32) {
	t.Helper()
	r := NewRegistry(st, config.Default(), t.TempDir(), w.opts())
	if err := r.Load(tctx()); err != nil {
		t.Fatal(err)
	}
	var n atomic.Int32
	r.OnChange(func() { n.Add(1) })
	return r, &n
}

func devIDs(ds []model.ASRDevice) []string {
	out := []string{}
	for _, d := range ds {
		out = append(out, d.ID)
	}
	return out
}

func regCode(err error) (string, map[string]any) {
	var ce *model.CodedError
	if errors.As(err, &ce) {
		return ce.Info.Code, ce.Info.Params
	}
	return "", nil
}

func wantCode(t *testing.T, what string, err error, sentinel error, code string, params map[string]any) {
	t.Helper()
	c, p := regCode(err)
	if !errors.Is(err, sentinel) || c != code {
		t.Fatalf("%s: err %v (code %q), want %s wrapping %v", what, err, c, code, sentinel)
	}
	if params != nil && !reflect.DeepEqual(p, params) {
		t.Fatalf("%s: params %v, want %v", what, p, params)
	}
}

func TestDetectInsertsNewDevicesWithDefaults(t *testing.T) {
	st := regStore(t)
	w := newWorld()
	r, hooks := newReg(t, st, w)
	if r.Detected() || len(r.List()) != 0 {
		t.Fatal("fresh registry")
	}
	d, newIDs, err := r.Detect(tctx())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{model.DeviceCUDA0, model.DeviceOVGPU, model.DeviceOVNPU, model.DeviceOVCPU}; !reflect.DeepEqual(newIDs, want) {
		t.Fatalf("newIDs %v, want %v", newIDs, want)
	}
	if d.CUDAName != rtx || d.CUDAErr != "" || d.OV == nil || d.OVErr != nil || len(d.OV.Ignored) != 1 || !d.At.Equal(w.now) {
		t.Fatalf("detection %+v", d)
	}
	if d.Python != config.Default().PythonOpenVINO {
		t.Fatalf("python %q", d.Python)
	}
	now := w.stamp()
	want := []model.ASRDevice{
		{ID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, Label: rtx, Enabled: true, Model: "large-v3", Precision: model.PrecisionAuto, MaxParallel: 1, Priority: 300, DetectedAt: &now},
		{ID: model.DeviceOVGPU, Backend: model.BackendOpenVINO, Label: "Intel(R) Arc(TM) 140T GPU (16GB) (iGPU)", Enabled: true, Model: "large-v3-turbo", Precision: model.PrecisionFP16, MaxParallel: 1, Priority: 200, DetectedAt: &now},
		{ID: model.DeviceOVNPU, Backend: model.BackendOpenVINO, Label: "Intel(R) AI Boost", Enabled: true, Model: "large-v3-turbo", Precision: model.PrecisionFP16, MaxParallel: 1, Priority: 100, DetectedAt: &now},
		// fw:cpu always has a row (3b Gate round 1, item 1): disabled, since
		// accelerators were found; not listed in newIDs.
		{ID: model.DeviceFWCPU, Backend: model.BackendFasterWhisper, Label: LabelFWCPU, Enabled: false, Model: "small", Precision: model.PrecisionAuto, MaxParallel: 1, Priority: 10, DetectedAt: &now},
		// ov:CPU is the last resort: disabled by default.
		{ID: model.DeviceOVCPU, Backend: model.BackendOpenVINO, Label: "Intel(R) Core(TM) Ultra 9 285H", Enabled: false, Model: "large-v3-turbo", Precision: model.PrecisionFP16, MaxParallel: 1, Priority: 0, DetectedAt: &now},
	}
	if got := r.List(); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows\n got %+v\nwant %+v", got, want)
	}
	// The database holds the same rows (the cache is not ahead of it).
	if rows, _ := st.ListASRDevices(tctx()); !reflect.DeepEqual(rows, want) {
		t.Fatalf("stored rows %+v", rows)
	}
	if !r.Detected() || hooks.Load() != 1 {
		t.Fatalf("detected %v hooks %d", r.Detected(), hooks.Load())
	}
	if !promptDismissed(t, st, model.DeviceFWCPU) {
		t.Fatal("the built-in fw:cpu must not prompt")
	}
	for _, id := range devIDs(want) {
		if !r.Present(id) {
			t.Errorf("%s not present", id)
		}
	}
	if !r.Present(model.DeviceFWCPU) {
		t.Error("fw:cpu is always present")
	}
	if r.Present("ov:XYZ") {
		t.Error("unknown id present")
	}
	if got := r.LastDetection(); !reflect.DeepEqual(got, d) {
		t.Fatalf("LastDetection %+v", got)
	}
}

func TestNewDeviceRowFallback(t *testing.T) {
	// fw:cpu has no recommendation: I6's small + int8, in faster-whisper's
	// names (small / auto, which resolves to int8 on the CPU).
	d := NewDeviceRow(model.DeviceFWCPU, LabelFWCPU)
	if d.Model != "small" || d.Precision != model.PrecisionAuto || !d.Enabled || d.Priority != 10 || d.Backend != model.BackendFasterWhisper || d.MaxParallel != 1 {
		t.Fatalf("fw:cpu row %+v", d)
	}
	if c := Fallback(model.BackendOpenVINO); c.Model != "small" || c.Precision != model.PrecisionINT8 {
		t.Fatalf("openvino fallback %+v", c)
	}
}

func TestDetectKeepsUserFieldsAndMarksMissing(t *testing.T) {
	st := regStore(t)
	w := newWorld()
	r, hooks := newReg(t, st, w)
	if _, _, err := r.Detect(tctx()); err != nil {
		t.Fatal(err)
	}
	off, small, int8, two := false, "small", model.PrecisionINT8, 2
	if _, err := r.Update(tctx(), model.DeviceOVGPU, model.ASRDevicePatch{Enabled: &off, Model: &small, Precision: &int8, MaxParallel: &two}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reorder(tctx(), []string{model.DeviceOVNPU, model.DeviceCUDA0, model.DeviceOVGPU, model.DeviceFWCPU, model.DeviceOVCPU}); err != nil {
		t.Fatal(err)
	}
	before := map[string]model.ASRDevice{}
	for _, d := range r.List() {
		before[d.ID] = d
	}
	// Second detection: new driver name of the GPU, the NPU and CUDA gone,
	// a minute later.
	w.set(func(w *world) {
		w.now = w.now.Add(time.Minute)
		w.cuda = ""
		p := intelProbe()
		p.Devices = []OVDevice{
			{Name: "GPU.0", Kind: model.DeviceKindIGPU, FullName: "Intel(R) Arc(TM) 140T GPU (new driver)", Vendor: "0x8086"},
			{Name: "CPU", Kind: model.DeviceKindCPU, FullName: "Intel(R) Core(TM) Ultra 9 285H", Vendor: "0x8086"},
		}
		w.probe = p
	})
	n := hooks.Load()
	d, newIDs, err := r.Detect(tctx())
	if err != nil || len(newIDs) != 0 {
		t.Fatalf("second detect: %v new %v", err, newIDs)
	}
	if d.CUDAName != "" || d.CUDAErr == "" {
		t.Fatalf("cuda: %+v", d)
	}
	if hooks.Load() != n+1 {
		t.Fatal("no OnChange after the detection")
	}
	later := w.stamp()
	gpu, _ := r.Get(model.DeviceOVGPU)
	// The user's enabled, model, precision, max_parallel and priority stay;
	// label and detected_at are refreshed.
	if gpu.Enabled || gpu.Model != "small" || gpu.Precision != model.PrecisionINT8 || gpu.MaxParallel != 2 ||
		gpu.Priority != before[model.DeviceOVGPU].Priority || gpu.Label != "Intel(R) Arc(TM) 140T GPU (new driver)" ||
		gpu.DetectedAt == nil || *gpu.DetectedAt != later {
		t.Fatalf("ov:GPU after the second detection: %+v", gpu)
	}
	// Rows not seen are kept unchanged and marked not present.
	for _, id := range []string{model.DeviceOVNPU, model.DeviceCUDA0} {
		got, ok := r.Get(id)
		if !ok || !reflect.DeepEqual(got, before[id]) {
			t.Fatalf("%s: %+v (ok %v), want unchanged %+v", id, got, ok, before[id])
		}
		if r.Present(id) {
			t.Fatalf("%s still present", id)
		}
	}
	if !r.Present(model.DeviceOVGPU) || !r.Present(model.DeviceOVCPU) {
		t.Fatal("seen devices not present")
	}
	if len(r.List()) != 5 { // four detected devices and the built-in fw:cpu
		t.Fatalf("rows %v", devIDs(r.List()))
	}

	// A failed probe: every OpenVINO row is kept and not present.
	w.set(func(w *world) {
		w.cuda = rtx
		w.probe, w.probeErr = nil, model.Errorf(CodeOVImportFailed, map[string]any{"error": "DLL load failed"}, errors.New("DLL load failed"))
	})
	d, _, err = r.Detect(tctx())
	if err != nil {
		t.Fatal(err)
	}
	if d.OV != nil || d.OVErr == nil || d.OVErr.Code != CodeOVImportFailed || d.OVErr.Params["error"] != "DLL load failed" || d.OVErr.Detail == "" {
		t.Fatalf("OVErr %+v", d.OVErr)
	}
	for _, id := range []string{model.DeviceOVGPU, model.DeviceOVNPU, model.DeviceOVCPU} {
		if r.Present(id) {
			t.Fatalf("%s present after a failed probe", id)
		}
	}
	if !r.Present(model.DeviceCUDA0) {
		t.Fatal("cuda:0 answered again")
	}
}

func TestDetectDuplicateKindAndUnknownKind(t *testing.T) {
	st := regStore(t)
	w := newWorld()
	w.cuda = ""
	w.probe = &OVProbe{Devices: []OVDevice{
		{Name: "GPU.0", Kind: model.DeviceKindIGPU, FullName: "first"},
		{Name: "GPU.2", Kind: model.DeviceKindIGPU, FullName: "second"},
		{Name: "X", Kind: "fpga", FullName: "other"},
		{Name: "NPU", Kind: model.DeviceKindNPU}, // no FULL_DEVICE_NAME: a generic label
	}}
	r, _ := newReg(t, st, w)
	_, newIDs, err := r.Detect(tctx())
	if err != nil || !reflect.DeepEqual(newIDs, []string{model.DeviceOVGPU, model.DeviceOVNPU}) {
		t.Fatalf("new %v err %v", newIDs, err)
	}
	if g, _ := r.Get(model.DeviceOVGPU); g.Label != "first" {
		t.Fatalf("gpu label %q", g.Label)
	}
	if n, _ := r.Get(model.DeviceOVNPU); n.Label != "Intel NPU" {
		t.Fatalf("npu label %q", n.Label)
	}
}

func TestDetectCancelledKeepsState(t *testing.T) {
	st := regStore(t)
	w := newWorld()
	r, _ := newReg(t, st, w)
	if _, _, err := r.Detect(tctx()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(tctx())
	w.set(func(w *world) { w.cuda, w.probe = "", &OVProbe{} })
	cancel()
	if _, _, err := r.Detect(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled detect: %v", err)
	}
	if !r.Present(model.DeviceCUDA0) || !r.Present(model.DeviceOVNPU) {
		t.Fatal("a cut-short detection marked devices absent")
	}
}

func TestDetectOneAtATime(t *testing.T) {
	st := regStore(t)
	w := newWorld()
	var inFlight, maxInFlight atomic.Int32
	o := w.opts()
	inner := o.Probe
	o.Probe = func(ctx context.Context) (*OVProbe, error) {
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		return inner(ctx)
	}
	r := NewRegistry(st, config.Default(), t.TempDir(), o)
	if err := r.Load(tctx()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var inserted atomic.Int32
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, newIDs, err := r.Detect(tctx())
			if err != nil {
				t.Error(err)
			}
			inserted.Add(int32(len(newIDs)))
		}()
	}
	wg.Wait()
	if maxInFlight.Load() != 1 || inserted.Load() != 4 || w.probes.Load() != 4 {
		t.Fatalf("max in flight %d, inserted %d, probes %d", maxInFlight.Load(), inserted.Load(), w.probes.Load())
	}
}

func TestPresenceBeforeFirstDetection(t *testing.T) {
	st := regStore(t)
	seen := "2026-10-01T00:00:00Z"
	for _, d := range []model.ASRDevice{
		{ID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, Label: rtx, Enabled: true, Model: "large-v3", Precision: "auto", MaxParallel: 1, Priority: 300, DetectedAt: &seen},
		{ID: model.DeviceOVNPU, Backend: model.BackendOpenVINO, Label: "NPU", Enabled: true, Model: "large-v3-turbo", Precision: "fp16", MaxParallel: 1, Priority: 100},
	} {
		if err := st.InsertASRDevice(tctx(), d); err != nil {
			t.Fatal(err)
		}
	}
	w := newWorld()
	w.cuda = ""
	w.probe = &OVProbe{Devices: []OVDevice{{Name: "NPU", Kind: model.DeviceKindNPU, FullName: "Intel(R) AI Boost"}}}
	r, _ := newReg(t, st, w)
	if r.Detected() || !r.Present(model.DeviceCUDA0) || r.Present(model.DeviceOVNPU) || !r.Present(model.DeviceFWCPU) {
		t.Fatalf("before detection: detected %v cuda %v npu %v", r.Detected(), r.Present(model.DeviceCUDA0), r.Present(model.DeviceOVNPU))
	}
	if _, _, err := r.Detect(tctx()); err != nil {
		t.Fatal(err)
	}
	if !r.Detected() || r.Present(model.DeviceCUDA0) || !r.Present(model.DeviceOVNPU) {
		t.Fatal("after detection")
	}
	// Load starts over: the stored detected_at counts again until the next
	// detection.
	if err := r.Load(tctx()); err != nil {
		t.Fatal(err)
	}
	if r.Detected() || !r.Present(model.DeviceCUDA0) || !r.Present(model.DeviceOVNPU) || (r.LastDetection() != Detection{}) {
		t.Fatal("after Load")
	}
}

func insertASRJob(t *testing.T, st db.Store, started bool) {
	t.Helper()
	j := model.Job{ID: db.NewID(), Scope: model.ScopeLecture, ScopeID: "L1", Stage: model.StageASR, Status: model.StatusQueued, IsCurrent: true}
	if started {
		at := "2026-10-01T00:00:00Z"
		j.Status, j.StartedAt = model.StatusDone, &at
	}
	if err := st.InsertJob(tctx(), j); err != nil {
		t.Fatal(err)
	}
}

func legacyASR(device string) model.ASRSettings {
	return model.ASRSettings{Model: "large-v2", ComputeType: "int8_float16", Device: device, Language: "en", MaxParallel: 3,
		VRAMReserveMB: 1000, VRAMPerJobMB: 5000}
}

func promptDismissed(t *testing.T, st db.Store, id string) bool {
	t.Helper()
	v, ok, err := st.GetKV(tctx(), model.KVASRDevicePromptDismissedPrefix+id)
	if err != nil {
		t.Fatal(err)
	}
	return ok && v == "1"
}

func TestMigrateLegacyCUDA(t *testing.T) {
	st := regStore(t)
	w := newWorld()
	r, hooks := newReg(t, st, w)
	migrated, err := r.MigrateLegacy(tctx(), legacyASR("cuda"))
	if err != nil || !migrated {
		t.Fatalf("migrate: %v %v", migrated, err)
	}
	now := w.stamp()
	want := []model.ASRDevice{{ID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, Label: rtx, Enabled: true,
		Model: "large-v2", Precision: model.PrecisionInt8Float16, MaxParallel: 3, Priority: 300, DetectedAt: &now}}
	if got := r.List(); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows %+v", got)
	}
	if !promptDismissed(t, st, model.DeviceCUDA0) || len(r.Prompts()) != 0 || hooks.Load() != 1 {
		t.Fatalf("prompt dismissed %v, prompts %v, hooks %d", promptDismissed(t, st, model.DeviceCUDA0), r.Prompts(), hooks.Load())
	}
	if v, ok, _ := st.GetKV(tctx(), model.KVASRDevicesMigrated); !ok || v != "1" {
		t.Fatal("flag not set")
	}
	// Only once: other settings later change nothing.
	again, err := r.MigrateLegacy(tctx(), model.ASRSettings{Model: "small", ComputeType: "float16", Device: "cpu", Language: "en", MaxParallel: 1})
	if err != nil || again || !reflect.DeepEqual(r.List(), want) || hooks.Load() != 1 {
		t.Fatalf("second migration: %v %v %+v", again, err, r.List())
	}
	// The first detection afterwards keeps the migrated configuration and
	// does not list cuda:0 as new.
	_, newIDs, err := r.Detect(tctx())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{model.DeviceOVGPU, model.DeviceOVNPU, model.DeviceOVCPU}; !reflect.DeepEqual(newIDs, want) {
		t.Fatalf("new %v", newIDs)
	}
	if c, _ := r.Get(model.DeviceCUDA0); c.Model != "large-v2" || c.Precision != model.PrecisionInt8Float16 || c.MaxParallel != 3 {
		t.Fatalf("cuda:0 after detection %+v", c)
	}
}

func TestMigrateLegacyCPU(t *testing.T) {
	// device cpu: fw:cpu with the legacy model; an NVIDIA GPU that answers
	// gets a disabled cuda:0 (the user chose the CPU).
	st := regStore(t)
	w := newWorld()
	r, _ := newReg(t, st, w)
	a := legacyASR("cpu")
	a.ComputeType = "auto"
	if m, err := r.MigrateLegacy(tctx(), a); err != nil || !m {
		t.Fatalf("migrate: %v %v", m, err)
	}
	if got := devIDs(r.List()); !reflect.DeepEqual(got, []string{model.DeviceCUDA0, model.DeviceFWCPU}) {
		t.Fatalf("rows %v", got)
	}
	cpu, _ := r.Get(model.DeviceFWCPU)
	if cpu.Backend != model.BackendFasterWhisper || !cpu.Enabled || cpu.Model != "large-v2" || cpu.Precision != model.PrecisionAuto ||
		cpu.MaxParallel != 3 || cpu.Priority != 10 || cpu.Label != LabelFWCPU || cpu.DetectedAt == nil {
		t.Fatalf("fw:cpu %+v", cpu)
	}
	cuda, _ := r.Get(model.DeviceCUDA0)
	if cuda.Enabled || cuda.Label != rtx {
		t.Fatalf("cuda:0 %+v", cuda)
	}
	if !promptDismissed(t, st, model.DeviceFWCPU) || !promptDismissed(t, st, model.DeviceCUDA0) {
		t.Fatal("migrated rows must not prompt")
	}
	// The snapshot of auto mode is fw:cpu (cuda:0 is disabled).
	if s, ok := r.CurrentASRSnapshot(model.ASRSettings{DeviceMode: model.ASRDeviceModeAuto, Language: "en"}); !ok || s.DeviceID != model.DeviceFWCPU {
		t.Fatalf("snapshot %+v %v", s, ok)
	}

	// Without NVIDIA: only fw:cpu.
	st2 := regStore(t)
	w2 := newWorld()
	w2.cuda = ""
	r2, _ := newReg(t, st2, w2)
	if m, err := r2.MigrateLegacy(tctx(), legacyASR("cpu")); err != nil || !m {
		t.Fatalf("migrate: %v %v", m, err)
	}
	if got := devIDs(r2.List()); !reflect.DeepEqual(got, []string{model.DeviceFWCPU}) {
		t.Fatalf("rows %v", got)
	}
}

func TestMigrateLegacyFreshInstallWithoutNVIDIA(t *testing.T) {
	st := regStore(t)
	w := newWorld()
	w.cuda = ""
	insertASRJob(t, st, false) // launched but never ran: not a CUDA user yet
	r, hooks := newReg(t, st, w)
	m, err := r.MigrateLegacy(tctx(), legacyASR("cuda"))
	if err != nil || m || len(r.List()) != 0 || hooks.Load() != 0 {
		t.Fatalf("fresh install: %v %v %v hooks %d", m, err, r.List(), hooks.Load())
	}
	if v, ok, _ := st.GetKV(tctx(), model.KVASRDevicesMigrated); !ok || v != "1" {
		t.Fatal("flag not set")
	}
	// No row: ok false (the settings then write the fallback auto snapshot,
	// settings.FallbackASRSnapshot; 3b Gate round 1, item 2).
	if _, ok := r.CurrentASRSnapshot(model.ASRSettings{DeviceMode: "auto", Language: "en"}); ok {
		t.Fatal("snapshot without rows")
	}
	// An NVIDIA card that appears later is a new device (with a prompt).
	w.set(func(w *world) { w.cuda = rtx })
	_, newIDs, err := r.Detect(tctx())
	if err != nil || newIDs[0] != model.DeviceCUDA0 {
		t.Fatalf("new %v %v", newIDs, err)
	}
	if c, _ := r.Get(model.DeviceCUDA0); c.Model != "large-v3" || c.Precision != model.PrecisionAuto || c.MaxParallel != 1 {
		t.Fatalf("detected cuda:0 %+v", c)
	}
	if p := devIDs(r.Prompts()); !reflect.DeepEqual(p, []string{model.DeviceCUDA0, model.DeviceOVGPU, model.DeviceOVNPU}) {
		t.Fatalf("prompts %v", p)
	}
}

func TestMigrateLegacyExistingUserNvidiaSilent(t *testing.T) {
	// An existing CUDA user whose nvidia-smi does not answer this time:
	// cuda:0 is created anyway (label "NVIDIA GPU", detected_at NULL, not
	// present until a detection sees it).
	st := regStore(t)
	insertASRJob(t, st, true)
	w := newWorld()
	w.cuda = ""
	r, _ := newReg(t, st, w)
	a := legacyASR("cuda")
	a.ComputeType, a.MaxParallel, a.Model = "int8", 0, " " // invalid legacy values
	if m, err := r.MigrateLegacy(tctx(), a); err != nil || !m {
		t.Fatalf("migrate: %v %v", m, err)
	}
	c, ok := r.Get(model.DeviceCUDA0)
	if !ok || c.Label != LabelCUDAUnknown || c.DetectedAt != nil || !c.Enabled || c.Model != "large-v3" ||
		c.Precision != model.PrecisionAuto || c.MaxParallel != 1 {
		t.Fatalf("cuda:0 %+v", c)
	}
	if r.Present(model.DeviceCUDA0) {
		t.Fatal("present before any detection saw it")
	}
	// max_parallel above the limit is clamped.
	st2 := regStore(t)
	r2, _ := newReg(t, st2, newWorld())
	a2 := legacyASR("cuda")
	a2.MaxParallel = 20
	r2.MigrateLegacy(tctx(), a2)
	if c, _ := r2.Get(model.DeviceCUDA0); c.MaxParallel != MaxParallelLimit {
		t.Fatalf("max_parallel %d", c.MaxParallel)
	}
}

// fixture is a registry after a detection of the reference laptop.
func regFixture(t *testing.T) (*Registry, db.Store, *world, *atomic.Int32) {
	t.Helper()
	st := regStore(t)
	w := newWorld()
	r, hooks := newReg(t, st, w)
	if _, _, err := r.Detect(tctx()); err != nil {
		t.Fatal(err)
	}
	return r, st, w, hooks
}

func TestCurrentASRSnapshot(t *testing.T) {
	r, _, w, _ := regFixture(t)
	a := model.ASRSettings{DeviceMode: model.ASRDeviceModeAuto, Language: "de"}
	s, ok := r.CurrentASRSnapshot(a)
	if !ok || !model.EqualASRSnapshot(s, model.ASRSnapshot{DeviceID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, Model: "large-v3",
		Precision: model.PrecisionAuto, Language: "de", Auto: true}) {
		t.Fatalf("auto: %+v %v", s, ok)
	}
	if got := model.MarshalSnapshot(s); got != `{"device_id":"cuda:0","backend":"faster-whisper","model":"large-v3","precision":"auto","language":"de","auto":true}` {
		t.Fatalf("json %s", got)
	}
	// "" is auto as well.
	if s2, _ := r.CurrentASRSnapshot(model.ASRSettings{Language: "de"}); !model.EqualASRSnapshot(s2, s) {
		t.Fatalf("empty mode %+v", s2)
	}
	// Device mode manual (power management, power-brief §3.2): an auto
	// snapshot of the list's highest-priority enabled device that is
	// present (the order of the list carries no meaning).
	if s, ok := r.CurrentASRSnapshot(model.ASRSettings{DeviceMode: model.ASRDeviceModeManual,
		ManualDevices: []string{model.DeviceOVNPU, model.DeviceOVGPU}, Language: "de"}); !ok || s.DeviceID != model.DeviceOVGPU ||
		s.Backend != model.BackendOpenVINO || !s.Auto || s.Language != "de" {
		t.Fatalf("manual [ov:NPU ov:GPU]: %+v %v", s, ok)
	}
	if s, ok := r.CurrentASRSnapshot(model.ASRSettings{DeviceMode: model.ASRDeviceModeManual,
		ManualDevices: []string{model.DeviceOVNPU}, Language: "de"}); !ok || s.DeviceID != model.DeviceOVNPU || !s.Auto {
		t.Fatalf("manual [ov:NPU]: %+v %v", s, ok)
	}
	// A phase-3b single device id is manual [id]: an auto snapshot of that
	// row (it was a fixed snapshot before power management).
	if s, ok := r.CurrentASRSnapshot(model.ASRSettings{DeviceMode: model.DeviceOVNPU, Language: "en"}); !ok || s.DeviceID != model.DeviceOVNPU || !s.Auto || s.Language != "en" {
		t.Fatalf("3b id ov:NPU: %+v %v", s, ok)
	}

	// The highest-priority enabled device that is present: CUDA gone →
	// the Intel GPU.
	w.set(func(w *world) { w.cuda = "" })
	r.Detect(tctx())
	if s, _ := r.CurrentASRSnapshot(a); s.DeviceID != model.DeviceOVGPU || s.Backend != model.BackendOpenVINO || s.Precision != model.PrecisionFP16 || !s.Auto {
		t.Fatalf("auto without CUDA: %+v", s)
	}
	// Nothing present: the highest-priority enabled row.
	w.set(func(w *world) { w.probe = &OVProbe{} })
	r.Detect(tctx())
	if s, _ := r.CurrentASRSnapshot(a); s.DeviceID != model.DeviceCUDA0 || !s.Auto {
		t.Fatalf("auto, nothing present: %+v", s)
	}
	// Manual, nothing of the list present: the list's highest-priority
	// enabled row, auto (power management; before it a device id wrote a
	// fixed snapshot of that row, even disabled or absent).
	if s, ok := r.CurrentASRSnapshot(model.ASRSettings{DeviceMode: model.ASRDeviceModeManual,
		ManualDevices: []string{model.DeviceOVNPU, model.DeviceOVGPU}, Language: "en"}); !ok || s.DeviceID != model.DeviceOVGPU || !s.Auto || s.Language != "en" {
		t.Fatalf("manual, nothing present: %+v %v", s, ok)
	}
	// A CPU device id (3b) reads as auto: a manual list never holds a CPU
	// device; auto with nothing present is the highest-priority enabled row.
	if s, ok := r.CurrentASRSnapshot(model.ASRSettings{DeviceMode: model.DeviceOVCPU, Language: "en"}); !ok || s.DeviceID != model.DeviceCUDA0 || !s.Auto || s.Language != "en" {
		t.Fatalf("3b CPU id: %+v %v", s, ok)
	}
	if _, ok := r.CurrentASRSnapshot(model.ASRSettings{DeviceMode: "ov:XYZ", Language: "en"}); ok {
		t.Fatal("manual [a missing row]")
	}
	// Every device of the list disabled: ok false (the settings' fallback).
	offNPU := false
	r.Update(tctx(), model.DeviceOVNPU, model.ASRDevicePatch{Enabled: &offNPU})
	if _, ok := r.CurrentASRSnapshot(model.ASRSettings{DeviceMode: model.ASRDeviceModeManual,
		ManualDevices: []string{model.DeviceOVNPU}, Language: "en"}); ok {
		t.Fatal("manual [a disabled row]")
	}
	// No enabled row in auto mode: ok false.
	off := false
	for _, d := range r.List() {
		r.Update(tctx(), d.ID, model.ASRDevicePatch{Enabled: &off})
	}
	if _, ok := r.CurrentASRSnapshot(a); ok {
		t.Fatal("auto without enabled rows")
	}
}

func TestUsable(t *testing.T) {
	r, _, w, _ := regFixture(t)
	off := false
	r.Update(tctx(), model.DeviceOVNPU, model.ASRDevicePatch{Enabled: &off})
	w.set(func(w *world) {
		w.probe = &OVProbe{Devices: []OVDevice{{Name: "NPU", Kind: model.DeviceKindNPU, FullName: "Intel(R) AI Boost"}}}
	})
	r.Detect(tctx()) // present: cuda:0, ov:NPU (disabled); ov:GPU absent
	fixed := func(id string) model.ASRSnapshot {
		return model.ASRSnapshot{DeviceID: id, Model: "m", Precision: "p", Language: "en"}
	}
	if ok, info := r.Usable(fixed(model.DeviceCUDA0)); !ok || info != nil {
		t.Fatalf("cuda:0: %v %+v", ok, info)
	}
	for id, label := range map[string]string{
		model.DeviceOVNPU: "Intel(R) AI Boost",                       // disabled
		model.DeviceOVGPU: "Intel(R) Arc(TM) 140T GPU (16GB) (iGPU)", // not present
		model.DeviceFWCPU: LabelFWCPU,                                // the built-in row, disabled (accelerators found)
		"ov:XYZ":          "ov:XYZ",                                  // no row: the id
	} {
		ok, info := r.Usable(fixed(id))
		if ok || info == nil || info.Code != CodeSnapshotDeviceUnavailable || info.Params["device"] != label || info.Detail == "" {
			t.Errorf("%s: %v %+v", id, ok, info)
		}
	}
	// A v1.1 snapshot is normalized: cuda → cuda:0, cpu → fw:cpu.
	if ok, _ := r.Usable(model.ASRSnapshot{Model: "large-v2", ComputeType: "auto", Device: "cuda", Language: "en"}); !ok {
		t.Fatal("v1.1 cuda snapshot")
	}
	if ok, _ := r.Usable(model.ASRSnapshot{Model: "large-v2", ComputeType: "auto", Device: "cpu", Language: "en"}); ok {
		t.Fatal("v1.1 cpu snapshot with fw:cpu disabled")
	}
	// auto: usable while some enabled device is present, whatever its own
	// device.
	auto := fixed(model.DeviceOVGPU)
	auto.Auto = true
	if ok, _ := r.Usable(auto); !ok {
		t.Fatal("auto with cuda:0 present")
	}
	r.Update(tctx(), model.DeviceCUDA0, model.ASRDevicePatch{Enabled: &off})
	if ok, info := r.Usable(auto); ok || info.Code != CodeSnapshotDeviceUnavailable {
		t.Fatalf("auto without an enabled present device: %v %+v", ok, info)
	}
}

func TestUpdateValidation(t *testing.T) {
	r, st, _, hooks := regFixture(t)
	n := hooks.Load()
	s := func(v string) *string { return &v }
	i := func(v int) *int { return &v }
	_, err := r.Update(tctx(), "ov:XYZ", model.ASRDevicePatch{Model: s("small")})
	wantCode(t, "unknown device", err, db.ErrNotFound, CodeNotFound, map[string]any{"device": "ov:XYZ"})
	for _, c := range []struct {
		id     string
		p      model.ASRDevicePatch
		code   string
		params map[string]any
	}{
		{model.DeviceOVGPU, model.ASRDevicePatch{Model: s("  ")}, CodeModelMissing, nil},
		{model.DeviceOVGPU, model.ASRDevicePatch{Model: s("large-v2")}, CodeModelInvalid, map[string]any{"value": "large-v2"}},
		{model.DeviceOVGPU, model.ASRDevicePatch{Precision: s("float16")}, CodePrecisionInvalid, map[string]any{"value": "float16"}},
		{model.DeviceCUDA0, model.ASRDevicePatch{Precision: s("fp16")}, CodePrecisionInvalid, map[string]any{"value": "fp16"}},
		{model.DeviceCUDA0, model.ASRDevicePatch{MaxParallel: i(0)}, CodeMaxParallelInvalid, map[string]any{"min": 1, "max": 8}},
		{model.DeviceCUDA0, model.ASRDevicePatch{MaxParallel: i(9)}, CodeMaxParallelInvalid, map[string]any{"min": 1, "max": 8}},
	} {
		before, _ := r.Get(c.id)
		_, err := r.Update(tctx(), c.id, c.p)
		wantCode(t, c.code, err, ErrInvalid, c.code, c.params)
		if after, _ := r.Get(c.id); !reflect.DeepEqual(after, before) {
			t.Fatalf("%s: a rejected patch changed the row", c.code)
		}
	}
	if hooks.Load() != n {
		t.Fatal("rejected patches called OnChange")
	}
	// faster-whisper accepts any model name, OpenVINO the five choices.
	d, err := r.Update(tctx(), model.DeviceCUDA0, model.ASRDevicePatch{Model: s(" distil-large-v3 "), Precision: s("float16"), MaxParallel: i(2)})
	if err != nil || d.Model != "distil-large-v3" || d.Precision != "float16" || d.MaxParallel != 2 || !d.Enabled {
		t.Fatalf("cuda patch: %+v %v", d, err)
	}
	d, err = r.Update(tctx(), model.DeviceOVNPU, model.ASRDevicePatch{Model: s("medium"), Precision: s("int8")})
	if err != nil || d.Model != "medium" || d.Precision != "int8" || d.Label != "Intel(R) AI Boost" || d.Priority != 100 {
		t.Fatalf("npu patch: %+v %v", d, err)
	}
	if hooks.Load() != n+2 {
		t.Fatalf("hooks %d", hooks.Load()-n)
	}
	// Persisted: a fresh registry reads the same rows.
	r2, _ := newReg(t, st, newWorld())
	if !reflect.DeepEqual(r2.List(), r.List()) {
		t.Fatalf("reloaded %+v", r2.List())
	}
}

func TestReorder(t *testing.T) {
	r, st, _, hooks := regFixture(t)
	for _, bad := range [][]string{
		{model.DeviceOVGPU, model.DeviceOVNPU, model.DeviceOVCPU, model.DeviceFWCPU},                              // one missing
		{model.DeviceOVGPU, model.DeviceOVNPU, model.DeviceOVCPU, model.DeviceFWCPU, model.DeviceFWCPU},           // duplicate
		{model.DeviceOVGPU, model.DeviceOVNPU, model.DeviceOVCPU, model.DeviceFWCPU, "ov:XYZ"},                    // unknown
		{model.DeviceOVGPU, model.DeviceOVNPU, model.DeviceOVCPU, model.DeviceCUDA0, model.DeviceFWCPU, "ov:XYZ"}, // extra
		nil,
	} {
		_, err := r.Reorder(tctx(), bad)
		wantCode(t, "order", err, ErrInvalid, CodeOrderInvalid, nil)
	}
	n := hooks.Load()
	order := []string{model.DeviceOVCPU, model.DeviceOVNPU, model.DeviceCUDA0, model.DeviceFWCPU, model.DeviceOVGPU}
	got, err := r.Reorder(tctx(), order)
	if err != nil || !reflect.DeepEqual(devIDs(got), order) {
		t.Fatalf("reorder: %v %v", devIDs(got), err)
	}
	// The priorities keep the default scale.
	prio := map[string]int{}
	for _, d := range got {
		prio[d.ID] = d.Priority
	}
	if !reflect.DeepEqual(prio, map[string]int{model.DeviceOVCPU: 300, model.DeviceOVNPU: 200, model.DeviceCUDA0: 100, model.DeviceFWCPU: 10, model.DeviceOVGPU: 0}) {
		t.Fatalf("priorities %v", prio)
	}
	if hooks.Load() != n+1 || !reflect.DeepEqual(devIDs(r.List()), order) {
		t.Fatal("cache or hooks")
	}
	if rows, _ := st.ListASRDevices(tctx()); !reflect.DeepEqual(devIDs(rows), order) {
		t.Fatalf("stored order %v", devIDs(rows))
	}
	// Equal priorities are made strictly decreasing.
	for _, d := range r.List() {
		d.Priority = 5
		st.UpdateASRDevice(tctx(), d)
	}
	r.Load(tctx())
	order = []string{model.DeviceOVGPU, model.DeviceCUDA0, model.DeviceOVCPU, model.DeviceFWCPU, model.DeviceOVNPU}
	got, err = r.Reorder(tctx(), order)
	if err != nil || !reflect.DeepEqual(devIDs(got), order) || got[0].Priority != 5 || got[4].Priority != 1 {
		t.Fatalf("equal priorities: %+v %v", got, err)
	}
}

func okBenchResult(mdl, precision string, rtf, wer float64, peak int) model.BenchmarkResult {
	return model.BenchmarkResult{Model: mdl, Precision: precision, OK: true, RTF: rtf, WER: &wer, PeakMemMB: peak}
}

func TestBenchmarkApplyAndPrompts(t *testing.T) {
	r, st, w, hooks := regFixture(t)
	// New devices (not migrated), present and enabled; ov:CPU is disabled.
	if p := devIDs(r.Prompts()); !reflect.DeepEqual(p, []string{model.DeviceCUDA0, model.DeviceOVGPU, model.DeviceOVNPU}) {
		t.Fatalf("prompts %v", p)
	}
	n := hooks.Load()
	if err := r.DismissPrompt(tctx(), model.DeviceCUDA0); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "dismiss unknown", r.DismissPrompt(tctx(), "ov:XYZ"), db.ErrNotFound, CodeNotFound, map[string]any{"device": "ov:XYZ"})
	if p := devIDs(r.Prompts()); !reflect.DeepEqual(p, []string{model.DeviceOVGPU, model.DeviceOVNPU}) || !promptDismissed(t, st, model.DeviceCUDA0) {
		t.Fatalf("prompts after dismiss %v", p)
	}
	// A device not present has no prompt.
	w.set(func(w *world) { w.probe = &OVProbe{Devices: intelProbe().Devices[:1]} })
	r.Detect(tctx())
	if p := devIDs(r.Prompts()); !reflect.DeepEqual(p, []string{model.DeviceOVGPU}) {
		t.Fatalf("prompts with the NPU absent %v", p)
	}

	b := model.DeviceBenchmark{Status: model.BenchmarkDone, StartedAt: "2026-10-24T01:00:00Z", FinishedAt: "2026-10-24T01:05:00Z",
		Audio: model.BenchmarkAudio{Source: model.BenchmarkSourceLec, LectureID: "L1", StartMS: 1000, EndMS: 121000, ReferenceWords: 300},
		Results: []model.BenchmarkResult{
			okBenchResult("large-v3-turbo", "fp16", 0.04, 0.05, 2700),
			{Model: "large-v3-turbo", Precision: "int8", OK: false, Error: &model.ErrorInfo{Code: "asr_ov.worker_failed"}},
		},
		Pick: &model.BenchmarkPick{Model: "large-v3-turbo", Precision: "fp16", Rule: model.PickRuleBest}}
	if err := r.SetBenchmark(tctx(), model.DeviceOVGPU, b); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "benchmark unknown", r.SetBenchmark(tctx(), "ov:XYZ", b), db.ErrNotFound, CodeNotFound, nil)
	if len(r.Prompts()) != 0 {
		t.Fatalf("a tested device still prompts: %v", devIDs(r.Prompts()))
	}
	g, _ := r.Get(model.DeviceOVGPU)
	stored, ok := ParseBenchmark(g.LastBenchmark)
	if b.Version = model.BenchmarkVersion; !ok || !reflect.DeepEqual(stored, b) {
		t.Fatalf("stored benchmark %+v", stored)
	}
	// Results are never null in the JSON.
	r.SetBenchmark(tctx(), model.DeviceOVNPU, model.DeviceBenchmark{Status: model.BenchmarkFailed,
		Error: &model.ErrorInfo{Code: "selftest.no_audio"}})
	if npu, _ := r.Get(model.DeviceOVNPU); npu.LastBenchmark == nil || !json.Valid([]byte(*npu.LastBenchmark)) ||
		!strings.Contains(*npu.LastBenchmark, `"results":[]`) || !strings.Contains(*npu.LastBenchmark, `"version":1`) {
		t.Fatalf("npu benchmark %v", npu.LastBenchmark)
	}

	// 应用: only an OK result of the last test.
	_, err := r.ApplyBenchmark(tctx(), model.DeviceOVGPU, "large-v3-turbo", "int8")
	wantCode(t, "apply failed result", err, ErrInvalid, CodeApplyNotTested, map[string]any{"model": "large-v3-turbo", "precision": "int8"})
	_, err = r.ApplyBenchmark(tctx(), model.DeviceOVGPU, "small", "int8")
	wantCode(t, "apply untested", err, ErrInvalid, CodeApplyNotTested, nil)
	_, err = r.ApplyBenchmark(tctx(), model.DeviceOVCPU, "large-v3-turbo", "fp16")
	wantCode(t, "apply without a benchmark", err, ErrInvalid, CodeApplyNotTested, nil)
	_, err = r.ApplyBenchmark(tctx(), "ov:XYZ", "large-v3-turbo", "fp16")
	wantCode(t, "apply unknown", err, db.ErrNotFound, CodeNotFound, nil)

	off, small, int8 := false, "small", "int8"
	r.Update(tctx(), model.DeviceOVGPU, model.ASRDevicePatch{Model: &small, Precision: &int8, Enabled: &off})
	w.set(func(w *world) { w.now = w.now.Add(time.Hour) })
	d, err := r.ApplyBenchmark(tctx(), model.DeviceOVGPU, "large-v3-turbo", "fp16")
	if err != nil || d.Model != "large-v3-turbo" || d.Precision != "fp16" || d.Enabled {
		t.Fatalf("apply: %+v %v", d, err)
	}
	applied, _ := ParseBenchmark(d.LastBenchmark)
	if applied.AppliedAt == nil || *applied.AppliedAt != w.stamp() || len(applied.Results) != 2 {
		t.Fatalf("applied benchmark %+v", applied)
	}
	if hooks.Load() <= n {
		t.Fatal("no hooks")
	}
}

func TestDefaultsInTestMode(t *testing.T) {
	// In test mode (fake_result set) the default CUDAName and Probe start no
	// process: a fake NVIDIA name; Intel devices only with
	// fake_result_openvino.
	st := regStore(t)
	cfg := config.Default()
	cfg.ASR.FakeResult = "testdata/fake_asr.json"
	cfg.NvidiaSMI = filepath.Join(t.TempDir(), "no-such-nvidia-smi.exe")
	cfg.PythonOpenVINO = filepath.Join(t.TempDir(), "no-such-python.exe")
	r := NewRegistry(st, cfg, t.TempDir(), RegistryOptions{})
	r.Load(tctx())
	d, newIDs, err := r.Detect(tctx())
	if err != nil || d.CUDAName != FakeCUDAName || d.OVErr != nil || d.OV == nil || len(d.OV.Devices) != 0 ||
		!reflect.DeepEqual(newIDs, []string{model.DeviceCUDA0}) {
		t.Fatalf("test mode: %+v %v %v", d, newIDs, err)
	}
	cfg.ASR.FakeResultOpenVINO = "testdata/fake_asr_openvino.json"
	_, newIDs, err = r.Detect(tctx())
	if err != nil || !reflect.DeepEqual(newIDs, []string{model.DeviceOVGPU, model.DeviceOVNPU}) {
		t.Fatalf("test mode with openvino: %v %v", newIDs, err)
	}
	if g, _ := r.Get(model.DeviceOVGPU); g.Label != FakeIntelGPUName {
		t.Fatalf("label %q", g.Label)
	}
}

// 3b Gate round 1, item 1: fw:cpu always has a row. Without NVIDIA and
// without OpenVINO it is created enabled (the machine can still
// transcribe); with an accelerator disabled; a row that exists is never
// changed by a detection.
func TestDetectEnsuresFWCPU(t *testing.T) {
	st := regStore(t)
	w := newWorld()
	w.cuda = ""
	w.probe, w.probeErr = nil, model.Errorf(CodeOVPythonMissing, map[string]any{"path": "x"}, errors.New("missing"))
	r, _ := newReg(t, st, w)
	if m, err := r.MigrateLegacy(tctx(), legacyASR("cuda")); err != nil || m {
		t.Fatalf("fresh install migration: %v %v", m, err)
	}
	_, newIDs, err := r.Detect(tctx())
	if err != nil || len(newIDs) != 0 {
		t.Fatalf("detect: %v new %v", err, newIDs)
	}
	cpu, ok := r.Get(model.DeviceFWCPU)
	if !ok || !cpu.Enabled || cpu.Model != "small" || cpu.Precision != model.PrecisionAuto || cpu.Priority != 10 ||
		cpu.Label != LabelFWCPU || cpu.DetectedAt == nil || !r.Present(model.DeviceFWCPU) {
		t.Fatalf("fw:cpu %+v (ok %v)", cpu, ok)
	}
	if !promptDismissed(t, st, model.DeviceFWCPU) || len(r.Prompts()) != 0 {
		t.Fatal("fw:cpu must not prompt")
	}
	if s, ok := r.CurrentASRSnapshot(model.ASRSettings{DeviceMode: model.ASRDeviceModeAuto, Language: "en"}); !ok || s.DeviceID != model.DeviceFWCPU || !s.Auto {
		t.Fatalf("snapshot %+v %v", s, ok)
	}
	// An NVIDIA card later: fw:cpu keeps the user's (here: the default) state.
	w.set(func(w *world) { w.cuda = rtx })
	if _, _, err := r.Detect(tctx()); err != nil {
		t.Fatal(err)
	}
	if c, _ := r.Get(model.DeviceFWCPU); !c.Enabled {
		t.Fatal("a detection changed fw:cpu's enabled")
	}

	// With NVIDIA from the start: disabled by default, the user can enable it.
	st2 := regStore(t)
	r2, _ := newReg(t, st2, func() *world { w := newWorld(); w.probe = &OVProbe{}; return w }())
	if _, _, err := r2.Detect(tctx()); err != nil {
		t.Fatal(err)
	}
	if c, ok := r2.Get(model.DeviceFWCPU); !ok || c.Enabled {
		t.Fatalf("fw:cpu with NVIDIA present: %+v (ok %v)", c, ok)
	}
	on := true
	if c, err := r2.Update(tctx(), model.DeviceFWCPU, model.ASRDevicePatch{Enabled: &on}); err != nil || !c.Enabled {
		t.Fatalf("enable fw:cpu: %+v %v", c, err)
	}
	if _, _, err := r2.Detect(tctx()); err != nil {
		t.Fatal(err)
	}
	if c, _ := r2.Get(model.DeviceFWCPU); !c.Enabled {
		t.Fatal("a detection overwrote the user's enabled")
	}

	// Only ov:CPU (an OpenVINO CPU, no Intel GPU / NPU) is not an
	// accelerator: fw:cpu is enabled.
	st3 := regStore(t)
	w3 := newWorld()
	w3.cuda = ""
	w3.probe = &OVProbe{Devices: []OVDevice{{Name: "CPU", Kind: model.DeviceKindCPU, FullName: "AMD Ryzen"}}}
	r3, _ := newReg(t, st3, w3)
	if _, _, err := r3.Detect(tctx()); err != nil {
		t.Fatal(err)
	}
	if c, _ := r3.Get(model.DeviceFWCPU); !c.Enabled {
		t.Fatal("fw:cpu disabled although only a CPU was found")
	}
}

// 3b Gate round 2, new problem 2: an existing CUDA user whose nvidia-smi
// is silent and whose probe times out at the first detection after the
// upgrade gets fw:cpu disabled — the cuda:0 row MigrateLegacy made counts
// as an accelerator although it is not present.
func TestFWCPUDisabledWhenAnAcceleratorRowExists(t *testing.T) {
	st := regStore(t)
	insertASRJob(t, st, true) // ran before: an existing CUDA user
	w := newWorld()
	w.cuda = ""
	w.probe, w.probeErr = nil, model.Errorf(CodeProbeFailed, map[string]any{"error": "timeout"}, errors.New("probe timed out after 60s"))
	r, _ := newReg(t, st, w)
	if m, err := r.MigrateLegacy(tctx(), legacyASR("cuda")); err != nil || !m {
		t.Fatalf("migrate: %v %v", m, err)
	}
	if c, ok := r.Get(model.DeviceCUDA0); !ok || c.Label != LabelCUDAUnknown || r.Present(model.DeviceCUDA0) {
		t.Fatalf("cuda:0 %+v (ok %v)", c, ok)
	}
	if _, _, err := r.Detect(tctx()); err != nil {
		t.Fatal(err)
	}
	cpu, ok := r.Get(model.DeviceFWCPU)
	if !ok || cpu.Enabled {
		t.Fatalf("fw:cpu with a cuda:0 row (not present): %+v (ok %v)", cpu, ok)
	}

	// An Intel row from an earlier detection counts the same way.
	st2 := regStore(t)
	npuSeen := "2026-10-01T00:00:00Z"
	if err := st2.InsertASRDevice(tctx(), model.ASRDevice{ID: model.DeviceOVNPU, Backend: model.BackendOpenVINO, Label: "NPU",
		Enabled: true, Model: "large-v3-turbo", Precision: "fp16", MaxParallel: 1, Priority: 100, DetectedAt: &npuSeen}); err != nil {
		t.Fatal(err)
	}
	w2 := newWorld()
	w2.cuda, w2.probe = "", &OVProbe{}
	r2, _ := newReg(t, st2, w2)
	if _, _, err := r2.Detect(tctx()); err != nil {
		t.Fatal(err)
	}
	if c, _ := r2.Get(model.DeviceFWCPU); c.Enabled {
		t.Fatal("fw:cpu enabled although an NPU row exists")
	}
}
