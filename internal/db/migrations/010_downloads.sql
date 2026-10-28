-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
-- SPEC-ext 下载队列 (.claude/ext-brief.md §6.2): the browser extension's
-- download tasks. A new table; no table of §3.3 changes. The lecture is
-- created only when a download is done, so a task is not a lecture state.
-- Credentials never reach this table: no cookie, no referer, no query
-- string (signatures and tokens live there). tracks_json holds per track
-- {kind, format, role, label, host, path} only.
-- AUTOINCREMENT: task ids are given to the extension (status pushes, open)
-- and must not be reused after a delete.
CREATE TABLE download_task (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  platform TEXT NOT NULL,                       -- profile id, or 'generic'
  session_id TEXT NOT NULL,                     -- dedup key with platform
  profile_id TEXT NOT NULL,
  profile_version TEXT NOT NULL DEFAULT '',
  override_disabled INTEGER NOT NULL DEFAULT 0, -- the user overrode "下载已关闭"
  course_id TEXT NOT NULL REFERENCES course(id) ON DELETE CASCADE,
  title TEXT NOT NULL,
  recorded_at TEXT,                             -- local "YYYY-MM-DDTHH:MM"; NULL = unknown
  duration_sec INTEGER NOT NULL DEFAULT 0,
  page_host TEXT NOT NULL DEFAULT '',
  page_path TEXT NOT NULL DEFAULT '',
  tracks_json TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('queued','downloading','merging','importing','done','failed','needs_refresh','cancelled')),
  error_code TEXT,                              -- why failed / needs_refresh; a note when done
  error_params TEXT,                            -- JSON object
  bytes_done INTEGER NOT NULL DEFAULT 0,
  bytes_total INTEGER NOT NULL DEFAULT -1,      -- -1 = unknown
  lecture_id TEXT REFERENCES lecture(id) ON DELETE SET NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE (platform, session_id)
);
CREATE INDEX download_task_course ON download_task(course_id);
CREATE INDEX download_task_lecture ON download_task(lecture_id);
