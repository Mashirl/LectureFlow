// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

// v2 phase 2 (SPEC-v2 part 2): auxiliary tasks (P13.5, P7, P8.1, P13.3.2;
// lead rulings 1 and 2 of phase 2), stale_reason as JSON (P14.3, ruling 5)
// and coded job errors (P14.3, ruling 6). See .claude/v2-brief.md §9.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// ---------- auxiliary tasks ----------
//
// An auxiliary task (model.AuxStages) is a job but not a pipeline stage: it
// is never part of StagesOf, so the pipeline order, Upstream/Downstream,
// DisplayStatuses, StaleStages, EarliestStale, StaleText, CanDelete, the
// invalidation, Launch snapshots and the one-click update all ignore it.
// It has its own dependencies (auxDeps), is retried and cancelled like a
// stage but never rerun (a new run is EnqueueAux), and is never retried
// automatically (the scheduler and Recover check model.IsAuxStage).

// SplitAux separates a scope's current jobs into pipeline jobs and
// auxiliary-task jobs, keeping their order. Pure.
func SplitAux(jobs []model.Job) (pipeline, aux []model.Job) {
	for _, j := range jobs {
		if model.IsAuxStage(j.Stage) {
			aux = append(aux, j)
		} else {
			pipeline = append(pipeline, j)
		}
	}
	return pipeline, aux
}

// AuxActions are the actions of an auxiliary task: queued/running → cancel,
// failed/cancelled → retry, nothing otherwise (no rerun: regenerating is a
// new run through EnqueueAux). Never nil.
func AuxActions(j model.Job) []string {
	switch j.Status {
	case model.StatusQueued, model.StatusRunning:
		return []string{model.ActionCancel}
	case model.StatusFailed, model.StatusCancelled:
		return []string{model.ActionRetry}
	}
	return []string{}
}

// auxDeps is Deps for an auxiliary task (ruling 1): title and chapters need
// the lecture's summary done or stale, stream needs prep done
// ("upstream:{stage}" otherwise; a draft reports "not_started" first);
// filename_rule has no dependency.
func auxDeps(ctx context.Context, st db.Store, job model.Job) (bool, string, error) {
	var up string
	switch job.Stage {
	case model.StageTitle, model.StageChapters:
		up = model.StageSummary
	case model.StageStream:
		up = model.StagePrep
	default:
		return true, "", nil
	}
	lec, err := st.GetLecture(ctx, job.ScopeID)
	if err != nil {
		return false, "", err
	}
	if IsDraft(lec) {
		return false, model.WaitNotStarted, nil
	}
	u, err := st.GetCurrentJob(ctx, model.ScopeLecture, job.ScopeID, up)
	if errors.Is(err, db.ErrNotFound) {
		return false, model.WaitUpstream(up), nil
	}
	if err != nil {
		return false, "", err
	}
	ok := u.Status == model.StatusDone
	if up == model.StageSummary {
		ok = satisfied(u.Status) // a stale summary still has its text
	}
	if !ok {
		return false, model.WaitUpstream(up), nil
	}
	return true, "", nil
}

// EnqueueAux asks for one run of an auxiliary task (ruling 1). By the status
// of the current job of (scope, scopeID, stage):
//   - pending, queued or running: nothing (queued=false; it runs anyway);
//   - none, done, stale, failed or cancelled: a new pending job (the old one
//     is retired like in a rerun: attempt 0, no error, snapshot "{}" —
//     auxiliary tasks use their role's current setting, no snapshot).
//
// queued reports whether a new job was created. It does not check whether
// the task's role is configured (callers decide). stage must be an
// auxiliary stage of scope (ErrBadInput otherwise); an unknown lecture or
// course → db.ErrNotFound. It may run inside or outside a transaction.
func EnqueueAux(ctx context.Context, st db.Store, scope, scopeID, stage string) (queued bool, err error) {
	if !model.IsAuxStage(stage) || model.AuxScope(stage) != scope {
		return false, fmt.Errorf("%w: %q is not an auxiliary task of scope %q", ErrBadInput, stage, scope)
	}
	err = st.Tx(ctx, func(tx db.Store) error {
		switch scope {
		case model.ScopeLecture:
			if _, err := tx.GetLecture(ctx, scopeID); err != nil {
				return err
			}
		case model.ScopeCourse:
			if _, err := tx.GetCourse(ctx, scopeID); err != nil {
				return err
			}
		}
		var cur *model.Job
		j, err := tx.GetCurrentJob(ctx, scope, scopeID, stage)
		switch {
		case errors.Is(err, db.ErrNotFound):
		case err != nil:
			return err
		default:
			if inProgress(j.Status) {
				return nil
			}
			cur = &j
		}
		if err := newPending(ctx, tx, scope, scopeID, stage, cur, model.EmptySnapshot, model.JobStats{}); err != nil {
			return err
		}
		queued = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return queued, nil
}

// RoleConfigured reports whether an LLM role is usable: its role_setting has
// a provider and a model, and that provider exists and has a non-empty
// api_key, or its base_url is this machine (localhost, 127.0.0.0/8, ::1),
// where servers may run without a key (like settings.SnapshotUsable). It
// reads the database, so it can be called inside a transaction.
func RoleConfigured(ctx context.Context, st db.Store, role string) (bool, error) {
	rows, err := st.ListRoleSettings(ctx)
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if r.Role != role {
			continue
		}
		if r.ProviderID == nil || *r.ProviderID == "" || r.Model == nil || strings.TrimSpace(*r.Model) == "" {
			return false, nil
		}
		p, err := st.GetProvider(ctx, *r.ProviderID)
		if errors.Is(err, db.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return p.APIKey != "" || isLocalURL(p.BaseURL), nil
	}
	return false, nil
}

// isLocalURL mirrors settings.IsLocal (state does not import settings):
// base_url points at this machine, where servers may run without a key.
func isLocalURL(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// FilenameRuleWanted is the automatic trigger of P13.3.2 (ruling 2): the
// course has a lecture whose recording time is unknown (recorded_at_source
// none) and whose original file name was never used to generate a rule
// (not in kv filename_rule_samples:{course}; lectures without an original
// file name never count). A rule the user edited (source "user") is never
// regenerated automatically. Whether the organizer role is configured is
// the caller's check. Unknown course → db.ErrNotFound.
func FilenameRuleWanted(ctx context.Context, st db.Store, courseID string) (bool, error) {
	c, err := st.GetCourse(ctx, courseID)
	if err != nil {
		return false, err
	}
	if c.FilenameRule != nil {
		var r struct {
			Source string `json:"source"`
		}
		if json.Unmarshal([]byte(*c.FilenameRule), &r) == nil && r.Source == "user" {
			return false, nil
		}
	}
	used := map[string]bool{}
	if v, ok, err := st.GetKV(ctx, model.KVFilenameSamples+courseID); err != nil {
		return false, err
	} else if ok && v != "" {
		var names []string
		if json.Unmarshal([]byte(v), &names) == nil { // a broken value counts as no samples
			for _, n := range names {
				used[n] = true
			}
		}
	}
	lecs, err := st.ListLectures(ctx, courseID)
	if err != nil {
		return false, err
	}
	for _, l := range lecs {
		src := l.RecordedAtSource
		if (src == "" || src == model.RecordedAtNone) && l.OriginalFilename != "" && !used[l.OriginalFilename] {
			return true, nil
		}
	}
	return false, nil
}

// ---------- P14.3 stale_reason ----------
//
// stale_reason is stored as model.StaleReasonJSON (ruling 5). The texts of
// v1.1 are converted once at startup (ConvertStaleReasons); until then, and
// for any reason written by an older binary, every reader here also accepts
// the old text (StaleReasonFromText).

// versionParams are the params of a glossary or janitor reason: from (the
// job's recorded glossary_version, absent when not recorded) and to.
func versionParams(from *int, to int) map[string]any {
	p := map[string]any{"to": to}
	if from != nil {
		p["from"] = *from
	}
	return p
}

func reasonInput() string { return model.StaleReasonJSON(model.StaleCodeInput, nil) }

// isReasonJSON: raw is already a stale reason in the P14.3 JSON form.
func isReasonJSON(raw string) bool {
	t := strings.TrimSpace(raw)
	if !strings.HasPrefix(t, "{") {
		return false
	}
	var r model.StaleReasonCode
	return json.Unmarshal([]byte(t), &r) == nil && r.Code != ""
}

// parseReason decodes a stored stale_reason, JSON or (not yet converted) text.
func parseReason(raw string) model.StaleReasonCode {
	if isReasonJSON(raw) {
		return model.ParseStaleReason(raw)
	}
	return StaleReasonFromText(raw)
}

var (
	reGlossaryRange = regexp.MustCompile(`^(术语表已更新|术语表已整理)（v(\d+) → v(\d+)）$`)
	reGlossaryNow   = regexp.MustCompile(`^(术语表已更新|术语表已整理)（现为 v(\d+)）$`)
	reMaterial      = regexp.MustCompile(`^课件已变化：(.+)$`)
)

// StaleReasonFromText converts a stale_reason text written before v2 (v1.1
// A2.2) to its code: "术语表已更新（vN → vM）" / "（现为 vN）" →
// glossary_updated {from, to}, the same with "术语表已整理" →
// glossary_janitor, "摘要已更新" → summary_updated, "课件已变化：X" →
// material_changed {filename} ("课件已变化" without a name: no params),
// "输入已变化" → input_changed; anything else → legacy {text}. Version
// params are ints. Pure.
func StaleReasonFromText(text string) model.StaleReasonCode {
	t := strings.TrimSpace(text)
	codeOf := func(prefix string) string {
		if prefix == janitorReasonPrefix {
			return model.StaleCodeJanitor
		}
		return model.StaleCodeGlossary
	}
	if m := reGlossaryRange.FindStringSubmatch(t); m != nil {
		from, err1 := strconv.Atoi(m[2])
		to, err2 := strconv.Atoi(m[3])
		if err1 == nil && err2 == nil {
			return model.StaleReasonCode{Code: codeOf(m[1]), Params: versionParams(&from, to)}
		}
	}
	if m := reGlossaryNow.FindStringSubmatch(t); m != nil {
		if to, err := strconv.Atoi(m[2]); err == nil {
			return model.StaleReasonCode{Code: codeOf(m[1]), Params: versionParams(nil, to)}
		}
	}
	switch t {
	case model.StaleReasonSummary():
		return model.StaleReasonCode{Code: model.StaleCodeSummary}
	case materialReasonPrefix:
		return model.StaleReasonCode{Code: model.StaleCodeMaterial}
	case reasonInputChanged:
		return model.StaleReasonCode{Code: model.StaleCodeInput}
	}
	if m := reMaterial.FindStringSubmatch(t); m != nil {
		return model.StaleReasonCode{Code: model.StaleCodeMaterial, Params: map[string]any{"filename": m[1]}}
	}
	return model.StaleReasonCode{Code: model.StaleCodeLegacy, Params: map[string]any{"text": text}}
}

// materialReasonPrefix is "课件已变化" (model.StaleReasonMaterial without a name).
var materialReasonPrefix, _, _ = strings.Cut(model.StaleReasonMaterial(""), "：")

// glossaryReasonPrefix is "术语表已更新", derived so the text lives in model.
var glossaryReasonPrefix, _, _ = strings.Cut(model.StaleReasonGlossary(nil, 0), "（")

// StaleReasonOf is the structured stale reason of a job for the API (P14.3):
// no reason (nil or blank) → input_changed; JSON as stored; a text not yet
// converted → its code (StaleReasonFromText).
func StaleReasonOf(j model.Job) model.StaleReasonCode {
	if j.StaleReason == nil || strings.TrimSpace(*j.StaleReason) == "" {
		return model.StaleReasonCode{Code: model.StaleCodeInput}
	}
	return parseReason(*j.StaleReason)
}

// intParam reads an int param (an int from StaleReasonFromText, a float64
// from JSON).
func intParam(p map[string]any, key string) (int, bool) {
	switch v := p[key].(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), v == float64(int(v))
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	}
	return 0, false
}

// StaleReasonText renders a stored stale_reason (JSON or old text) as the
// Chinese text of v1.1 A2.2, e.g. "术语表已更新（v3 → v4）"; a blank reason
// or an unknown code → "输入已变化". TRANSITIONAL (P14.3): the front end
// translates "stale.reason.{code}" itself; this keeps StaleText and the
// old stale_reason texts of the API until the i18n extraction is done.
func StaleReasonText(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return reasonInputChanged
	}
	r := parseReason(raw)
	switch r.Code {
	case model.StaleCodeGlossary, model.StaleCodeJanitor:
		to, ok := intParam(r.Params, "to")
		prefix := glossaryReasonPrefix
		if r.Code == model.StaleCodeJanitor {
			prefix = janitorReasonPrefix
		}
		if !ok {
			return prefix
		}
		var from *int
		if f, ok := intParam(r.Params, "from"); ok {
			from = &f
		}
		if r.Code == model.StaleCodeJanitor {
			return model.StaleReasonJanitor(from, to)
		}
		return model.StaleReasonGlossary(from, to)
	case model.StaleCodeSummary:
		return model.StaleReasonSummary()
	case model.StaleCodeMaterial:
		if f, _ := r.Params["filename"].(string); f != "" {
			return model.StaleReasonMaterial(f)
		}
		return materialReasonPrefix
	case model.StaleCodeMaterials:
		return materialsReasonText(r.Params)
	case model.StaleCodeLegacy:
		if t, _ := r.Params["text"].(string); strings.TrimSpace(t) != "" {
			return t
		}
	}
	return reasonInputChanged
}

// isGlossaryReason: a stored reason (JSON or old text) made by a glossary
// change or a janitor run; such a reason follows later glossary changes.
// (model.IsGlossaryReason only knows the old texts.)
func isGlossaryReason(raw string) bool {
	c := parseReason(raw).Code
	return c == model.StaleCodeGlossary || c == model.StaleCodeJanitor
}

// ConvertStaleReasons is the one-time P14.3 conversion at startup (ruling
// 5): every job's stale_reason text (current or retired) becomes JSON
// (StaleReasonFromText; unknown texts → legacy). Reasons already in JSON
// and blank ones are left alone. Guarded by kv model.KVStaleReasonJSON: the
// second call does nothing. One transaction. Returns the number converted.
func ConvertStaleReasons(ctx context.Context, st db.Store) (int, error) {
	n := 0
	err := st.Tx(ctx, func(tx db.Store) error {
		if v, ok, err := tx.GetKV(ctx, model.KVStaleReasonJSON); err != nil || (ok && v == "1") {
			return err
		}
		reasons, err := tx.ListStaleReasons(ctx)
		if err != nil {
			return err
		}
		for id, raw := range reasons {
			if strings.TrimSpace(raw) == "" || isReasonJSON(raw) {
				continue
			}
			r := StaleReasonFromText(raw)
			if err := tx.ReplaceStaleReason(ctx, id, model.StaleReasonJSON(r.Code, r.Params)); err != nil {
				return err
			}
			n++
		}
		return tx.SetKV(ctx, model.KVStaleReasonJSON, "1")
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ---------- P14.3 coded job errors ----------

// EncodeJobError is the job.error text of a failed run (ruling 6): when the
// error chain holds a *model.CodedError, the JSON of its ErrorInfo with
// Detail = the text of the error it wraps (its own Detail when it wraps
// none); otherwise the error's text (legacy).
func EncodeJobError(err error) string {
	if err == nil {
		return ""
	}
	var ce *model.CodedError
	if !errors.As(err, &ce) || ce.Info.Code == "" {
		return err.Error()
	}
	info := ce.Info
	if ce.Err != nil {
		info.Detail = ce.Err.Error()
	}
	b, merr := json.Marshal(info)
	if merr != nil {
		return err.Error()
	}
	return string(b)
}

// ParseJobError decodes job.error for the API (ruling 6): an ErrorInfo
// written by EncodeJobError → info, and text = its Detail (the code when
// it has none); the §4.7 text "interrupted by restart" (stored verbatim by
// Recover) → {code: job.interrupted, detail: the text}; anything else is
// legacy text → (nil, raw). Only an object with exactly the ErrorInfo
// fields and a non-empty code counts, so a provider's JSON body quoted in
// a plain error stays text.
func ParseJobError(raw string) (info *model.ErrorInfo, text string) {
	t := strings.TrimSpace(raw)
	if t == model.ErrInterruptedByRestart {
		return &model.ErrorInfo{Code: CodeInterrupted, Detail: t}, t
	}
	if !strings.HasPrefix(t, "{") {
		return nil, raw
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(t)))
	dec.DisallowUnknownFields()
	var e model.ErrorInfo
	if err := dec.Decode(&e); err != nil || e.Code == "" || dec.More() {
		return nil, raw
	}
	if e.Detail != "" {
		return &e, e.Detail
	}
	return &e, e.Code
}
