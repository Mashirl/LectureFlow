// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/colab"
	"lectureflow/internal/model"
	"lectureflow/internal/remote"
	"lectureflow/internal/sched"
)

// colab-obs-core: Snapshot.Remotes, Colab's remote.Card.
func TestRemoteCards(t *testing.T) {
	if remoteCards(nil, deviceInput{}) != nil {
		t.Fatal("cards without Colab")
	}
	at := time.Date(2026, 10, 27, 15, 0, 0, 0, time.UTC)
	n := func(v int) *int { return &v }
	st := colab.Status{Service: "colab", Configured: true, Accelerator: "L4", State: colab.StateRunning, Slots: 2, Planned: 3,
		UploadedAhead: 1, EffectiveAccelerator: "L4",
		Jobs: []colab.JobInfo{
			{ID: "job-1", JobID: "job-1", Step: colab.StepRemoteTranscribe, Stage: "transcribe", Overall: 0.5, ETASec: 61.4, Split: 2},
			{ID: "job-2-abc123", JobID: "job-2", Step: colab.StepUploadAudio, Overall: 0.02, ETASec: -1},
		},
		Machine:     &remote.Machine{Source: remote.SourceNvidiaSMI, GPUName: "NVIDIA L4", Requested: "L4"},
		Usage:       &remote.Usage{Source: remote.SourceWorker, CPUPct: n(40), RAM: []remote.RAMSegment{{Kind: remote.SegFree, MB: 1}}},
		LastError:   &model.ErrorInfo{Code: colab.CodeUnavailable, Params: map[string]any{"endpoint": "runtime"}},
		LastErrorAt: &at,
	}
	in := deviceInput{statuses: map[string]sched.DeviceStatus{model.DeviceColab: {ID: model.DeviceColab, Running: []sched.RunningASR{
		{JobID: "job-1", LectureID: "lec-1"}, {JobID: "job-2", LectureID: "lec-2"}}}},
		titles: map[string]model.Lecture{"lec-1": {ID: "lec-1", Title: "W1 Ciphers"}}}
	cards := remoteCards(&st, in)
	if len(cards) != 1 {
		t.Fatalf("cards %+v", cards)
	}
	c := cards[0]
	if c.ID != model.DeviceColab || c.Kind != remote.KindColab || c.Label != "Google Colab · L4" || c.State != colab.StateRunning ||
		c.Slots != 2 || c.Queued != 3 || c.UploadedAhead != 1 || c.Machine.GPUName != "NVIDIA L4" || *c.Usage.CPUPct != 40 {
		t.Errorf("card %+v", c)
	}
	if len(c.Jobs) != 2 || c.Jobs[0].LectureID != "lec-1" || c.Jobs[0].LectureTitle != "W1 Ciphers" || c.Jobs[1].LectureTitle != "" || c.Jobs[0].Stage != "transcribe" || *c.Jobs[0].Progress != 0.5 ||
		*c.Jobs[0].ETASec != 61 || c.Jobs[0].Split != 2 || c.Jobs[1].LectureID != "lec-2" || c.Jobs[1].Stage != "upload" ||
		c.Jobs[1].ETASec != nil {
		t.Errorf("jobs %+v", c.Jobs)
	}
	if c.LastError == nil || c.LastError.Code != colab.CodeUnavailable || !c.LastError.At.Equal(at) {
		t.Errorf("last error %+v", c.LastError)
	}
	if got, ok := c.Colab.(colab.Status); !ok || got.EffectiveAccelerator != "L4" {
		t.Errorf("colab %+v", c.Colab)
	}
	b, _ := json.Marshal(Snapshot{Remote: &st, Remotes: cards, Providers: []Provider{}})
	for _, w := range []string{`"remotes":[{"id":"colab:remote","kind":"colab","label":"Google Colab · L4"`, `"colab":{"service":"colab"`,
		`"remote":{"service":"colab"`} {
		if !strings.Contains(string(b), w) {
			t.Errorf("JSON lacks %s: %s", w, b)
		}
	}
	// Without remotes the snapshot's bytes stay as they were.
	if b, _ := json.Marshal(Snapshot{Providers: []Provider{}}); strings.Contains(string(b), "remotes") {
		t.Errorf("JSON %s", b)
	}
	for step, want := range map[string]string{colab.StepWaitAccelerator: "wait", colab.StepDownloadResult: "download",
		colab.StepVerify: "verify", colab.StepRemoteTranscribe: "check"} {
		if got := cardStage(colab.JobInfo{Step: step}); got != want {
			t.Errorf("%s → %s", step, got)
		}
	}
}

func TestServiceClients(t *testing.T) {
	s := New(Deps{Now: time.Now})
	if s.Clients() != 0 {
		t.Fatal("clients")
	}
	s.SetClients(2)
	if s.Clients() != 2 {
		t.Errorf("clients %d", s.Clients())
	}
	s.SetClients(-1)
	if s.Clients() != 0 {
		t.Errorf("clients %d", s.Clients())
	}
}
