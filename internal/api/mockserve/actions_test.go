// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"testing"

	"lectureflow/internal/model"
)

// v2 P13.5 (fe-fix): the mock offers regenerate_title in lecture_actions as
// the real views.go does — summary done or stale (the example settings have
// the organizer role configured) — and not before the summary.
func TestLectureActionRegenerateTitle(t *testing.T) {
	srv := testServer(t)
	has := func(id string) bool {
		for _, a := range lectureOf(t, srv.URL, id).LectureActions {
			if a == model.LectureActionRegenerateTitle {
				return true
			}
		}
		return false
	}
	if l := lectureOf(t, srv.URL, lecID(c3631, 1)); stageStatus(&l, model.StageSummary) != model.StatusDone || !has(l.ID) {
		t.Errorf("summary done: %v", l.LectureActions)
	}
	if l := lectureOf(t, srv.URL, lecID(c3631, 7)); stageStatus(&l, model.StageSummary) == model.StatusDone || has(l.ID) {
		t.Errorf("no summary yet (%s): %v", stageStatus(&l, model.StageSummary), l.LectureActions)
	}
}
