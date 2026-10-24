-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
-- Lecture kinds, per-kind numbering and slides bound to a teaching week
-- (user request 2026-10-23, lead freeze .claude/kinds-brief.md section 2).
-- DDL verbatim from the freeze. 3b's asr devices move to 008.
CREATE TABLE lecture_kind (
  id TEXT PRIMARY KEY,              -- 内置：lecture、tutorial、seminar、past_lecture、material；自定义：ULID
  builtin INTEGER NOT NULL DEFAULT 0,
  code TEXT NOT NULL UNIQUE,        -- 1–3 个大写字母
  name TEXT,                        -- 只有自定义类型有；内置类型的名称由界面翻译
  grp TEXT NOT NULL CHECK (grp IN ('session','material')),
  ord INTEGER NOT NULL DEFAULT 0
);
INSERT INTO lecture_kind (id, builtin, code, name, grp, ord) VALUES
  ('lecture', 1, 'L', NULL, 'session', 10),
  ('tutorial', 1, 'T', NULL, 'session', 20),
  ('seminar', 1, 'S', NULL, 'session', 30),
  ('past_lecture', 1, 'P', NULL, 'material', 40),
  ('material', 1, 'M', NULL, 'material', 50);
ALTER TABLE lecture ADD COLUMN kind TEXT;              -- NULL = 未判断（按 lecture 处理）
ALTER TABLE lecture ADD COLUMN kind_source TEXT NOT NULL DEFAULT 'none'
  CHECK (kind_source IN ('none','filename','llm','user'));
ALTER TABLE course ADD COLUMN numbering TEXT NOT NULL DEFAULT 'auto'
  CHECK (numbering IN ('auto','always','never'));
ALTER TABLE material ADD COLUMN bind_week INTEGER;     -- 绑定的教学周（1–60）；与 lecture_id 不同时非空（由 Store 保证）
