-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
PRAGMA foreign_keys = ON;

CREATE TABLE course (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  glossary_version INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);

CREATE TABLE syllabus (
  course_id TEXT PRIMARY KEY REFERENCES course(id) ON DELETE CASCADE,
  filename TEXT NOT NULL,
  path TEXT NOT NULL,
  text TEXT
);

CREATE TABLE lecture (
  id TEXT PRIMARY KEY,
  course_id TEXT NOT NULL REFERENCES course(id) ON DELETE CASCADE,
  title TEXT NOT NULL,
  media_path TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE material (
  id TEXT PRIMARY KEY,
  course_id TEXT NOT NULL REFERENCES course(id) ON DELETE CASCADE,
  lecture_id TEXT REFERENCES lecture(id) ON DELETE CASCADE,
  filename TEXT NOT NULL,
  path TEXT NOT NULL,
  pages_md TEXT,                      -- JSON 字符串数组
  created_at TEXT NOT NULL
);

CREATE TABLE term (
  id TEXT PRIMARY KEY,
  course_id TEXT NOT NULL REFERENCES course(id) ON DELETE CASCADE,
  en TEXT NOT NULL,
  en_key TEXT NOT NULL,               -- lower(trim(en))，内部连续空白压成一个空格
  zh TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL CHECK (source IN ('syllabus','material','review','manual')),
  enabled INTEGER NOT NULL DEFAULT 1,
  freq INTEGER NOT NULL DEFAULT 0,
  UNIQUE (course_id, en_key)
);

CREATE TABLE sentence (
  lecture_id TEXT NOT NULL REFERENCES lecture(id) ON DELETE CASCADE,
  idx INTEGER NOT NULL,
  start_ms INTEGER NOT NULL,
  end_ms INTEGER NOT NULL,
  asr_text TEXT NOT NULL,
  low_conf_words TEXT NOT NULL DEFAULT '[]',
  repaired_text TEXT NOT NULL,
  human_text TEXT,
  text TEXT NOT NULL,
  zh TEXT,
  zh_status TEXT NOT NULL DEFAULT 'none' CHECK (zh_status IN ('none','ok','qa_failed','stale')),
  zh_hash TEXT,
  qa_error TEXT,
  PRIMARY KEY (lecture_id, idx)
);

CREATE TABLE patch (
  id TEXT PRIMARY KEY,
  lecture_id TEXT NOT NULL REFERENCES lecture(id) ON DELETE CASCADE,
  sentence_idx INTEGER NOT NULL,
  before TEXT NOT NULL,
  after TEXT NOT NULL,
  class TEXT NOT NULL CHECK (class IN ('auto','absorb','decide')),
  reason TEXT NOT NULL DEFAULT '',
  review TEXT NOT NULL CHECK (review IN ('none','pending','accepted','rejected','edited')),
  edited_text TEXT,
  created_at TEXT NOT NULL
);
CREATE INDEX patch_review ON patch(lecture_id, review);

CREATE TABLE summary (
  lecture_id TEXT PRIMARY KEY REFERENCES lecture(id) ON DELETE CASCADE,
  text TEXT NOT NULL,
  input_hash TEXT NOT NULL
);

CREATE TABLE job (
  id TEXT PRIMARY KEY,
  scope TEXT NOT NULL CHECK (scope IN ('course','material','lecture')),
  scope_id TEXT NOT NULL,
  stage TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pending','queued','running','done','failed','cancelled','stale')),
  input_hash TEXT,
  attempt INTEGER NOT NULL DEFAULT 0,
  error TEXT,
  progress REAL NOT NULL DEFAULT 0,
  is_current INTEGER NOT NULL DEFAULT 1,
  stats TEXT NOT NULL DEFAULT '{}',   -- JSON：token 用量、缓存命中、耗时
  created_at TEXT NOT NULL,
  started_at TEXT,
  finished_at TEXT
);
CREATE UNIQUE INDEX job_current ON job(scope, scope_id, stage) WHERE is_current = 1;
