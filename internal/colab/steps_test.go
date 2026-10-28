// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// 3.2.0 (the user's A100 lecture in speed mode: the hover card stayed on
// 等卡): the steps a lecture reports — the progress events the asr stage
// turns into stats.remote, and Status.Jobs the resource bar's card is built
// from — go 等卡 → 上传音频 → 远端转写 → 下载结果 → 校验 in order, with the
// worker's stages during the transcription, for one lecture, in speed mode
// and for two lectures at once. (The card was stale in the front end:
// web/pipeline.js now follows the resources; this checks what they carry.)

import (
	"context"
	"slices"
	"sync"
	"testing"
)

var wantSteps = []string{StepWaitAccelerator, StepUploadAudio, StepRemoteTranscribe, StepDownloadResult, StepVerify}

// stepTrace records a lecture's events: the steps in order (each once per
// run of the same step), the stages of remote_transcribe, and whether the
// overall progress ever went back.
type stepTrace struct {
	mu     sync.Mutex
	steps  []string
	stages []string
	last   float64
	back   bool
}

func (s *stepTrace) add(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(s.steps); n == 0 || s.steps[n-1] != ev.Step {
		s.steps = append(s.steps, ev.Step)
	}
	if ev.Step == StepRemoteTranscribe && ev.Stage != "" && (len(s.stages) == 0 || s.stages[len(s.stages)-1] != ev.Stage) {
		s.stages = append(s.stages, ev.Stage)
	}
	if ev.Overall+1e-9 < s.last {
		s.back = true
	}
	s.last = max(s.last, ev.Overall)
}

func (s *stepTrace) check(t *testing.T, what string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Equal(s.steps, wantSteps) {
		t.Errorf("%s: steps %v, want %v", what, s.steps, wantSteps)
	}
	if !slices.Contains(s.stages, "transcribe") || s.back || s.last != 1 {
		t.Errorf("%s: stages %v, progress went back %v, last %v", what, s.stages, s.back, s.last)
	}
}

func startTraced(e *fakeEnv, ctx context.Context, j Job, tr *stepTrace) chan error {
	done := make(chan error, 1)
	go func() {
		_, err := e.b.Transcribe(ctx, j, tr.add)
		done <- err
	}()
	return done
}

// jobStep is the step and stage Status reports for the lecture of job id.
func jobStep(e *fakeEnv, id string) (string, string) {
	for _, j := range e.b.Status(context.Background()).Jobs {
		if j.JobID == id {
			return j.Step, j.Stage
		}
	}
	return "", ""
}

func TestStepsOfOneLecture(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		set  func() Settings
	}{
		{"T4", t4},
		{"speed mode on an A100", func() Settings {
			s := DefaultSettings()
			s.Accelerator, s.AcceleratorChosen, s.FastMode = "A100", true, true
			return s
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFakeEnv(t, tc.set())
			e.b.RefreshAccount(ctx) // a paid account that may borrow the A100
			a := e.lecture(t, "a")
			e.f.hold("a")
			var tr stepTrace
			done := startTraced(e, ctx, a, &tr)
			e.transcribingNow(t)
			e.waitFor(t, "the transcription in Status", func() bool {
				step, stage := jobStep(e, "a")
				return step == StepRemoteTranscribe && stage == "transcribe"
			})
			if tc.name != "T4" {
				if js := e.b.Status(ctx).Jobs; len(js) != 1 || js[0].Split != 2 {
					t.Errorf("speed mode: jobs %+v", js)
				}
			}
			e.f.release("a")
			if err := waitErr(t, done); err != nil {
				t.Fatal(err)
			}
			tr.check(t, "a")
			_ = e.b.QueueDrained(ctx)
		})
	}
}

// Two lectures at once on an L4: each reports its own steps; Status lists
// both transcribing.
func TestStepsOfTwoLecturesAtOnce(t *testing.T) {
	ctx := context.Background()
	e := newFakeEnv(t, DefaultSettings())
	e.b.RefreshAccount(ctx) // paid: the L4, two at once
	a, b := e.lecture(t, "a"), e.lecture(t, "b")
	e.f.hold("a")
	e.f.hold("b")
	var ta, tb stepTrace
	da := startTraced(e, ctx, a, &ta)
	e.transcribingNow(t)
	db := startTraced(e, ctx, b, &tb)
	e.transcribingNow(t)
	e.waitFor(t, "both transcribing in Status", func() bool {
		sa, _ := jobStep(e, "a")
		sb, _ := jobStep(e, "b")
		return sa == StepRemoteTranscribe && sb == StepRemoteTranscribe
	})
	e.f.release("a")
	e.f.release("b")
	if err := waitErr(t, da); err != nil {
		t.Fatal(err)
	}
	if err := waitErr(t, db); err != nil {
		t.Fatal(err)
	}
	ta.check(t, "a")
	tb.check(t, "b")
	_ = e.b.QueueDrained(ctx)
}
