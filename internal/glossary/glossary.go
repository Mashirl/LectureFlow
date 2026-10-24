// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package glossary holds the deterministic glossary rules of SPEC-v2 G1–G3
// and G6: the common-word pre-screen (G2), its fallback tiering, and the
// janitor report bookkeeping shared by the glossary_review stage and the
// HTTP API (report, undo).
//
// Signatures FROZEN by the lead; bodies by pipeline-A (.claude/v2-brief.md).
// No function here calls an LLM.
package glossary

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// IsCommonWord is the G2 pre-screen: true iff en is a single word, not all
// capital letters, without digits or hyphens, and its lower-case form (or
// that form without a trailing "s" / "es") is in commonwords.txt.
// (A trailing "ies" is also read as "y": NOTES.md, pipeline-A.)
func IsCommonWord(en string) bool {
	s := strings.TrimSpace(en)
	if s == "" || strings.ContainsFunc(s, unicode.IsSpace) || allCapitals(s) {
		return false
	}
	if strings.ContainsFunc(s, func(r rune) bool { return unicode.IsDigit(r) || r == '-' || unicode.Is(unicode.Pd, r) }) {
		return false
	}
	return isCommon(strings.ToLower(s))
}

// Fallback applies the G2 fallback rule to the course's unlocked terms using
// tx (the caller's transaction): common_word = 1 → reference; of two terms
// whose en_key differ only by plural form, the plural is disabled. It
// returns the changes as report items (Action reference / disable) and does
// not bump glossary_version.
//
// Details (NOTES.md, pipeline-A): common_word is recomputed for every term
// first (written when it differs, locked terms included); only enabled,
// unlocked terms are changed; a term that is both a plural and a common word
// is disabled; the plural's suffix must be lower case ("MACs", not "DES");
// each changed term gets a janitor_note. Items are ordered by en_key.
func Fallback(ctx context.Context, tx db.Store, courseID string) ([]model.JanitorItem, error) {
	terms, err := tx.ListTerms(ctx, courseID)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]*model.Term, len(terms))
	for i := range terms {
		byKey[termKey(terms[i])] = &terms[i]
	}
	var items []model.JanitorItem
	for i := range terms {
		t := &terms[i]
		before := model.StateOf(normalized(*t))
		cw := IsCommonWord(t.En)
		write := cw != t.CommonWord
		t.CommonWord = cw
		action, note := "", ""
		if !t.Locked && t.Enabled {
			if s, ok := singularOf(*t, byKey); ok {
				t.Enabled = false
				action, note = model.JanitorDisable, notePlural(s.En)
			} else if cw && tierOf(*t) != model.TierReference {
				t.Tier = model.TierReference
				action, note = model.JanitorReference, noteCommonWord
			}
		}
		if action != "" {
			t.JanitorNote = note
			items = append(items, model.JanitorItem{
				TermID: t.ID, En: t.En, Action: action, Reason: note,
				Before: before, After: model.StateOf(normalized(*t)),
			})
			write = true
		}
		if write {
			if err := tx.UpdateTerm(ctx, *t); err != nil {
				return nil, err
			}
		}
	}
	return items, nil
}

// Report notes of fallback runs (JanitorReport.Note).
const (
	// NoteFallbackManual: "整理术语表" without a configured janitor (ruling 5).
	NoteFallbackManual = "未配置术语表清道夫，按常用词规则整理"
	// NoteFallbackMerge: the fallback MergeTerms runs when no janitor is configured.
	NoteFallbackMerge = "合并术语后按常用词规则整理（未配置术语表清道夫）"
)

// NoteFallbackFailed is the note of the fallback run after the janitor
// failed for good (appendix D2).
func NoteFallbackFailed(reason string) string {
	return "术语表清道夫调用失败（" + reason + "），已按常用词规则整理"
}

// RunFallback applies Fallback in one transaction, bumps glossary_version by
// exactly 1 when something changed, and stores the run (Fallback = true) with
// SaveRunReport: the versioned report when something changed, the last run
// always. changed tells the caller to run the course invalidation
// (state.StaleJanitor).
func RunFallback(ctx context.Context, st db.Store, courseID string, now time.Time) (rep model.JanitorReport, changed bool, err error) {
	return RunFallbackNote(ctx, st, courseID, now, NoteFallbackManual, NoteInfo(CodeNoteFallbackManual, nil))
}

// RunFallbackNote is RunFallback with the report note and its coded form
// (v2 P14.3) given (the glossary_review stage writes why the janitor could
// not run, D2).
func RunFallbackNote(ctx context.Context, st db.Store, courseID string, now time.Time, note string, info *model.ErrorInfo) (rep model.JanitorReport, changed bool, err error) {
	err = st.Tx(ctx, func(tx db.Store) error {
		c, err := tx.GetCourse(ctx, courseID)
		if err != nil {
			return err
		}
		items, err := Fallback(ctx, tx, courseID)
		if err != nil {
			return err
		}
		rep = model.JanitorReport{
			CourseID: courseID, FromVersion: c.GlossaryVersion, Version: c.GlossaryVersion,
			CreatedAt: Timestamp(now), Fallback: true, Note: note, NoteInfo: info, Items: items,
		}
		changed = len(items) > 0
		if changed {
			if rep.Version, err = tx.BumpGlossaryVersion(ctx, courseID); err != nil {
				return err
			}
		}
		return SaveRunReport(ctx, tx, rep)
	})
	if err != nil {
		return model.JanitorReport{}, false, err
	}
	return rep, changed, nil
}

// SaveRunReport stores the report of a run (janitor or fallback, lead after
// the first Gate review): a run that changed something is stored as the
// versioned report and becomes the latest (SaveReport, used by undo); every
// run, with or without changes, is stored as the course's last run
// (kv janitor_last_run:{course}, LastRun: time, model, rejected, note).
func SaveRunReport(ctx context.Context, st db.Store, rep model.JanitorReport) error {
	if rep.Items == nil {
		rep.Items = []model.JanitorItem{}
	}
	b, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	return st.Tx(ctx, func(tx db.Store) error {
		if len(rep.Items) > 0 {
			if err := SaveReport(ctx, tx, rep); err != nil {
				return err
			}
		}
		return tx.SetKV(ctx, lastRunKey(rep.CourseID), string(b))
	})
}

// LastRun returns the course's latest janitor or fallback run, also one
// that changed nothing (Items may be empty). Without a stored run it falls
// back to LatestReport (reports stored before last runs were recorded);
// db.ErrNotFound when there is neither.
func LastRun(ctx context.Context, st db.Store, courseID string) (model.JanitorReport, error) {
	v, ok, err := st.GetKV(ctx, lastRunKey(courseID))
	if err != nil {
		return model.JanitorReport{}, err
	}
	if !ok {
		return LatestReport(ctx, st, courseID)
	}
	var rep model.JanitorReport
	if err := json.Unmarshal([]byte(v), &rep); err != nil {
		return model.JanitorReport{}, fmt.Errorf("kv %s: %w", lastRunKey(courseID), err)
	}
	if rep.Items == nil {
		rep.Items = []model.JanitorItem{}
	}
	return rep, nil
}

// SaveReport stores rep under kv janitor_report:{course}:{rep.Version} and
// points janitor_report_latest:{course} at it.
func SaveReport(ctx context.Context, st db.Store, rep model.JanitorReport) error {
	if rep.Items == nil {
		rep.Items = []model.JanitorItem{}
	}
	b, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	return st.Tx(ctx, func(tx db.Store) error {
		if err := tx.SetKV(ctx, reportKey(rep.CourseID, rep.Version), string(b)); err != nil {
			return err
		}
		return tx.SetKV(ctx, latestKey(rep.CourseID), strconv.Itoa(rep.Version))
	})
}

// LatestReport returns the course's latest report (db.ErrNotFound when none).
func LatestReport(ctx context.Context, st db.Store, courseID string) (model.JanitorReport, error) {
	v, ok, err := st.GetKV(ctx, latestKey(courseID))
	if err != nil {
		return model.JanitorReport{}, err
	}
	if !ok {
		return model.JanitorReport{}, db.ErrNotFound
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return model.JanitorReport{}, fmt.Errorf("kv %s = %q: %w", latestKey(courseID), v, err)
	}
	return Report(ctx, st, courseID, n)
}

// Report returns the report stored for that glossary version (db.ErrNotFound when none).
func Report(ctx context.Context, st db.Store, courseID string, version int) (model.JanitorReport, error) {
	v, ok, err := st.GetKV(ctx, reportKey(courseID, version))
	if err != nil {
		return model.JanitorReport{}, err
	}
	if !ok {
		return model.JanitorReport{}, db.ErrNotFound
	}
	var rep model.JanitorReport
	if err := json.Unmarshal([]byte(v), &rep); err != nil {
		return model.JanitorReport{}, fmt.Errorf("kv %s: %w", reportKey(courseID, version), err)
	}
	if rep.Items == nil {
		rep.Items = []model.JanitorItem{}
	}
	return rep, nil
}

// Undo reverts one report item (G3.5) in one transaction: the term goes back
// to item.Before (tier, enabled, zh, zh_source) and is locked;
// glossary_version + 1; the item is marked Undone in the stored report.
// A merge that filled its target's empty zh has a second item for the target
// (Action fix_zh, Into = the merged term's id); undoing the merge item also
// restores the target when that item is not undone yet and the target is
// still exactly as the run left it (unlocked, state = the item's After): both
// are marked undone, the version still goes up by 1. A target the user has
// changed since stays as it is and its item stays not undone (lead, Gate
// review round 2). Undoing the target's item alone restores only the target.
// Errors: db.ErrNotFound (report, item or term gone), ErrAlreadyUndone.
// The caller runs the course invalidation (state.StaleJanitor) afterwards.
//
// Restored terms get their janitor_note cleared: they are back to the
// earlier state and now belong to the user (locked). The latest-report
// pointer is unchanged; the last run is updated too when it is the same run.
func Undo(ctx context.Context, st db.Store, courseID string, version int, termID string, now time.Time) (model.JanitorReport, error) {
	var rep model.JanitorReport
	err := st.Tx(ctx, func(tx db.Store) error {
		var err error
		if rep, err = Report(ctx, tx, courseID, version); err != nil {
			return err
		}
		k := -1
		for i, it := range rep.Items {
			if it.TermID == termID {
				k = i
				break
			}
		}
		if k < 0 {
			return db.ErrNotFound
		}
		it := &rep.Items[k]
		if it.Undone {
			return ErrAlreadyUndone
		}
		undo := []*model.JanitorItem{it}
		if it.Action == model.JanitorMerge {
			for i := range rep.Items {
				f := &rep.Items[i]
				if f.Action != model.JanitorFixZh || f.Into != it.TermID || f.TermID != it.Into || f.Undone {
					continue
				}
				switch t, err := tx.GetTerm(ctx, f.TermID); {
				case errors.Is(err, db.ErrNotFound):
				case err != nil:
					return err
				case t.CourseID == courseID && !t.Locked && model.StateOf(normalized(t)) == f.After:
					undo = append(undo, f)
				}
			}
		}
		for _, u := range undo {
			t, err := tx.GetTerm(ctx, u.TermID)
			if err != nil {
				return err
			}
			if t.CourseID != courseID {
				return db.ErrNotFound
			}
			b := u.Before
			t.Tier, t.Enabled, t.Zh, t.ZhSource = b.Tier, b.Enabled, b.Zh, b.ZhSource
			t.Locked = true
			t.JanitorNote = ""
			if err := tx.UpdateTerm(ctx, normalized(t)); err != nil {
				return err
			}
			u.Undone = true
			u.UndoneAt = Timestamp(now)
		}
		if _, err := tx.BumpGlossaryVersion(ctx, courseID); err != nil {
			return err
		}
		raw, err := json.Marshal(rep)
		if err != nil {
			return err
		}
		if err := tx.SetKV(ctx, reportKey(courseID, version), string(raw)); err != nil {
			return err
		}
		// keep the course header's change count right when the last run is this one
		last, ok, err := tx.GetKV(ctx, lastRunKey(courseID))
		if err != nil || !ok {
			return err
		}
		var lr model.JanitorReport
		if json.Unmarshal([]byte(last), &lr) == nil && lr.Version == rep.Version && lr.CreatedAt == rep.CreatedAt && len(lr.Items) > 0 {
			return tx.SetKV(ctx, lastRunKey(courseID), string(raw))
		}
		return nil
	})
	if err != nil {
		return model.JanitorReport{}, err
	}
	return rep, nil
}

// ErrAlreadyUndone is returned by Undo for an item that was already undone.
var ErrAlreadyUndone = errors.New("already undone")

// BackfillCommonWords computes common_word for every term of every course
// once (kv v2.common_word_backfilled). It changes no tier and no version.
func BackfillCommonWords(ctx context.Context, st db.Store) (int, error) {
	n := 0
	err := st.Tx(ctx, func(tx db.Store) error {
		n = 0
		if _, done, err := tx.GetKV(ctx, model.KVCommonWordBackfill); err != nil || done {
			return err
		}
		cs, err := tx.ListCourses(ctx)
		if err != nil {
			return err
		}
		for _, c := range cs {
			ts, err := tx.ListTerms(ctx, c.ID)
			if err != nil {
				return err
			}
			for _, t := range ts {
				if cw := IsCommonWord(t.En); cw != t.CommonWord {
					t.CommonWord = cw
					if err := tx.UpdateTerm(ctx, t); err != nil {
						return err
					}
					n++
				}
			}
		}
		return tx.SetKV(ctx, model.KVCommonWordBackfill, "1")
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// Timestamp formats a report time (UTC RFC3339).
func Timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func reportKey(courseID string, version int) string {
	return model.KVJanitorReportPrefix + courseID + ":" + strconv.Itoa(version)
}

func latestKey(courseID string) string { return model.KVJanitorLatestPrefix + courseID }

func lastRunKey(courseID string) string { return model.KVJanitorLastRunPrefix + courseID }

// tierOf reads "" as enforce (model.Term.Tier).
func tierOf(t model.Term) string {
	if t.Tier == "" {
		return model.TierEnforce
	}
	return t.Tier
}

// normalized fills the defaults of the v2 columns ("" tier → enforce,
// "" zh_source → extractor), so that report states compare as stored.
func normalized(t model.Term) model.Term {
	t.Tier = tierOf(t)
	if t.ZhSource == "" {
		t.ZhSource = model.ZhSourceExtractor
	}
	return t
}
