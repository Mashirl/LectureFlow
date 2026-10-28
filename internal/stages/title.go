// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"lectureflow/internal/calendar"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/llm"
	lfmedia "lectureflow/internal/media"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// KVTitleForcePrefix: title_force:{lecture_id} = "1" while a "重新生成标题与
// 简介" that may overwrite a user title is pending (P13.5). Set by
// RequestTitleForce before the title task is queued; the task reads and
// clears it when it writes its result. "" = not set.
const KVTitleForcePrefix = "title_force:"

// RequestTitleForce marks the next title run of the lecture as allowed to
// overwrite a user title (P13.5: the user confirmed 重新生成). Call it before
// queueing the title task.
func RequestTitleForce(ctx context.Context, st db.Store, lectureID string) error {
	return st.SetKV(ctx, KVTitleForcePrefix+lectureID, "1")
}

// auxAttempts is the number of requests of an auxiliary task whose answer
// is unparsable or fails its check: P7 "校验失败重试一次" (also used for the
// title and filename_rule tasks).
const auxAttempts = 2

// retryProblems is a check error that lists its problems for the retry.
type retryProblems interface{ Problems() []string }

// askJSONRetry is askJSON with auxAttempts tries whose retry tells the
// model why its answer was rejected (lead ruling after the phase 2
// acceptance): the same system prompt and user message, followed by
// ctxbuild.WithRetryFeedback with the problems of the rejection — the
// error's Problems() when it has them (a retryProblems), else its text; an
// unparsable answer gets ctxbuild.NotJSONProblem. last is the reason of the
// last rejected answer; transport and context errors are returned at once
// (unusable = false).
func askJSONRetry[T any](ctx context.Context, c llm.Client, role, system, user string, check func(*T) error) (v T, unusable bool, err error) {
	var last error
	u := user
	reject := func(problems []string) { u = ctxbuild.WithRetryFeedback(user, problems) }
	for range auxAttempts {
		var a T
		res, err := c.Complete(ctx, role, system, u, true)
		if err != nil {
			return a, false, err
		}
		if err := llm.ParseJSON(res.Text, &a); err != nil {
			last = err
			reject([]string{ctxbuild.NotJSONProblem(err.Error())})
			continue
		}
		if check != nil {
			if err := check(&a); err != nil {
				last = err
				var rp retryProblems
				if errors.As(err, &rp) {
					reject(rp.Problems())
				} else {
					reject([]string{err.Error()})
				}
				continue
			}
		}
		return a, false, nil
	}
	var zero T
	return zero, true, last
}

// titleStage is the auxiliary task title (SPEC-v2 P13.5): a title and a
// one-sentence description from the summary. Scope lecture, role organizer
// (current setting: auxiliary tasks have no snapshot; the run records its
// target and prompt version in its job, recordPromptVersion). Not a
// pipeline stage: no input hash, no invalidation; a failure leaves the
// lecture unchanged.
//
// Lecture kinds (lead freeze .claude/kinds-brief.md 1.2, 1.3): the prompt
// (version ctxbuild.TitlePromptVersion) also asks for the lecture's kind,
// chosen from the kind catalog; it is written with source llm unless a
// file-name or user kind is set (calendar.KindOverrides). An id that is
// not in the catalog is dropped; it never fails the task.
type titleStage struct{ apiStage }

func (s *titleStage) Name() string { return model.StageTitle }

func (s *titleStage) InputHash(context.Context, string) (string, error) { return "", nil }

// titleAnswer is the P13.5 answer.
type titleAnswer struct {
	TitleEn string `json:"title_en"`
	TitleZh string `json:"title_zh"`
	BlurbEn string `json:"blurb_en"`
	BlurbZh string `json:"blurb_zh"`
	// Kind is a kind id of the catalog (lecture kinds); "", an unknown id or
	// a value that is not a string = no kind from this answer (the optional
	// field never makes the answer unusable).
	Kind looseString `json:"kind"`
}

// titleRejected lists what an answer breaks of the P13.5 limits, the
// titles' problems apart from the descriptions' (a failed task still writes
// valid descriptions, see Run). The texts are English: they are the retry
// feedback (askJSONRetry) and the job error's detail.
type titleRejected struct{ titles, blurbs []string }

func (e *titleRejected) Problems() []string {
	return append(append([]string(nil), e.titles...), e.blurbs...)
}

func (e *titleRejected) Error() string { return strings.Join(e.Problems(), "; ") }

// checkTitleAnswer applies the limits of the P13.5 prompt; the error is a
// *titleRejected naming every field that breaks them, with the counts.
func checkTitleAnswer(a *titleAnswer) error {
	// no bidirectional controls in a title (test-media I8)
	a.TitleEn = oneLineTrim(lfmedia.StripBidi(a.TitleEn))
	a.TitleZh = oneLineTrim(lfmedia.StripBidi(a.TitleZh))
	a.BlurbEn = oneLineTrim(a.BlurbEn)
	a.BlurbZh = oneLineTrim(a.BlurbZh)
	a.Kind = looseString(strings.TrimSpace(string(a.Kind)))
	var r titleRejected
	words := func(list *[]string, field, v string, limit int) {
		switch n := ctxbuild.EnglishWords(v); {
		case v == "":
			*list = append(*list, field+" is empty")
		case n > limit:
			*list = append(*list, fmt.Sprintf("%s has %d words; at most %d are allowed: %q", field, n, limit, v))
		}
	}
	zh := func(list *[]string, field, v string, limit int) {
		switch n := ctxbuild.ZhLen(v); {
		case v == "":
			*list = append(*list, field+" is empty")
		case n > limit:
			*list = append(*list, fmt.Sprintf("%s has %d characters; at most %d are allowed (each Chinese character or punctuation mark counts as one, an English term as one): %q", field, n, limit, v))
		}
	}
	words(&r.titles, "title_en", a.TitleEn, ctxbuild.TitleWords)
	zh(&r.titles, "title_zh", a.TitleZh, ctxbuild.TitleZhLen)
	words(&r.blurbs, "blurb_en", a.BlurbEn, ctxbuild.BlurbWords)
	zh(&r.blurbs, "blurb_zh", a.BlurbZh, ctxbuild.BlurbZhLen)
	if len(r.titles)+len(r.blurbs) > 0 {
		return &r
	}
	return nil
}

// oneLineTrim collapses whitespace (line breaks included) and trims.
func oneLineTrim(s string) string { return strings.Join(strings.Fields(s), " ") }

func (s *titleStage) Run(ctx context.Context, lectureID string, progress func(float64)) error {
	d := s.d
	in, err := d.titleInput(ctx, lectureID)
	if err != nil {
		return err
	}
	d.recordPromptVersion(ctx, s.role, ctxbuild.TitlePromptVersion)
	req := ctxbuild.TitleRequest(in)
	progress(0.1)
	// The last answer whose descriptions keep the limits although its titles
	// do not (lead ruling after the phase 2 acceptance: a whole lecture's
	// title came back too long twice).
	var blurbs *titleAnswer
	check := func(a *titleAnswer) error {
		err := checkTitleAnswer(a)
		var tr *titleRejected
		if errors.As(err, &tr) && len(tr.blurbs) == 0 {
			b := *a
			blurbs = &b
		}
		return err
	}
	a, unusable, err := askJSONRetry(ctx, d.LLM, s.role, req.System, req.User, check)
	if err != nil {
		if !unusable {
			return err
		}
		// Both answers failed: the valid descriptions are still written
		// (P13.5: “简介每次都写入”), with the kind of that answer (it does
		// not depend on the titles' length); the titles are not, and the
		// task fails.
		if blurbs != nil {
			var kw kindWrite
			if wErr := d.Store.Tx(context.WithoutCancel(ctx), func(tx db.Store) error {
				kw = kindWrite{}
				if err := tx.SetLectureBlurb(ctx, lectureID, &blurbs.BlurbEn, &blurbs.BlurbZh); err != nil {
					return err
				}
				var err error
				kw, err = d.applyTitleKind(ctx, tx, lectureID, string(blurbs.Kind))
				return err
			}); wErr != nil {
				return wErr
			}
			d.events().Changed("lecture", lectureID)
			d.kindEvents(kw)
		}
		return model.Errorf("title.invalid_output", map[string]any{"attempts": auxAttempts}, err)
	}
	progress(0.9)
	kw, err := d.applyTitle(context.WithoutCancel(ctx), lectureID, a)
	if err != nil {
		return err
	}
	d.events().Changed("lecture", lectureID)
	d.kindEvents(kw)
	progress(1)
	return nil
}

// titleInput loads the P13.5 input: the summary, the Weekly Topics entry of
// the lecture's teaching week (only when the week can be computed and the
// syllabus has one; never other weeks, G6 rule 5), the original file name
// and the kind catalog (lecture kinds).
func (d Deps) titleInput(ctx context.Context, lectureID string) (ctxbuild.TitleInput, error) {
	var in ctxbuild.TitleInput
	err := d.Store.Tx(ctx, func(tx db.Store) error {
		in = ctxbuild.TitleInput{}
		l, err := tx.GetLecture(ctx, lectureID)
		if err != nil {
			return err
		}
		sum, err := tx.GetSummary(ctx, lectureID)
		switch {
		case errors.Is(err, db.ErrNotFound) || (err == nil && strings.TrimSpace(sum.Text) == ""):
			return model.Errorf("title.no_summary", nil, errors.New("the lecture has no summary yet"))
		case err != nil:
			return err
		}
		in.Summary = sum.Text
		in.OriginalFilename = l.OriginalFilename
		if in.OriginalFilename == "" && titleSource(l.TitleSource) == model.TitleSourceFilename {
			in.OriginalFilename = l.Title // lectures uploaded before v2: the title is the file name
		}
		c, err := tx.GetCourse(ctx, l.CourseID)
		if err != nil {
			return err
		}
		ls, err := tx.ListLectures(ctx, l.CourseID)
		if err != nil {
			return err
		}
		if in.Kinds, err = tx.ListLectureKinds(ctx); err != nil {
			return err
		}
		// the teaching week of any kind (a material kind is numbered
		// without one, so Numbered.OK does not say there is a week)
		n, ok := courseLabels(c, ls, in.Kinds)[lectureID]
		if !ok || n.Week < 1 {
			return nil
		}
		syl, err := tx.GetSyllabus(ctx, l.CourseID)
		switch {
		case errors.Is(err, db.ErrNotFound):
			return nil
		case err != nil:
			return err
		}
		if syl.Text != nil {
			in.SyllabusEntry = ctxbuild.SyllabusWeekEntry(*syl.Text, n.Week)
		}
		return nil
	})
	return in, err
}

// applyTitle writes the answer (P13.5 应用): the blurbs always; the titles
// when title_source is filename or llm (not set by the user), or when the
// user confirmed a regeneration (title_force) — then the source becomes llm;
// the kind as applyTitleKind decides. Reading title_source, kind_source and
// the force flag in the same transaction as the write means an edit made by
// the user while the request ran is kept.
func (d Deps) applyTitle(ctx context.Context, lectureID string, a titleAnswer) (kindWrite, error) {
	var kw kindWrite
	err := d.Store.Tx(ctx, func(tx db.Store) error {
		kw = kindWrite{}
		l, err := tx.GetLecture(ctx, lectureID)
		if err != nil {
			return err
		}
		force := false
		if v, ok, err := tx.GetKV(ctx, KVTitleForcePrefix+lectureID); err != nil {
			return err
		} else if ok && v != "" {
			force = true
			if err := tx.SetKV(ctx, KVTitleForcePrefix+lectureID, ""); err != nil {
				return err
			}
		}
		if err := tx.SetLectureBlurb(ctx, lectureID, &a.BlurbEn, &a.BlurbZh); err != nil {
			return err
		}
		if kw, err = d.applyTitleKind(ctx, tx, lectureID, string(a.Kind)); err != nil {
			return err
		}
		if titleSource(l.TitleSource) == model.TitleSourceUser && !force {
			return nil
		}
		return tx.SetLectureTitles(ctx, lectureID, a.TitleEn, &a.TitleZh, model.TitleSourceLLM)
	})
	return kw, err
}

// applyTitleKind writes the kind of a title answer inside tx (lead ruling
// 1.2: user > filename > llm > none): kind (an id of the current catalog;
// matched ignoring case) becomes the lecture's kind with source llm when
// calendar.KindOverrides(llm, the stored source) allows — never over a kind
// from the file name or the user; an empty kind or one not in the catalog
// changes nothing. "Regenerate" (title_force) does not change this. A
// catalog kind is remembered in either case (state.RememberLLMKind): a
// lecture whose file-name or user kind goes falls back to it.
//
// The write renumbers the course (lead ruling on K2's report): when the
// course's citations were exported (P13.6) and the citation label of a
// lecture other than this one changed, a renumbering notice is recorded
// (recordRenumberNotice); this lecture's own label changing is not told.
// The result says what to announce after the commit (kindEvents).
func (d Deps) applyTitleKind(ctx context.Context, tx db.Store, lectureID, kind string) (kindWrite, error) {
	if strings.TrimSpace(kind) == "" {
		return kindWrite{}, nil
	}
	cat, err := listKinds(ctx, tx)
	if err != nil {
		return kindWrite{}, err
	}
	id := cat.resolve(kind)
	if id == "" {
		slog.Info("title: kind not in the catalog, dropped", "lecture", lectureID, "kind", kind)
		return kindWrite{}, nil
	}
	l, err := tx.GetLecture(ctx, lectureID)
	if err != nil {
		return kindWrite{}, err
	}
	src := kindSource(l.KindSource)
	if !calendar.KindOverrides(model.KindSourceLLM, src) || (l.Kind == id && src == model.KindSourceLLM) {
		// not written (a file-name or user kind holds the lecture, or it
		// is already this kind): the judgement is remembered, so that the
		// lecture falls back to it when the stronger source goes
		// (state.FallbackKind; lecture kinds review item 1)
		return kindWrite{}, state.RememberLLMKind(ctx, tx, lectureID, id)
	}
	before, err := courseLabelsTx(ctx, tx, l.CourseID)
	if err != nil {
		return kindWrite{}, err
	}
	if err := state.WriteLectureKind(ctx, tx, l, id, model.KindSourceLLM); err != nil {
		return kindWrite{}, err
	}
	after, err := courseLabelsTx(ctx, tx, l.CourseID)
	if err != nil {
		return kindWrite{}, err
	}
	kw := kindWrite{courseID: l.CourseID, renumbered: renumberedOthers(before, after, lectureID)}
	if len(kw.renumbered) > 0 {
		if kw.notified, err = d.recordRenumberNotice(ctx, tx, l.CourseID, lectureID, before, after, kw.renumbered); err != nil {
			return kindWrite{}, err
		}
	}
	return kw, nil
}
