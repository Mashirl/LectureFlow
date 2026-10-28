// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"lectureflow/internal/colab"
)

// colab-mem (user's decision of 2026-10-27): Colab's memory mode is on by
// default and a switch on the settings page (memory_mode in GET /api/colab
// and PUT /api/colab/settings) turns it off. The kv is written only when
// the user changes the switch, so a database that never had it keeps
// following the default, and "off" survives a restart.
func TestColabMemoryModeSetting(t *testing.T) {
	ce := newColabEnv(t)
	ctx := context.Background()
	kvValue := func() (string, bool) {
		t.Helper()
		v, ok, err := ce.st.GetKV(ctx, colab.KVMemoryMode)
		if err != nil {
			t.Fatal(err)
		}
		return v, ok && v != ""
	}

	res, raw := ce.do("GET", "/api/colab", nil)
	if res.StatusCode != http.StatusOK || !bytes.Contains(raw, []byte(`"memory_mode":true`)) {
		t.Fatalf("initial view: %d %s", res.StatusCode, raw)
	}
	if v, ok := kvValue(); ok {
		t.Fatalf("GET wrote kv %s = %q", colab.KVMemoryMode, v)
	}

	// Other settings, and the switch sent unchanged, leave the kv unset.
	var v ColabView
	ce.call("PUT", "/api/colab/settings", map[string]any{"enabled": true, "memory_mode": true, "accelerator": "L4"}, http.StatusOK, &v)
	if !v.Settings.MemoryMode || !v.Settings.Enabled || v.Settings.Accelerator != "L4" {
		t.Fatalf("view after PUT = %+v", v.Settings)
	}
	if val, ok := kvValue(); ok {
		t.Fatalf("an unchanged switch wrote kv %s = %q", colab.KVMemoryMode, val)
	}

	// Off: stored, shown, and kept by a backend that starts on the same
	// database (an app restart).
	ce.call("PUT", "/api/colab/settings", map[string]any{"memory_mode": false}, http.StatusOK, &v)
	if v.Settings.MemoryMode || !v.Settings.Enabled || v.Settings.Accelerator != "L4" {
		t.Fatalf("view after switching memory mode off = %+v", v.Settings)
	}
	if val, ok := kvValue(); !ok || val != colab.MemoryModeOff {
		t.Fatalf("kv %s = %q %v", colab.KVMemoryMode, val, ok)
	}
	b2, err := colab.New(colab.Options{KV: ce.st})
	if err != nil {
		t.Fatal(err)
	}
	if on, err := b2.MemoryMode(ctx); err != nil || on {
		t.Fatalf("after a restart: memory mode %v %v", on, err)
	}
	ce.call("PUT", "/api/colab/settings", map[string]any{"enabled": false}, http.StatusOK, &v)
	if v.Settings.MemoryMode {
		t.Fatal("changing another setting switched memory mode on again")
	}

	// The diagnostics' colab.json says which mode the user chose.
	got, ok := diagColab(ctx, ce.srv)
	db, _ := json.Marshal(got)
	if !ok || !bytes.Contains(db, []byte(`"memory_mode":false`)) {
		t.Fatalf("colab.json = %s", db)
	}

	// On again.
	ce.call("PUT", "/api/colab/settings", map[string]any{"memory_mode": true}, http.StatusOK, &v)
	if !v.Settings.MemoryMode {
		t.Fatalf("view after switching memory mode on = %+v", v.Settings)
	}
	if val, ok := kvValue(); !ok || val != colab.MemoryModeAuto {
		t.Fatalf("kv %s = %q %v", colab.KVMemoryMode, val, ok)
	}
	if on, err := b2.MemoryMode(ctx); err != nil || !on {
		t.Fatalf("memory mode %v %v", on, err)
	}
}
