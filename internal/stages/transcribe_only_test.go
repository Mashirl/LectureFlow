// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

// "只转写" (SPEC-v3 V4.2 step 4, 3.1.0; NOTES.md): the filename rule
// without a model, the on-demand English files, and the whole pipeline
// through the real scheduler with no provider configured, then with one.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/calendar"
	"lectureflow/internal/db"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
	"lectureflow/internal/state"
	"lectureflow/internal/subs"
)

// ---------- filename rule without a model ----------

// No organizer model: the model is never asked, the built-in Panopto
// pattern is checked and applied (first exception) like a model's rule.
func TestFilenameRuleNoModelUsesFallback(t *testing.T) {
	e := newEnv(t) // the organizer role is not configured
	d := e.deps(nil)
	var ids []string
	for i, n := range llmtest.PanoptoNames {
		ids = append(ids, e.upload(n, lt(2026, 10, 24, 18, i)).ID)
	}
	r := e.runFilenameRule(d)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if n := len(e.fake.Requests("")); n != 0 {
		t.Errorf("%d LLM requests without a model", n)
	}
	if r.job.Status != model.StatusDone || !model.IsEmptySnapshot(r.job.ConfigSnapshot) {
		t.Errorf("job %s snapshot %q", r.job.Status, r.job.ConfigSnapshot)
	}
	var rule calendar.FilenameRule
	if raw := e.courseRow().FilenameRule; raw == nil || json.Unmarshal([]byte(*raw), &rule) != nil {
		t.Fatal("rule not applied")
	}
	if rule.Regex != panoptoFallbackRegex || rule.Source != calendar.RuleSourceLLM || rule.SamplesMatched != len(ids) || rule.SamplesTotal != len(ids) {
		t.Errorf("rule %+v", rule)
	}
	if got := e.lec(ids[0]); ptrStr(got.RecordedAt) != "2026-10-07T09:35" || got.RecordedAtSource != model.RecordedAtFilename {
		t.Errorf("recorded_at %s (%s)", ptrStr(got.RecordedAt), got.RecordedAtSource)
	}
	if e.kv(model.KVFilenameUndo+e.course.ID) == "" {
		t.Error("no undo record")
	}
	if s := e.samplesUsed(); len(s) != len(ids) {
		t.Errorf("samples used %v", s)
	}
}

// No organizer model and a name the pattern does not match: proposed only,
// with the no-model explanation.
func TestFilenameRuleNoModelProposal(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil)
	e.upload("CSC1003-L01-01 Wed Oct 07 09-35-00 BST 2026.mp4", lt(2026, 10, 24, 18, 0))
	e.upload("CSC1003-L01-02 Wed Oct 14 09-35-00 BST 2026.mp4", lt(2026, 10, 24, 18, 1))
	e.upload("revision session.mp4", lt(2026, 10, 24, 18, 2))
	if r := e.runFilenameRule(d); r.err != nil {
		t.Fatal(r.err)
	}
	if e.courseRow().FilenameRule != nil {
		t.Error("rule applied although a name does not match")
	}
	p := e.proposal()
	if p == nil {
		t.Fatal("no proposal")
	}
	var rule calendar.FilenameRule
	e.must(json.Unmarshal(p.Rule, &rule))
	if rule.Regex != panoptoFallbackRegex || rule.SamplesMatched != 2 || rule.SamplesTotal != 3 || p.Explanation != panoptoNoModelExplanation {
		t.Errorf("proposal %+v / %+v", rule, p)
	}
	if n := len(e.fake.Requests("")); n != 0 {
		t.Errorf("%d LLM requests without a model", n)
	}
}

// No organizer model and names the pattern does not fit: the task fails
// with filename_rule.not_configured (how to get the recording times),
// nothing is applied or proposed.
func TestFilenameRuleNoModelNoMatch(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil)
	e.upload("CS101_2026-10-06_1000.mp4", lt(2026, 10, 24, 18, 0))
	e.upload("CS101_2026-10-13_1000.mp4", lt(2026, 10, 24, 18, 1))
	r := e.runFilenameRule(d)
	if r.err == nil || codeOf(r.err) != "filename_rule.not_configured" {
		t.Fatalf("err %v (code %q)", r.err, codeOf(r.err))
	}
	if r.job.Status != model.StatusFailed || jobErrorCode(r.job) != "filename_rule.not_configured" {
		t.Errorf("job %s %q", r.job.Status, jobErrorCode(r.job))
	}
	if e.courseRow().FilenameRule != nil || e.proposal() != nil {
		t.Error("rule applied or proposed")
	}
	if n := len(e.fake.Requests("")); n != 0 {
		t.Errorf("%d LLM requests without a model", n)
	}
}

// With an organizer model the model is still asked first (unchanged).
func TestFilenameRuleWithModelAsksModel(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	for i, n := range llmtest.PanoptoNames {
		e.upload(n, lt(2026, 10, 24, 18, i))
	}
	e.fake.Handle(llmtest.RoleFilenameRule, func(string, string) string { return llmtest.PanoptoFilenameRuleAnswer })
	if r := e.runFilenameRule(d); r.err != nil {
		t.Fatal(r.err)
	}
	if n := e.fake.Count(llmtest.RoleFilenameRule); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

// ---------- English files on demand ----------

func TestEnglishFiles(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("Caches", "m.mp4")
	ss := e.insertSample(l.ID)
	zh := "缓存"
	ss[1].Zh, ss[1].ZhStatus = &zh, model.ZhOK // a translation never shows up in the English files

	var vtt bytes.Buffer
	e.must(WriteEnglishVTT(&vtt, ss))
	out := vtt.String()
	if !strings.HasPrefix(out, "WEBVTT\n\n1\n") || strings.Contains(out, zh) || strings.Contains(out, MissingZh) {
		t.Fatalf("vtt:\n%s", out)
	}
	cues := subs.SegmentLecture(CueSentences(ss)) // the player's cues
	var en []string
	for _, c := range cues {
		if s := cueText(c.En); s != "" {
			en = append(en, vttTime(c.StartMS)+" --> "+vttTime(c.EndMS)+"\n"+s+"\n")
		}
	}
	for i, want := range en {
		if !strings.Contains(out, "\n"+strconv.Itoa(i+1)+"\n"+want) {
			t.Fatalf("cue %d %q missing in\n%s", i+1, want, out)
		}
	}
	if n := strings.Count(out, " --> "); n != len(en) {
		t.Fatalf("%d cues, want %d", n, len(en))
	}

	var txt bytes.Buffer
	e.must(WriteEnglishText(&txt, ss))
	lines := strings.Split(strings.TrimSuffix(txt.String(), "\n"), "\n")
	if len(lines) != len(ss) || lines[0] != oneLine(ss[0].Text) || strings.Contains(txt.String(), zh) {
		t.Fatalf("txt:\n%s", txt.String())
	}
}

func TestEnglishFileName(t *testing.T) {
	cases := []struct {
		l     model.Lecture
		label string
		want  string
	}{
		{model.Lecture{Title: "Stream Ciphers and LFSRs"}, "W02L1", "W02L1 Stream Ciphers and LFSRs.en.vtt"},
		{model.Lecture{Title: "  ", OriginalFilename: "CSC1003-L01-01 Wed Oct 07 09-35-00 BST 2026.mp4"}, "W??-01JB2A", "W__-01JB2A CSC1003-L01-01 Wed Oct 07 09-35-00 BST 2026.en.vtt"},
		{model.Lecture{MediaPath: filepath.Join("x", "rec.mkv")}, "", "rec.en.vtt"},
		{model.Lecture{Title: "a/b: c?"}, "W01L2", "W01L2 a_b_ c_.en.vtt"},
	}
	for _, c := range cases {
		if got := EnglishFileName(c.label, c.l, EnglishVTTSuffix); got != c.want {
			t.Errorf("%+v: %q, want %q", c.l, got, c.want)
		}
	}
	if got := EnglishFileName("W02L1", model.Lecture{Title: "T"}, EnglishTxtSuffix); got != "W02L1 T.en.txt" {
		t.Errorf("txt %q", got)
	}
}

// ---------- the whole pipeline (SPEC §14 style) ----------

// TestTranscribeOnlyThroughScheduler wires stages.All into the real
// scheduler with settings that have no provider (SPEC-v3 V4.2 "只转写"),
// the fake ASR and a fake extract_pdf worker: the lecture launches, prep
// and asr finish (asr does not wait for the bound slides' term
// extraction), summary waits with waiting_for no_provider, no request is
// sent, the English files can be made, export does not run. Once a
// provider is configured for every role the lecture finishes by itself.
func TestTranscribeOnlyThroughScheduler(t *testing.T) {
	e := newEnv(t)
	ff := ffmpegOrSkip(t, e.cfg)
	absFake, _ := filepath.Abs(fakeASR)
	e.cfg.ASR.FakeResult = absFake
	pages := filepath.Join(e.root, "pages.json")
	e.must(os.WriteFile(pages, []byte(`{"pages": ["Cache hierarchy", "Direct mapped caches and the miss rate"]}`), 0o644))
	e.useFakeWorker("ok", pages) // extract_pdf only: asr is in fake_result mode

	set := settings.New(e.st, e.cfg) // no legacy import: no provider, no role
	e.must(set.Load(e.ctx))
	var s *sched.Scheduler
	d := e.deps(nil)
	d.Settings = set
	d.Invalidate = func(ctx context.Context, courseID string, cause state.StaleCause) error {
		return s.InvalidateCourseWith(ctx, courseID, cause)
	}
	d.GPUTotalMB = func(ctx context.Context) int { return s.GPUStatus(ctx).TotalMB }
	s, err := sched.New(sched.Options{
		Store: e.st, Config: e.cfg, Settings: set, Events: e.ev, DataDir: e.cfg.DataDir,
		GPUQuery: func(context.Context) (sched.GPUMem, error) {
			return sched.GPUMem{FreeMB: 20000, TotalMB: 24000, UsedMB: 1000}, nil
		},
		ScanInterval: 5 * time.Millisecond, ProgressMinInterval: 5 * time.Millisecond, IncrementalDebounce: 20 * time.Millisecond,
		RetryBackoff: []time.Duration{20 * time.Millisecond, 40 * time.Millisecond},
	})
	e.must(err)
	client := llm.NewWithSettings(set, s.Pool(), s.Pool(), "", nil)
	client.Backoff = []time.Duration{time.Millisecond}
	d.LLM = client
	for _, stg := range All(d) {
		s.Register(stg)
	}

	l := e.draft("Lecture 1: Caches", "upload.wav")
	silentMedia(t, ff, filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), "upload.wav"), 3)
	lid := l.ID
	m := model.Material{ID: db.NewID(), CourseID: e.course.ID, LectureID: &lid, Filename: "caches.pdf", Path: "caches.pdf", CreatedAt: db.Now()}
	e.must(e.st.CreateMaterial(e.ctx, m))
	mdir := model.MaterialDir(e.cfg.DataDir, m.CourseID, m.ID)
	e.must(os.MkdirAll(mdir, 0o755))
	e.must(os.WriteFile(filepath.Join(mdir, "caches.pdf"), []byte("%PDF-1.4"), 0o644))

	e.must(state.Recover(e.ctx, e.st, e.cfg.DataDir))
	e.must(s.Start(e.ctx))
	t.Cleanup(s.Stop)
	if err := s.Launch(e.ctx, l.ID); err != nil {
		t.Fatalf("transcribe-only launch: %v", err)
	}

	waitUntil(t, e, l.ID, 60*time.Second, func() bool {
		return e.job(model.ScopeLecture, l.ID, model.StageASR).Status == model.StatusDone &&
			s.WaitingFor(e.job(model.ScopeLecture, l.ID, model.StageSummary)) == sched.WaitNoProvider
	})
	if mt := e.job(model.ScopeMaterial, m.ID, model.StageMaterialTerms); mt.Status == model.StatusDone {
		t.Fatal("material_terms ran without a provider")
	}
	time.Sleep(100 * time.Millisecond) // many scans
	for _, st := range []string{model.StageSummary, model.StageRepair, model.StageTranslate, model.StageExport} {
		if j := e.job(model.ScopeLecture, l.ID, st); j.Status != model.StatusPending {
			t.Errorf("%s: %s before a provider is configured", st, j.Status)
		}
	}
	if n := len(e.fake.Requests("")); n != 0 {
		t.Fatalf("%d LLM requests without a provider", n)
	}
	ss := e.sentences(l.ID)
	if len(ss) == 0 {
		t.Fatal("asr wrote no sentences")
	}
	var vtt bytes.Buffer
	e.must(WriteEnglishVTT(&vtt, ss))
	if strings.Count(vtt.String(), " --> ") == 0 {
		t.Fatalf("no English cues:\n%s", vtt.String())
	}
	if _, err := os.Stat(filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), model.ExportDirName)); !os.IsNotExist(err) {
		t.Fatalf("export directory exists before translate: %v", err)
	}

	// First the term extraction role only: the slides' terms are extracted
	// by themselves, the lecture's API stages keep waiting (a deferred
	// lecture needs every lecture role). Doing this first keeps the test
	// deterministic: terms that arrive after repair and translate make them
	// stale (A2.2), which is right but not what this test is about.
	name, typ, key, conc, rpm := "Fake", model.ProviderOpenAI, llmtest.TestKey, 8, 6000 // the fake server: no pacing
	p, err := set.CreateProvider(e.ctx, settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &e.fake.URL, APIKey: &key, MaxConcurrency: &conc, RPM: &rpm})
	e.must(err)
	e.must(set.SetRole(e.ctx, model.RoleGlossary, p.ID, "fake-model", model.RoleParams{}))
	waitUntil(t, e, l.ID, 30*time.Second, func() bool {
		return e.job(model.ScopeMaterial, m.ID, model.StageMaterialTerms).Status == model.StatusDone
	})
	if w := s.WaitingFor(e.job(model.ScopeLecture, l.ID, model.StageSummary)); w != sched.WaitNoProvider {
		t.Fatalf("glossary role only: summary waiting_for %q", w)
	}

	// A provider for every role: the lecture finishes without another click.
	_, err = e.fake.ConfigureSettings(e.ctx, set, "Fake", model.ProviderOpenAI, "fake-model")
	e.must(err)
	waitUntil(t, e, l.ID, 60*time.Second, func() bool {
		for _, st := range model.LectureStages {
			if e.job(model.ScopeLecture, l.ID, st).Status != model.StatusDone {
				return false
			}
		}
		return true
	})
	cur, err := set.CurrentSnapshots()
	e.must(err)
	for _, st := range []string{model.StageSummary, model.StageRepair, model.StageTranslate} {
		j := e.job(model.ScopeLecture, l.ID, st)
		if !model.SameSnapshot(st, j.ConfigSnapshot, cur[st]) {
			t.Errorf("%s snapshot %s, want %s", st, j.ConfigSnapshot, cur[st])
		}
	}
	for _, sn := range e.sentences(l.ID) {
		if sn.ZhStatus != model.ZhOK {
			t.Errorf("sentence %d: %s", sn.Idx, sn.ZhStatus)
		}
	}
	dir := filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), model.ExportDirName)
	for _, n := range e.exportNames(l) {
		if fi, err := os.Stat(filepath.Join(dir, n)); err != nil || fi.Size() == 0 {
			t.Errorf("export %s: %v", n, err)
		}
	}
}
