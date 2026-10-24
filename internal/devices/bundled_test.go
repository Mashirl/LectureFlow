// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBundledClip(t *testing.T) {
	if _, _, ok := BundledClip(""); ok {
		t.Error("empty dir must not be ok")
	}
	dir := t.TempDir()
	if _, _, ok := BundledClip(dir); ok {
		t.Error("missing audio must not be ok")
	}
	audio := filepath.Join(dir, BundledAudioFile)
	if err := os.WriteFile(audio, []byte("fLaC"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, ref, ok := BundledClip(dir)
	if !ok || a != audio || ref != "" {
		t.Errorf("no reference: %q %q %v", a, ref, ok)
	}
	if err := os.WriteFile(filepath.Join(dir, BundledReferenceFile), []byte(bom+"I purpose, in return.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ref, ok = BundledClip(dir); !ok || ref != "I purpose, in return." {
		t.Errorf("reference %q %v", ref, ok)
	}

	// The repository's sample (SPEC-v3 V4.3).
	a, ref, ok = BundledClip(filepath.Join("..", "..", "assets", "sample"))
	if !ok || !filepath.IsAbs(a) {
		t.Fatalf("assets/sample: %q %v", a, ok)
	}
	if _, n := WER(ref, ""); n != 174 {
		t.Errorf("reference words %d, want 174", n)
	}
}
