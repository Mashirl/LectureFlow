// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sysprobe

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"lectureflow/internal/resources"
)

// The agent's nvidia-smi parser and the resource bar's
// (internal/resources) read the same output the same way.
func TestParseQueryMatchesResources(t *testing.T) {
	if !slices.Equal(QueryArgs, resources.QueryArgs) {
		t.Fatalf("query arguments differ: %v vs %v", QueryArgs, resources.QueryArgs)
	}
	for _, out := range []string{
		"NVIDIA GeForce RTX 4090, 37, 2048, 24564, 51, 61.25, 450.00\n",
		"Tesla T4, 0, 0, 15360, 39, 9.82, 70.00\nTesla T4, 1, 1, 15360, 40, 10, 70\n",
		"NVIDIA A100-SXM4-40GB, [N/A], 100, 40960, [Not Supported], [N/A], N/A\n",
		"\n  NVIDIA L4, 12.0, 3000, 23034, 60, 30.5, 72.0\n",
		"garbage\n", "", "a, b, c, d, e, f, g\n", "x, 1, 2, 3, 4, 5\n",
	} {
		a, errA := ParseQuery(out)
		b, errB := resources.ParseQuery(out)
		if (errA == nil) != (errB == nil) {
			t.Errorf("%q: errors differ: %v / %v", out, errA, errB)
			continue
		}
		if errA != nil {
			continue
		}
		want := GPUSample{Name: b.Name, UtilPct: b.UtilPct, MemUsedMB: b.MemUsedMB, MemTotalMB: b.MemTotalMB,
			TempC: b.TempC, PowerW: b.PowerW, PowerLimitW: b.PowerLimitW}
		if !reflect.DeepEqual(a, want) {
			t.Errorf("%q: %+v vs %+v", out, a, want)
		}
	}
}

func TestParseMachine(t *testing.T) {
	m, err := ParseMachine("NVIDIA GeForce RTX 4090, 24564, 550.54.14, 8.9, 2\n")
	if err != nil || m.Name != "NVIDIA GeForce RTX 4090" || *m.MemTotalMB != 24564 || m.Driver != "550.54.14" ||
		m.ComputeCap != "8.9" || *m.Count != 2 {
		t.Fatalf("%+v %v", m, err)
	}
	if _, err := ParseMachine("x, y\n"); err == nil {
		t.Fatal("bad output accepted")
	}
}

func TestDarwinParsers(t *testing.T) {
	vm := `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                               12345.
Pages active:                            300000.
Pages inactive:                          200000.
Pages speculative:                         5000.
Pages purgeable:                           1000.
`
	mb, ok := parseVMStat(vm)
	if want := int((int64(12345+200000+5000+1000) * 16384) >> 20); !ok || mb != want {
		t.Fatalf("vm_stat %d %v, want %d", mb, ok, want)
	}
	if _, ok := parseVMStat("nothing"); ok {
		t.Fatal("bad vm_stat accepted")
	}
	sp := `{"SPDisplaysDataType":[{"sppci_model":"Apple M6","spdisplays_vendor":"sppci_vendor_Apple","sppci_cores":"10"}]}`
	if name, ok := parseAppleGPU(sp); !ok || name != "Apple M6" {
		t.Fatalf("system_profiler %q %v", name, ok)
	}
	io := `+-o AGXAcceleratorG16X  <class AGXAcceleratorG16X, id 0x100000a3b>
    {
      "PerformanceStatistics" = {"In use system memory"=123,"Device Utilization %"=37,"Renderer Utilization %"=35}
    }`
	if u, ok := parseIORegUtil(io); !ok || u != 37 {
		t.Fatalf("ioreg %d %v", u, ok)
	}
	if pct, ok := parsePSCPU(" 0.0\n 50.5\n 49,5\n", 4); !ok || pct != 25 {
		t.Fatalf("ps %d %v", pct, ok)
	}
	tab := parsePSTable("  1  0  100\n 50  1  2048\n 51 50  1024\n 52  1  4096\n")
	if len(tab) != 4 || tab[51].ppid != 50 {
		t.Fatalf("ps table %+v", tab)
	}
}

func TestLinuxParsers(t *testing.T) {
	total, avail, ok := parseMeminfo("MemTotal:       65792000 kB\nMemFree:  100 kB\nMemAvailable:   32896000 kB\n")
	if !ok || total != 64250 || avail != 32125 {
		t.Fatalf("meminfo %d %d %v", total, avail, ok)
	}
	a, _ := parseProcStat("cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 1 2 3 4\n")
	b, _ := parseProcStat("cpu  200 0 200 1600 0 0 0 0 0 0\n")
	if pct := cpuPercent(a, b); pct != 20 {
		t.Fatalf("cpu %d", pct)
	}
	if ppid, rss, ok := parseProcStatus("Name:\tpython\nPPid:\t42\nVmRSS:\t  2048 kB\n"); !ok || ppid != 42 || rss != 2048 {
		t.Fatalf("status %d %d", ppid, rss)
	}
}

func TestSamplerRuns(t *testing.T) {
	s := NewSampler()
	s.smiOnce.Do(func() {}) // no nvidia-smi in the tests (the GPU is not touched)
	m := s.Machine(context.Background())
	if m.VCPUs <= 0 {
		t.Fatalf("machine %+v", m)
	}
	u := s.Sample(context.Background(), nil, t.TempDir())
	if u.WhisperMB != 0 || u.DataDiskMB != 0 {
		t.Fatalf("usage %+v", u)
	}
}
