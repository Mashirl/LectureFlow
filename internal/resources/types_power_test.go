// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"lectureflow/internal/devices"
	"lectureflow/internal/sched"
)

// testdata/api/power_resources.json is the frozen example of the resource
// snapshot with the power-management fields (power-brief §6): a laptop on
// battery, the NPU active, a CUDA job suspended. It must decode without
// unknown fields and keep the invariants the front end relies on.
func TestPowerResourcesExample(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "api", "power_resources.json"))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var s Snapshot
	if err := dec.Decode(&s); err != nil {
		t.Fatal(err)
	}
	if s.Power == nil || s.System == nil || s.Policy == nil {
		t.Fatalf("power, system and policy expected: %+v", s)
	}
	if !slices.Contains([]string{PowerSourceAC, PowerSourceBattery, ""}, s.Power.Source) {
		t.Errorf("power source %q", s.Power.Source)
	}
	if s.Power.Source == PowerSourceBattery != s.Power.OnBattery {
		t.Errorf("source %q with on_battery %v", s.Power.Source, s.Power.OnBattery)
	}
	p := s.Policy
	if !slices.Contains([]string{devices.PolicyReasonACAuto, devices.PolicyReasonBattery, devices.PolicyReasonManual, devices.PolicyReasonDesktop}, p.Reason) {
		t.Errorf("reason %q", p.Reason)
	}
	var active []string
	for _, d := range s.Devices {
		if d.Active {
			active = append(active, d.ID)
		}
	}
	if !slices.Equal(active, p.Active) {
		t.Errorf("active cards %v, policy %v", active, p.Active)
	}
	if p.Notices == nil {
		t.Error("notices must not be null")
	}
	for _, n := range p.Notices {
		if !slices.Contains([]string{sched.CodePowerSwitched, sched.CodePowerSuspended, sched.CodePowerResumed}, n.Info.Code) || n.ID == "" || n.At == "" {
			t.Errorf("notice %+v", n)
		}
	}
	// The power-management fields are omitted when not wired (the 3b and
	// v1.1 fixtures keep their bytes).
	out, _ := json.Marshal(Snapshot{Providers: []Provider{}})
	for _, k := range []string{`"system"`, `"policy"`, `"devices"`, `"power"`} {
		if bytes.Contains(out, []byte(k)) {
			t.Errorf("%s in %s", k, out)
		}
	}
}
