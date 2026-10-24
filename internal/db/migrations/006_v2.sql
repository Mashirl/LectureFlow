-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
-- SPEC-v2.md part 2 (named 005_v2.sql in the spec; renumbered, see NOTES.md
-- "v2 冻结与裁决"). Column definitions verbatim from P4.2, P12.2, P12.5,
-- P13.2 and P13.3.1; offpeak_only is the P12.6 switch (no DDL in the spec).
-- role_setting was already rebuilt with chapters and organizer in 005.

-- P4.2 word timestamps: JSON [{"w","s","e"}], ms from lecture start; NULL = none.
ALTER TABLE sentence ADD COLUMN words TEXT;

-- P12.2 / D3 time-of-day pricing (JSON, NULL = no tariff); P12.6 dispatch only off-peak.
ALTER TABLE provider ADD COLUMN tariff TEXT;
ALTER TABLE provider ADD COLUMN offpeak_only INTEGER NOT NULL DEFAULT 0;

-- P12.5 per-request estimated cost, accumulated at request time.
ALTER TABLE usage_daily ADD COLUMN estimated_cost REAL NOT NULL DEFAULT 0;

-- P13.2 calendar numbering.
ALTER TABLE course ADD COLUMN term_start TEXT;        -- 第 1 教学周的周一，YYYY-MM-DD，可空
ALTER TABLE course ADD COLUMN skip_weeks TEXT NOT NULL DEFAULT '[]';  -- 不计入教学周的日历周（以该周周一的日期表示），JSON 数组
ALTER TABLE lecture ADD COLUMN recorded_at TEXT;       -- 录制时间（本地时间，精确到分钟），可空
ALTER TABLE lecture ADD COLUMN recorded_at_source TEXT NOT NULL DEFAULT 'none'
  CHECK (recorded_at_source IN ('none','metadata','filename','user'));
ALTER TABLE lecture ADD COLUMN title_zh TEXT;
ALTER TABLE lecture ADD COLUMN title_source TEXT NOT NULL DEFAULT 'filename'
  CHECK (title_source IN ('filename','llm','user'));
ALTER TABLE lecture ADD COLUMN blurb_en TEXT;           -- 一句话简介
ALTER TABLE lecture ADD COLUMN blurb_zh TEXT;

-- P13.3.1 per-course filename rule.
ALTER TABLE course ADD COLUMN filename_rule TEXT;   -- JSON，可空
ALTER TABLE lecture ADD COLUMN original_filename TEXT NOT NULL DEFAULT '';
