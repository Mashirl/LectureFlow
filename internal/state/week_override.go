// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import "lectureflow/internal/model"

// Week overrides (user request 2026-10-27, course-ux; model.WeekOverride).
// Numbering reads them through LectureTimes, material sets through
// LectureWeek; both apply an override only when the course has a term
// start.

// OverrideConflicts reports whether the user setting a lecture's kind to a
// kind of group (model.KindGroup*) contradicts its week override, which
// then goes (the later instruction wins): a supplement is a teaching
// material, so a session kind ends it; a make-up is a session of its week,
// so a material kind ends it. Kinds that the program decides (file names,
// the title task) never end an override: the override is the user's.
func OverrideConflicts(o *model.WeekOverride, group string) bool {
	if o == nil {
		return false
	}
	switch o.Kind {
	case model.WeekOverrideSupplement:
		return group == model.KindGroupSession
	case model.WeekOverrideMakeup:
		return group == model.KindGroupMaterial
	}
	return false
}

// KindGroupOf is the group of kind id in the catalog kinds (the built-in
// lecture's group, session, when the id is not in it, as
// calendar.EffectiveKind treats an unknown kind).
func KindGroupOf(kinds []model.LectureKind, id string) string {
	for _, k := range kinds {
		if k.ID == id {
			return k.Group
		}
	}
	return model.KindGroupSession
}
