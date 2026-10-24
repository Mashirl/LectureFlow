// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package hwdetect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recommendCase struct {
	Name           string         `json:"name"`
	HW             HW             `json:"hw"`
	Recommendation Recommendation `json:"recommendation"`
}

func win(gpus ...GPU) HW {
	return HW{OS: "windows", Arch: "amd64", CPU: "Intel(R) Core(TM) i7-12700H", RAMMB: 32768, DiskFreeMB: 200000, NVIDIA: gpus}
}

func nv(vram int) GPU {
	return GPU{Name: "NVIDIA GeForce RTX Test", VRAMMB: vram, Driver: "560.00", Source: "nvidia-smi"}
}

func withNPU(h HW) HW {
	h.IntelNPU, h.IntelNPUName = true, "Intel(R) AI Boost"
	return h
}

func withIGPU(h HW) HW {
	h.IntelGPU, h.IntelGPUName = true, "Intel(R) Graphics"
	return h
}

func mac(mem int) HW {
	return HW{OS: "darwin", Arch: "arm64", CPU: "Apple M3", RAMMB: mem, DiskFreeMB: 100000,
		AppleSilicon: true, Chip: "Apple M3", UnifiedMemMB: mem, HasBattery: true}
}

func recommendCases() []recommendCase {
	thisMachine := withNPU(withIGPU(HW{OS: "windows", Arch: "amd64", CPU: "Intel(R) Core(TM) Ultra 9 386H", RAMMB: 64907,
		DiskFreeMB: 3155806, HasBattery: true,
		NVIDIA: []GPU{{Name: "NVIDIA GeForce RTX 5070 Ti Laptop GPU", VRAMMB: 12227, Driver: "592.00", Source: "nvidia-smi"}}}))
	thisMachine.IntelNPUName = "Intel(R) NPU"

	lowRAM := win()
	lowRAM.RAMMB = 4096
	lowRAM8 := win()
	lowRAM8.RAMMB = 7999
	ram8000 := win()
	ram8000.RAMMB = 8000
	noCPU := win()
	noCPU.CPU = ""
	unnamedNPU := win()
	unnamedNPU.IntelNPU = true
	macUnknown := mac(0)
	macNoChip := mac(16384)
	macNoChip.Chip = ""
	intelMac := HW{OS: "darwin", Arch: "amd64", CPU: "Intel(R) Core(TM) i7-9750H CPU @ 2.60GHz", RAMMB: 16384}
	linux := HW{OS: "linux", Arch: "amd64", NVIDIA: []GPU{nv(24576)}}
	nvOnMacOS := mac(16384)
	nvOnMacOS.NVIDIA = []GPU{nv(8192)} // impossible; NVIDIA is only for Windows

	return []recommendCase{
		{Name: "this_machine_nvidia_12gb_igpu_npu", HW: thisMachine},
		{Name: "nvidia_24gb", HW: win(nv(24576))},
		{Name: "nvidia_10240_float16_boundary", HW: win(nv(10240))},
		{Name: "nvidia_10239_int8_float16", HW: win(nv(10239))},
		{Name: "nvidia_8gb", HW: win(nv(8192))},
		{Name: "nvidia_6000_large_boundary", HW: win(nv(6000))},
		{Name: "nvidia_5999_turbo", HW: win(nv(5999))},
		{Name: "nvidia_4000_turbo_boundary", HW: win(nv(4000))},
		{Name: "nvidia_3999_medium", HW: win(nv(3999))},
		{Name: "nvidia_2gb_medium", HW: win(nv(2048))},
		{Name: "nvidia_vram_unknown", HW: win(GPU{Name: "NVIDIA GeForce GTX 1060", Source: "wmi"})},
		{Name: "nvidia_two_gpus_picks_largest", HW: win(nv(4096), GPU{Name: "NVIDIA RTX A4000", VRAMMB: 16384, Driver: "560.00", Source: "nvidia-smi"})},
		{Name: "nvidia_with_npu", HW: withNPU(win(nv(8192)))},
		{Name: "nvidia_small_with_npu", HW: withNPU(win(nv(4096)))},
		{Name: "nvidia_with_igpu_only", HW: withIGPU(win(nv(8192)))},
		{Name: "intel_igpu_and_npu", HW: withNPU(withIGPU(win()))},
		{Name: "intel_igpu_only", HW: withIGPU(win())},
		{Name: "intel_npu_only", HW: withNPU(win())},
		{Name: "intel_npu_unnamed", HW: unnamedNPU},
		{Name: "apple_32gb", HW: mac(32768)},
		{Name: "apple_16gb", HW: mac(16384)},
		{Name: "apple_8gb_turbo_boundary", HW: mac(8192)},
		{Name: "apple_8191_small", HW: mac(8191)},
		{Name: "apple_memory_unknown", HW: macUnknown},
		{Name: "apple_no_chip_name", HW: macNoChip},
		{Name: "cpu_only", HW: win()},
		{Name: "cpu_only_no_cpu_name", HW: noCPU},
		{Name: "cpu_only_low_ram_4gb", HW: lowRAM},
		{Name: "cpu_only_low_ram_7999", HW: lowRAM8},
		{Name: "cpu_only_ram_8000_no_note", HW: ram8000},
		{Name: "intel_mac_unsupported", HW: intelMac},
		{Name: "linux_unsupported", HW: linux},
		{Name: "nvidia_ignored_on_macos", HW: nvOnMacOS},
		{Name: "empty_hw", HW: HW{}},
	}
}

func TestRecommendGolden(t *testing.T) {
	cases := recommendCases()
	for i := range cases {
		cases[i].Recommendation = Recommend(cases[i].HW)
	}
	checkGolden(t, "recommend.golden.json", cases)
}

// Invariants on top of the golden file.
func TestRecommendInvariants(t *testing.T) {
	for _, c := range recommendCases() {
		r := Recommend(c.HW)
		if len(r.Components) == 0 || r.Components[0] != ComponentPython {
			t.Errorf("%s: python must come first: %v", c.Name, r.Components)
		}
		seen := map[string]bool{}
		for _, id := range r.Components {
			if seen[id] {
				t.Errorf("%s: duplicate %s", c.Name, id)
			}
			seen[id] = true
			if strings.HasPrefix(id, "model:") {
				f := strings.Split(id, ":")
				if len(f) != 4 || f[2] == "" || f[3] == "" {
					t.Errorf("%s: bad model id %s", c.Name, id)
				}
				backend := map[string]string{ModelBackendFasterWhisper: ComponentASRCUDA, ModelBackendOpenVINO: ComponentASROpenVINO, ModelBackendMLX: ComponentASRMLX}[f[1]]
				if backend == "" || !seen[backend] {
					t.Errorf("%s: model %s without its backend component", c.Name, id)
				}
			}
		}
		if r.Reasons == nil || r.Notes == nil {
			t.Errorf("%s: reasons/notes must be non-nil (JSON arrays)", c.Name)
		}
	}
}

// Every reason and note code has zh-CN and en text, and the texts use
// exactly the parameters the code sends.
func TestRecommendI18n(t *testing.T) {
	codes := map[string]map[string]bool{}
	for _, c := range recommendCases() {
		r := Recommend(c.HW)
		for _, e := range append(append(r.Reasons[:0:0], r.Reasons...), r.Notes...) {
			if codes[e.Code] == nil {
				codes[e.Code] = map[string]bool{}
			}
			for k := range e.Params {
				codes[e.Code][k] = true
			}
		}
	}
	for _, code := range []string{ReasonNVIDIA, ReasonNVIDIAVRAMUnknown, ReasonNVIDIASmallVRAM, ReasonNPUBattery,
		ReasonIntelGPU, ReasonIntelNPU, ReasonApple, ReasonAppleSmallMem, ReasonCPUOnly,
		NoteCPUSlow, NoteVRAMUnknown, NoteLowRAM, NoteUnsupported} {
		if codes[code] == nil {
			t.Errorf("code %s not covered by the cases", code)
		}
	}
	for _, lang := range []string{"zh-CN", "en"} {
		// The catalogs proper: the lead merged web/i18n/_parts/v3-hw.* into them.
		path := filepath.Join("..", "..", "web", "i18n", lang+".json")
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for code, params := range codes {
			text, ok := m[code].(string)
			if !ok {
				t.Errorf("%s: missing %s", lang, code)
				continue
			}
			for p := range params {
				if !strings.Contains(text, "{"+p+"}") {
					t.Errorf("%s %s: text does not use {%s}: %q", lang, code, p, text)
				}
			}
			for _, p := range placeholders(text) {
				if !params[p] {
					t.Errorf("%s %s: text uses {%s}, which the code does not always send", lang, code, p)
				}
			}
		}
	}
}

func placeholders(s string) []string {
	var out []string
	for {
		i := strings.IndexByte(s, '{')
		if i < 0 {
			return out
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			return out
		}
		out = append(out, s[i+1:i+j])
		s = s[i+j+1:]
	}
}
