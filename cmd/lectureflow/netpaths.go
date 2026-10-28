// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"errors"
	"log/slog"

	"lectureflow/internal/db"
	"lectureflow/internal/state"
)

// networkPaths counts the stored paths that name another computer.
type networkPaths struct {
	lectures, materials, syllabi int
}

// scanNetworkPaths looks once at startup for stored paths that name another
// computer — a UNC share, a device path or a URL in lecture.media_path,
// material.path or syllabus.path (state.StoredPathUnsafe; security review
// 2, M3) — and logs how many there are. Paths inside the data directory
// (state.SetDataDir, set first by buildApp) do not count even when it is
// on a network share: they are the user's own data. Only a database edited
// elsewhere or imported by an older version has the others (LectureFlow
// stores plain paths, the data import refuses such ones). They stay in the
// database, the user's data, but are treated as missing files and never
// opened: listing a lecture would stat its media, and on Windows that
// connects to the server with the user's NTLM credentials. Each path is
// logged once here (Warn), so the checks that meet it later do not log it
// again. Nothing is opened; a database error only ends the scan (logged).
func scanNetworkPaths(ctx context.Context, st db.Store) networkPaths {
	var n networkPaths
	courses, err := st.ListCourses(ctx)
	if err != nil {
		slog.Warn("startup: network path scan skipped", "err", err)
		return n
	}
	for _, c := range courses {
		lectures, err := st.ListLectures(ctx, c.ID)
		if err != nil {
			slog.Warn("startup: network path scan ended early", "course", c.ID, "err", err)
			return n
		}
		for _, l := range lectures {
			if state.StoredPathUnsafe(state.PathLectureMedia, l.ID, l.MediaPath) {
				n.lectures++
			}
		}
		materials, err := st.ListMaterialRefs(ctx, c.ID)
		if err != nil {
			slog.Warn("startup: network path scan ended early", "course", c.ID, "err", err)
			return n
		}
		for _, m := range materials {
			if state.StoredPathUnsafe(state.PathMaterial, m.ID, m.Path) {
				n.materials++
			}
		}
		sy, err := st.GetSyllabus(ctx, c.ID)
		switch {
		case errors.Is(err, db.ErrNotFound):
		case err != nil:
			slog.Warn("startup: network path scan ended early", "course", c.ID, "err", err)
			return n
		case state.StoredPathUnsafe(state.PathSyllabus, c.ID, sy.Path):
			n.syllabi++
		}
	}
	if total := n.lectures + n.materials + n.syllabi; total > 0 {
		slog.Warn("startup: stored paths pointing to another computer are treated as missing files and never opened",
			"total", total, "lectures", n.lectures, "materials", n.materials, "syllabi", n.syllabi)
	} else {
		slog.Info("startup: no stored path points to another computer", "courses", len(courses))
	}
	return n
}
