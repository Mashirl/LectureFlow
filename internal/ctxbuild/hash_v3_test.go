// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import "testing"

// SPEC-v2 I6 (phase 3b freeze): the v3 asr hash depends on the wav and the
// language only, and never equals a v2 hash of the same audio.
func TestASRInputHashV3(t *testing.T) {
	h := ASRInputHashV3("abc", "en")
	if len(h) != 64 || h != ASRInputHashV3("abc", "en") {
		t.Fatalf("not a stable SHA-256: %q", h)
	}
	if h == ASRInputHashV3("abd", "en") || h == ASRInputHashV3("abc", "de") {
		t.Fatal("wav or language change does not change the hash")
	}
	for _, p := range []ASRParams{{Model: "large-v3", ComputeType: "auto", Language: "en"}, {Language: "en"}} {
		if h == ASRInputHash("abc", p) {
			t.Fatalf("v3 equals v2 for %+v", p)
		}
	}
	// Separator-safe: moving characters between the parts changes it.
	if ASRInputHashV3("ab", "cen") == ASRInputHashV3("abc", "en") {
		t.Fatal("parts are not length-prefixed")
	}
}

// TestASRInputHashV3Pinned (task D): the formula is stored in job.input_hash
// of every new asr run, so it must never change: SHA-256 of
// "3:asr" "2:v3" "3:abc" "2:en".
func TestASRInputHashV3Pinned(t *testing.T) {
	const want = "31b900ec13bf1f0c87b0b7d9f4ed43cd63744af6765d38e824184e2d816c1299"
	if got := ASRInputHashV3("abc", "en"); got != want {
		t.Errorf("ASRInputHashV3 = %s, want %s", got, want)
	}
	// language only: an empty language is still a distinct part
	if ASRInputHashV3("abc", "") == want || ASRInputHashV3("abc", "") != hashParts("asr", "v3", "abc", "") {
		t.Error("empty language")
	}
}
