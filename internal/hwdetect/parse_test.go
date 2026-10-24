// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package hwdetect

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestParseNvidiaSMI(t *testing.T) {
	got := ParseNvidiaSMI(string(readTestdata(t, "windows/this_machine.smi.txt")))
	want := []GPU{{Name: "NVIDIA GeForce RTX 5070 Ti Laptop GPU", VRAMMB: 12227, Driver: "592.00", Source: "nvidia-smi"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("this machine: %+v", got)
	}
	got = ParseNvidiaSMI(smiMulti)
	want = []GPU{
		{Name: "NVIDIA RTX A2000 8GB Laptop GPU", VRAMMB: 8192, Driver: "552.22", Source: "nvidia-smi"},
		{Name: "Tesla T4", VRAMMB: 0, Driver: "", Source: "nvidia-smi"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("multi: %+v", got)
	}
	for _, in := range []string{"", "\r\n", "No devices were found", "NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver."} {
		if g := ParseNvidiaSMI(in); len(g) != 0 {
			t.Errorf("%q: %+v", in, g)
		}
	}
	bom := string([]byte{0xEF, 0xBB, 0xBF}) + "NVIDIA GeForce GTX 1650, 4096, 537.13\n"
	if g := ParseNvidiaSMI(bom); len(g) != 1 || g[0].Name != "NVIDIA GeForce GTX 1650" || g[0].VRAMMB != 4096 {
		t.Errorf("bom: %+v", g)
	}
}

func TestNvidiaDriverFromWindows(t *testing.T) {
	for in, want := range map[string]string{
		"32.0.15.9200":  "592.00",
		"31.0.15.5222":  "552.22",
		"27.21.14.5671": "456.71",
		"30.0.14.7141":  "471.41",
		"31.0.101.4502": "145.02", // not an NVIDIA version; harmless (only NVIDIA adapters use it)
		"":              "",
		"1.2.3":         "",
		"32.0.x.9200":   "",
		"1.0.0.1":       "",
	} {
		if got := NvidiaDriverFromWindows(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestNum(t *testing.T) {
	for in, want := range map[string]uint64{
		`12820938752`:               12820938752,
		`"34359738368"`:             34359738368,
		`null`:                      0,
		``:                          0,
		`-1`:                        0,
		`[0,0,0,64,0,0,0,0]`:        1 << 30,
		`[0,0,0,128]`:               1 << 31,
		`[1,2,3,4,5,6,7,8,9]`:       0, // longer than a QWORD
		`{"a":1}`:                   0,
		`"not a number"`:            0,
		`4293918720`:                4293918720,
		`[255,255,255,255,0,0,0,0]`: 4294967295,
	} {
		if got := num(json.RawMessage(in)); got != want {
			t.Errorf("%s: got %d, want %d", in, got, want)
		}
	}
}

func TestIntelNPUName(t *testing.T) {
	cases := []struct {
		pnp  []WinPnP
		want string
	}{
		{[]WinPnP{{Name: "USB Input Device", PNPClass: "HIDClass", Manufacturer: "(Standard system devices)"}}, ""},
		{[]WinPnP{{Name: "Microsoft Input Configuration Device", PNPClass: "HIDClass", Manufacturer: "Microsoft"}}, ""},
		{[]WinPnP{{Name: "Intel(R) NPU", PNPClass: "ComputeAccelerator", Manufacturer: "Intel Corporation"}}, "Intel(R) NPU"},
		{[]WinPnP{{Name: "Intel(R) AI Boost", PNPClass: "System", Manufacturer: "Intel Corporation"}}, "Intel(R) AI Boost"},
		{[]WinPnP{{Name: "Intel NPU Driver", PNPClass: "SoftwareComponent", Manufacturer: ""}}, "Intel NPU Driver"},
		{[]WinPnP{{Name: "AMD IPU Device", PNPClass: "ComputeAccelerator", Manufacturer: "Advanced Micro Devices"}}, ""},
		{[]WinPnP{{Name: "Qualcomm(R) Hexagon(TM) NPU", PNPClass: "ComputeAccelerator", Manufacturer: "Qualcomm"}}, ""},
		{nil, ""},
	}
	for i, c := range cases {
		if got := (WinProbe{PnP: c.pnp}).IntelNPUName(); got != c.want {
			t.Errorf("%d: got %q, want %q", i, got, c.want)
		}
	}
}

func TestIntelGPUName(t *testing.T) {
	p := WinProbe{Video: []WinVideo{
		{Name: "MuMuPlayer Virtual Display Adapter", AdapterCompatibility: "MuMuPlayer"},
		{Name: "NVIDIA GeForce RTX 4060", AdapterCompatibility: "NVIDIA"},
		{Name: "Intel(R) UHD Graphics", AdapterCompatibility: "", PNPDeviceID: `PCI\VEN_8086&DEV_9BC4`},
	}}
	if got := p.IntelGPUName(); got != "Intel(R) UHD Graphics" {
		t.Errorf("got %q", got)
	}
	if got := (WinProbe{Video: []WinVideo{{Name: "Microsoft Basic Display Adapter", AdapterCompatibility: "(Standard display types)"}}}).IntelGPUName(); got != "" {
		t.Errorf("basic display: %q", got)
	}
}

func TestParseWinProbeErrors(t *testing.T) {
	if _, err := ParseWinProbe([]byte("")); err == nil {
		t.Error("empty output must be an error")
	}
	if _, err := ParseWinProbe([]byte("Access denied")); err == nil {
		t.Error("text must be an error")
	}
	// Keys of the wrong type stay empty, the others are read.
	p, err := ParseWinProbe([]byte(`{"video":"oops","cpu":[{"Name":"X"}],"cs":7,"battery":[1,2]}`))
	if err != nil || len(p.Video) != 0 || p.CPUName() != "X" || p.RAMMB() != 0 || len(p.Battery) != 0 {
		t.Errorf("got %+v, %v", p, err)
	}
}

func TestParseMac(t *testing.T) {
	if got := ParseSysctlString(string(readTestdata(t, "darwin/m2pro.brand.txt"))); got != "Apple M2 Pro" {
		t.Errorf("brand: %q", got)
	}
	if got := ParseMemsizeMB(string(readTestdata(t, "darwin/m2pro.memsize.txt"))); got != 32768 {
		t.Errorf("memsize: %d", got)
	}
	if got := ParseMemsizeMB("garbage"); got != 0 {
		t.Errorf("garbage memsize: %d", got)
	}
	if !ParseSysctlBool("1\n") || ParseSysctlBool("0\n") || ParseSysctlBool("") {
		t.Error("sysctl bool")
	}
	if !ParsePmsetBattery(string(readTestdata(t, "darwin/pmset.laptop.txt"))) {
		t.Error("laptop battery")
	}
	if ParsePmsetBattery(string(readTestdata(t, "darwin/pmset.desktop.txt"))) {
		t.Error("desktop battery")
	}
}
