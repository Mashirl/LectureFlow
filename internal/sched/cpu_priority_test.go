// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// cpuOrder records the order in which CPU jobs start.
type cpuOrder struct {
	mu     sync.Mutex
	starts []string
}

func (o *cpuOrder) add(s string) {
	o.mu.Lock()
	o.starts = append(o.starts, s)
	o.mu.Unlock()
}

func (o *cpuOrder) list() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.starts)
}

// prepQueuesStream is a prep that, like the real one for a source to
// transcode (stream-parallel), queues the lecture's stream task at its end.
func prepQueuesStream(e **env, order *cpuOrder, run func(ctx context.Context, id string) error) *fakeStage {
	return &fakeStage{name: model.StagePrep, res: ResourceCPU, run: func(ctx context.Context, id string, _ func(float64)) error {
		order.add("prep:" + id)
		if run != nil {
			if err := run(ctx, id); err != nil {
				return err
			}
		}
		_, err := state.EnqueueAux(context.WithoutCancel(ctx), (*e).st, model.ScopeLecture, id, model.StageStream)
		return err
	}}
}

func streamStage(order *cpuOrder, g *gate) *fakeStage {
	return &fakeStage{name: model.StageStream, res: ResourceCPU, run: func(ctx context.Context, id string, p func(float64)) error {
		order.add("stream:" + id)
		return g.run(ctx, id, p)
	}}
}

// The CPU pool serves pipeline stages before auxiliary tasks (lead ruling
// 2026-10-27 14:30): in a batch, the transcodes (stream tasks) that the
// first lectures' prep queued do not hold up the later lectures' prep; each
// group keeps the FIFO order of SPEC §5; once no pipeline stage waits, the
// stream tasks get the slot (not starved).
func TestCPUPoolPipelineBeforeAux(t *testing.T) {
	order := &cpuOrder{}
	transcode := newGate()
	var e *env
	e = newEnvOpts(t, nil, func(o *Options) { o.CPUSlots = 1 }, prepQueuesStream(&e, order, nil), streamStage(order, transcode))
	ls := []model.Lecture{e.lecture(), e.lecture(), e.lecture()}
	e.start()
	// Before the fix the first lecture's transcode took the only slot right
	// after its prep, and the other preps waited for it (this wait timed out).
	for _, l := range ls {
		e.waitStatus(l.ID, model.StagePrep, model.StatusDone)
	}
	e.waitFor("the first transcode", 5*time.Second, func() bool { c, _, _ := transcode.stats(); return c == 1 })
	got := order.list()
	if len(got) != 4 {
		t.Fatalf("CPU starts %v, want 3 preps then one transcode", got)
	}
	for _, s := range got[:3] {
		if !strings.HasPrefix(s, "prep:") {
			t.Fatalf("CPU starts %v: a transcode ran before a waiting prep", got)
		}
	}
	// FIFO within the auxiliary tasks: the transcode of the lecture first in
	// the queue (its prep ran first)
	if first := strings.TrimPrefix(got[0], "prep:"); got[3] != "stream:"+first {
		t.Fatalf("CPU starts %v: first transcode not of the first lecture", got)
	}
	// Still one at a time (CPUSlots = 1): a scan after the first transcode
	// started refuses the other two for the slot.
	var states []string
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(3 * time.Millisecond) {
		states = states[:0]
		waiting := 0
		for _, l := range ls {
			j := e.lj(l.ID, model.StageStream)
			w := e.s.WaitingFor(j)
			states = append(states, j.Status+"/"+w)
			if j.Status == model.StatusQueued && w == model.WaitCPUSlot {
				waiting++
			}
		}
		if waiting == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("transcodes %v, want two queued for %q", states, model.WaitCPUSlot)
		}
	}
	if c, _, _ := transcode.stats(); c != 1 {
		t.Errorf("%d transcodes running (%v), want 1", c, states)
	}
	close(transcode.release)
	for _, l := range ls {
		e.waitStatus(l.ID, model.StageStream, model.StatusDone)
	}
	if _, max, runs := transcode.stats(); max != 1 || runs != 3 {
		t.Errorf("transcodes: max %d at once, %d runs; want 1, 3", max, runs)
	}
}

// A free CPU slot goes to an auxiliary task when no pipeline stage takes
// it: here one slot runs a prep, the stream task of an earlier lecture
// takes the other at once; a pipeline stage that cannot start (pending on
// its dependencies) holds nothing back.
func TestCPUPoolAuxUsesFreeSlot(t *testing.T) {
	order := &cpuOrder{}
	transcode, slowPrep := newGate(), newGate()
	var e *env
	e = newEnvOpts(t, nil, func(o *Options) { o.CPUSlots = 2 },
		prepQueuesStream(&e, order, func(ctx context.Context, id string) error { return slowPrep.run(ctx, id, nil) }),
		streamStage(order, transcode), instant(model.StageExport, ResourceCPU))
	a := e.lecture()
	e.prepDone(a) // a's prep finished earlier: its transcode is queued, its export pending
	if _, err := state.EnqueueAux(e.ctx, e.st, model.ScopeLecture, a.ID, model.StageStream); err != nil {
		t.Fatal(err)
	}
	b := e.lecture()
	e.start()
	e.waitFor("b's prep and a's transcode side by side", 5*time.Second, func() bool {
		cp, _, _ := slowPrep.stats()
		ct, _, _ := transcode.stats()
		return cp == 1 && ct == 1
	})
	if j := e.lj(a.ID, model.StageExport); j.Status != model.StatusPending {
		t.Errorf("a's export %s, want pending (its upstream stages have not run)", j.Status)
	}
	close(slowPrep.release)
	close(transcode.release)
	e.waitStatus(b.ID, model.StagePrep, model.StatusDone)
	e.waitStatus(a.ID, model.StageStream, model.StatusDone)
	e.waitStatus(b.ID, model.StageStream, model.StatusDone)
}

// cpuAuxLast only reorders the CPU pool's auxiliary tasks: the other jobs
// (an API auxiliary task included) keep their order, and so do the moved
// ones.
func TestCPUAuxLastOrder(t *testing.T) {
	e := newEnv(t, nil, instant(model.StagePrep, ResourceCPU), instant(model.StageStream, ResourceCPU),
		&fakeStage{name: model.StageTitle, res: ResourceAPI}, instant(model.StageASR, ResourceGPU))
	jobs := []model.Job{
		{ID: "1", Stage: model.StageStream},
		{ID: "2", Stage: model.StageTitle},
		{ID: "3", Stage: model.StagePrep},
		{ID: "4", Stage: model.StageStream},
		{ID: "5", Stage: model.StageASR},
		{ID: "6", Stage: model.StageChapters}, // no implementation registered: left in place
	}
	var ids []string
	for _, j := range e.s.cpuAuxLast(jobs) {
		ids = append(ids, j.ID)
	}
	if want := []string{"2", "3", "5", "6", "1", "4"}; !slices.Equal(ids, want) {
		t.Errorf("order %v, want %v", ids, want)
	}
	plain := []model.Job{{ID: "1", Stage: model.StagePrep}, {ID: "2", Stage: model.StageTitle}}
	if got := e.s.cpuAuxLast(plain); len(got) != 2 || got[0].ID != "1" || got[1].ID != "2" {
		t.Errorf("without CPU auxiliary tasks: %v", got)
	}
}
