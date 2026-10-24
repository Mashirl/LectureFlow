// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"encoding/json"
	"testing"

	"lectureflow/internal/model"
)

// JoinNote and NoteParts (v2 P14.3 convention): main part + params.extra,
// flattened, also after a JSON round trip (job.stats, reports).
func TestJoinNoteAndParts(t *testing.T) {
	if JoinNote() != nil || NoteParts(nil) != nil {
		t.Fatal("empty")
	}
	inner := JoinNote(model.ErrorInfo{Code: "b", Params: map[string]any{"n": 2}}, model.ErrorInfo{Code: "c"})
	n := JoinNote(model.ErrorInfo{Code: "a"}, *inner)
	codes := func(ps []model.ErrorInfo) (out []string) {
		for _, p := range ps {
			out = append(out, p.Code)
		}
		return out
	}
	if got := codes(NoteParts(n)); len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("parts %v", got)
	}
	if _, ok := inner.Params["n"]; !ok || NoteParts(inner)[0].Params["extra"] != nil {
		t.Fatalf("inner changed or main kept extra: %+v", inner)
	}
	b, _ := json.Marshal(n)
	var back model.ErrorInfo
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	ps := NoteParts(&back)
	if got := codes(ps); len(got) != 3 || got[1] != "b" || ps[1].Params["n"] != float64(2) {
		t.Fatalf("after JSON %+v", ps)
	}
	if !IsInterrupted(&interruptedError) || IsInterrupted(nil) {
		t.Fatal("IsInterrupted")
	}
	legacy := model.ErrInterruptedByRestart
	if !IsInterrupted(&legacy) {
		t.Fatal("IsInterrupted: text written before v2")
	}
}
