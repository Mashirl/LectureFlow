-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
-- Week overrides (user request 2026-10-27, course-ux; model/week_override.go):
-- a teaching material hung under a week as its supplement, or a lecture made
-- the content of another week ("补"). A new table: no table of §3.3 (or of a
-- later migration) changes. One row per lecture, deleted with it. The week
-- range is that of material.bind_week (1–60, model.MaxBindWeek).
CREATE TABLE lecture_week_override (
  lecture_id TEXT PRIMARY KEY REFERENCES lecture(id) ON DELETE CASCADE,
  week INTEGER NOT NULL CHECK (week BETWEEN 1 AND 60),
  kind TEXT NOT NULL CHECK (kind IN ('supplement','makeup')),
  created_at TEXT NOT NULL
);
