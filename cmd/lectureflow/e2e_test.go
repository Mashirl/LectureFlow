// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// End-to-end tests of SPEC §14 through the real HTTP API, the real scheduler
// and every real stage, with the fake LLM and the fake ASR result. No GPU, no
// network, no API key.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/config"
	"lectureflow/internal/devices"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

type e2e struct {
	t    *testing.T
	app  *App
	srv  *httptest.Server
	fake *llmtest.Server
	cfg  *config.Config
	dir  string
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	return newE2EWith(t, nil)
}

// newE2EWith is newE2E with a change of the test configuration (applied
// after the fake ASR and LLM are set up, before the app is built).
func newE2EWith(t *testing.T, mut func(*config.Config)) *e2e {
	t.Helper()
	return newE2EDevices(t, mut, nil)
}

// newE2EDevices is newE2EWith that also injects the device registry's
// outside world (e.g. no NVIDIA, no .venv-ov).
func newE2EDevices(t *testing.T, mut func(*config.Config), devOpts func(*devices.RegistryOptions)) *e2e {
	t.Helper()
	cfg := config.Default()
	ff, err := exec.LookPath(cfg.FFmpeg)
	if err != nil {
		t.Skip("ffmpeg not found: prep cannot run, skipping the end-to-end test (SPEC §14)")
	}
	cfg.FFmpeg = ff
	dir := t.TempDir()
	cfg.DataDir = filepath.Join(dir, "data")
	fakeASR, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fake_asr.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ASR.FakeResult = fakeASR
	fake := llmtest.New(t)
	fake.Configure(cfg)
	if mut != nil {
		mut(cfg)
	}

	ctx, cancel := context.WithCancel(context.Background())
	app, err := buildApp(ctx, cfg, AppOptions{Sched: func(o *sched.Options) {
		o.GPUQuery = func(context.Context) (sched.GPUMem, error) {
			return sched.GPUMem{FreeMB: 20000, TotalMB: 24000, UsedMB: 1000}, nil
		}
		o.ScanInterval = 10 * time.Millisecond
		o.ProgressMinInterval = 10 * time.Millisecond
		o.IncrementalDebounce = 50 * time.Millisecond
		// The filename rule of an upload is queued after 5 s of quiet: a
		// slow machine reached that inside a test and ran it with the
		// fake answer, which does not fit the test's file names. No test
		// here is about it (internal/sched and internal/stages are).
		o.FilenameRuleDebounce = time.Hour
	}, Devices: devOpts})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := app.Start(ctx); err != nil { // as main: scheduler + device detection
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.Server.Handler())
	t.Cleanup(func() {
		app.Hub.Close()
		srv.Close()
		cancel()
		app.Close()
	})
	return &e2e{t: t, app: app, srv: srv, fake: fake, cfg: cfg, dir: dir}
}

func (e *e2e) do(method, path string, body io.Reader, contentType string, out any) int {
	e.t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		e.t.Fatalf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatalf("%s %s: decode: %v (%s)", method, path, err, b)
		}
	}
	return resp.StatusCode
}

func (e *e2e) postJSON(path string, in, out any) {
	b, _ := json.Marshal(in)
	e.do(http.MethodPost, path, bytes.NewReader(b), "application/json", out)
}

// upload sends a multipart body with the text fields first, then the file.
func (e *e2e) upload(path string, fields map[string]string, filename string, data []byte, out any) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("file", filename)
	fw.Write(data)
	mw.Close()
	e.do(http.MethodPost, path, &buf, mw.FormDataContentType(), out)
}

func (e *e2e) lecture(id string) api.Lecture {
	var l api.Lecture
	e.do(http.MethodGet, "/api/lectures/"+id, nil, "", &l)
	return l
}

// waitFor polls until cond holds, failing fast when a stage failed.
func (e *e2e) waitFor(what string, cond func() bool, lectureID string) {
	e.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		if lectureID != "" {
			for _, s := range e.lecture(lectureID).Stages {
				if s.Status == model.StatusFailed {
					e.t.Fatalf("waiting for %s: stage %s failed: %v", what, s.Stage, deref(s.Error))
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for %s; lecture=%+v", what, e.lecture(lectureID).Stages)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func allDone(l api.Lecture) bool {
	if len(l.Stages) != len(model.LectureStages) {
		return false
	}
	for _, s := range l.Stages {
		if s.Status != model.StatusDone {
			return false
		}
	}
	return true
}

func sampleWAV(t *testing.T, ffmpeg, dir string) []byte {
	t.Helper()
	p := filepath.Join(dir, "sample.wav")
	out, err := exec.Command(ffmpeg, "-y", "-v", "error", "-f", "lavfi", "-i", "anullsrc=r=16000:cl=mono", "-t", "3", "-c:a", "pcm_s16le", p).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// runPipeline creates a course with a syllabus and uploads one lecture,
// checks that it stays a draft (SPEC-v1.1 A3), launches it, then waits
// without any further action until every stage is done.
func (e *e2e) runPipeline() (courseID, lectureID string) {
	var c api.Course
	e.postJSON("/api/courses", map[string]string{"name": "E2E Course"}, &c)
	e.upload("/api/courses/"+c.ID+"/syllabus", nil, "syllabus.md",
		[]byte("# CSC0000\nCaches: cache line, write-back, LRU replacement, set associative caches.\n"), nil)
	var l api.Lecture
	e.upload("/api/courses/"+c.ID+"/lectures", map[string]string{"title": "Week 1 - Caching"},
		"week1.wav", sampleWAV(e.t, e.cfg.FFmpeg, e.dir), &l)
	e.assertDraft(l.ID)
	e.launch(l.ID)
	e.waitFor("pipeline", func() bool { return allDone(e.lecture(l.ID)) }, l.ID)
	return c.ID, l.ID
}

// assertDraft checks that an uploaded lecture is a draft that nothing
// advances: every stage pending with waiting_for = not_started, even after
// several scheduler scans (A3.1).
func (e *e2e) assertDraft(lectureID string) {
	e.t.Helper()
	time.Sleep(150 * time.Millisecond) // ~15 scans at the test scan interval
	l := e.lecture(lectureID)
	if !l.Draft || l.LaunchedAt != nil {
		e.t.Fatalf("uploaded lecture is not a draft: draft=%v launched_at=%v", l.Draft, l.LaunchedAt)
	}
	for _, s := range l.Stages {
		if s.Status != model.StatusPending || s.WaitingFor == nil || *s.WaitingFor != model.WaitNotStarted {
			e.t.Fatalf("draft stage %s: status %s waiting_for %v", s.Stage, s.Status, s.WaitingFor)
		}
	}
	if !contains(l.LectureActions, model.LectureActionLaunch) {
		e.t.Fatalf("draft lecture actions %v lack launch", l.LectureActions)
	}
}

func (e *e2e) launch(lectureID string) api.Lecture {
	e.t.Helper()
	var l api.Lecture
	e.postJSON("/api/lectures/"+lectureID+"/launch", struct{}{}, &l)
	if l.Draft || l.LaunchedAt == nil {
		e.t.Fatalf("launch response is still a draft: %+v", l)
	}
	return l
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestPipelineEndToEnd(t *testing.T) {
	e := newE2E(t)
	courseID, lectureID := e.runPipeline()
	ctx := context.Background()

	l := e.lecture(lectureID)
	// SPEC §11's four files plus the citation JSON of SPEC-v1.1 A11.2.
	if len(l.Exports) != 5 {
		t.Fatalf("want 5 export files, got %+v", l.Exports)
	}
	for _, f := range l.Exports {
		resp, err := http.Get(e.srv.URL + f.URL)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || len(b) == 0 {
			t.Fatalf("export %s: HTTP %d, %d bytes", f.Name, resp.StatusCode, len(b))
		}
	}
	if l.Counts.Sentences != 14 || l.Counts.ZhOK+l.Counts.ZhQAFailed != 14 {
		t.Fatalf("counts: %+v", l.Counts)
	}

	// Every current job — the lecture's six and the course's glossary_seed — is done.
	for _, scope := range [][2]string{{model.ScopeLecture, lectureID}, {model.ScopeCourse, courseID}} {
		jobs, err := e.app.Store.ListCurrentJobs(ctx, scope[0], scope[1])
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) == 0 {
			t.Fatalf("no jobs for %s", scope[0])
		}
		for _, j := range jobs {
			if j.Status != model.StatusDone {
				t.Fatalf("%s/%s is %s", scope[0], j.Stage, j.Status)
			}
		}
	}
	// API stages recorded token usage through the scheduler (SPEC §12, §17.7).
	if l.Usage.PromptTokens == 0 {
		t.Fatalf("no usage recorded: %+v", l.Usage)
	}
}

func TestReviewEndToEnd(t *testing.T) {
	e := newE2E(t)
	_, lectureID := e.runPipeline()
	x := e.cfg.LLM.Roles[config.RoleTranslate].Window

	var pending []api.Patch
	e.do(http.MethodGet, "/api/lectures/"+lectureID+"/patches?review=pending", nil, "", &pending)
	var decide *api.Patch
	for i := range pending {
		if pending[i].Class == model.ClassDecide {
			decide = &pending[i]
			break
		}
	}
	if decide == nil {
		t.Fatalf("no pending decide patch: %+v", pending)
	}
	before := e.exportText(lectureID)
	e.fake.Reset()

	var reviewed api.Patch
	e.postJSON("/api/patches/"+decide.ID+"/review", map[string]string{"action": "reject"}, &reviewed)
	if reviewed.Review != model.ReviewRejected || reviewed.Retranslated == nil || *reviewed.Retranslated == 0 {
		t.Fatalf("review response: %+v", reviewed)
	}

	e.waitFor("incremental translate and re-export", func() bool {
		l := e.lecture(lectureID)
		return e.fake.Count(llmtest.RoleTranslate) > 0 && allDone(l) && l.Counts.ZhStale == 0 && e.exportText(lectureID) != before
	}, lectureID)

	i := decide.SentenceIdx
	idxs := e.fake.TranslateIdxs()
	seen := map[int]bool{}
	for _, idx := range idxs {
		if idx < i-x || idx > i+x {
			t.Fatalf("sentence %d retranslated, outside [%d, %d]; all: %v", idx, i-x, i+x, idxs)
		}
		seen[idx] = true
	}
	if len(seen) != *reviewed.Retranslated {
		t.Fatalf("retranslated %d distinct sentences %v, review marked %d stale", len(seen), idxs, *reviewed.Retranslated)
	}
	if n := e.fake.Count(llmtest.RoleRepair) + e.fake.Count(llmtest.RoleSummary); n != 0 {
		t.Fatalf("review must not rerun summary/repair, got %d requests", n)
	}
}

func (e *e2e) exportText(lectureID string) string {
	e.t.Helper()
	l := e.lecture(lectureID)
	for _, f := range l.Exports {
		if filepath.Ext(f.Name) == ".md" {
			resp, err := http.Get(e.srv.URL + f.URL)
			if err != nil {
				e.t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return string(b)
		}
	}
	return fmt.Sprint("no md export: ", l.Exports)
}
