// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llmtest_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/qa"
	"lectureflow/internal/sched"
)

func setup(t *testing.T) (*llmtest.Server, *llm.HTTPClient) {
	t.Helper()
	s := llmtest.New(t)
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	s.Configure(cfg)
	c := llm.New(cfg, nil, "", nil)
	c.Backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	return s, c
}

type sampleRow struct {
	Idx          int                 `json:"idx"`
	ASRText      string              `json:"asr_text"`
	LowConfWords []model.LowConfWord `json:"low_conf_words"`
}

func sample(t *testing.T) []ctxbuild.Sentence {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(llmtest.FixtureDir(), "..", "sample_sentences.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []sampleRow
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	out := make([]ctxbuild.Sentence, len(rows))
	for i, r := range rows {
		out[i] = ctxbuild.Sentence{Idx: r.Idx, Text: r.ASRText, ASRText: r.ASRText, LowConfWords: r.LowConfWords}
	}
	return out
}

type termsResp struct {
	Terms []struct{ En, Zh string } `json:"terms"`
}

type patchesResp struct {
	Patches []struct {
		Idx    int    `json:"idx"`
		After  string `json:"after"`
		Class  string `json:"class"`
		Reason string `json:"reason"`
	} `json:"patches"`
}

// mergeTerms is a minimal stand-in for the MergeTerms of SPEC §6.0.1.
func mergeTerms(terms []model.Term, resp termsResp, source string) []model.Term {
	for _, nt := range resp.Terms {
		if ctxbuild.DropTerm(nt.En) {
			continue
		}
		key := ctxbuild.EnKey(nt.En)
		i := slices.IndexFunc(terms, func(t model.Term) bool { return t.EnKey == key })
		if i < 0 {
			terms = append(terms, model.Term{En: strings.TrimSpace(nt.En), EnKey: key, Zh: nt.Zh, Source: source, Enabled: true})
		} else if terms[i].Zh == "" && nt.Zh != "" && terms[i].Source != model.SourceManual {
			terms[i].Zh = nt.Zh
		}
	}
	return terms
}

func complete(t *testing.T, c *llm.HTTPClient, role string, r ctxbuild.Request, jsonMode bool) string {
	t.Helper()
	res, err := c.Complete(context.Background(), role, r.System, r.User, jsonMode)
	if err != nil {
		t.Fatalf("%s: %v", role, err)
	}
	return res.Text
}

// TestPipelineFixtures runs the canned answers through ctxbuild-built requests
// the way the stages will, and checks the fixtures meet SPEC §14: role
// detection, repair classes incl. a >50% edit, translations that pass qa.
func TestPipelineFixtures(t *testing.T) {
	s, c := setup(t)
	ss := sample(t)

	// glossary_seed then material_terms (and the reverse order) give the same glossary
	var seed, mat termsResp
	if err := llm.ParseJSON(complete(t, c, config.RoleGlossary, ctxbuild.GlossarySeedRequest("Syllabus: caches."), true), &seed); err != nil {
		t.Fatal(err)
	}
	mreqs := ctxbuild.MaterialTermsRequests([]string{"Caches", "Write-back"})
	if err := llm.ParseJSON(complete(t, c, config.RoleGlossary, mreqs[0], true), &mat); err != nil {
		t.Fatal(err)
	}
	dropped := 0
	for _, nt := range seed.Terms {
		if ctxbuild.DropTerm(nt.En) {
			dropped++
		}
	}
	if dropped != 2 {
		t.Errorf("glossary_seed.json should contain 2 terms the discard rule drops, has %d", dropped)
	}
	terms := mergeTerms(mergeTerms(nil, seed, model.SourceSyllabus), mat, model.SourceMaterial)
	reverse := mergeTerms(mergeTerms(nil, mat, model.SourceMaterial), seed, model.SourceSyllabus)
	zhOf := func(ts []model.Term) map[string]string {
		m := map[string]string{}
		for _, t := range ts {
			m[t.EnKey] = t.Zh
		}
		return m
	}
	if a, b := zhOf(terms), zhOf(reverse); len(a) != len(b) || !mapsEqual(a, b) {
		t.Errorf("merge order changes the glossary:\n%v\n%v", a, b)
	}
	if zhOf(terms)["write-back"] != "写回" || zhOf(terms)["cache"] != "缓存" {
		t.Errorf("merged glossary %v", zhOf(terms))
	}

	lec := ctxbuild.LectureSnapshot{LectureID: "L", Sentences: ss, Terms: terms}
	summary := complete(t, c, config.RoleSummary, ctxbuild.SummaryRequest(lec), false)
	if len(strings.Fields(summary)) < 50 || strings.HasPrefix(summary, "{") {
		t.Errorf("summary fixture must be plain English text: %q", summary)
	}
	lec.Summary = summary

	// repair
	chunks := ctxbuild.BuildRepairRequest(lec)
	var pr patchesResp
	if err := llm.ParseJSON(complete(t, c, config.RoleRepair, ctxbuild.Request{System: chunks[0].System, User: chunks[0].User}, true), &pr); err != nil {
		t.Fatal(err)
	}
	classes := map[string]bool{}
	final := map[int]string{} // last patch per idx in the chunk
	downgraded := 0
	for _, p := range pr.Patches {
		classes[p.Class] = true
		if !chunks[0].Contains(p.Idx) {
			continue
		}
		if ctxbuild.EditExceedsHalf(ss[p.Idx].ASRText, p.After) {
			downgraded++
			if p.Idx != 11 || p.Class == model.ClassDecide {
				t.Errorf("unexpected >50%% edit at idx %d (%s)", p.Idx, p.Class)
			}
		}
		final[p.Idx] = p.After
	}
	for _, cl := range []string{model.ClassAuto, model.ClassAbsorb, model.ClassDecide} {
		if !classes[cl] {
			t.Errorf("repair.json lacks a %q patch", cl)
		}
	}
	if downgraded != 1 {
		t.Errorf("repair.json needs exactly one patch to downgrade (idx 11), has %d", downgraded)
	}
	if len(final) != 5 || final[4] != "So the first question is how we find a line in the cache." {
		t.Errorf("patched sentences %v", final)
	}

	// translate after repair, then after rejecting the decide patches (9, 11)
	for _, variant := range []string{"repaired", "rejected"} {
		snap := lec
		snap.Sentences = slices.Clone(ss)
		for idx, after := range final {
			if variant == "rejected" && (idx == 9 || idx == 11) {
				continue
			}
			snap.Sentences[idx].Text = after
		}
		for i := range snap.Sentences {
			req := ctxbuild.BuildTranslateRequest(snap, i, 3)
			var out struct {
				Idx int    `json:"idx"`
				Zh  string `json:"zh"`
			}
			if err := llm.ParseJSON(complete(t, c, config.RoleTranslate, ctxbuild.Request{System: req.System, User: req.User}, true), &out); err != nil {
				t.Fatal(err)
			}
			if out.Idx != i {
				t.Errorf("translate idx %d answered %d", i, out.Idx)
			}
			if ok, reason := qa.Check(snap.Sentences[i].Text, out.Zh, snap.Terms); !ok {
				t.Errorf("%s idx %d: fixture translation %q fails qa: %s", variant, i, out.Zh, reason)
			}
		}
	}
	got := []int{s.Count(llmtest.RoleGlossarySeed), s.Count(llmtest.RoleMaterialTerms), s.Count(llmtest.RoleSummary),
		s.Count(llmtest.RoleRepair), s.Count(llmtest.RoleTranslate)}
	if want := []int{1, 1, 1, 1, 28}; !slices.Equal(got, want) {
		t.Errorf("counts %v, want %v", got, want)
	}
	idxs := s.TranslateIdxs()
	if len(idxs) != 28 || idxs[0] != 0 || idxs[13] != 13 || idxs[14] != 0 {
		t.Errorf("translate idxs %v", idxs)
	}
	for _, r := range s.Requests("") {
		if r.Auth != "Bearer "+llmtest.TestKey {
			t.Errorf("auth %q", r.Auth)
		}
	}
	if r := s.Requests(llmtest.RoleSummary)[0]; r.JSONMode || r.Temperature != 0.2 {
		t.Errorf("summary request %+v", r)
	}
	if r := s.Requests(llmtest.RoleRepair)[0]; !r.JSONMode || r.Temperature != 0.1 {
		t.Errorf("repair request json=%v temp=%v", r.JSONMode, r.Temperature)
	}
}

func mapsEqual(a, b map[string]string) bool {
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestRoleOf(t *testing.T) {
	cases := map[string]string{
		ctxbuild.GlossarySeedSystem:       llmtest.RoleGlossarySeed,
		ctxbuild.MaterialTermsSystem:      llmtest.RoleMaterialTerms,
		ctxbuild.SummarySystemTemplate:    llmtest.RoleSummary,
		ctxbuild.RepairSystemTemplate:     llmtest.RoleRepair,
		ctxbuild.TranslateSystemTemplate:  llmtest.RoleTranslate,
		"You are a helpful assistant.\nx": "",
	}
	for sys, want := range cases {
		if got := llmtest.RoleOf(sys); got != want {
			t.Errorf("RoleOf(%q...) = %q, want %q", ctxbuild.FirstLine(sys), got, want)
		}
	}
}

func translateOnce(t *testing.T, c *llm.HTTPClient, idx int) (llm.Result, error) {
	lec := ctxbuild.LectureSnapshot{Sentences: sample(t)}
	req := ctxbuild.BuildTranslateRequest(lec, idx, 3)
	return c.Complete(context.Background(), config.RoleTranslate, req.System, req.User, true)
}

func TestTranslateOverridesAndUsage(t *testing.T) {
	s, c := setup(t)
	s.SetTranslation(5, "覆盖的译文。")
	res, err := translateOnce(t, c, 5)
	if err != nil || !strings.Contains(res.Text, "覆盖的译文") || !strings.Contains(res.Text, `"idx":5`) {
		t.Fatalf("%+v %v", res, err)
	}
	if res.PromptTokens == 0 || res.CompletionTokens == 0 || res.CacheHitTokens != 0 {
		t.Errorf("first request usage %+v", res)
	}
	res, _ = translateOnce(t, c, 6)
	if res.CacheHitTokens == 0 || res.CacheHitTokens >= res.PromptTokens {
		t.Errorf("same prefix should report a cache hit: %+v", res)
	}
	if got := s.TranslateIdxs(); !slices.Equal(got, []int{5, 6}) {
		t.Errorf("idxs %v", got)
	}
	s.Reset()
	if s.Count(llmtest.RoleTranslate) != 0 || len(s.TranslateIdxs()) != 0 {
		t.Error("Reset kept requests")
	}
	if res, _ := translateOnce(t, c, 5); !strings.Contains(res.Text, "覆盖的译文") {
		t.Error("Reset dropped overrides")
	}
	// unknown idx → default
	s.SetResponse(llmtest.RoleSummary, "custom summary")
	sum := ctxbuild.SummaryRequest(ctxbuild.LectureSnapshot{Sentences: sample(t)})
	if res, err := c.Complete(context.Background(), config.RoleSummary, sum.System, sum.User, false); err != nil || res.Text != "custom summary" {
		t.Errorf("SetResponse: %+v %v", res, err)
	}
}

func TestFailureInjection(t *testing.T) {
	s, c := setup(t)

	s.FailNext(llmtest.RoleTranslate, 2, llmtest.FailHTTP500)
	if _, err := translateOnce(t, c, 1); err != nil {
		t.Fatalf("500s should be retried: %v", err)
	}
	if s.Count(llmtest.RoleTranslate) != 3 {
		t.Errorf("count %d, want 3", s.Count(llmtest.RoleTranslate))
	}

	s.FailNext(llmtest.RoleTranslate, 1, llmtest.FailHTTP429)
	if _, err := translateOnce(t, c, 1); err != nil {
		t.Errorf("429 should be retried: %v", err)
	}

	s.FailNext(llmtest.RoleTranslate, 1, llmtest.FailHTTP401)
	if _, err := translateOnce(t, c, 1); err == nil || !sched.IsPermanent(err) {
		t.Errorf("401: %v", err)
	}

	var out struct {
		Idx int    `json:"idx"`
		Zh  string `json:"zh"`
	}
	s.FailNext(llmtest.RoleTranslate, 1, llmtest.FailNotJSON)
	res, err := translateOnce(t, c, 1)
	if err != nil || llm.ParseJSON(res.Text, &out) == nil {
		t.Errorf("not-JSON answer: %+v %v", res, err)
	}

	s.FailNext(llmtest.RoleTranslate, 1, llmtest.FailWrongIdx)
	res, _ = translateOnce(t, c, 1)
	if err := llm.ParseJSON(res.Text, &out); err != nil || out.Idx == 1 {
		t.Errorf("wrong idx answer: %+v %v", out, err)
	}

	s.FailNext(llmtest.RoleTranslate, 1, llmtest.FailFenced)
	res, _ = translateOnce(t, c, 1)
	if !strings.HasPrefix(res.Text, "```json") || llm.ParseJSON(res.Text, &out) != nil || out.Idx != 1 {
		t.Errorf("fenced answer: %q", res.Text)
	}

	// failures are per role
	s.FailNext(llmtest.RoleRepair, 1, llmtest.FailHTTP401)
	if _, err := translateOnce(t, c, 2); err != nil {
		t.Errorf("repair failure leaked into translate: %v", err)
	}
}

func TestUnknownRoleRejected(t *testing.T) {
	_, c := setup(t)
	_, err := c.Complete(context.Background(), config.RoleSummary, "Some other prompt", "u", false)
	if err == nil || !sched.IsPermanent(err) {
		t.Errorf("unknown system prompt: %v", err)
	}
}

func TestTargetParsing(t *testing.T) {
	u := "[上文]\nA.\n[目标 #12]\nThe target.\n[原始识别]\nThe targat.\n\nYour previous translation was rejected: x. Translate again."
	if llmtest.TargetIdx(u) != 12 || llmtest.TargetText(u) != "The target." {
		t.Errorf("idx %d text %q", llmtest.TargetIdx(u), llmtest.TargetText(u))
	}
	if llmtest.TargetIdx("no target") != -1 {
		t.Error("missing target")
	}
}
