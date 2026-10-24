-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
CREATE TABLE provider (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  type TEXT NOT NULL CHECK (type IN ('openai','anthropic')),
  base_url TEXT NOT NULL,
  api_key TEXT NOT NULL,
  max_concurrency INTEGER NOT NULL DEFAULT 4,
  rpm INTEGER NOT NULL DEFAULT 60,
  balance_api TEXT NOT NULL DEFAULT 'none' CHECK (balance_api IN ('none','deepseek')),
  price_input REAL, price_cache_hit REAL, price_output REAL,   -- 每百万 token，可空
  currency TEXT,
  created_at TEXT NOT NULL
);

CREATE TABLE role_setting (
  role TEXT PRIMARY KEY CHECK (role IN ('syllabus_format','glossary','summary','repair','translate')),
  provider_id TEXT REFERENCES provider(id),
  model TEXT,
  params TEXT NOT NULL DEFAULT '{}'      -- temperature、translate 的 window 等
);

CREATE TABLE usage_daily (
  provider_id TEXT NOT NULL,
  day TEXT NOT NULL,                     -- 本地日期 YYYY-MM-DD
  requests INTEGER NOT NULL DEFAULT 0,
  prompt_tokens INTEGER NOT NULL DEFAULT 0,
  cache_hit_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (provider_id, day)
);

ALTER TABLE lecture ADD COLUMN launched_at TEXT;
ALTER TABLE lecture ADD COLUMN seq INTEGER;          -- 课程内序号，用于引用
ALTER TABLE course ADD COLUMN code TEXT;             -- 课程代码，如 CSC1003
ALTER TABLE job ADD COLUMN config_snapshot TEXT NOT NULL DEFAULT '{}';
ALTER TABLE job ADD COLUMN stale_reason TEXT;
