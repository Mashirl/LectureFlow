// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/stages"
	"lectureflow/internal/state"
)

// "修复断句" (textrepair-int): the asr rerun with text_only
// (ModelChoiceRequest.TextOnly) repairs a lecture's stored transcript
// without transcribing it again (sched.RerunTextRepair). The asr stage
// offers it (model.ActionRerunTextOnly in allowed_actions) only where it can
// help: a done or stale transcription that never went through the repair
// (transcribed before it existed), with word timestamps, of an English
// lecture, whose text is degenerate (stages.MeasureTextDegeneration: 10% of
// the words or more in unpunctuated stretches, or an all-caps span).

// textRepairOffers caches the degeneration verdict per lecture: reading the
// sentences on every lecture view would be wasteful, and the ASR text of a
// lecture changes only with a new asr run (a new job, or a new attempt).
type textRepairOffers struct {
	mu sync.Mutex
	m  map[string]textRepairOffer // lecture id →
}

type textRepairOffer struct {
	key   string // the asr job it was measured for (id, attempt, finished_at)
	offer bool
}

// offerKey identifies the transcript an asr job left behind.
func offerKey(j model.Job) string {
	fin := ""
	if j.FinishedAt != nil {
		fin = *j.FinishedAt
	}
	return j.ID + "|" + strconv.Itoa(j.Attempt) + "|" + fin
}

// textRepairOffer reports whether lecture l's asr stage offers "修复断句":
// the asr job is done or stale and its transcript never went through the
// repair (no textrepair counters: transcribed before it existed; a "修复断句"
// run has them), the transcript has word timestamps (hasWords), the
// snapshot's language is English, and the stored text is degenerate enough
// (measured once per transcript, cached). A store error: not offered.
func (s *Server) textRepairOffer(ctx context.Context, l model.Lecture, jobs []model.Job, hasWords bool) bool {
	asr, ok := findJob(jobs, model.StageASR)
	if !ok || !hasWords || (asr.Status != model.StatusDone && asr.Status != model.StatusStale) {
		return false
	}
	if t := asr.Stats.ASRTiming; t != nil && t.Postprocess != nil && t.Postprocess.TextRepair != nil {
		return false
	}
	if asr.Stats.ASRMode == model.ASRModeTextRepair {
		return false
	}
	snap, ok := model.ParseASRSnapshot(asr.ConfigSnapshot)
	if !ok || !model.TextRepairLanguage(snap.Language) {
		return false
	}
	key := offerKey(asr)
	s.trOffers.mu.Lock()
	c, hit := s.trOffers.m[l.ID]
	s.trOffers.mu.Unlock()
	if hit && c.key == key {
		return c.offer
	}
	ss, err := s.st.ListSentences(ctx, l.ID, 0, 0)
	if err != nil {
		return false
	}
	offer := stages.MeasureTextDegeneration(ss).Worthwhile()
	s.trOffers.mu.Lock()
	if s.trOffers.m == nil {
		s.trOffers.m = map[string]textRepairOffer{}
	}
	s.trOffers.m[l.ID] = textRepairOffer{key: key, offer: offer}
	s.trOffers.mu.Unlock()
	return offer
}

// withTextRepairAction adds model.ActionRerunTextOnly to the asr stage view
// of a lecture when textRepairOffer holds (state.WithTextRepair: done or
// stale, rerun allowed now).
func (s *Server) withTextRepairAction(ctx context.Context, v *Lecture, l model.Lecture, jobs []model.Job) {
	asr, ok := findJob(jobs, model.StageASR)
	if !ok {
		return
	}
	for i := range v.Stages {
		st := &v.Stages[i]
		if st.Stage != model.StageASR || !containsString(st.AllowedActions, model.ActionRerun) {
			continue
		}
		if s.textRepairOffer(ctx, l, jobs, v.HasWords) {
			st.AllowedActions = state.WithTextRepair(asr, st.AllowedActions, true)
		}
	}
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// rerunTextRepair is POST /api/lectures/{id}/stages/asr/rerun with
// text_only: the transcript must have word timestamps and the lecture must
// be English (409 textrepair.no_words / textrepair.language; the
// degeneration threshold is only the offer's); then sched.RerunTextRepair
// (409 as any rerun; needs_model_choice for the stages after asr).
func (s *Server) rerunTextRepair(ctx context.Context, lectureID, choice string) error {
	asr, err := s.st.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageASR)
	if errors.Is(err, db.ErrNotFound) {
		if _, lerr := s.st.GetLecture(ctx, lectureID); lerr != nil {
			return lerr
		}
		return fmt.Errorf("%w: the lecture has no asr job", state.ErrConflict)
	}
	if err != nil {
		return err
	}
	if snap, ok := model.ParseASRSnapshot(asr.ConfigSnapshot); !ok || !model.TextRepairLanguage(snap.Language) {
		return &model.CodedError{Info: model.ErrorInfo{Code: stages.CodeTextRepairLanguage, Params: map[string]any{"language": snap.Language}},
			Err: fmt.Errorf("%w: 修复断句只用于英语讲座（这节是 %q）", state.ErrConflict, snap.Language)}
	}
	n, err := s.st.CountSentences(ctx, lectureID)
	if err != nil {
		return err
	}
	words := n > 0
	for _, idx := range []int{0, n - 1} {
		if !words {
			break
		}
		x, err := s.st.GetSentence(ctx, lectureID, idx)
		if err != nil {
			return err
		}
		words = len(x.Words) > 0
	}
	if !words {
		return &model.CodedError{Info: model.ErrorInfo{Code: stages.CodeTextRepairNoWords},
			Err: fmt.Errorf("%w: 这节讲座保存的转写没有逐词时间，无法只修复断句", state.ErrConflict)}
	}
	return s.sc.RerunTextRepair(ctx, lectureID, choice)
}
