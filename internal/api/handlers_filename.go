// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"lectureflow/internal/calendar"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/stages"
	"lectureflow/internal/state"
)

// Per-course file-name rules for the recording time (SPEC-v2 P13.3;
// phase-2 lead rulings 2, 3): the saved rule and its preview, a generated
// proposal waiting for 应用 / 编辑 / 忽略, the live preview while editing,
// applying (with the renumbering check of P13.6) and undoing an automatic
// first apply. The calendar service itself is organizer-p2's
// (stages.ApplyFilenameRule, stages.UndoFilenameRule).

// filenameSamples are the course's lectures that have an original file
// name, as rule samples (ListLectures order).
func (s *Server) filenameSamples(ctx context.Context, lecs []model.Lecture) []calendar.Sample {
	out := []calendar.Sample{}
	for _, l := range lecs {
		if strings.TrimSpace(l.OriginalFilename) != "" {
			out = append(out, s.sample(ctx, l))
		}
	}
	return out
}

// PreviewRows converts parsed samples to the preview table rows (shared
// with mockserve).
func PreviewRows(ps []calendar.Parsed) []FilenamePreviewRow {
	out := make([]FilenamePreviewRow, 0, len(ps))
	for _, p := range ps {
		row := FilenamePreviewRow{Filename: p.Filename, Status: p.Status(), HasTime: p.HasTime, Title: p.Title,
			Suspicious: p.Suspicious, Error: p.Error, KindCode: p.KindCode, Kind: p.Kind, Past: p.Past}
		if p.Problem != nil {
			info := ProblemInfo(*p.Problem)
			row.ErrorInfo = &info
		}
		if p.RecordedAt != nil {
			v := calendar.FormatRecordedAt(*p.RecordedAt)
			row.RecordedAt = &v
		}
		out = append(out, row)
	}
	return out
}

// ProblemInfo is a calendar problem as an ErrorInfo (P14.3): the Chinese
// text becomes the detail; a nested problem (file_invalid's reason) is
// converted too.
func ProblemInfo(p calendar.Problem) model.ErrorInfo {
	info := model.ErrorInfo{Code: p.Code, Detail: p.Text}
	if len(p.Params) > 0 {
		info.Params = make(map[string]any, len(p.Params))
		for k, v := range p.Params {
			if sub, ok := v.(calendar.Problem); ok {
				v = ProblemInfo(sub)
			}
			info.Params[k] = v
		}
	}
	return info
}

// ValidateRule is the deterministic check of P13.3.2 over the samples
// (FilenamePreviewResult; shared with mockserve): calendar.ValidateRule, so
// a date more than 30 days before the term start is a past lecture, not a
// failure (lead ruling 1.1), and every row carries the kind its "kind"
// group maps to through rule.KindMap.
func ValidateRule(rule calendar.FilenameRule, samples []calendar.Sample, termStart *time.Time) FilenamePreviewResult {
	parsed, matched, ok, problems := calendar.ValidateRule(rule, samples, termStart)
	res := FilenamePreviewResult{Preview: PreviewRows(parsed), Matched: matched, Total: len(samples), OK: ok}
	for _, p := range problems {
		res.Problems = append(res.Problems, p.Text)
		res.ProblemsInfo = append(res.ProblemsInfo, ProblemInfo(p))
	}
	return res
}

// validateRule is ValidateRule with the course's term start.
func validateRule(rule calendar.FilenameRule, samples []calendar.Sample, termStart *string) FilenamePreviewResult {
	ts, _ := courseTerm(termStart, nil)
	return ValidateRule(rule, samples, ts)
}

// CleanKindMap is a rule's kind_map as saved and previewed: codes trimmed,
// empty codes dropped; nil when nothing is left. unknown is the first kind
// id (in code order) that is not in the catalog ("" = none): the preview
// drops such entries, PUT refuses them (kind.unknown).
func CleanKindMap(in map[string]string, kinds []model.LectureKind) (out map[string]string, unknown string) {
	known := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		known[k.ID] = true
	}
	codes := make([]string, 0, len(in))
	for code := range in {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	for _, raw := range codes {
		code, kind := strings.TrimSpace(raw), strings.TrimSpace(in[raw])
		if code == "" {
			continue
		}
		if !known[kind] {
			if unknown == "" {
				unknown = kind
			}
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[code] = kind
	}
	return out, unknown
}

// sameKindMap compares two kind maps (nil equals empty).
func sameKindMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// ruleKinds drops the kind_map entries of rule whose kind is not in the
// catalog (a kind deleted since; the rows then show the code as unmapped).
func ruleKinds(rule calendar.FilenameRule, kinds []model.LectureKind) calendar.FilenameRule {
	rule.KindMap, _ = CleanKindMap(rule.KindMap, kinds)
	return rule
}

// filenameProposal reads kv filename_rule_proposal:{course} ("" = none).
func (s *Server) filenameProposal(ctx context.Context, courseID string) (*model.FilenameProposal, *calendar.FilenameRule, error) {
	raw, ok, err := s.kvValue(ctx, model.KVFilenameProposal+courseID)
	if err != nil || !ok {
		return nil, nil, err
	}
	var p model.FilenameProposal
	var rule calendar.FilenameRule
	if json.Unmarshal([]byte(raw), &p) != nil || json.Unmarshal(p.Rule, &rule) != nil || rule.Regex == "" {
		return nil, nil, nil
	}
	return &p, &rule, nil
}

// filenameRuleView is GET /api/courses/{id}/filename-rule.
func (s *Server) filenameRuleView(ctx context.Context, c model.Course) (FilenameRuleView, error) {
	lecs, err := s.st.ListLectures(ctx, c.ID)
	if err != nil {
		return FilenameRuleView{}, err
	}
	kinds, err := s.st.ListLectureKinds(ctx)
	if err != nil {
		return FilenameRuleView{}, err
	}
	samples := s.filenameSamples(ctx, lecs)
	v := FilenameRuleView{Preview: []FilenamePreviewRow{}}
	if rule := parseRule(c.FilenameRule); rule != nil {
		v.Rule = rule
		v.KindMap = rule.KindMap
		parsed := make([]calendar.Parsed, len(samples))
		compiled, cerr := calendar.Compile(ruleKinds(*rule, kinds))
		ts, _ := courseTerm(c.TermStart, nil)
		for i, sm := range samples {
			if cerr != nil {
				parsed[i] = calendar.Failed(sm.Filename, cerr)
			} else {
				parsed[i] = compiled.Parse(sm)
				parsed[i].Past = calendar.PastRecording(parsed[i].RecordedAt, ts)
			}
		}
		v.Preview = PreviewRows(parsed)
	}
	p, rule, err := s.filenameProposal(ctx, c.ID)
	if err != nil {
		return FilenameRuleView{}, err
	}
	if p != nil {
		res := validateRule(ruleKinds(*rule, kinds), samples, c.TermStart)
		v.Proposal = &FilenameProposal{Rule: *rule, Explanation: p.Explanation, CreatedAt: p.CreatedAt,
			Preview: res.Preview, Matched: res.Matched, Total: res.Total, OK: res.OK, Problems: res.Problems,
			ProblemsInfo: res.ProblemsInfo, KindMap: rule.KindMap}
	}
	if j, err := s.st.GetCurrentJob(ctx, model.ScopeCourse, c.ID, model.StageFilenameRule); err == nil {
		t := s.auxView(j)
		v.Task = &t
	} else if !errors.Is(err, db.ErrNotFound) {
		return FilenameRuleView{}, err
	}
	return v, nil
}

func (s *Server) respondFilenameRule(w http.ResponseWriter, r *http.Request, c model.Course, status int) {
	v, err := s.filenameRuleView(r.Context(), c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, status, v)
}

// GET /api/courses/{id}/filename-rule → FilenameRuleView.
func (s *Server) getFilenameRule(w http.ResponseWriter, r *http.Request) {
	c, err := s.courseOr404(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respondFilenameRule(w, r, c, http.StatusOK)
}

// POST /api/courses/{id}/filename-rule/generate ("重新识别文件名格式",
// P13.3.2): queue the filename_rule task now (no debounce) → 202 with the
// view (task pending). 409 when the organizer role is not configured or the
// course has no file names yet.
func (s *Server) generateFilenameRule(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, err := s.courseOr404(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !s.roleConfigured(model.RoleOrganizer) {
		writeError(w, http.StatusConflict, "filename_rule.not_configured", nil)
		return
	}
	lecs, err := s.st.ListLectures(ctx, c.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(s.filenameSamples(ctx, lecs)) == 0 {
		writeError(w, http.StatusConflict, "filename_rule.no_samples", nil)
		return
	}
	if _, err := s.sc.EnqueueAux(ctx, model.ScopeCourse, c.ID, model.StageFilenameRule); err != nil {
		s.fail(w, r, notFoundAs(err, "course.not_found"))
		return
	}
	s.hub.Changed("course", c.ID)
	s.respondFilenameRule(w, r, c, http.StatusAccepted)
}

// POST /api/courses/{id}/filename-rule/preview {regex}: the validation
// table of an edited rule, nothing saved (P13.3.3 实时刷新预览表).
func (s *Server) previewFilenameRule(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in FilenamePreviewInput
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if strings.TrimSpace(in.Regex) == "" {
		writeError(w, http.StatusBadRequest, "filename_rule.regex_empty", nil)
		return
	}
	c, err := s.courseOr404(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	lecs, err := s.st.ListLectures(ctx, c.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	kinds, err := s.st.ListLectureKinds(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	km, _ := CleanKindMap(in.KindMap, kinds)
	writeJSON(w, http.StatusOK, validateRule(calendar.FilenameRule{Regex: in.Regex, KindMap: km}, s.filenameSamples(ctx, lecs), c.TermStart))
}

// PUT /api/courses/{id}/filename-rule {regex, confirm} (P13.3.3 应用 and
// 手动编辑): the proposal's regex is saved as the model's rule (source llm),
// the current rule's regex keeps its source, any other regex is the user's
// (source user; never regenerated automatically). It must compile with the
// month and day groups (400 filename_rule.invalid); rows it cannot parse
// simply stay without a recording time. Every lecture whose recording time
// is not the user's is re-resolved; when that renumbers lectures of a
// course whose citations were exported → 409 RenumberConflict unless
// confirmed (P13.6). → the course.
func (s *Server) putFilenameRule(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in FilenamePreviewInput
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if strings.TrimSpace(in.Regex) == "" {
		writeError(w, http.StatusBadRequest, "filename_rule.regex_empty", nil)
		return
	}
	c, err := s.courseOr404(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	kinds, err := s.st.ListLectureKinds(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	kindMap, unknown := CleanKindMap(in.KindMap, kinds)
	if unknown != "" {
		writeError(w, http.StatusBadRequest, "kind.unknown", map[string]any{"kind": unknown})
		return
	}
	rule := calendar.FilenameRule{Regex: in.Regex, Source: calendar.RuleSourceUser}
	_, proposed, err := s.filenameProposal(ctx, c.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The rule keeps its source when neither the regex nor the kind_map was
	// edited; any edit makes it the user's (never regenerated automatically).
	switch cur := parseRule(c.FilenameRule); {
	case proposed != nil && proposed.Regex == in.Regex && sameKindMap(ruleKinds(*proposed, kinds).KindMap, kindMap):
		rule = *proposed
	case cur != nil && cur.Regex == in.Regex && sameKindMap(ruleKinds(*cur, kinds).KindMap, kindMap):
		rule = *cur
	}
	rule.KindMap = kindMap
	if rule.Source == "" {
		rule.Source = calendar.RuleSourceLLM
	}
	if _, err := calendar.Compile(rule); err != nil {
		writeErrorInfo(w, http.StatusBadRequest, model.ErrorInfo{Code: "filename_rule.invalid", Detail: err.Error()})
		return
	}
	lecs, err := s.st.ListLectures(ctx, c.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	samples := s.filenameSamples(ctx, lecs)
	res := validateRule(rule, samples, c.TermStart)
	rule.SamplesMatched, rule.SamplesTotal = res.Matched, res.Total
	if rule.CreatedAt == "" {
		rule.CreatedAt = s.now().UTC().Format(time.RFC3339)
	}
	// What the lectures' recording times become (stages.ApplyFilenameRule):
	// the rule's date where it parses, else the media's creation time.
	raw, _ := json.Marshal(rule)
	ruleText := string(raw)
	withRule := c
	withRule.FilenameRule = &ruleText
	after := make([]model.Lecture, len(lecs))
	for i, l := range lecs {
		if l.RecordedAtSource != model.RecordedAtUser {
			l.RecordedAt, l.RecordedAtSource = s.predictRecordedAt(ctx, withRule, l)
		}
		llm, err := state.RememberedLLMKind(ctx, s.st, l)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		l.Kind, l.KindSource = predictKind(withRule, l, kinds, llm)
		after[i] = l
	}
	if err := s.checkRenumber(ctx, c, lecs, withRule, after, in.Confirm); err != nil {
		s.failRenumber(w, r, err)
		return
	}
	if err := stages.ApplyFilenameRule(ctx, s.stageDeps(), c.ID, rule); err != nil {
		s.fail(w, r, notFoundAs(err, "course.not_found"))
		return
	}
	// stages.ApplyFilenameRule asks for the material-set comparison itself
	// when a recording time changed (K2)
	s.hub.Changed("course", c.ID)
	s.respondCourse(w, r, c.ID, http.StatusOK)
}

// DELETE /api/courses/{id}/filename-rule/proposal (P13.3.3 忽略): the
// proposal is dropped, nothing else changes. → the course; 404 when there
// is none.
func (s *Server) dismissFilenameProposal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, err := s.courseOr404(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if _, ok, err := s.kvValue(ctx, model.KVFilenameProposal+c.ID); err != nil {
		s.fail(w, r, err)
		return
	} else if !ok {
		writeError(w, http.StatusNotFound, "filename_rule.no_proposal", nil)
		return
	}
	// kv has no delete: "" means none (organizer-p2 reads it the same way).
	if err := s.st.SetKV(ctx, model.KVFilenameProposal+c.ID, ""); err != nil {
		s.fail(w, r, err)
		return
	}
	s.hub.Changed("course", c.ID)
	s.respondCourse(w, r, c.ID, http.StatusOK)
}

// POST /api/courses/{id}/filename-rule/undo (P13.3.3 "已根据文件名设置录制
// 时间，撤销"): the automatic first apply is undone (stages.UndoFilenameRule
// restores what the user has not changed since). → the course; 404
// filename_rule.no_undo when there is nothing to undo.
func (s *Server) undoFilenameRule(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, err := s.courseOr404(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := stages.UndoFilenameRule(ctx, s.stageDeps(), c.ID); err != nil {
		s.fail(w, r, notFoundAs(err, "filename_rule.no_undo"))
		return
	}
	s.hub.Changed("course", c.ID) // UndoFilenameRule asks for the material-set comparison (K2)
	s.respondCourse(w, r, c.ID, http.StatusOK)
}
