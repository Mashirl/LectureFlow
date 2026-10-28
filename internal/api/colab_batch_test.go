// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"net/http"
	"testing"
)

// colab-batch (user decision 2026-10-27 05:40): the paid level borrows an
// L4 unless the user picked an accelerator; the free level a T4; a pick is
// never replaced by the level's default. colab-obs-core: the level follows
// the account (no switch: a tier in the request is ignored). "极速模式" is a
// setting, off by default.
func TestColabAcceleratorFollowsTheAccountUntilPicked(t *testing.T) {
	ce := newColabEnv(t)
	var v ColabView
	ce.call("GET", "/api/colab", nil, http.StatusOK, &v)
	if v.Settings.Accelerator != "T4" || v.Settings.AcceleratorChosen || v.Settings.FastMode || v.Status.Slots != 1 {
		t.Fatalf("defaults before the account was read %+v / slots %d", v.Settings, v.Status.Slots)
	}
	ce.call("POST", "/api/colab/auth/import", map[string]any{"lifecycle": "manual"}, http.StatusOK, nil)
	ce.call("POST", "/api/colab/account/refresh", nil, http.StatusOK, &v)
	if v.Settings.Accelerator != "L4" || v.Settings.AcceleratorChosen || v.Status.Slots != 2 || v.Settings.Tier != "paid" {
		t.Fatalf("paid account %+v / slots %d", v.Settings, v.Status.Slots)
	}
	ce.call("PUT", "/api/colab/settings", map[string]any{"tier": "free"}, http.StatusOK, &v)
	if v.Settings.Accelerator != "L4" || v.Settings.AcceleratorChosen || v.Status.Slots != 2 || v.Settings.Tier != "paid" {
		t.Fatalf("the tier was not ignored %+v", v.Settings)
	}
	// A pick, even of the level's own default, is kept from now on.
	ce.call("PUT", "/api/colab/settings", map[string]any{"accelerator": "L4"}, http.StatusOK, &v)
	if v.Settings.Accelerator != "L4" || !v.Settings.AcceleratorChosen {
		t.Fatalf("picked %+v", v.Settings)
	}
	ce.call("PUT", "/api/colab/settings", map[string]any{"accelerator": "T4", "tier": "paid"}, http.StatusOK, &v)
	if v.Settings.Accelerator != "T4" || !v.Settings.AcceleratorChosen {
		t.Fatalf("T4 picked %+v", v.Settings)
	}
	// 极速模式
	ce.call("PUT", "/api/colab/settings", map[string]any{"fast_mode": true, "accelerator": "G4"}, http.StatusOK, &v)
	if !v.Settings.FastMode || v.Status.Slots != 1 {
		t.Fatalf("fast mode %+v / slots %d", v.Settings, v.Status.Slots)
	}
	// Stored: a new backend on the same database reads them back.
	set := ce.b.Settings()
	if set.Accelerator != "G4" || !set.AcceleratorChosen || !set.FastMode {
		t.Fatalf("backend settings %+v", set)
	}
}
