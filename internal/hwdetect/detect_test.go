// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package hwdetect

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// thisMachinePS is the real psScript output of the development machine
// (RTX 5070 Ti Laptop, Intel iGPU, Intel NPU, battery), with device
// instance ids, duplicate input devices and the battery model removed.
func thisMachinePS(t *testing.T) []byte { return readTestdata(t, "windows/this_machine.ps.json") }

// thisMachineSMI is the real nvidia-smi output of the same machine (CRLF).
func thisMachineSMI(t *testing.T) []byte { return readTestdata(t, "windows/this_machine.smi.txt") }

// psNoRegistryVRAM is this machine's output without the registry memory sizes.
func psNoRegistryVRAM(t *testing.T) []byte {
	s := string(thisMachinePS(t))
	return []byte(strings.Replace(s, `"HardwareInformation.qwMemorySize": 12820938752`, `"HardwareInformation.qwMemorySize": null`, 1))
}

// An Intel-only laptop as PowerShell 5.1 prints one-element arrays (single
// objects), with a BOM and a leading warning line; the NPU shows up under
// its "AI Boost" name in another PnP class, next to a "USB Input Device"
// that a substring match on "NPU" would wrongly accept.
func psIntelOnly() []byte {
	body := `WARNING: something harmless
{"video":{"Name":"Intel(R) Iris(R) Xe Graphics","AdapterCompatibility":"Intel Corporation","AdapterRAM":1073741824,"PNPDeviceID":"PCI\\VEN_8086&DEV_9A49","DriverVersion":"31.0.101.4502"},` +
		`"vram":{"DriverDesc":"Intel(R) Iris(R) Xe Graphics","HardwareInformation.qwMemorySize":[0,0,0,64,0,0,0,0]},` +
		`"pnp":[{"Name":"USB Input Device","PNPClass":"HIDClass","Manufacturer":"(Standard system devices)"},{"Name":"Intel(R) AI Boost","PNPClass":"System","Manufacturer":"Intel Corporation"}],` +
		`"cpu":{"Name":"Intel(R) Core(TM) Ultra 7 155H   "},"cs":{"TotalPhysicalMemory":"34359738368"},"battery":{"Name":"Battery"}}`
	return append([]byte{0xEF, 0xBB, 0xBF}, body...)
}

// A desktop with an NVIDIA card and an AMD NPU-like accelerator (not
// Intel), no battery, and an empty battery list.
func psDesktopAMDAccel() []byte {
	return []byte(`{"video":[{"Name":"NVIDIA GeForce RTX 3060","AdapterCompatibility":"NVIDIA","AdapterRAM":4293918720,"PNPDeviceID":"PCI\\VEN_10DE&DEV_2503","DriverVersion":"31.0.15.5222"}],` +
		`"vram":[{"DriverDesc":"NVIDIA GeForce RTX 3060","HardwareInformation.qwMemorySize":12884901888}],` +
		`"pnp":[{"Name":"AMD IPU Device","PNPClass":"ComputeAccelerator","Manufacturer":"Advanced Micro Devices"}],` +
		`"cpu":[{"Name":"AMD Ryzen 7 5800X 8-Core Processor"}],"cs":[{"TotalPhysicalMemory":34359738368}],"battery":[]}`)
}

const smiMulti = "NVIDIA RTX A2000 8GB Laptop GPU, 8192, 552.22\r\n" +
	"Tesla T4, [N/A], [N/A]\r\n" +
	"NVIDIA-SMI has failed, because of something, sorry\r\n" +
	"\r\n"

type detectCase struct {
	Name string `json:"name"`
	HW   HW     `json:"hw"`
	// Calls are the commands run (sorted for the concurrent Windows probes).
	Calls []string `json:"calls"`
}

func TestDetectGolden(t *testing.T) {
	ok := func(b []byte) fakeCmd { return fakeCmd{out: b} }
	fail := fakeCmd{err: errors.New("exit status 1")}
	darwin := func(brand, mem, batt string, arm64 bool) map[string]fakeCmd {
		m := map[string]fakeCmd{
			"sysctl -n machdep.cpu.brand_string": ok(readTestdata(t, "darwin/"+brand)),
			"sysctl -n hw.memsize":               ok(readTestdata(t, "darwin/"+mem)),
			"pmset -g batt":                      ok(readTestdata(t, "darwin/"+batt)),
		}
		if arm64 {
			m["sysctl -n hw.optional.arm64"] = ok(readTestdata(t, "darwin/arm64.txt"))
		} else {
			// Intel Macs: "sysctl: unknown oid 'hw.optional.arm64'", exit 1.
			m["sysctl -n hw.optional.arm64"] = fail
		}
		return m
	}
	cases := []struct {
		name string
		os   string
		cmds map[string]fakeCmd
		ram  int // TotalRAMMB fallback; 0 = none
	}{
		{"windows_this_machine", "windows", map[string]fakeCmd{smiKey: ok(thisMachineSMI(t)), "powershell.exe": ok(thisMachinePS(t))}, 0},
		{"windows_no_nvidia_smi", "windows", map[string]fakeCmd{"powershell.exe": ok(thisMachinePS(t))}, 0},
		{"windows_nvidia_smi_fails_vram_unknown", "windows", map[string]fakeCmd{smiKey: fail, "powershell.exe": ok(psNoRegistryVRAM(t))}, 0},
		{"windows_powershell_fails", "windows", map[string]fakeCmd{smiKey: ok(thisMachineSMI(t)), "powershell.exe": fail}, 16384},
		{"windows_powershell_garbage", "windows", map[string]fakeCmd{smiKey: ok(thisMachineSMI(t)), "powershell.exe": ok([]byte("Get-CimInstance : Access denied"))}, 8000},
		{"windows_intel_only", "windows", map[string]fakeCmd{"powershell.exe": ok(psIntelOnly())}, 0},
		{"windows_desktop_amd_accelerator", "windows", map[string]fakeCmd{smiKey: ok([]byte("NVIDIA GeForce RTX 3060, 12288, 552.22\n")), "powershell.exe": ok(psDesktopAMDAccel())}, 0},
		{"windows_multi_gpu_smi", "windows", map[string]fakeCmd{smiKey: ok([]byte(smiMulti)), "powershell.exe": ok(psDesktopAMDAccel())}, 0},
		{"windows_everything_fails", "windows", map[string]fakeCmd{}, 0},
		{"darwin_m2pro_laptop", "darwin", darwin("m2pro.brand.txt", "m2pro.memsize.txt", "pmset.laptop.txt", true), 0},
		{"darwin_m2pro_desktop", "darwin", darwin("m2pro.brand.txt", "m2pro.memsize.txt", "pmset.desktop.txt", true), 0},
		{"darwin_intel_laptop", "darwin", darwin("intel.brand.txt", "intel.memsize.txt", "pmset.laptop.txt", false), 0},
		{"darwin_everything_fails", "darwin", map[string]fakeCmd{}, 0},
		{"linux", "linux", map[string]fakeCmd{}, 0},
	}
	dataDir := filepath.Join(t.TempDir(), "LectureFlow", "not-created-yet")
	var got []detectCase
	for _, c := range cases {
		f := &fakeRunner{cmds: c.cmds}
		var diskDir string
		o := Options{OS: c.os, Arch: "amd64", Run: f.run,
			DiskFreeMB: func(dir string) (int, error) { diskDir = dir; return 51200, nil }}
		if c.os == "darwin" {
			o.Arch = "arm64"
		}
		if c.ram > 0 {
			ram := c.ram
			o.TotalRAMMB = func() (int, error) { return ram, nil }
		}
		hw := DetectWith(context.Background(), dataDir, o)
		if diskDir == "" || !strings.HasPrefix(dataDir, diskDir) {
			t.Errorf("%s: disk free asked for %q, want an existing ancestor of %q", c.name, diskDir, dataDir)
		}
		calls := append([]string(nil), f.calls...)
		sort.Strings(calls)
		got = append(got, detectCase{Name: c.name, HW: hw, Calls: calls})
	}
	checkGolden(t, "detect.golden.json", got)

	// The real machine's expectations, independent of the golden file.
	hw := got[0].HW
	if len(hw.NVIDIA) != 1 || hw.NVIDIA[0].VRAMMB != 12227 || hw.NVIDIA[0].Driver != "592.00" ||
		!hw.IntelGPU || !hw.IntelNPU || !hw.HasBattery || hw.RAMMB != 64907 || hw.DiskFreeMB != 51200 {
		t.Errorf("this machine: %+v", hw)
	}
	// The WMI fallback reads the 64-bit registry size: same VRAM as nvidia-smi.
	if fb := got[1].HW.NVIDIA; len(fb) != 1 || fb[0].VRAMMB != 12227 || fb[0].Driver != "592.00" || fb[0].Source != "wmi" {
		t.Errorf("fallback: %+v", fb)
	}
	if fb := got[2].HW.NVIDIA; len(fb) != 1 || fb[0].VRAMMB != 0 {
		t.Errorf("fallback without registry size must be unknown: %+v", fb)
	}
	if hw := got[5].HW; !hw.IntelNPU || hw.IntelNPUName != "Intel(R) AI Boost" || hw.IntelGPUName != "Intel(R) Iris(R) Xe Graphics" {
		t.Errorf("intel only: %+v", hw)
	}
	if hw := got[6].HW; hw.IntelNPU || hw.HasBattery {
		t.Errorf("AMD accelerator / no battery: %+v", hw)
	}
}

// A hanging probe is cut off by its timeout and only empties its own fields.
func TestDetectTimeout(t *testing.T) {
	hang := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "sysctl" && len(args) == 2 && args[1] == "hw.memsize" {
			return []byte("17179869184\n"), nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	start := time.Now()
	hw := DetectWith(context.Background(), "", Options{OS: "darwin", Arch: "arm64", Run: hang, Timeout: 30 * time.Millisecond})
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("detect took %v", d)
	}
	if hw.RAMMB != 16384 || hw.CPU != "" || hw.HasBattery {
		t.Errorf("got %+v", hw)
	}

	start = time.Now()
	hw = DetectWith(context.Background(), "", Options{OS: "windows", Arch: "amd64", Run: hang, Timeout: 30 * time.Millisecond,
		TotalRAMMB: func() (int, error) { return 4096, nil }})
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("windows detect took %v", d)
	}
	if hw.RAMMB != 4096 || len(hw.NVIDIA) != 0 || hw.IntelGPU {
		t.Errorf("got %+v", hw)
	}
}

func TestDetectCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := func(ctx context.Context, name string, args ...string) ([]byte, error) { return nil, ctx.Err() }
	hw := DetectWith(ctx, "", Options{OS: "windows", Arch: "amd64", Run: f})
	if hw.OS != "windows" || len(hw.NVIDIA) != 0 {
		t.Errorf("got %+v", hw)
	}
}

// Detect on the real machine: whatever is found, it must not fail, must
// fill OS/Arch and must finish within the probe budget. (No hardware is
// asserted: the tests run on CI machines too.)
func TestDetectReal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hw, err := Detect(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if hw.OS == "" || hw.Arch == "" {
		t.Errorf("got %+v", hw)
	}
	t.Logf("%+v", hw)
	_ = Recommend(hw)
}

func TestExistingAncestor(t *testing.T) {
	dir := t.TempDir()
	if got := existingAncestor(dir); got != dir {
		t.Errorf("existing: %q", got)
	}
	if got := existingAncestor(filepath.Join(dir, "a", "b")); got != dir {
		t.Errorf("missing: %q, want %q", got, dir)
	}
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := existingAncestor(filepath.Join(file, "x")); got != dir {
		t.Errorf("under a file: %q, want %q", got, dir)
	}
}
