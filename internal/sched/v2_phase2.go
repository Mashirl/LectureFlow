// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// v2 phase 2 scheduler rules for the auxiliary tasks (SPEC-v2 P13.5, P7,
// P8.1, P13.3.2; lead rulings 1 and 2 of phase 2, .claude/v2-brief.md §9).
//
// Auxiliary tasks run like any job (registered stages, resource pools,
// state.Deps for their dependencies), with these differences:
//   - an API auxiliary task uses its role's CURRENT setting (providerOf);
//   - a failure is never retried automatically (finish; state.Recover);
//   - they never become stale (state.CheckStaleJob) and never block the
//     deletion of a lecture: DeleteLecture cancels them first;
//   - they are queued at fixed moments: title after every completion of
//     summary (queueTitle), filename_rule after uploads (RequestFilenameRule,
//     debounced), stream when the player asks for media (EnsureStream);
//     chapters and the explicit regenerations through EnqueueAux.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// DefaultFilenameRuleDebounce is the quiet time after the last upload of a
// batch before the course's filename rule is generated (ruling 2).
const DefaultFilenameRuleDebounce = 5 * time.Second

// streamNoAutoRun are the kv stream states (model.StreamState.Status, the
// values of api.StreamInfo.Status) of a finished attempt without a
// stream.mp4: EnsureStream does not start another one by itself.
var streamNoAutoRun = map[string]bool{"audio_only": true, "no_space": true, "failed": true}

// fnRequest is a pending RequestFilenameRule of one course.
type fnRequest struct {
	at time.Time // the latest request (the debounce restarts with each)
	// waited: when the request was due, the course's filename_rule was
	// running, so it waits for that run to end. A run that then ends
	// failed or cancelled drops the request (no automatic retry); a done
	// run gets the trigger evaluated again (names uploaded meanwhile).
	waited bool
}

// EnqueueAux is state.EnqueueAux plus the events and a scan: the API uses
// it for "生成章节", "重新生成标题与简介" and "重新识别文件名格式". queued is
// false when a run was already pending, queued or running. The role is not
// checked here (the API answers 409 when it is not configured).
func (s *Scheduler) EnqueueAux(ctx context.Context, scope, scopeID, stage string) (bool, error) {
	queued, err := state.EnqueueAux(ctx, s.st, scope, scopeID, stage)
	if err != nil {
		return false, err
	}
	if queued {
		s.emitCurrent(ctx, scope, scopeID, stage)
		s.Kick()
	}
	return queued, nil
}

func (s *Scheduler) emitCurrent(ctx context.Context, scope, scopeID, stage string) {
	if j, err := s.st.GetCurrentJob(context.WithoutCancel(ctx), scope, scopeID, stage); err == nil {
		s.emitJob(ctx, j)
	}
}

// queueTitle queues the lecture's title task after its summary completed
// (P13.5, ruling 2: every completion, also after a rerun; a title already
// waiting or running is not queued twice). Nothing when the organizer role
// is not configured.
func (s *Scheduler) queueTitle(lectureID string) {
	ctx := context.Background()
	var queued bool
	err := s.st.Tx(ctx, func(tx db.Store) error {
		ok, err := state.RoleConfigured(ctx, tx, model.AuxRole(model.StageTitle))
		if err != nil || !ok {
			return err
		}
		queued, err = state.EnqueueAux(ctx, tx, model.ScopeLecture, lectureID, model.StageTitle)
		return err
	})
	if err != nil {
		if !errors.Is(err, db.ErrNotFound) { // the lecture was deleted meanwhile
			slog.Error("queue the title after the summary", "lecture", lectureID, "err", err)
		}
		return
	}
	if queued {
		slog.Info("title queued after the summary", "lecture", lectureID)
		s.emitCurrent(ctx, model.ScopeLecture, lectureID, model.StageTitle)
	}
}

// RequestFilenameRule is what the API calls after a lecture was uploaded
// (ruling 2). The trigger of P13.3.2 is evaluated once the course has had
// no new request for FilenameRuleDebounce (default 5 s), so a batch upload
// queues at most one run: the organizer role must be configured and
// state.FilenameRuleWanted true. A request that comes due while the
// course's rule is being generated waits for that run (see fnRequest).
func (s *Scheduler) RequestFilenameRule(courseID string) {
	s.mu.Lock()
	s.fnReq[courseID] = &fnRequest{at: s.now()}
	s.mu.Unlock()
}

// processFilenameRules handles the filename-rule requests that are due
// (called by every scan, before the jobs are listed).
func (s *Scheduler) processFilenameRules(ctx context.Context) {
	now := s.now()
	type due struct {
		id     string
		at     time.Time
		waited bool
	}
	var list []due
	s.mu.Lock()
	for id, r := range s.fnReq {
		if now.Sub(r.at) >= s.fnDebounce {
			list = append(list, due{id, r.at, r.waited})
		}
	}
	s.mu.Unlock()
	for _, d := range list {
		keep, queued, err := s.filenameRuleDue(ctx, d.id, d.waited)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("filename rule trigger", "course", d.id, "err", err)
		}
		s.mu.Lock()
		if r := s.fnReq[d.id]; r != nil && r.at.Equal(d.at) { // no newer request meanwhile
			if keep {
				r.waited = true
			} else {
				delete(s.fnReq, d.id)
			}
		}
		s.mu.Unlock()
		if queued {
			slog.Info("filename rule queued", "course", d.id)
			s.emitCurrent(ctx, model.ScopeCourse, d.id, model.StageFilenameRule)
		}
	}
}

// filenameRuleDue evaluates one due request in one transaction. keep: the
// course's filename_rule is running, wait for its end. waited: this request
// already waited for a run. By the course's current filename_rule job:
//   - pending or queued: drop (that run reads the course's lectures when it runs);
//   - running: keep;
//   - failed or cancelled after the request waited for it: drop (a failed
//     auxiliary task is not retried automatically; the user retries);
//   - otherwise (none, done, an older failure before a fresh upload): queue
//     a run when the organizer is configured and the trigger holds.
func (s *Scheduler) filenameRuleDue(ctx context.Context, courseID string, waited bool) (keep, queued bool, err error) {
	err = s.st.Tx(ctx, func(tx db.Store) error {
		j, err := tx.GetCurrentJob(ctx, model.ScopeCourse, courseID, model.StageFilenameRule)
		switch {
		case err == nil:
			switch j.Status {
			case model.StatusPending, model.StatusQueued:
				return nil
			case model.StatusRunning:
				keep = true
				return nil
			case model.StatusFailed, model.StatusCancelled:
				if waited {
					return nil
				}
			}
		case !errors.Is(err, db.ErrNotFound):
			return err
		}
		ok, err := state.RoleConfigured(ctx, tx, model.AuxRole(model.StageFilenameRule))
		if err != nil || !ok {
			return err
		}
		want, err := state.FilenameRuleWanted(ctx, tx, courseID)
		if errors.Is(err, db.ErrNotFound) {
			return nil // course deleted
		}
		if err != nil || !want {
			return err
		}
		queued, err = state.EnqueueAux(ctx, tx, model.ScopeCourse, courseID, model.StageFilenameRule)
		return err
	})
	if err != nil {
		return false, false, err
	}
	return keep, queued, nil
}

// EnsureStream is what GET /api/lectures/{id}/media calls when stream.mp4
// is missing (P8.1, ruling 2): a lecture prepared before v2 gets its
// playback file made in the background (stream task, CPU pool). generating
// reports whether a stream task is pending, queued or running afterwards
// (queued now or already). Nothing is queued (false) for a draft, before
// prep is done, when stream.mp4 exists, when the original media is gone,
// when the current stream task failed or was cancelled, or when the kv
// stream state records a result without a file (audio_only: the source has
// no video; no_space or failed: written by prep or an earlier stream task).
// Those are never retried automatically: the user retries from the
// lecture's hint (the API turns that into a new run). A lecture prepared
// before v2 has no kv state and no stream task: it gets one.
// Unknown lecture → db.ErrNotFound.
func (s *Scheduler) EnsureStream(ctx context.Context, lectureID string) (generating bool, err error) {
	l, err := s.st.GetLecture(ctx, lectureID)
	if err != nil {
		return false, err
	}
	if state.IsDraft(l) {
		return false, nil
	}
	prep, err := s.st.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StagePrep)
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if prep.Status != model.StatusDone {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(model.LectureDir(s.dataDir, l.CourseID, l.ID), model.StreamFile)); err == nil {
		return false, nil
	}
	if !state.MediaExists(s.dataDir, l) {
		return false, nil
	}
	queued := false
	err = s.st.Tx(ctx, func(tx db.Store) error {
		j, err := tx.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageStream)
		switch {
		case err == nil:
			switch j.Status {
			case model.StatusPending, model.StatusQueued, model.StatusRunning:
				generating = true
				return nil
			case model.StatusFailed, model.StatusCancelled:
				return nil
			}
		case !errors.Is(err, db.ErrNotFound):
			return err
		}
		v, ok, err := tx.GetKV(ctx, model.KVStreamPrefix+lectureID)
		if err != nil {
			return err
		}
		if ok && v != "" {
			var ss model.StreamState
			if json.Unmarshal([]byte(v), &ss) == nil && streamNoAutoRun[ss.Status] {
				return nil
			}
		}
		queued, err = state.EnqueueAux(ctx, tx, model.ScopeLecture, lectureID, model.StageStream)
		generating = queued
		return err
	})
	if err != nil {
		return false, err
	}
	if queued {
		slog.Info("stream queued for an old lecture", "lecture", lectureID)
		s.emitCurrent(ctx, model.ScopeLecture, lectureID, model.StageStream)
		s.Kick()
	}
	return generating, nil
}
