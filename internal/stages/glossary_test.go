// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

func termMap(ts []model.Term) map[string]model.Term {
	m := map[string]model.Term{}
	for _, t := range ts {
		m[t.EnKey] = t
	}
	return m
}

func (e *env) material(lectureID *string, filename string, pages []string) model.Material {
	e.t.Helper()
	m := model.Material{ID: db.NewID(), CourseID: e.course.ID, LectureID: lectureID, Filename: filename, Path: filename, CreatedAt: db.Now()}
	e.must(e.st.CreateMaterial(e.ctx, m))
	if pages != nil {
		e.must(e.st.SetMaterialPages(e.ctx, m.ID, pages))
		m.PagesMD = pages
	}
	return m
}

func TestMergeTermsRules(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil)
	e.addTerm("Cache", "", model.SourceManual, 0)       // manual: never touched by a merge
	e.addTerm("TLB", "", model.SourceSyllabus, 0)       // empty zh: filled
	e.addTerm("LRU", "最近最少使用", model.SourceSyllabus, 0) // has zh: kept
	e.material(nil, "a.pdf", []string{"A cache line holds data. Cache lines and the LRU policy.", "TLB, tlb, TLBs; cache-line"})

	found := []FoundTerm{
		{"cache", "缓存"},
		{"tlb", "快表"},
		{"LRU", "LRU 替换"},
		{" write-back ", " 写回 "},
		{"cache   line", ""},
		{"a", "一"}, {"64", ""}, {"--", ""}, {"one two three four five six seven", ""}, // dropped
		{"MESI", ""}, {"mesi", "MESI 协议"}, // duplicate in one batch: inserted once, zh filled by the second
	}
	v0 := e.course.GlossaryVersion
	res, err := MergeTerms(e.ctx, d, e.course.ID, found, model.SourceMaterial)
	if err != nil {
		t.Fatal(err)
	}
	m := termMap(e.terms())
	if len(m) != 6 {
		t.Fatalf("terms %v", m)
	}
	check := func(key, en, zh, source string, freq int) {
		t.Helper()
		x, ok := m[key]
		if !ok || x.En != en || x.Zh != zh || x.Source != source || x.Freq != freq || !x.Enabled {
			t.Errorf("term %s: %+v, want en=%q zh=%q source=%s freq=%d", key, x, en, zh, source, freq)
		}
	}
	check("cache", "Cache", "", model.SourceManual, 3) // freq still recomputed (whole words: cache, Cache, cache-line)
	check("tlb", "TLB", "快表", model.SourceSyllabus, 2) // "TLBs" is not a whole word
	check("lru", "LRU", "最近最少使用", model.SourceSyllabus, 1)
	check("write-back", "write-back", "写回", model.SourceMaterial, 0)
	check("cache line", "cache line", "", model.SourceMaterial, 1) // "Cache lines" and "cache-line" do not count
	check("mesi", "MESI", "MESI 协议", model.SourceMaterial, 0)
	if res.Inserted != 3 || res.FilledZh != 2 || !res.Changed() {
		t.Errorf("result %+v", res)
	}
	c, _ := e.st.GetCourse(e.ctx, e.course.ID)
	if c.GlossaryVersion != v0+1 || res.Version != c.GlossaryVersion {
		t.Errorf("glossary_version %d (result %d), want %d", c.GlossaryVersion, res.Version, v0+1)
	}
	if got := e.invalidations(); len(got) != 1 || got[0] != e.course.ID {
		t.Errorf("invalidations %v", got)
	}
	// v1.1 A2.2: the invalidation says why (stale_reason 术语表已更新)
	if c := e.invalCauses(); len(c) != 1 || c[0] != (state.StaleCause{Kind: state.StaleGlossary}) {
		t.Errorf("invalidation cause %+v", c)
	}
	if !e.ev.has("terms", e.course.ID) {
		t.Error("no terms event")
	}

	// the same merge again changes nothing: no version bump, no invalidation
	res, err = MergeTerms(e.ctx, d, e.course.ID, found, model.SourceMaterial)
	if err != nil || res.Changed() {
		t.Errorf("second merge: %+v %v", res, err)
	}
	if c2, _ := e.st.GetCourse(e.ctx, e.course.ID); c2.GlossaryVersion != c.GlossaryVersion || len(e.invalidations()) != 1 {
		t.Error("an unchanged merge bumped the version or invalidated")
	}

	// a new material changes only freq: still a change
	e.material(nil, "b.pdf", []string{"MESI MESI write-back"})
	res, _ = MergeTerms(e.ctx, d, e.course.ID, nil, model.SourceMaterial)
	if res.FreqChanged != 2 || res.Inserted != 0 || len(e.invalidations()) != 2 {
		t.Errorf("freq-only merge %+v, invalidations %v", res, e.invalidations())
	}
}

func TestGlossarySeedFromTextFile(t *testing.T) {
	e := newEnv(t)
	dir := model.SyllabusDir(e.cfg.DataDir, e.course.ID)
	e.must(os.MkdirAll(dir, 0o755))
	e.must(os.WriteFile(filepath.Join(dir, "syllabus.md"), []byte("\xef\xbb\xbf# Computer Architecture\nCaches, write-back, LRU."), 0o644))
	e.must(e.st.UpsertSyllabus(e.ctx, model.Syllabus{CourseID: e.course.ID, Filename: "syllabus.md", Path: "syllabus.md"}))
	e.must(state.ResetGlossarySeed(e.ctx, e.st, e.course.ID))

	r := e.run(e.stage(e.deps(nil), model.StageGlossarySeed), e.course.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	sy, _ := e.st.GetSyllabus(e.ctx, e.course.ID)
	if sy.Text == nil || *sy.Text != "# Computer Architecture\nCaches, write-back, LRU." {
		t.Errorf("syllabus text %v", sy.Text)
	}
	reqs := e.fake.Requests(llmtest.RoleGlossarySeed)
	if len(reqs) != 1 || reqs[0].User != "SYLLABUS:\n# Computer Architecture\nCaches, write-back, LRU." || !reqs[0].JSONMode {
		t.Errorf("requests %+v", reqs)
	}
	m := termMap(e.terms())
	if len(m) != 6 || m["cache"].Zh != "缓存" || m["lru"].Source != model.SourceSyllabus || m["x86"].Zh != "" {
		t.Errorf("terms %v (the 2 discard-rule entries must be dropped)", m)
	}
	if infoCode(r.stats.NoteInfo) != NoteGlossarySeedDone || !paramsMatch(r.stats.NoteInfo, "inserted", 6) || r.stats.Requests != 1 {
		t.Errorf("stats %+v", r.stats)
	}
}

// TestGlossarySeedReadsSyllabusText: v1.1 A8.2 — the input is syllabus.text
// (SYLLABUS.md, the formatted syllabus the user confirmed), not the raw
// upload; the files are not read.
func TestGlossarySeedReadsSyllabusText(t *testing.T) {
	e := newEnv(t)
	dir := model.SyllabusDir(e.cfg.DataDir, e.course.ID)
	e.must(os.MkdirAll(dir, 0o755))
	e.must(os.WriteFile(filepath.Join(dir, "syllabus.raw.txt"), []byte("RAW TEXT THAT MUST NOT BE SENT"), 0o644))
	md := "# CSC1003 Computer Architecture\n## Weekly Topics\n| Week | Topic | Notes |\n|---|---|---|\n| 1 | Caches | |\n## Key Terms\ncache, write-back, LRU\n"
	e.must(e.st.UpsertSyllabus(e.ctx, model.Syllabus{CourseID: e.course.ID, Filename: "SYLLABUS.md", Path: "SYLLABUS.md", Text: &md}))
	e.must(state.ResetGlossarySeed(e.ctx, e.st, e.course.ID))

	r := e.run(e.stage(e.deps(nil), model.StageGlossarySeed), e.course.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	reqs := e.fake.Requests(llmtest.RoleGlossarySeed)
	if len(reqs) != 1 || reqs[0].User != "SYLLABUS:\n"+md {
		t.Errorf("glossary_seed must send syllabus.text: %+v", reqs)
	}
	if sy, _ := e.st.GetSyllabus(e.ctx, e.course.ID); sy.Text == nil || *sy.Text != md {
		t.Error("syllabus.text must not be rewritten")
	}
	if len(e.terms()) == 0 {
		t.Error("no terms merged")
	}
}

func TestGlossarySeedPDFAndEmpty(t *testing.T) {
	e := newEnv(t)
	pages := filepath.Join(e.root, "pages.json")
	e.must(os.WriteFile(pages, []byte(`{"pages": ["Week 1: caches", "", "Week 2: virtual memory"]}`), 0o644))
	e.useFakeWorker("ok", pages)
	dir := model.SyllabusDir(e.cfg.DataDir, e.course.ID)
	e.must(os.MkdirAll(dir, 0o755))
	e.must(os.WriteFile(filepath.Join(dir, "s.pdf"), []byte("%PDF-1.4 fake"), 0o644))
	e.must(e.st.UpsertSyllabus(e.ctx, model.Syllabus{CourseID: e.course.ID, Filename: "s.pdf", Path: "s.pdf"}))
	e.must(state.ResetGlossarySeed(e.ctx, e.st, e.course.ID))
	if r := e.run(e.stage(e.deps(nil), model.StageGlossarySeed), e.course.ID); r.err != nil {
		t.Fatal(r.err)
	}
	sy, _ := e.st.GetSyllabus(e.ctx, e.course.ID)
	if sy.Text == nil || *sy.Text != "Week 1: caches\n\n\n\nWeek 2: virtual memory" {
		t.Errorf("pdf syllabus text %q", *sy.Text)
	}
	if _, err := os.Stat(filepath.Join(dir, "result.json")); err != nil {
		t.Error("extract_pdf result.json missing")
	}

	// a syllabus without text: done, no LLM call, a note explains why
	e2 := newEnv(t)
	dir2 := model.SyllabusDir(e2.cfg.DataDir, e2.course.ID)
	e2.must(os.MkdirAll(dir2, 0o755))
	e2.must(os.WriteFile(filepath.Join(dir2, "s.txt"), []byte("   \n"), 0o644))
	e2.must(e2.st.UpsertSyllabus(e2.ctx, model.Syllabus{CourseID: e2.course.ID, Filename: "s.txt", Path: "s.txt"}))
	e2.must(state.ResetGlossarySeed(e2.ctx, e2.st, e2.course.ID))
	r := e2.run(e2.stage(e2.deps(nil), model.StageGlossarySeed), e2.course.ID)
	if r.err != nil || r.job.Status != model.StatusDone || infoCode(r.stats.NoteInfo) != NoteGlossarySeedNoText || e2.fake.Count(llmtest.RoleGlossarySeed) != 0 {
		t.Errorf("empty syllabus: %v %s %q", r.err, r.job.Status, r.stats.Note)
	}
}

func TestMaterialStages(t *testing.T) {
	e := newEnv(t)
	pages := filepath.Join(e.root, "pages.json")
	e.must(os.WriteFile(pages, []byte(`{"pages": ["Cache hierarchy and the miss rate", "", "Direct mapped cache"]}`), 0o644))
	e.useFakeWorker("ok", pages)
	m := e.material(nil, "slides.pdf", nil)
	mdir := model.MaterialDir(e.cfg.DataDir, m.CourseID, m.ID)
	e.must(os.MkdirAll(mdir, 0o755))
	e.must(os.WriteFile(filepath.Join(mdir, "slides.pdf"), []byte("%PDF-1.4"), 0o644))
	d := e.deps(nil)

	// material_terms before extraction is an error
	if r := e.run(e.stage(d, model.StageMaterialTerms), m.ID); r.err == nil {
		t.Error("material_terms without pages must fail")
	}

	if r := e.run(e.stage(d, model.StageMaterialExtract), m.ID); r.err != nil {
		t.Fatal(r.err)
	}
	got, _ := e.st.GetMaterial(e.ctx, m.ID)
	if len(got.PagesMD) != 3 || got.PagesMD[2] != "Direct mapped cache" {
		t.Errorf("pages %v", got.PagesMD)
	}
	if len(e.invalidations()) != 1 || !e.ev.has("course", e.course.ID) {
		t.Errorf("extract must trigger course invalidation: %v", e.invalidations())
	}
	if c := e.invalCauses(); len(c) != 1 || c[0] != (state.StaleCause{Kind: state.StaleMaterial, Material: "slides.pdf"}) {
		t.Errorf("extract invalidation cause %+v (stale_reason 课件已变化：slides.pdf)", c)
	}

	r := e.run(e.stage(d, model.StageMaterialTerms), m.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	tm := termMap(e.terms())
	if len(tm) != 7 || tm["cache hierarchy"].Source != model.SourceMaterial || tm["cache hierarchy"].Freq != 1 ||
		tm["cache"].Freq != 2 || tm["miss rate"].Freq != 1 || tm["write-back"].Zh != "" {
		t.Errorf("terms %v", tm)
	}
	reqs := e.fake.Requests(llmtest.RoleMaterialTerms)
	if len(reqs) != 1 || reqs[0].User != "SLIDES:\nCache hierarchy and the miss rate\n\nDirect mapped cache" {
		t.Errorf("requests %+v", reqs)
	}
	if p, ok := e.stage(d, model.StageMaterialTerms).(sched.ProviderStage); !ok || p.Provider() != "deepseek" {
		t.Error("material_terms must report its provider")
	}
}

func TestMaterialExtractInputError(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("exit2", "")
	m := e.material(nil, "bad.pdf", nil)
	mdir := model.MaterialDir(e.cfg.DataDir, m.CourseID, m.ID)
	e.must(os.MkdirAll(mdir, 0o755))
	e.must(os.WriteFile(filepath.Join(mdir, "bad.pdf"), []byte("not a pdf"), 0o644))
	r := e.run(e.stage(e.deps(nil), model.StageMaterialExtract), m.ID)
	if r.err == nil || !sched.IsPermanent(r.err) || !strings.Contains(r.err.Error(), "ERROR:") || codeOf(r.err) != workerPDF+workerInputError {
		t.Errorf("err %v", r.err)
	}
	m2 := e.material(nil, "missing.pdf", nil)
	if r := e.run(e.stage(e.deps(nil), model.StageMaterialExtract), m2.ID); r.err == nil || !sched.IsPermanent(r.err) || codeOf(r.err) != CodeMaterialFileMissing {
		t.Errorf("missing file: %v", r.err)
	}
}

func TestMaterialTermsPartialFailureWarns(t *testing.T) {
	e := newEnv(t)
	big := strings.Repeat("cache line ", 1500) // ~16k chars: one page per chunk
	m := e.material(nil, "big.pdf", []string{big, big})
	fake := &scriptLLM{fn: func(_ context.Context, n int, _ llmCall) (string, error) {
		if n == 0 {
			return `{"terms": [{"en": "cache line", "zh": "缓存行"}]}`, nil
		}
		return "not json", nil
	}}
	d := e.deps(fake)
	d.Concurrency = 1
	r := e.run(e.stage(d, model.StageMaterialTerms), m.ID)
	if r.err != nil || r.job.Status != model.StatusDone {
		t.Fatalf("err %v status %s", r.err, r.job.Status)
	}
	if w := r.stats.WarningInfo; infoCode(w) != WarnMaterialTermsLost || !paramsMatch(w, "chunks", "2", "attempts", 3) ||
		infoCode(r.stats.NoteInfo) != NoteMaterialTermsDone || !slices.Contains(extraCodes(r.stats.NoteInfo), NoteMaterialSkipped) || r.stats.Warning == "" {
		t.Errorf("warning %+v note %+v", r.stats.WarningInfo, r.stats.NoteInfo)
	}
	if tm := termMap(e.terms()); tm["cache line"].Zh != "缓存行" || tm["cache line"].Freq != 3000 {
		t.Errorf("terms %v", tm)
	}
}

// manyTerms is a terms answer with n distinct, valid terms in order.
func manyTerms(n int) string {
	var b strings.Builder
	b.WriteString(`{"terms": [`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"en": "widget%03d protocol", "zh": "协议%03d"}`, i, i)
	}
	b.WriteString(`]}`)
	return b.String()
}

// The §13.1 prompt asks for "at most 150 terms, most subject-specific
// first"; a model that returns more (real run: 423 from a 1,856-character
// SYLLABUS.md) is cut to the first 150 (lead decision, NOTES.md).
func TestGlossarySeedKeepsPromptLimit(t *testing.T) {
	e := newEnv(t)
	md := "# CSC1003 Cryptography\n## Key Terms\nwidget000 protocol\n"
	e.must(e.st.UpsertSyllabus(e.ctx, model.Syllabus{CourseID: e.course.ID, Filename: "SYLLABUS.md", Path: "SYLLABUS.md", Text: &md}))
	e.must(state.ResetGlossarySeed(e.ctx, e.st, e.course.ID))
	fake := &scriptLLM{fn: func(context.Context, int, llmCall) (string, error) { return manyTerms(423), nil }}
	r := e.run(e.stage(e.deps(fake), model.StageGlossarySeed), e.course.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	tm := termMap(e.terms())
	if len(tm) != 150 || tm["widget000 protocol"].Zh != "协议000" || tm["widget149 protocol"].Zh != "协议149" {
		t.Fatalf("%d terms; want the first 150", len(tm))
	}
	if _, ok := tm["widget150 protocol"]; ok {
		t.Error("term 151 must be dropped")
	}
	if n := r.stats.NoteInfo; !slices.Contains(extraCodes(n), NoteGlossarySeedCapped) || !paramsMatch(n, "n", 150) {
		t.Errorf("note %+v must say how many were returned and kept", n)
	}
	for _, p := range extras(r.stats.NoteInfo) {
		if p.Code == NoteGlossarySeedCapped && !paramsMatch(&p, "returned", 423, "max", 150) {
			t.Errorf("capped part %+v", p)
		}
	}
}

// manyTermsFrom is manyTerms numbered from start.
func manyTermsFrom(start, n int) string {
	var b strings.Builder
	b.WriteString(`{"terms": [`)
	for i := start; i < start+n; i++ {
		if i > start {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"en": "widget%03d protocol", "zh": "协议%03d"}`, i, i)
	}
	b.WriteString(`]}`)
	return b.String()
}

// §13.2: "at most 100" per slides chunk (each chunk is cut on its own: two
// chunks of 130 distinct terms give 200), and the note says so.
func TestMaterialTermsKeepPromptLimitPerChunk(t *testing.T) {
	e := newEnv(t)
	big := strings.Repeat("cache line ", 1500) // ~16k chars: one page per chunk
	m := e.material(nil, "big.pdf", []string{big, big})
	fake := &scriptLLM{fn: func(_ context.Context, n int, _ llmCall) (string, error) { return manyTermsFrom(n*1000, 130), nil }}
	d := e.deps(fake)
	d.Concurrency = 1
	r := e.run(e.stage(d, model.StageMaterialTerms), m.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	tm := termMap(e.terms())
	if len(tm) != 200 {
		t.Errorf("%d terms; want 200 (the first 100 of each chunk)", len(tm))
	}
	if _, ok := tm["widget099 protocol"]; !ok {
		t.Error("chunk 1 term 100 missing")
	}
	if _, ok := tm["widget100 protocol"]; ok {
		t.Error("chunk 1 term 101 must be dropped")
	}
	capped := false
	if n := r.stats.NoteInfo; n != nil {
		for _, p := range extras(n) {
			capped = capped || (p.Code == NoteMaterialCapped && paramsMatch(&p, "chunks", "1, 2", "returned", "130, 130"))
		}
	}
	if !capped {
		t.Errorf("note %+v must say which chunks were cut", r.stats.NoteInfo)
	}
}

// Entries the §6.0.1 rules drop and repeats do not use up the limit: the
// first 150 usable distinct terms are kept.
func TestGlossarySeedLimitCountsUsableTerms(t *testing.T) {
	e := newEnv(t)
	md := "# C\n## Key Terms\nx\n"
	e.must(e.st.UpsertSyllabus(e.ctx, model.Syllabus{CourseID: e.course.ID, Filename: "SYLLABUS.md", Path: "SYLLABUS.md", Text: &md}))
	e.must(state.ResetGlossarySeed(e.ctx, e.st, e.course.ID))
	var b strings.Builder
	b.WriteString(`{"terms": [{"en": "x", "zh": ""}, {"en": "1234", "zh": ""}, {"en": "widget000 protocol", "zh": ""}, {"en": "Widget000  Protocol", "zh": "协议000"}`)
	for i := 1; i < 200; i++ {
		fmt.Fprintf(&b, `, {"en": "widget%03d protocol", "zh": "协议%03d"}`, i, i)
	}
	b.WriteString(`]}`)
	answer := b.String()
	fake := &scriptLLM{fn: func(context.Context, int, llmCall) (string, error) { return answer, nil }}
	r := e.run(e.stage(e.deps(fake), model.StageGlossarySeed), e.course.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	tm := termMap(e.terms())
	if len(tm) != 150 {
		t.Fatalf("%d terms; want 150 usable distinct terms", len(tm))
	}
	if _, ok := tm["widget149 protocol"]; !ok {
		t.Error("term 150 missing: dropped or repeated entries must not use up the limit")
	}
	if tm["widget000 protocol"].Zh != "协议000" {
		t.Errorf("a repeat of a kept term still fills its empty zh: %+v", tm["widget000 protocol"])
	}
	if n := r.stats.NoteInfo; infoCode(n) != NoteGlossarySeedDone || !paramsMatch(n, "n", 150) ||
		!paramsMatch(&extras(n)[len(extras(n))-1], "returned", 200) {
		t.Errorf("note %q: 150 kept of 200 distinct usable terms", r.stats.Note)
	}
}
