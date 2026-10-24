// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"lectureflow/internal/calendar"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// The past-lecture rule and citations exported before it (lecture kinds
// review, item 3). The rule (lead ruling 1.1: a session kind not set by
// the user, recorded more than 30 days before the term, is numbered as a
// past_lecture) is computed on every read, so it took effect without any
// write — and without the renumbering notice that an automatic kind write
// records (stages, kv renumbered_auto:{course}). A course whose citations
// were exported before the upgrade would see its W??-… citations become P1
// silently. CheckPastRuleRenumbering, run once at startup, records the
// notice those courses would have had.

// CodeRenumberedUpgrade is the code of the notice CheckPastRuleRenumbering
// records (kind note; front end notes.calendar.renumbered_upgrade). Params:
// n (number of changes), changes ([{lecture_id, from, to}], ordered by the
// new label, then id). The same shape as stages.CodeRenumberedAuto without
// lecture (no single lecture caused it).
const CodeRenumberedUpgrade = "calendar.renumbered_upgrade"

// KVPastRuleChecked = "1" once CheckPastRuleRenumbering ran (it then does
// nothing).
const KVPastRuleChecked = "past_rule_renumber_checked"

// CiteChange is one lecture whose citation label changed.
type CiteChange struct {
	LectureID string
	From, To  string
}

// numberWithoutPastRule numbers course c like NumberCourse but without the
// past-lecture rule: the numbering a course had before the rule (the
// stored kinds as they are; calendar.EffectiveKind applies the rule only
// to kinds not set by the user, so every lecture is passed as user-set —
// the source changes nothing else in calendar.LabelKinds). For a course of
// lectures only this is exactly the P13.4 numbering of before lecture
// kinds (calendar's TestLabelKindsAllLecturesUnchanged).
func numberWithoutPastRule(c model.Course, ls []model.Lecture, kinds []model.LectureKind) []calendar.Numbered {
	ts, skips := calendar.CourseTerm(c.TermStart, c.SkipWeeks, time.Local)
	lt := LectureTimes(ls)
	for i := range lt {
		lt[i].KindSource = calendar.KindSourceUser
	}
	return calendar.LabelKinds(lt, ts, skips, calendar.NumberOptions{Kinds: KindCatalog(kinds), Numbering: c.Numbering})
}

// PastRuleCiteChanges lists the lectures of course c (every lecture, in
// ListLectures order) whose citation label differs between the numbering
// without the past-lecture rule and NumberCourse, ordered by the new label,
// then id. Empty when the rule changes nothing (no term start, nothing
// recorded long before it, kinds set by the user).
func PastRuleCiteChanges(c model.Course, ls []model.Lecture, kinds []model.LectureKind) []CiteChange {
	before := numberWithoutPastRule(c, ls, kinds)
	after := NumberCourse(c, ls, kinds)
	var out []CiteChange
	for i := range after {
		if before[i].CiteLabel != after[i].CiteLabel {
			out = append(out, CiteChange{LectureID: after[i].ID, From: before[i].CiteLabel, To: after[i].CiteLabel})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].To != out[j].To {
			return out[i].To < out[j].To
		}
		return out[i].LectureID < out[j].LectureID
	})
	return out
}

// PastRuleNotice is the coded notice for changes (CodeRenumberedUpgrade;
// detail = the Chinese sentence with the changes).
func PastRuleNotice(changes []CiteChange) model.ErrorInfo {
	list := make([]map[string]any, len(changes))
	texts := make([]string, len(changes))
	for i, ch := range changes {
		list[i] = map[string]any{"lecture_id": ch.LectureID, "from": ch.From, "to": ch.To}
		texts[i] = ch.From + " → " + ch.To
	}
	return model.ErrorInfo{
		Code:   CodeRenumberedUpgrade,
		Params: map[string]any{"n": len(changes), "changes": list},
		Detail: fmt.Sprintf("录制时间早于开课 30 天以上的讲座改按往期讲座编号后，%d 节讲座的编号发生变化（%s），之前导出的引用将与新编号不一致",
			len(changes), strings.Join(texts, "、")),
	}
}

// CheckPastRuleRenumbering is the one-time check at startup after the
// past-lecture rule shipped: for every course whose citations were
// exported (kv cite_exported:{course} not blank, P13.6) and whose citation
// labels the rule changed (PastRuleCiteChanges), record(tx, course, the
// notice) is called to store the notice (cmd appends it to the course's
// renumbering notices). Guarded by kv KVPastRuleChecked, set in the same
// transaction: the second call does nothing, and a failure leaves nothing
// half done (tried again on the next start). Returns the number of courses
// recorded.
func CheckPastRuleRenumbering(ctx context.Context, st db.Store, record func(ctx context.Context, tx db.Store, courseID string, notice model.ErrorInfo) error) (int, error) {
	n := 0
	err := st.Tx(ctx, func(tx db.Store) error {
		n = 0
		if v, ok, err := tx.GetKV(ctx, KVPastRuleChecked); err != nil || (ok && v == "1") {
			return err
		}
		cs, err := tx.ListCourses(ctx)
		if err != nil {
			return err
		}
		var kinds []model.LectureKind
		for _, c := range cs {
			if v, ok, err := tx.GetKV(ctx, model.KVCiteExportPrefix+c.ID); err != nil {
				return err
			} else if !ok || strings.TrimSpace(v) == "" {
				continue
			}
			if kinds == nil {
				if kinds, err = tx.ListLectureKinds(ctx); err != nil {
					return err
				}
			}
			ls, err := tx.ListLectures(ctx, c.ID)
			if err != nil {
				return err
			}
			changes := PastRuleCiteChanges(c, ls, kinds)
			if len(changes) == 0 {
				continue
			}
			if err := record(ctx, tx, c.ID, PastRuleNotice(changes)); err != nil {
				return err
			}
			n++
		}
		return tx.SetKV(ctx, KVPastRuleChecked, "1")
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}
