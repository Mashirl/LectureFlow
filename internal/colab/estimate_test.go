// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/model"
)

var update = flag.Bool("update", false, "rewrite testdata/estimate.golden.json")

// estimateCase is one golden case: kind decide | plan | approx | rate.
type estimateCase struct {
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Input  DispatchInput   `json:"input"`
	Queue  []float64       `json:"queue,omitempty"`   // plan: audio seconds
	Typ    float64         `json:"typical,omitempty"` // approx: audio seconds
	Rate   Rate            `json:"rate_before,omitempty"`
	Sample []Sample        `json:"samples,omitempty"` // rate: folded in order
	Output json.RawMessage `json:"output"`
}

// goldenDefaults are the defaults the golden cases were written for
// (15 s per audio minute, 180 s overhead, 420 s setup): the cases test
// the pure functions, their names describe outcomes under these values,
// so they stay when DefaultEstimateDefaults changes (colab-batch:
// measured values, TestDefaultEstimateDefaults).
func goldenDefaults() EstimateDefaults {
	return EstimateDefaults{
		SecPerAudioMin: 15, OverheadSec: 180, SetupSec: 420, AuthMarginSec: 600,
		CUPerHour: map[string]float64{"T4": 2.0, "L4": 5.0, "G4": 12.0, "A100": 13.0, "H100": 20.0},
	}
}

func estimateCases() []estimateCase {
	d := goldenDefaults()
	base := DispatchInput{WindowSec: FairWindow.Seconds(), Defaults: d, Paid: true, BalanceKnown: true, BalanceCU: 100,
		CUPerHour: 2, AuthRemainingSec: -1}
	with := func(f func(*DispatchInput)) DispatchInput {
		in := base
		f(&in)
		return in
	}
	measured := Rate{SecPerAudioMin: 6, OverheadSec: 90, Samples: 2, AudioMin: 100}
	return []estimateCase{
		{Name: "fresh session, 50-minute lecture: fits", Kind: "decide", Input: with(func(in *DispatchInput) { in.AudioSec = 3000 })},
		{Name: "window: 3.8 h used, 50-minute lecture does not fit", Kind: "decide",
			Input: with(func(in *DispatchInput) { in.AudioSec = 3000; in.UsedSec = 3.8 * 3600; in.SetupDone = true })},
		{Name: "window: 3.5 h used, 50-minute lecture fits", Kind: "decide",
			Input: with(func(in *DispatchInput) { in.AudioSec = 3000; in.UsedSec = 3.5 * 3600; in.SetupDone = true })},
		{Name: "window: exactly the estimate left", Kind: "decide",
			Input: with(func(in *DispatchInput) {
				in.AudioSec = 3000
				in.SetupDone = true
				in.UsedSec = FairWindow.Seconds() - (3000.0/60*15 + 180)
			})},
		{Name: "window used up completely", Kind: "decide",
			Input: with(func(in *DispatchInput) { in.AudioSec = 600; in.UsedSec = 5 * 3600; in.SetupDone = true })},
		{Name: "balance too low", Kind: "decide",
			Input: with(func(in *DispatchInput) { in.AudioSec = 3000; in.BalanceCU = 0.1 })},
		{Name: "balance unknown on the paid tier", Kind: "decide",
			Input: with(func(in *DispatchInput) { in.AudioSec = 3000; in.BalanceKnown = false })},
		{Name: "free tier ignores the balance", Kind: "decide",
			Input: with(func(in *DispatchInput) { in.AudioSec = 3000; in.Paid = false; in.BalanceCU = 0 })},
		{Name: "credential ends before the lecture could finish", Kind: "decide",
			Input: with(func(in *DispatchInput) { in.AudioSec = 3000; in.AuthRemainingSec = 1500 })},
		{Name: "credential outlives the lecture with the margin", Kind: "decide",
			Input: with(func(in *DispatchInput) { in.AudioSec = 3000; in.AuthRemainingSec = 3000 })},
		{Name: "auth checked before window and balance", Kind: "decide",
			Input: with(func(in *DispatchInput) {
				in.AudioSec = 3000
				in.AuthRemainingSec = 10
				in.UsedSec = 5 * 3600
				in.BalanceCU = 0
			})},
		{Name: "window checked before balance", Kind: "decide",
			Input: with(func(in *DispatchInput) { in.AudioSec = 3000; in.UsedSec = 5 * 3600; in.BalanceCU = 0 })},
		{Name: "measured rate makes a lecture fit that the default does not", Kind: "decide",
			Input: with(func(in *DispatchInput) {
				in.AudioSec = 90 * 60
				in.UsedSec = 3.65 * 3600
				in.SetupDone = true
				in.Rate = measured
			})},
		{Name: "same lecture with the default rate", Kind: "decide",
			Input: with(func(in *DispatchInput) { in.AudioSec = 90 * 60; in.UsedSec = 3.65 * 3600; in.SetupDone = true })},
		{Name: "real hourly rate from the running session", Kind: "decide",
			Input: with(func(in *DispatchInput) {
				in.AudioSec = 3000
				in.CUPerHour = 11.77
				in.BalanceCU = 3
				in.SetupDone = true
			})},
		{Name: "queue of lectures of different lengths, 2.5 h used", Kind: "plan",
			Input: with(func(in *DispatchInput) { in.UsedSec = 2.5 * 3600; in.SetupDone = true }),
			Queue: []float64{90 * 60, 20 * 60, 75 * 60, 50 * 60, 120 * 60, 10 * 60}},
		{Name: "queue on a fresh session (setup paid once)", Kind: "plan",
			Input: base, Queue: []float64{50 * 60, 50 * 60, 50 * 60, 50 * 60, 50 * 60, 50 * 60, 50 * 60, 50 * 60, 50 * 60, 50 * 60, 50 * 60, 50 * 60, 50 * 60, 50 * 60, 50 * 60, 50 * 60}},
		{Name: "queue limited by the balance", Kind: "plan",
			Input: with(func(in *DispatchInput) { in.BalanceCU = 1.2; in.SetupDone = true }),
			Queue: []float64{50 * 60, 50 * 60, 50 * 60, 50 * 60}},
		{Name: "queue limited by the credential", Kind: "plan",
			Input: with(func(in *DispatchInput) { in.AuthRemainingSec = 3600; in.SetupDone = true }),
			Queue: []float64{50 * 60, 50 * 60, 50 * 60, 50 * 60}},
		{Name: "queue with a measured rate", Kind: "plan",
			Input: with(func(in *DispatchInput) { in.UsedSec = 1800; in.SetupDone = true; in.Rate = measured }),
			Queue: []float64{90 * 60, 90 * 60, 90 * 60, 90 * 60, 90 * 60, 90 * 60, 90 * 60, 90 * 60, 90 * 60, 90 * 60, 90 * 60, 90 * 60, 90 * 60, 90 * 60, 90 * 60, 90 * 60, 90 * 60, 90 * 60, 90 * 60, 90 * 60, 90 * 60, 90 * 60}},
		{Name: "about N lectures of 50 minutes, fresh session", Kind: "approx", Input: base, Typ: TypicalLectureSec},
		{Name: "about N lectures, 3 h used", Kind: "approx",
			Input: with(func(in *DispatchInput) { in.UsedSec = 3 * 3600; in.SetupDone = true }), Typ: TypicalLectureSec},
		{Name: "about N lectures, measured rate", Kind: "approx",
			Input: with(func(in *DispatchInput) { in.UsedSec = 3600; in.SetupDone = true; in.Rate = measured }), Typ: TypicalLectureSec},
		{Name: "about N lectures, nothing fits", Kind: "approx",
			Input: with(func(in *DispatchInput) { in.UsedSec = 4 * 3600 }), Typ: TypicalLectureSec},
		{Name: "rate: first measurement replaces the default", Kind: "rate",
			Sample: []Sample{{AudioSec: 3000, TranscribeSec: 400, OverheadSec: 150}}},
		{Name: "rate: weighted by audio length", Kind: "rate",
			Sample: []Sample{{AudioSec: 3000, TranscribeSec: 400, OverheadSec: 150}, {AudioSec: 600, TranscribeSec: 120, OverheadSec: 90},
				{AudioSec: 5400, TranscribeSec: 650, OverheadSec: 210}}},
		{Name: "rate: a sample without audio is ignored", Kind: "rate",
			Rate: Rate{SecPerAudioMin: 8, OverheadSec: 100, Samples: 1, AudioMin: 50}, Sample: []Sample{{AudioSec: 0, TranscribeSec: 30}}},
	}
}

func runEstimateCase(c estimateCase) any {
	switch c.Kind {
	case "decide":
		return Decide(c.Input)
	case "plan":
		return PlanQueue(c.Input, c.Queue)
	case "approx":
		return map[string]int{"lectures": ApproxLectures(c.Input, c.Typ)}
	case "rate":
		r := c.Rate
		for _, s := range c.Sample {
			r = r.Add(s)
		}
		r.SecPerAudioMin, r.OverheadSec, r.AudioMin = round2(r.SecPerAudioMin), round2(r.OverheadSec), round2(r.AudioMin)
		return r
	}
	panic(c.Kind)
}

func TestEstimateGolden(t *testing.T) {
	path := filepath.Join("testdata", "estimate.golden.json")
	cases := estimateCases()
	for i := range cases {
		b, _ := json.Marshal(runEstimateCase(cases[i]))
		cases[i].Output = b
	}
	got, _ := json.MarshalIndent(cases, "", " ")
	got = append(got, '\n')
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/colab/ -run TestEstimateGolden -update)", err)
	}
	if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), got) {
		var w []estimateCase
		_ = json.Unmarshal(want, &w)
		for i, c := range cases {
			if i >= len(w) || w[i].Name != c.Name || !bytes.Equal(compact(w[i].Output), compact(c.Output)) {
				t.Errorf("case %q: got %s", c.Name, c.Output)
			}
		}
		t.Fatal("estimate.golden.json differs (run with -update after checking the change)")
	}
}

func compact(b []byte) []byte {
	var out bytes.Buffer
	_ = json.Compact(&out, b)
	return out.Bytes()
}

// The properties the golden file is meant to show, checked independently
// of it.
func TestEstimateProperties(t *testing.T) {
	cases := estimateCases()
	get := func(name string) estimateCase {
		for _, c := range cases {
			if c.Name == name {
				return c
			}
		}
		t.Fatalf("no case %q", name)
		return estimateCase{}
	}
	dec := func(name string) Decision { return Decide(get(name).Input) }
	if d := dec("fresh session, 50-minute lecture: fits"); !d.Dispatch || d.EstimateSec != 3000.0/60*15+180+420 {
		t.Errorf("fresh: %+v", d)
	}
	if d := dec("window: 3.8 h used, 50-minute lecture does not fit"); d.Dispatch || d.WaitingFor != model.WaitRemoteWindow {
		t.Errorf("window: %+v", d)
	}
	if d := dec("window: exactly the estimate left"); !d.Dispatch {
		t.Errorf("exact fit: %+v", d)
	}
	if d := dec("balance too low"); d.Dispatch || d.WaitingFor != model.WaitRemoteBalance {
		t.Errorf("balance: %+v", d)
	}
	if d := dec("free tier ignores the balance"); !d.Dispatch {
		t.Errorf("free: %+v", d)
	}
	if d := dec("credential ends before the lecture could finish"); d.Dispatch || d.WaitingFor != model.WaitRemoteAuth {
		t.Errorf("auth: %+v", d)
	}
	if a, b := dec("measured rate makes a lecture fit that the default does not"), dec("same lecture with the default rate"); !a.Dispatch || b.Dispatch {
		t.Errorf("measured %+v default %+v", a, b)
	}
	p := PlanQueue(get("queue of lectures of different lengths, 2.5 h used").Input, get("queue of lectures of different lengths, 2.5 h used").Queue)
	// 1.5 h left: 90 min (25.5 min) fits, 20 (8) fits, 75 (21.75) fits, 50 (15.5) fits,
	// 120 (33) does not after those (1.5 h − 70.75 min = 19.25 min), 10 (5.5) fits.
	if want := []bool{true, true, true, true, false, true}; !boolsEqual(p.Fits, want) || p.Lectures != 5 {
		t.Errorf("plan %+v", p)
	}
}

func boolsEqual(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// colab-batch: the defaults are the speeds and rates measured on Colab
// (colab-prof §9.1), per accelerator.
func TestDefaultEstimateDefaults(t *testing.T) {
	d := DefaultEstimateDefaults()
	for acc, want := range map[string][2]float64{"T4": {4.2, 1.07}, "L4": {3.1, 1.54}, "A100": {2.3, 5.3}, "G4": {1.8, 8.9}} {
		if got := d.For(acc); got.SecPerAudioMin != want[0] || d.CUPerHour[acc] != want[1] {
			t.Errorf("%s: %v s per audio minute, %v CU/h; want %v", acc, got.SecPerAudioMin, d.CUPerHour[acc], want)
		}
	}
	// Unknown accelerators take the slowest speed; H100 keeps a high guess of its rate.
	if got := d.For("H100"); got.SecPerAudioMin != 4.2 || d.CUPerHour["H100"] < 10 {
		t.Errorf("H100: %+v", got)
	}
	if d.OverheadSec != 90 || d.SetupSec != 60 {
		t.Errorf("overhead %v, setup %v", d.OverheadSec, d.SetupSec)
	}
	// A 55-minute lecture on a fresh L4: transcription, overhead and setup.
	if e := EstimateSec(55*60, Rate{}, d.For("L4"), false); e != 55*3.1+90+60 {
		t.Errorf("estimate %v", e)
	}
}
