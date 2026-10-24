-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
-- SPEC-v2.md part 3 I6: transcription devices (named 006_asr_devices.sql in
-- the spec; renumbered to 008 because 005-007 were taken, see NOTES.md
-- "v2 冻结与裁决" 1 and "v3b 冻结（core-builder）"). DDL verbatim from I6.
-- Migrating the legacy CUDA settings to the cuda:0 row, detecting devices
-- and writing their defaults happen in Go at startup (internal/devices,
-- kv-guarded), not here: this migration changes no existing row.
CREATE TABLE asr_device (
  id TEXT PRIMARY KEY,                 -- 'cuda:0'、'ov:GPU'、'ov:NPU'、'ov:CPU'
  backend TEXT NOT NULL CHECK (backend IN ('faster-whisper','openvino')),
  label TEXT NOT NULL,                 -- 显示名，如 “NVIDIA GeForce RTX 5070 Ti Laptop GPU”、“Intel AI Boost (NPU)”
  enabled INTEGER NOT NULL DEFAULT 1,
  model TEXT NOT NULL,                 -- 如 large-v3、large-v3-turbo
  precision TEXT NOT NULL,             -- float16、int8、int4 等
  max_parallel INTEGER NOT NULL DEFAULT 1,
  priority INTEGER NOT NULL DEFAULT 0, -- 自动选择时的优先级，数值大者优先
  detected_at TEXT,
  last_benchmark TEXT                  -- JSON：最近一次自测结果（I9）
);
