// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"time"
)

// Accelerators the backend may request: the GPU members of upstream's
// Accelerator enum (google-colab-cli 0.7.5 colab_cli/client.py). TPUs
// (V5E1, V6E1) and CPU (NONE) cannot run faster-whisper on CUDA.
var Accelerators = []string{"T4", "L4", "G4", "A100", "H100"}

// knownAccelerators is the whole upstream enum: values the service may
// report back (anything else in a response is CodeAPIChanged).
var knownAccelerators = []string{"NONE", "G4", "T4", "L4", "A100", "H100", "V5E1", "V6E1"}

// Tiers (V14.1): paid = compute units, the recommended use; free = best
// effort (no balance check, the runtime may be taken back at any time).
// colab-obs-core: the tier follows the account (account.go: paid with a
// paid balance > 0, else free); the user no longer picks it.
const (
	TierPaid = "paid"
	TierFree = "free"
)

// FairWindow is LectureFlow's own fair-use window for one borrowed runtime
// (V14.3, last paragraph): not a Colab limit, and no promise that Colab
// keeps a runtime that long.
const FairWindow = 4 * time.Hour

// Settings of the Colab backend (kv colab.settings).
type Settings struct {
	// Accelerator requested from Colab (Accelerators). Until the user
	// picks one (AcceleratorChosen) it follows the account's level: L4 when
	// paid (user decision 2026-10-27 05:40: faster than a T4 at about the
	// same cost per lecture), T4 when free (its only GPU); stored as T4.
	// A pick the account does not allow (account.go) stays the user's
	// choice while a T4 is borrowed.
	Accelerator string `json:"accelerator"`
	// AcceleratorChosen: the user picked Accelerator on the settings page;
	// a choice is never replaced by the tier's default. Settings stored
	// before this field existed count as not chosen.
	AcceleratorChosen bool `json:"accelerator_chosen,omitempty"`
	// Tier is the account's (colab-obs-core, user decision 2026-10-27: no
	// switch any more): Backend.Settings and UserSettings report it, stored
	// settings and PUT /api/colab/settings may carry it and it is ignored
	// (always stored as ""). TierPaid: the balance is checked before every
	// lecture; TierFree: best effort.
	Tier string `json:"tier"`
	// MaxQueueWaitMin: how long to wait for the accelerator before the
	// lecture goes back to the local devices (V14.3 step 1). 1..720.
	MaxQueueWaitMin int `json:"max_queue_wait_min"`
	// Model and ComputeType of faster-whisper on the remote GPU.
	Model       string `json:"model"`
	ComputeType string `json:"compute_type"`
	// IdleReleaseMin: a runtime with nothing to do is released after this
	// many minutes even when nobody said the queue is empty (0 = right
	// after each lecture). 0..60.
	IdleReleaseMin int `json:"idle_release_min"`
	// WindowCooldownMin: after a runtime was released because its fair-use
	// window was used up, no new one starts for this long (0..1440).
	WindowCooldownMin int `json:"window_cooldown_min"`
	// HighMem asks for the high-RAM machine shape (Colab Pro); ignored for
	// the accelerators that only have one shape (upstream
	// HIGH_MEM_ONLY_ACCELERATORS: L4).
	HighMem bool `json:"high_mem"`
	// FastMode is "极速模式" (user decision 2026-10-27 05:40, off by
	// default): on the larger GPUs (L4, A100, G4, H100) each lecture is cut
	// into parts at silences that transcribe at once; the result differs
	// slightly from the whole lecture's (colab-prof §9.3). No effect on a
	// T4. Off: the larger GPUs transcribe two lectures at once instead.
	FastMode bool `json:"fast_mode,omitempty"`
}

// DefaultAccelerator is the accelerator of a tier (the account's level)
// while the user has not picked one.
func DefaultAccelerator(tier string) string {
	if tier == TierFree {
		return "T4"
	}
	return "L4"
}

// DefaultSettings are the settings without a stored value.
func DefaultSettings() Settings {
	return Settings{
		Accelerator: DefaultAccelerator(TierFree), MaxQueueWaitMin: 30, Model: "large-v3", ComputeType: "float16",
		IdleReleaseMin: 5, WindowCooldownMin: 60,
	}
}

// normalized is the stored form: no tier (the account's), an accelerator
// the user has not picked stored as the free level's (Backend.UserSettings
// shows the account level's default).
func (s Settings) normalized() Settings {
	s.Tier = ""
	if !s.AcceleratorChosen {
		s.Accelerator = DefaultAccelerator(TierFree)
	}
	return s
}

var (
	modelRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,79}$`)
	computeTypes  = []string{"float16", "int8_float16", "int8", "bfloat16", "float32", "int8_bfloat16", "int8_float32"}
	singleShapeAc = []string{"L4", "V5E1", "V6E1"}
)

// Validate reports the first invalid field (CodeSettingsInvalid {field}).
func (s Settings) Validate() error {
	bad := func(field string) error {
		return newErr(CodeSettingsInvalid, map[string]any{"field": field}, kindLocal, "",
			fmt.Errorf("invalid colab setting %s", field))
	}
	switch {
	case !slices.Contains(Accelerators, s.Accelerator):
		return bad("accelerator")
	case s.Tier != "" && s.Tier != TierPaid && s.Tier != TierFree:
		return bad("tier")
	case s.MaxQueueWaitMin < 1 || s.MaxQueueWaitMin > 720:
		return bad("max_queue_wait_min")
	case !modelRe.MatchString(s.Model):
		return bad("model")
	case !slices.Contains(computeTypes, s.ComputeType):
		return bad("compute_type")
	case s.IdleReleaseMin < 0 || s.IdleReleaseMin > 60:
		return bad("idle_release_min")
	case s.WindowCooldownMin < 0 || s.WindowCooldownMin > 1440:
		return bad("window_cooldown_min")
	}
	return nil
}

// LoadSettings reads kv colab.settings; missing or unreadable = defaults,
// and every invalid field falls back to its default.
func LoadSettings(ctx context.Context, kv KV) (Settings, error) {
	def := DefaultSettings()
	v, ok, err := getKV(ctx, kv, KVSettings)
	if err != nil || !ok {
		return def, err
	}
	s := def
	if json.Unmarshal([]byte(v), &s) != nil {
		return def, nil
	}
	// Field by field: one bad value does not throw away the others.
	fix := []struct {
		field string
		reset func()
	}{
		{"accelerator", func() { s.Accelerator = def.Accelerator }},
		{"tier", func() { s.Tier = def.Tier }},
		{"max_queue_wait_min", func() { s.MaxQueueWaitMin = def.MaxQueueWaitMin }},
		{"model", func() { s.Model = def.Model }},
		{"compute_type", func() { s.ComputeType = def.ComputeType }},
		{"idle_release_min", func() { s.IdleReleaseMin = def.IdleReleaseMin }},
		{"window_cooldown_min", func() { s.WindowCooldownMin = def.WindowCooldownMin }},
	}
	for range fix {
		err := s.Validate()
		if err == nil {
			break
		}
		for _, f := range fix {
			if field, _ := codedParam(err, "field"); field == f.field {
				f.reset()
			}
		}
	}
	return s.normalized(), nil
}

// SaveSettings validates and stores s.
func SaveSettings(ctx context.Context, kv KV, s Settings) error {
	s = s.normalized()
	if err := s.Validate(); err != nil {
		return err
	}
	b, _ := json.Marshal(s)
	return kv.SetKV(ctx, KVSettings, string(b))
}

// shape is the assign "shape" query value: "hm" for HighMem where the
// accelerator has a choice (upstream resolve_assign_shape), "" otherwise.
func (s Settings) shape() string {
	if s.HighMem && !slices.Contains(singleShapeAc, s.Accelerator) {
		return "hm"
	}
	return ""
}
