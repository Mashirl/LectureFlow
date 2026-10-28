// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// "修复断句" (textrepair-int): sched.RerunTextRepair and the CPU pool of its
// asr job.

// textRepairEnv is a scheduler with all six lecture stages: asr is a GPU
// stage that records, per run, the job it got and the pools running then.
type textRepairEnv struct {
	*env
	mu    sync.Mutex
	runs  []model.Job // asr runs, in order
	pools []string    // per asr run: "cpu=N gpu=M" while it ran
	asr   *gate
}

func newTextRepairEnv(t *testing.T) *textRepairEnv {
	te := &textRepairEnv{asr: newGate()}
	asr := &fakeStage{name: model.StageASR, res: ResourceGPU, run: func(ctx context.Context, id string, p func(float64)) error {
		j, _ := JobFrom(ctx)
		te.mu.Lock()
		te.runs = append(te.runs, j)
		te.pools = append(te.pools, poolText(te.env.s))
		te.mu.Unlock()
		if !model.IsTextRepairJob(j) {
			return nil // a transcription: done at once
		}
		return te.asr.run(ctx, id, p)
	}}
	te.env = newEnv(t, nil, instant(model.StagePrep, ResourceCPU), asr, instant(model.StageSummary, ResourceAPI),
		instant(model.StageRepair, ResourceAPI), instant(model.StageTranslate, ResourceAPI), instant(model.StageExport, ResourceCPU))
	return te
}

func poolText(s *Scheduler) string {
	return "cpu=" + strconv.Itoa(s.countRunning(ResourceCPU)) + " gpu=" + strconv.Itoa(s.countRunning(ResourceGPU))
}

// changed reports whether a Changed(kind, id) event was sent.
func (r *recEvents) sent(kind, id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.changed, kind+":"+id)
}

// doneLecture is a lecture whose six stages ran.
func (te *textRepairEnv) doneLecture() model.Lecture {
	te.t.Helper()
	l := te.lecture()
	te.waitStatus(l.ID, model.StageExport, model.StatusDone)
	return l
}

func TestRerunTextRepairRunsInTheCPUPool(t *testing.T) {
	te := newTextRepairEnv(t)
	te.start()
	l := te.doneLecture()
	oldASR := te.lj(l.ID, model.StageASR)
	old := map[string]string{}
	for _, s := range model.LectureStages {
		old[s] = te.lj(l.ID, s).ID
	}
	// the transcription devices are paused: a transcription would wait
	if err := te.s.SetGPUPaused(true, false); err != nil {
		t.Fatal(err)
	}
	if err := te.s.RerunTextRepair(te.ctx, l.ID, ""); err != nil {
		t.Fatal(err)
	}
	if !te.ev.sent("sentences", l.ID) || !te.ev.sent("patches", l.ID) {
		t.Error("no sentences / patches change event")
	}
	j := te.waitStatus(l.ID, model.StageASR, model.StatusRunning)
	if j.ID == oldASR.ID || j.Stats.ASRMode != model.ASRModeTextRepair {
		t.Fatalf("asr job %+v", j)
	}
	te.waitFor("the repair's run", 5*time.Second, func() bool {
		te.mu.Lock()
		defer te.mu.Unlock()
		return len(te.runs) == 2
	})
	te.mu.Lock()
	last := te.pools[1]
	te.mu.Unlock()
	if last != "cpu=1 gpu=0" {
		t.Errorf("pools while the repair ran: %s, want it in the CPU pool", last)
	}
	// a transcription of another lecture still waits for the paused devices
	l2 := te.lecture()
	te.waitFor("gpu_paused", 5*time.Second, func() bool {
		return te.s.WaitingFor(te.lj(l2.ID, model.StageASR)) == model.WaitGPUPaused
	})
	close(te.asr.release)
	te.waitStatus(l.ID, model.StageASR, model.StatusDone)
	// the stages after it run again (new jobs); prep is kept
	for _, s := range model.LectureStages {
		j := te.waitStatus(l.ID, s, model.StatusDone)
		if (j.ID != old[s]) != (s != model.StagePrep) {
			t.Errorf("%s: job %s, before %s", s, j.ID, old[s])
		}
	}
	if s := te.lj(l2.ID, model.StageASR).Status; s != model.StatusPending && s != model.StatusQueued {
		t.Errorf("the other lecture's transcription is %s while the devices are paused", s)
	}
}

func TestRerunTextRepairDoesNotWaitForATranscription(t *testing.T) {
	g := newGate()
	te := newTextRepairEnv(t)
	te.start()
	l := te.doneLecture()
	// another lecture's transcription takes the only GPU slot
	busy := &fakeStage{name: model.StageASR, res: ResourceGPU, run: func(ctx context.Context, id string, p func(float64)) error {
		j, _ := JobFrom(ctx)
		if model.IsTextRepairJob(j) {
			te.mu.Lock()
			te.pools = append(te.pools, poolText(te.s))
			te.mu.Unlock()
			return nil
		}
		return g.run(ctx, id, p)
	}}
	te.s.mu.Lock()
	te.s.stages[model.StageASR] = busy
	te.s.mu.Unlock()
	l2 := te.lecture()
	te.waitStatus(l2.ID, model.StageASR, model.StatusRunning)
	if err := te.s.RerunTextRepair(te.ctx, l.ID, model.ModelChoiceSnapshot); err != nil {
		t.Fatal(err)
	}
	te.waitStatus(l.ID, model.StageExport, model.StatusDone)
	te.mu.Lock()
	got := slices.Clone(te.pools)
	te.mu.Unlock()
	if len(got) == 0 || got[len(got)-1] != "cpu=1 gpu=1" {
		t.Errorf("pools while the repair ran: %v (the transcription keeps its GPU slot)", got)
	}
	if s := te.lj(l2.ID, model.StageASR).Status; s != model.StatusRunning {
		t.Errorf("the transcription is %s", s)
	}
	close(g.release)
}

func TestRerunTextRepairGuards(t *testing.T) {
	te := newTextRepairEnv(t)
	te.start()
	l := te.doneLecture()
	if err := te.s.RerunTextRepair(te.ctx, l.ID, "bogus"); !errors.Is(err, state.ErrBadInput) {
		t.Errorf("bad choice: %v", err)
	}
	d := te.draft()
	if err := te.s.RerunTextRepair(te.ctx, d.ID, ""); !errors.Is(err, state.ErrDraft) {
		t.Errorf("draft: %v", err)
	}
	// once it runs, a second one is refused (asr is not done or stale)
	if err := te.s.RerunTextRepair(te.ctx, l.ID, ""); err != nil {
		t.Fatal(err)
	}
	te.waitStatus(l.ID, model.StageASR, model.StatusRunning)
	if err := te.s.RerunTextRepair(te.ctx, l.ID, ""); !errors.Is(err, state.ErrConflict) {
		t.Errorf("asr running: %v", err)
	}
	close(te.asr.release)
	te.waitStatus(l.ID, model.StageExport, model.StatusDone)
	// the asr job kept the snapshot of the transcription it repaired
	te.mu.Lock()
	runs := slices.Clone(te.runs)
	te.mu.Unlock()
	first, last := runs[0], runs[len(runs)-1]
	if first.ConfigSnapshot != last.ConfigSnapshot || !model.IsTextRepairJob(last) {
		t.Errorf("snapshot %q → %q, mode %q", first.ConfigSnapshot, last.ConfigSnapshot, last.Stats.ASRMode)
	}
}

func TestResourceOf(t *testing.T) {
	gpu := instant(model.StageASR, ResourceGPU)
	j := model.Job{Scope: model.ScopeLecture, Stage: model.StageASR}
	if r := resourceOf(gpu, j); r != ResourceGPU {
		t.Errorf("transcription: %s", r)
	}
	j.Stats.ASRMode = model.ASRModeTextRepair
	if r := resourceOf(gpu, j); r != ResourceCPU {
		t.Errorf("repair: %s", r)
	}
	// "改用 Colab 处理" makes it a transcription on Colab again
	j.Stats.RemotePin = true
	if r := resourceOf(gpu, j); r != ResourceGPU {
		t.Errorf("pinned to Colab: %s", r)
	}
	other := model.Job{Scope: model.ScopeLecture, Stage: model.StageSummary, Stats: model.JobStats{ASRMode: model.ASRModeTextRepair}}
	if r := resourceOf(instant(model.StageSummary, ResourceAPI), other); r != ResourceAPI {
		t.Errorf("another stage: %s", r)
	}
}
