-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
-- SPEC-ext 下载队列: a file's connections are min(the user's setting, the
-- profile's request.maxConnectionsPerFile). The profile's limit came with the
-- enqueue only and lived in memory, so a task resumed after a restart or a
-- refresh (needs_refresh) downloaded without it (SPEC-ext conformance
-- review, lead 2026-10-27). A new column of download_task (migration 010);
-- no table of §3.3 changes. 0 = the profile sets no limit.
ALTER TABLE download_task ADD COLUMN max_connections INTEGER NOT NULL DEFAULT 0;
