// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"lectureflow/internal/model"
)

// The product copy agrees with the 3a file it was taken from (the fields
// it keeps), and the defaults follow I6 and lead rulings 3 and 14.
func TestRecommendationCopyMatchesSpike(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "spikes", "openvino", "recommendation.json"))
	if err != nil {
		t.Fatal(err) // committed with 3a (c853bfd)
	}
	var spike struct {
		Devices map[string]struct {
			Model     string  `json:"model"`
			Precision string  `json:"precision"`
			RTF       float64 `json:"rtf"`
			WER       float64 `json:"wer"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(b, &spike); err != nil {
		t.Fatal(err)
	}
	f := loadRecommendation()
	if len(f.Devices) != len(spike.Devices) {
		t.Fatalf("copy has %d devices, spike %d", len(f.Devices), len(spike.Devices))
	}
	for name, s := range spike.Devices {
		c, ok := f.Devices[name]
		if !ok || c.Model != s.Model || c.Precision != s.Precision || c.RTF != s.RTF || c.WER != s.WER {
			t.Errorf("%s: copy %+v, spike %+v", name, c, s)
		}
	}
}

func TestRecommended(t *testing.T) {
	cases := []struct {
		id   string
		want Candidate
		ok   bool
	}{
		{model.DeviceCUDA0, Candidate{Model: "large-v3", Precision: "auto"}, true},
		{model.DeviceOVGPU, Candidate{Model: "large-v3-turbo", Precision: "fp16"}, true}, // ruling 14: fp16
		{model.DeviceOVNPU, Candidate{Model: "large-v3-turbo", Precision: "fp16"}, true},
		{model.DeviceOVCPU, Candidate{Model: "large-v3-turbo", Precision: "fp16"}, true},
		{model.DeviceFWCPU, Candidate{}, false},
		{"rocm:0", Candidate{}, false},
	}
	for _, c := range cases {
		got, ok := Recommended(c.id)
		if got != c.want || ok != c.ok {
			t.Errorf("Recommended(%q) = %+v, %v; want %+v, %v", c.id, got, ok, c.want, c.ok)
		}
	}
	if Fallback(model.BackendOpenVINO) != (Candidate{Model: "small", Precision: "int8"}) ||
		Fallback(model.BackendFasterWhisper) != (Candidate{Model: "small", Precision: "auto"}) {
		t.Fatal("Fallback is not I6's small + int8")
	}
	if e, ok := RecommendedInfo(model.DeviceOVNPU); !ok || e.Kind != model.DeviceKindNPU || e.FirstLoadS < 200 {
		t.Fatalf("NPU entry %+v, %v", e, ok)
	}
}

func TestCandidates(t *testing.T) {
	for _, id := range []string{model.DeviceCUDA0, model.DeviceOVGPU, model.DeviceOVNPU, model.DeviceOVCPU, model.DeviceFWCPU} {
		cs := Candidates(id)
		if len(cs) < 2 {
			t.Fatalf("%s: %d candidates", id, len(cs))
		}
		backend := model.DeviceBackend(id)
		seen := map[Candidate]bool{}
		for i, c := range cs {
			if !model.ValidPrecision(backend, c.Precision) || c.Model == "" || seen[c] {
				t.Errorf("%s[%d] = %+v: invalid or repeated", id, i, c)
			}
			seen[c] = true
			if backend == model.BackendOpenVINO && !ValidOVModel(c.Model) {
				t.Errorf("%s[%d] = %+v: not an OpenVINO model choice", id, i, c)
			}
			if i == 0 && c.Fallback {
				t.Errorf("%s: the first candidate is a fallback", id)
			}
		}
		if r, ok := Recommended(id); ok && id != model.DeviceOVCPU && (cs[0].Model != r.Model || cs[0].Precision != r.Precision) {
			t.Errorf("%s: first candidate %+v is not the recommendation %+v", id, cs[0], r)
		}
	}
	// Lead ruling 3: the NPU never tries int4.
	for _, c := range Candidates(model.DeviceOVNPU) {
		if c.Precision == model.PrecisionINT4 {
			t.Fatalf("NPU candidate %+v", c)
		}
	}
	if Candidates("rocm:0") != nil {
		t.Fatal("unknown device has candidates")
	}
}

func TestChoices(t *testing.T) {
	if !reflect.DeepEqual(PrecisionChoices(model.BackendOpenVINO), []string{"fp16", "int8", "int4"}) ||
		!reflect.DeepEqual(PrecisionChoices(model.BackendFasterWhisper), []string{"auto", "float16", "int8_float16"}) {
		t.Fatal("precision choices")
	}
	for _, b := range []string{model.BackendOpenVINO, model.BackendFasterWhisper} {
		for _, p := range PrecisionChoices(b) {
			if !model.ValidPrecision(b, p) {
				t.Errorf("%s: %s offered but invalid", b, p)
			}
		}
	}
	for _, m := range ModelChoices(model.BackendOpenVINO) {
		for _, p := range PrecisionChoices(model.BackendOpenVINO) {
			if mb, ok := ModelSizeMB(m, p); !ok || mb <= 0 {
				t.Errorf("no 3a size for %s %s", m, p)
			}
		}
	}
	if ValidOVModel("large-v2") || ValidOVModel("") || !ValidOVModel("large-v3-turbo") {
		t.Fatal("ValidOVModel")
	}
	if mb, _ := ModelSizeMB("large-v3-turbo", "fp16"); mb != 1551 {
		t.Fatalf("turbo fp16 size %d", mb)
	}
}
