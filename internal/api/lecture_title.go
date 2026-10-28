// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"path/filepath"
	"strings"

	"lectureflow/internal/media"
)

// defaultLectureTitle is the title of a lecture whose file name gives none
// (a file called ".mp4"): the name a download without a title is imported
// as (fetch.fileStem) and an export without a title is named
// (stages.SafeFileName). A lecture title is never empty (PATCH refuses
// one, lecture.title_empty).
const defaultLectureTitle = "lecture"

// cleanLectureTitle is a title as it is stored (test-media I8): without
// bidirectional control characters (media.StripBidi; they would make an
// export's file name show another extension) and without surrounding
// white space.
func cleanLectureTitle(s string) string {
	return strings.TrimSpace(media.StripBidi(s))
}

// titleFromFilename is the title a file name gives: the name without its
// extension, cleaned (cleanLectureTitle); defaultLectureTitle when nothing
// is left.
func titleFromFilename(name string) string {
	if t := cleanLectureTitle(strings.TrimSuffix(name, filepath.Ext(name))); t != "" {
		return t
	}
	return defaultLectureTitle
}
