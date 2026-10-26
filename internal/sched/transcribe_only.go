// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// "只转写" (SPEC-v3 V4.2 step 4, 3.1.0; pipeline-builder, NOTES.md
// "决定（3.1.0 只转写）"): with no provider for any lecture role the
// lecture still launches; prep and asr run as usual, and the API stages
// wait for a provider instead of failing:
//
//   - Launch with none of summary, repair, translate configured writes the
//     asr snapshot as usual and EMPTY snapshots for the three LLM stages
//     ("recorded later"). Some but not all configured is refused as before
//     (settings.ErrRoleNotConfigured).
//   - A lecture whose three current LLM jobs all have empty snapshots is
//     "deferred". When one of those jobs has its dependencies met, it waits
//     with waiting_for = WaitNoProvider while any of the three roles is not
//     configured; once all are, the three snapshots are written from the
//     current settings (the format Launch writes, A4.1) right before the job
//     is queued (or started, for a queued job), so the stage still reads
//     only its own snapshot.
//   - Course glossary_seed and material material_terms (role glossary, the
//     role's current setting) wait with WaitNoProvider while that role is
//     not configured, and asr does not wait for them meanwhile
//     (state.DepsOpts.SkipTermJobs).
//
// No status or transition is added: a waiting job simply stays pending
// (or queued), like any job whose dependencies are not met.

import (
	"context"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// WaitNoProvider is the waiting_for of a job that needs an LLM provider
// while its role has none ("等待配置服务商"; the front end translates it as
// waiting.no_provider). Kept here rather than in model (frozen) until the
// lead moves it.
const WaitNoProvider = "no_provider"

// lectureLLMStages are the lecture stages that use an LLM role.
var lectureLLMStages = []string{model.StageSummary, model.StageRepair, model.StageTranslate}

func isLectureLLMStage(stage string) bool {
	for _, s := range lectureLLMStages {
		if s == stage {
			return true
		}
	}
	return false
}

// roleReady reports whether role has a provider and a model now
// (settings.RoleTarget, the check Launch and the LLM client use).
func (s *Scheduler) roleReady(role string) bool {
	_, err := s.set.RoleTarget(role)
	return err == nil
}

// lectureRolesReady counts the configured roles of summary, repair and
// translate.
func (s *Scheduler) lectureRolesReady() int {
	n := 0
	for _, st := range lectureLLMStages {
		if s.roleReady(model.RoleOfStage(st)) {
			n++
		}
	}
	return n
}

// TranscribeOnly reports whether a launch now is "只转写": none of the
// lecture roles (summary, repair, translate) is configured.
func (s *Scheduler) TranscribeOnly() bool { return s.lectureRolesReady() == 0 }

// LaunchSnapshots returns the snapshots a launch writes now (A4.1): the
// current settings (settings.CurrentSnapshots) when every lecture role is
// configured; with none configured ("只转写") the asr snapshot of the
// current settings and empty snapshots for every other stage; otherwise
// the error of the first unconfigured role (settings.ErrRoleNotConfigured).
func (s *Scheduler) LaunchSnapshots() (map[string]string, error) {
	snaps, err := s.set.CurrentSnapshots()
	if err == nil || !s.TranscribeOnly() {
		return snaps, err
	}
	return map[string]string{
		model.StagePrep:      model.EmptySnapshot,
		model.StageASR:       s.set.CurrentASR(),
		model.StageSummary:   model.EmptySnapshot,
		model.StageRepair:    model.EmptySnapshot,
		model.StageTranslate: model.EmptySnapshot,
		model.StageExport:    model.EmptySnapshot,
	}, nil
}

// deferredLLM reports whether a lecture's LLM snapshots are still to be
// recorded: its current summary, repair and translate jobs all exist and
// all have empty snapshots (a "只转写" launch, or a rerun or retry of such
// a lecture, whose new jobs keep the empty snapshots).
func deferredLLM(jobs []model.Job) bool {
	n := 0
	for _, j := range jobs {
		if model.IsAuxStage(j.Stage) || !isLectureLLMStage(j.Stage) {
			continue
		}
		if !model.IsEmptySnapshot(j.ConfigSnapshot) {
			return false
		}
		n++
	}
	return n == len(lectureLLMStages)
}

// withoutLLM drops the lecture LLM stages from stages (a deferred lecture's
// rerun and retry decisions leave them out: there is no recorded model to
// keep or to compare, and they take the settings when they start).
func withoutLLM(stages []string) []string {
	out := make([]string, 0, len(stages))
	for _, st := range stages {
		if !isLectureLLMStage(st) {
			out = append(out, st)
		}
	}
	return out
}

// deps is state.DepsWith with the transcribe-only rules: asr does not wait
// for term extraction while the glossary role is not configured, and a job
// whose dependencies are met waits for a provider (providerWait). Read
// only: a deferred lecture's snapshots are written by fillDeferred.
func (s *Scheduler) deps(ctx context.Context, j model.Job) (bool, string, error) {
	opt := state.DepsOpts{}
	if j.Scope == model.ScopeLecture && j.Stage == model.StageASR {
		opt.SkipTermJobs = !s.roleReady(model.RoleGlossary)
	}
	ok, w, err := state.DepsWith(ctx, s.st, j, opt)
	if err != nil || !ok {
		return ok, w, err
	}
	if w, err := s.providerWait(ctx, j); err != nil || w != "" {
		return false, w, err
	}
	return true, "", nil
}

// providerWait is WaitNoProvider for a job that needs a provider which is
// not configured: a lecture LLM job of a deferred lecture while any lecture
// role is not configured; glossary_seed and material_terms while the
// glossary role is not configured. "" otherwise.
func (s *Scheduler) providerWait(ctx context.Context, j model.Job) (string, error) {
	switch {
	case j.Scope == model.ScopeLecture && isLectureLLMStage(j.Stage):
		// a deferred lecture's jobs all have empty snapshots
		if !model.IsEmptySnapshot(j.ConfigSnapshot) || s.lectureRolesReady() == len(lectureLLMStages) {
			return "", nil
		}
		jobs, err := s.st.ListCurrentJobs(ctx, model.ScopeLecture, j.ScopeID)
		if err != nil {
			return "", err
		}
		if deferredLLM(jobs) {
			return WaitNoProvider, nil
		}
	case j.Stage == model.StageGlossarySeed || j.Stage == model.StageMaterialTerms:
		if !s.roleReady(model.RoleOfStage(j.Stage)) {
			return WaitNoProvider, nil
		}
	}
	return "", nil
}

// fillDeferred records the LLM snapshots of j's lecture when it is
// deferred and every lecture role is configured: in one transaction, each
// current LLM job with an empty snapshot gets the current settings' snapshot
// (settings.CurrentSnapshots, with the prompt version, like Launch). It
// returns whether it wrote them. The caller holds the scan lock and has
// checked deps (providerWait) just before.
func (s *Scheduler) fillDeferred(ctx context.Context, j model.Job) (bool, error) {
	if j.Scope != model.ScopeLecture || !isLectureLLMStage(j.Stage) || !model.IsEmptySnapshot(j.ConfigSnapshot) {
		return false, nil
	}
	cur, err := s.set.CurrentSnapshots()
	if err != nil {
		return false, nil // providerWait keeps the job waiting
	}
	filled := false
	err = s.st.Tx(ctx, func(tx db.Store) error {
		filled = false
		jobs, err := tx.ListCurrentJobs(ctx, model.ScopeLecture, j.ScopeID)
		if err != nil {
			return err
		}
		if !deferredLLM(jobs) {
			return nil
		}
		for _, x := range jobs {
			if model.IsAuxStage(x.Stage) || !isLectureLLMStage(x.Stage) || !model.IsEmptySnapshot(x.ConfigSnapshot) {
				continue
			}
			if err := tx.SetJobSnapshot(ctx, x.ID, cur[x.Stage]); err != nil {
				return err
			}
		}
		filled = true
		return nil
	})
	return filled, err
}
