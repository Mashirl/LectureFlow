// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"encoding/json"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/stages"
	"lectureflow/internal/state"
)

// maxRenumberNotices is the bound stages keeps on a course's renumbering
// notices (the newest are kept).
const maxRenumberNotices = 20

// checkPastRuleRenumbering is the one-time startup check of the
// past-lecture rule (lecture kinds review, item 3; state.
// CheckPastRuleRenumbering): a course whose exported citations the rule
// renumbered gets a notice (code state.CodeRenumberedUpgrade) appended to
// its renumbering notices (kv renumbered_auto:{course}, the list the
// course page shows and the user dismisses), dated now. Returns the number
// of courses noticed.
func checkPastRuleRenumbering(ctx context.Context, st db.Store, now time.Time) (int, error) {
	at := now.UTC().Format(time.RFC3339)
	return state.CheckPastRuleRenumbering(ctx, st, func(ctx context.Context, tx db.Store, courseID string, info model.ErrorInfo) error {
		list, err := stages.RenumberNotices(ctx, tx, courseID)
		if err != nil {
			return err
		}
		list = append(list, stages.RenumberNotice{ErrorInfo: info, At: at})
		if len(list) > maxRenumberNotices {
			list = list[len(list)-maxRenumberNotices:]
		}
		b, err := json.Marshal(list)
		if err != nil {
			return err
		}
		return tx.SetKV(ctx, stages.KVRenumberedAutoPrefix+courseID, string(b))
	})
}
