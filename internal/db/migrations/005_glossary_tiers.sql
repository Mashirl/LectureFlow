-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
-- SPEC-v2.md G1 (named 004_glossary_tiers.sql in the spec; 004 is taken by
-- v1.1, see NOTES.md "v2 冻结与裁决"). The term columns are verbatim from G1/G6.
ALTER TABLE term ADD COLUMN tier TEXT NOT NULL DEFAULT 'enforce'
  CHECK (tier IN ('enforce','reference'));
ALTER TABLE term ADD COLUMN locked INTEGER NOT NULL DEFAULT 0;   -- 用户手动改过的条目，清道夫不得修改
ALTER TABLE term ADD COLUMN common_word INTEGER NOT NULL DEFAULT 0; -- 预筛结果：是否为常见英文词
ALTER TABLE term ADD COLUMN janitor_note TEXT;                    -- 清道夫最近一次处理的理由
ALTER TABLE term ADD COLUMN zh_source TEXT NOT NULL DEFAULT 'extractor'
  CHECK (zh_source IN ('extractor','janitor','user'));             -- 译法来源，见 G6

-- G1: source = manual entries are locked from creation; their zh is the user's.
UPDATE term SET locked = 1, zh_source = 'user' WHERE source = 'manual';

-- role_setting is rebuilt once, here, with every role v2 adds (G3.1
-- glossary_janitor, P7 chapters, P13.2 organizer): SQLite cannot alter a
-- CHECK constraint. Same columns as migrations/003_v1_1.sql.
CREATE TABLE role_setting_v2 (
  role TEXT PRIMARY KEY CHECK (role IN ('syllabus_format','glossary','summary','repair','translate',
                                        'glossary_janitor','chapters','organizer')),
  provider_id TEXT REFERENCES provider(id),
  model TEXT,
  params TEXT NOT NULL DEFAULT '{}'
);
INSERT INTO role_setting_v2 (role, provider_id, model, params)
  SELECT role, provider_id, model, params FROM role_setting;
DROP TABLE role_setting;
ALTER TABLE role_setting_v2 RENAME TO role_setting;
