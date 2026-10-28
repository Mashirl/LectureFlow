// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

// ssh-mvp: an asr job bound to an SSH server runs through the fleet of the
// run's context: the job.json of the engine's local device without paths
// (only keys the agent knows), the checkpoint naming the server, the
// result stored like a local one; errors pass through for the scheduler.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/remote"
	"lectureflow/internal/sched"
)

type fakeSSHServer struct {
	mu     sync.Mutex
	jobs   []remote.Job
	err    error
	result string
}

func (f *fakeSSHServer) ID() string                                    { return "ssh:box" }
func (f *fakeSSHServer) Ready(context.Context, float64) (bool, string) { return true, "" }
func (f *fakeSSHServer) Slots() int                                    { return 1 }
func (f *fakeSSHServer) Card(context.Context) remote.Card              { return remote.Card{} }
func (f *fakeSSHServer) Transcribe(ctx context.Context, job remote.Job, progress func(remote.Event)) (string, error) {
	f.mu.Lock()
	f.jobs = append(f.jobs, job)
	err := f.err
	f.mu.Unlock()
	progress(remote.Event{Step: remote.StepUpload, Fraction: 0.5, Overall: 0.05, ETASec: -1})
	progress(remote.Event{Step: remote.StepTranscribe, Stage: remote.StageTranscribe, Fraction: 0.5, Overall: 0.5, ETASec: 30})
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(f.result)
	if err != nil {
		return "", err
	}
	clean, _, err := remote.ValidateResult(b, int64(job.DurationSec*1000))
	if err != nil {
		return "", err
	}
	out := filepath.Join(job.OutDir, "result.json")
	if err := writeFileAtomic(out, clean); err != nil {
		return "", err
	}
	progress(remote.Event{Step: remote.StepVerify, Fraction: 1, Overall: 1, ETASec: 0})
	return out, nil
}

type oneServer struct{ d remote.Device }

func (o oneServer) Device(id string) (remote.Device, bool) {
	if id == "ssh:box" {
		return o.d, true
	}
	return nil, false
}

func (e *env) sshLecture(backend, modelName, precision string) model.Lecture {
	e.t.Helper()
	l := e.lecture("L ssh", "m.mp4")
	e.must(os.WriteFile(filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), model.AudioFile), silenceWAV(200000), 0o644))
	snap := model.ASRSnapshot{DeviceID: "ssh:box", Backend: backend, Model: modelName, Precision: precision, Language: "en", Auto: true}
	e.must(e.st.SetJobSnapshot(e.ctx, e.job(model.ScopeLecture, l.ID, model.StageASR).ID, model.MarshalSnapshot(snap)))
	return l
}

func TestASRSSHRun(t *testing.T) {
	for _, tc := range []struct{ backend, model, precision string }{
		{model.BackendMLX, "large-v3-turbo", model.PrecisionFP16},
		{model.BackendFasterWhisper, "large-v3", model.PrecisionAuto},
	} {
		t.Run(tc.backend, func(t *testing.T) {
			e := newEnv(t)
			e.cfg.ASR.FakeResult = fakeASR // an SSH snapshot never uses the local fake
			e.addTerm("cache", "缓存", model.SourceSyllabus, 9)
			l := e.sshLecture(tc.backend, tc.model, tc.precision)
			dir, _ := filepath.Abs(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID))
			srv := &fakeSSHServer{result: fakeASR}
			r := e.runCtx(sched.WithSSH(e.ctx, oneServer{srv}), e.stage(e.deps(nil), model.StageASR), l.ID)
			if r.err != nil {
				t.Fatal(r.err)
			}
			if len(srv.jobs) != 1 {
				t.Fatalf("%d remote jobs", len(srv.jobs))
			}
			j := srv.jobs[0]
			if j.ID != r.job.ID || j.Engine != tc.backend || j.Model != tc.model || j.Precision != tc.precision ||
				j.OutDir != dir || j.AudioPath != filepath.Join(dir, model.AudioFile) || !remote.SHA256Pattern.MatchString(j.AudioSHA256) {
				t.Fatalf("remote job %+v", j)
			}
			// No path in the parameters, only keys the agent accepts.
			raw, _ := json.Marshal(j.Params)
			if strings.Contains(string(raw), dir) || strings.Contains(string(raw), "remote\"") {
				t.Fatalf("a path in the remote parameters: %s", raw)
			}
			if _, err := remote.CheckParams(tc.backend, raw); err != nil {
				t.Fatalf("parameters refused: %v (%s)", err, raw)
			}
			ck, _ := j.Params["checkpoint"].(map[string]any)
			if ck["device_id"] != "ssh:box" || ck["job_id"] != r.job.ID {
				t.Fatalf("checkpoint %v", ck)
			}
			if j.Params["initial_prompt"] != "cache" || j.Params["language"] != "en" {
				t.Fatalf("params %v", j.Params)
			}
			if n := len(e.sentences(l.ID)); n != 14 {
				t.Fatalf("%d sentences", n)
			}
			if rs := r.stats.Remote; rs == nil || rs.Step != model.RemoteStepVerify || !slices.Equal(rs.Steps, model.RemoteSteps) {
				t.Fatalf("stats.remote %+v", rs)
			}
		})
	}
}

func TestASRSSHErrorsPassThrough(t *testing.T) {
	e := newEnv(t)
	l := e.sshLecture(model.BackendMLX, "large-v3-turbo", model.PrecisionFP16)
	fb := &remote.Error{Kind: remote.ErrFallback, Code: remote.CodeResultInvalid, Err: errors.New("refused")}
	srv := &fakeSSHServer{err: fb, result: fakeASR}
	r := e.runCtx(sched.WithSSH(e.ctx, oneServer{srv}), e.stage(e.deps(nil), model.StageASR), l.ID)
	if !remote.IsFallback(r.err) {
		t.Fatalf("err %v", r.err)
	}
	// Without the fleet in the context: back to the local devices.
	r = e.runCtx(e.ctx, e.stage(e.deps(nil), model.StageASR), l.ID)
	if !errors.Is(r.err, sched.ErrRemoteFallback) {
		t.Fatalf("without a fleet: %v", r.err)
	}
}

// The remote parameters follow the local job.json: every key of the local
// constructor's output is a path the agent sets or one it accepts, so a new
// worker option cannot silently break remote runs.
func TestRemoteParamKeysCoverTheLocalJobs(t *testing.T) {
	for engine, typ := range map[string]reflect.Type{
		model.BackendMLX: reflect.TypeOf(mlxJob{}), model.BackendFasterWhisper: reflect.TypeOf(asrJob{}),
	} {
		for i := 0; i < typ.NumField(); i++ {
			tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
			if slices.Contains(remote.PathKeys[engine], tag) {
				continue
			}
			if _, ok := remote.ParamKeys[engine][tag]; !ok {
				t.Errorf("%s: job.json key %q is neither a path nor a remote parameter (internal/remote/params.go)", engine, tag)
			}
		}
	}
}
