-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
-- v1.1 data backfill for databases created by v1 (lead, NOTES.md "v1.1 冻结").
-- v1 started every lecture on upload (SPEC §0.1), so existing lectures are
-- launched at their creation time. seq numbers each course's lectures by
-- creation order (SPEC-v1.1 A11.3). On a fresh database both are no-ops.
UPDATE lecture SET launched_at = created_at WHERE launched_at IS NULL;
UPDATE lecture SET seq = (
  SELECT COUNT(*) FROM lecture l2
  WHERE l2.course_id = lecture.course_id
    AND (l2.created_at < lecture.created_at OR (l2.created_at = lecture.created_at AND l2.id <= lecture.id))
) WHERE seq IS NULL;
