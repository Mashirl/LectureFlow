-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
-- SPEC-ext 下载队列, security review L3 (NOTES "sec-fetch"): download_task
-- kept the path of the page a lecture was sent from (page_path), which
-- nothing reads and which may name the recording or carry a token. Only
-- the page's host stays (page_host); the column of migration 010 is kept,
-- always ''. No table of §3.3 changes.
UPDATE download_task SET page_path = '' WHERE page_path <> '';
