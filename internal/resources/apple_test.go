// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// mac-opti M6: the macOS readings (apple.go).

func init() {
	// The tests of this package fake the readers of the upper area and
	// expect Windows' marks (none); on a Mac the real platformReadings
	// would add the unsupported temperature and the unified memory. The
	// macOS marks are tested in system_darwin_test.go and with s.platform.
	platformMarks = func() ([]string, bool) { return nil, false }
}

// ioregM6 is `ioreg -r -d 1 -w 0 -c IOAccelerator` of the Apple M6
// (shortened: the header, two properties, the statistics, a piece of the
// IOReport legend that names the same counters).
const ioregM6 = `+-o AGXAcceleratorG18G  <class AGXAcceleratorG18G, id 0x1000005a5, registered, matched, active, busy 0 (110 ms), retain 42>
    {
      "IOMatchedAtBoot" = Yes
      "PerformanceStatistics" = {"In use system memory (driver)"=0,"Alloc system memory"=477872128,"Tiler Utilization %"=2,"recoveryCount"=0,"lastRecoveryTime"=0,"Renderer Utilization %"=3,"TiledSceneBytes"=524288,"Device Utilization %"=37,"SplitSceneCount"=0,"Allocated PB Size"=25165824,"In use system memory"=1677721600}
      "model" = "Apple M6"
      "IOReportLegend" = ({"IOReportChannels"=((1,6442450945,"Alloc system memory"),(2,6442450945,"In use system memory"),(4,6442450945,"In use system memory (driver)"))})
    }
`

func TestParseIOAccelerator(t *testing.T) {
	util, mem := ParseIOAccelerator(ioregM6)
	if intOrNil(util) != 37 || intOrNil(mem) != 1600 {
		t.Fatalf("util %v mem %v", intOrNil(util), intOrNil(mem))
	}
	if util, mem := ParseIOAccelerator("+-o IOService\n"); util != nil || mem != nil {
		t.Fatalf("nothing to read: %v %v", intOrNil(util), intOrNil(mem))
	}
	if util, _ := ParseIOAccelerator(`"Device Utilization %"=104`); intOrNil(util) != 100 {
		t.Fatalf("capped: %v", intOrNil(util))
	}
}

// appleFixture is an Apple Silicon Mac with mlx:gpu: no NVIDIA (nvidia-smi
// fails), ioreg answers with out.
func appleFixture(t *testing.T, ioreg func(n int) (string, error)) (*devFixture, *fakeClock, Deps, *int) {
	t.Helper()
	f := newDevFixture(t)
	at := "2026-10-26T08:00:00Z"
	f.rows = []model.ASRDevice{{ID: model.DeviceMLXGPU, Backend: model.BackendMLX, Label: "Apple M6", Enabled: true,
		Model: "large-v3-turbo", Precision: model.PrecisionFP16, MaxParallel: 1, Priority: 250, DetectedAt: &at}}
	f.stats = []sched.DeviceStatus{}
	f.det = devices.Detection{At: time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC), MLX: "Apple M6"}
	clk := newClock()
	d := f.deps(clk)
	calls := new(int)
	d.RunCmd = func(ctx context.Context, name string, args ...string) (string, error) {
		if name != ioregExe {
			return "", errors.New("exec: \"nvidia-smi\": executable file not found in $PATH")
		}
		if !slices.Equal(args, IORegArgs) {
			t.Errorf("ioreg %v", args)
		}
		n := *calls
		*calls++
		return ioreg(n)
	}
	d.SystemStats = func() System {
		return System{CPUPct: iptr(12), MemUsedMB: iptr(5819), MemTotalMB: iptr(16384), Unsupported: []string{ReadingCPUTemp}, UnifiedMemory: true}
	}
	return f, clk, d, calls
}

// The Apple GPU card: utilization and its history from ioreg, the GPU's
// share of the unified memory against the physical memory; the upper area
// marks the memory unified and the temperature unsupported.
func TestAppleGPUCard(t *testing.T) {
	_, clk, d, calls := appleFixture(t, func(n int) (string, error) {
		return strings.Replace(ioregM6, `"Device Utilization %"=37`, `"Device Utilization %"=`+[]string{"20", "60"}[min(n, 1)], 1), nil
	})
	s := New(d)
	sampleNow(s)
	clk.Advance(2 * time.Second)
	sampleNow(s)
	snap := s.build(context.Background())
	c := cardOf(t, snap, model.DeviceMLXGPU)
	if intOrNil(c.UtilPct) != 60 || !slices.Equal(c.UtilHistory, []int{20, 60}) || intOrNil(c.MemUsedMB) != 1600 || intOrNil(c.MemTotalMB) != 16384 || c.Kind != model.DeviceKindApple {
		t.Fatalf("card %+v (mem %v / %v)", c, intOrNil(c.MemUsedMB), intOrNil(c.MemTotalMB))
	}
	if *calls != 2 {
		t.Fatalf("ioreg ran %d times", *calls)
	}
	b, err := json.Marshal(snap.System)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"unsupported":["cpu_temp"]`) || !strings.Contains(string(b), `"unified_memory":true`) {
		t.Fatalf("system %s", b)
	}
}

// ioreg fails: the card has no readings ("--"), the rest is unchanged; no
// MLX detected: ioreg never runs.
func TestAppleGPUCardUnreadable(t *testing.T) {
	f, _, d, calls := appleFixture(t, func(int) (string, error) {
		return "", errors.New("exec: \"ioreg\": executable file not found in $PATH")
	})
	s := New(d)
	sampleNow(s)
	c := cardOf(t, s.build(context.Background()), model.DeviceMLXGPU)
	if c.UtilPct != nil || len(c.UtilHistory) != 0 || c.MemUsedMB != nil || c.MemTotalMB != nil || !c.Present {
		t.Fatalf("card %+v", c)
	}
	f.mu.Lock()
	f.det.MLX = ""
	f.mu.Unlock()
	before := *calls
	sampleNow(s)
	if *calls != before {
		t.Fatal("ioreg ran without the Apple GPU")
	}
	if c := cardOf(t, s.build(context.Background()), model.DeviceMLXGPU); c.Present || c.UtilPct != nil {
		t.Fatalf("card without the Apple GPU %+v", c)
	}
}

// The readers' platform marks pass through readSystem (the real readers,
// not SystemStats); Windows marks nothing, so its JSON keeps its bytes.
func TestSystemPlatformMarks(t *testing.T) {
	f := newDevFixture(t)
	d := f.deps(newClock())
	d.SystemStats = nil
	s := New(d)
	s.newEngines = (&fakeEngines{reads: [][]EngineSample{nil}}).open
	s.newSystem = func() (systemReader, error) { return &fakeSystem{reads: []sysReading{{}}}, nil }
	s.memStatus = func() (uint64, uint64, error) { return 16 << 30, 10 << 30, nil }
	s.platform = func() ([]string, bool) { return []string{ReadingCPUTemp}, true }
	sampleNow(s)
	if v := s.build(context.Background()).System; !slices.Equal(v.Unsupported, []string{ReadingCPUTemp}) || !v.UnifiedMemory {
		t.Fatalf("mac marks %+v", v)
	}
	s.platform = func() ([]string, bool) { return nil, false }
	sampleNow(s)
	b, _ := json.Marshal(s.build(context.Background()).System)
	if strings.Contains(string(b), "unsupported") || strings.Contains(string(b), "unified_memory") {
		t.Fatalf("windows system json %s", b)
	}
}
