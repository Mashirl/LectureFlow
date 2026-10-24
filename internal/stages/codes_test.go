// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// jobErrorCode is the code stored in job.error ("" for a legacy text or none).
func jobErrorCode(j model.Job) string {
	if j.Error == nil {
		return ""
	}
	info, _ := state.ParseJobError(*j.Error)
	if info == nil {
		return ""
	}
	return info.Code
}

// paramsMatch reports whether info has each of kv (pairs of name and value,
// compared as printed).
func paramsMatch(info *model.ErrorInfo, kv ...any) bool {
	if info == nil {
		return false
	}
	for i := 0; i+1 < len(kv); i += 2 {
		v, ok := info.Params[kv[i].(string)]
		if !ok || fmt.Sprint(v) != fmt.Sprint(kv[i+1]) {
			return false
		}
	}
	return true
}

// infoCode is info's code ("" for nil).
func infoCode(info *model.ErrorInfo) string {
	if info == nil {
		return ""
	}
	return info.Code
}

// extras is a note's params.extra, in order: []model.ErrorInfo as written,
// or decoded from the JSON of a stored note.
func extras(info *model.ErrorInfo) []model.ErrorInfo {
	if info == nil {
		return nil
	}
	switch x := info.Params["extra"].(type) {
	case []model.ErrorInfo:
		return x
	case nil:
		return nil
	default:
		var out []model.ErrorInfo
		b, _ := json.Marshal(x)
		_ = json.Unmarshal(b, &out)
		return out
	}
}

// extraCodes are the codes of a note's params.extra, in order.
func extraCodes(info *model.ErrorInfo) []string {
	var out []string
	for _, p := range extras(info) {
		out = append(out, p.Code)
	}
	return out
}

func TestCodesHelpers(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range ErrorCodes {
		if c == "" || seen[c] {
			t.Fatalf("ErrorCodes: %q empty or repeated", c)
		}
		seen[c] = true
	}
	if noteInfo() != nil {
		t.Fatal("noteInfo without parts")
	}
	first := part(NoteGlossarySeedDone, map[string]any{"n": 3})
	n := noteInfo(first, part(NoteFallbackApplied, map[string]any{"reference": 1, "disabled": 0}))
	if n.Code != NoteGlossarySeedDone || n.Params["n"] != 3 || fmt.Sprint(extraCodes(n)) != "["+NoteFallbackApplied+"]" {
		t.Fatalf("noteInfo %+v", n)
	}
	if _, ok := first.Params["extra"]; ok {
		t.Fatal("noteInfo changed the first part's params")
	}
	if numList([]int{2, 5}) != "2, 5" || numList(nil) != "" {
		t.Fatal("numList")
	}
	// errUnusable is coded, keeps its text and its chain.
	inner := errors.New("bad json")
	err := fmt.Errorf("wrapped: %w", &errUnusable{last: inner})
	if codeOf(err) != CodeOutputUnusable || !errors.Is(err, inner) || detailOf(&errUnusable{last: inner}) != (&errUnusable{last: inner}).Error() {
		t.Fatalf("errUnusable: %q", codeOf(err))
	}
	// withText keeps the chain under its own text.
	w := model.Errorf(CodeJanitorFailedFallback, nil, &withText{text: "t", err: inner})
	if detailOf(w) != "t" || !errors.Is(w, inner) {
		t.Fatal("withText")
	}
}

// Job errors of input problems carry their codes (v2 P14.3).
func TestStageInputErrorCodes(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil)
	check := func(name string, r runResult, code string, kv ...any) {
		t.Helper()
		var ce *model.CodedError
		if r.err == nil || !errors.As(r.err, &ce) || ce.Info.Code != code || !paramsMatch(&ce.Info, kv...) || jobErrorCode(r.job) != code {
			t.Errorf("%s: err %v, want %s %v", name, r.err, code, kv)
		}
	}

	// glossary_seed: no syllabus, a missing file, a file that is not UTF-8
	e.must(state.ResetGlossarySeed(e.ctx, e.st, e.course.ID))
	check("no syllabus", e.run(e.stage(d, model.StageGlossarySeed), e.course.ID), CodeGlossaryNoSyllabus)
	e.must(e.st.UpsertSyllabus(e.ctx, model.Syllabus{CourseID: e.course.ID, Filename: "gone.txt", Path: "gone.txt"}))
	e.must(state.ResetGlossarySeed(e.ctx, e.st, e.course.ID))
	check("missing syllabus file", e.run(e.stage(d, model.StageGlossarySeed), e.course.ID), CodeGlossarySyllabusMissing, "filename", "gone.txt")
	dir := model.SyllabusDir(e.cfg.DataDir, e.course.ID)
	e.must(os.MkdirAll(dir, 0o755))
	e.must(os.WriteFile(filepath.Join(dir, "latin1.txt"), []byte("caf\xe9"), 0o644))
	e.must(e.st.UpsertSyllabus(e.ctx, model.Syllabus{CourseID: e.course.ID, Filename: "latin1.txt", Path: "latin1.txt"}))
	e.must(state.ResetGlossarySeed(e.ctx, e.st, e.course.ID))
	check("not UTF-8", e.run(e.stage(d, model.StageGlossarySeed), e.course.ID), CodeGlossarySyllabusNotUTF8, "filename", "latin1.txt")

	// material_terms before material_extract
	m := e.material(nil, "s.pdf", nil)
	check("not extracted", e.run(e.stage(d, model.StageMaterialTerms), m.ID), CodeMaterialNotExtracted, "filename", "s.pdf")

	// summary without sentences; translate without a summary
	l := e.lecture("L", "m.mp4")
	check("no sentences", e.run(e.stage(d, model.StageSummary), l.ID), CodeSummaryNoSentences)
	e.insertSample(l.ID)
	check("no summary", e.run(e.stage(d, model.StageTranslate), l.ID), CodeTranslateNoSummary)

	// MergeTerms refuses other sources (G6 rule 1)
	if _, err := MergeTerms(e.ctx, d, e.course.ID, []FoundTerm{{En: "cache"}}, "review"); codeOf(err) != CodeGlossaryBadSource {
		t.Errorf("MergeTerms(review): %v", err)
	}
}
