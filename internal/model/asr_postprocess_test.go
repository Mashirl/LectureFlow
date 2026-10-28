// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package model

import (
	"encoding/json"
	"slices"
	"testing"
)

// colab-obs-core: timing.postprocess.split of a "极速模式" result.json.
func TestASRSplit(t *testing.T) {
	var p ASRPostprocess
	raw := `{"redecode":{"windows":3,"unpunctuated":0,"uppercase":0,"redecoded":0,"accepted":0,"redecode_ms":0},` +
		`"split":{"parts":2,"cuts_ms":[415800]}}`
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if s := p.Split.Checked(); s == nil || s.Parts != 2 || !slices.Equal(s.CutsMS, []int64{415800}) {
		t.Fatalf("split %+v", s)
	}
	for _, bad := range []*ASRSplit{nil, {Parts: 1}, {Parts: 5, CutsMS: []int64{1, 2, 3, 4}}, {Parts: 3, CutsMS: []int64{5}},
		{Parts: 3, CutsMS: []int64{9, 4}}, {Parts: 2, CutsMS: []int64{0}}} {
		if bad.Checked() != nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	// Whole lectures keep their bytes: no split field.
	b, _ := json.Marshal(ASRPostprocess{})
	if string(b) != `{}` {
		t.Errorf("JSON %s", b)
	}
	b, _ = json.Marshal(ASRPostprocess{Split: (&ASRSplit{Parts: 4, CutsMS: []int64{1, 2, 3}}).Checked()})
	if string(b) != `{"split":{"parts":4,"cuts_ms":[1,2,3]}}` {
		t.Errorf("JSON %s", b)
	}
}
