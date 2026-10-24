-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
-- SPEC-v3 V3.1 (asr-mlx: backend mlx, device mlx:gpu) and V14 (Google Colab:
-- backend colab): extend the asr_device backend CHECK of 008. SQLite cannot
-- alter a CHECK constraint, so the table is rebuilt with the same columns
-- and rows; nothing references asr_device. Colab's own tables (credential,
-- sessions) come in 010 with step 8b.
CREATE TABLE asr_device_v3 (
  id TEXT PRIMARY KEY,                 -- 'cuda:0'、'ov:GPU'、'ov:NPU'、'ov:CPU'、'mlx:gpu'、'colab:remote'
  backend TEXT NOT NULL CHECK (backend IN ('faster-whisper','openvino','mlx','colab')),
  label TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  model TEXT NOT NULL,
  precision TEXT NOT NULL,
  max_parallel INTEGER NOT NULL DEFAULT 1,
  priority INTEGER NOT NULL DEFAULT 0,
  detected_at TEXT,
  last_benchmark TEXT
);
INSERT INTO asr_device_v3 (id, backend, label, enabled, model, precision, max_parallel, priority, detected_at, last_benchmark)
  SELECT id, backend, label, enabled, model, precision, max_parallel, priority, detected_at, last_benchmark FROM asr_device;
DROP TABLE asr_device;
ALTER TABLE asr_device_v3 RENAME TO asr_device;
