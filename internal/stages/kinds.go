// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"lectureflow/internal/calendar"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// Lecture kinds in the pipeline (user request 2026-10-23, lead freeze
// .claude/kinds-brief.md; K2): the kind_map of a generated file-name rule
// and the prompt versions of the organizer tasks. The kind catalog type
// (kindCatalog) and the writes of lecture.kind are in calendar.go
// (resolveOne) and title.go (applyTitle).

// looseString decodes a JSON string; any other JSON value (a number, an
// array, an object) decodes as "". For optional answer fields the model may
// get wrong (the title answer's kind) without making the whole answer
// unusable.
type looseString string

func (s *looseString) UnmarshalJSON(b []byte) error {
	var v string
	if json.Unmarshal(b, &v) != nil {
		v = ""
	}
	*s = looseString(v)
	return nil
}

// hasKindGroup reports whether regex compiles and has a (?P<kind>…) group.
func hasKindGroup(regex string) (compiles, has bool) {
	re, err := regexp.Compile(regex)
	if err != nil {
		return false, false
	}
	return true, slices.Contains(re.SubexpNames(), calendar.GroupKind)
}

// cleanKindMap checks the kind_map of a filename_rule answer against the
// kind catalog (lead freeze 1.3): an entry whose code is empty or whose
// kind is not a catalog id is dropped (an id differing only in case is
// corrected: "Tutorial" → tutorial); a map that is not a JSON object of
// codes, or one given without a kind group in the regex, is ignored as a
// whole. Dropping never rejects the rule (the kind is optional); notes say
// in English what was dropped, for the retry feedback when the rule is
// asked again for another reason. The result is nil when nothing is left.
// Call it only for a regex that compiles.
func cleanKindMap(raw json.RawMessage, hasGroup bool, kinds kindCatalog) (map[string]string, []string) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(t, &m); err != nil {
		return nil, []string{ctxbuild.FilenameKindMapNotObject()}
	}
	if len(m) == 0 {
		return nil, nil
	}
	if !hasGroup {
		return nil, []string{ctxbuild.FilenameKindMapNoGroup()}
	}
	codes := make([]string, 0, len(m))
	for c := range m {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	out := map[string]string{}
	var notes []string
	for _, code := range codes {
		v, isString := m[code].(string)
		if !isString {
			v = fmt.Sprint(m[code])
		}
		k := ""
		if isString {
			k = kinds.resolve(v)
		}
		switch {
		case strings.TrimSpace(code) == "":
			notes = append(notes, ctxbuild.FilenameKindMapEmptyCode(v))
		case k == "":
			notes = append(notes, ctxbuild.FilenameKindMapUnknownKind(code, v))
		default:
			out[strings.TrimSpace(code)] = k
		}
	}
	if len(out) == 0 {
		return nil, notes
	}
	return out, notes
}

// dropUncapturedCodes removes from kindMap the codes that the kind group
// captured in none of the samples (parsed: ValidateRule's rows; a code
// counts when a row's code equals it, also ignoring case, as LookupKind
// matches). Lead ruling after the real check of prompt version 3: a
// rejected answer mapped codes the course's names never had (S → seminar,
// P → past_lecture, M → material); kept, a later "P01" meaning practical
// would be read as a past lecture. Deterministic: no prompt change, no
// extra request. notes say in English what was dropped (the retry
// feedback); the result is nil when nothing is left. The rows' kinds do not
// depend on the dropped entries.
func dropUncapturedCodes(kindMap map[string]string, parsed []calendar.Parsed) (map[string]string, []string) {
	if len(kindMap) == 0 {
		return kindMap, nil
	}
	var captured []string
	for _, p := range parsed {
		if p.KindCode != "" {
			captured = append(captured, p.KindCode)
		}
	}
	seen := func(code string) bool {
		for _, c := range captured {
			if c == code || strings.EqualFold(c, code) {
				return true
			}
		}
		return false
	}
	codes := make([]string, 0, len(kindMap))
	for c := range kindMap {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	out := map[string]string{}
	var notes []string
	for _, c := range codes {
		if seen(strings.TrimSpace(c)) {
			out[c] = kindMap[c]
			continue
		}
		notes = append(notes, fmt.Sprintf("kind_map entry %q → %q was dropped: no file name has the code %q in its kind group.", c, kindMap[c], c))
	}
	if len(out) == 0 {
		return nil, notes
	}
	return out, notes
}

// unmappedKindCodes are the distinct codes the kind group captured in
// parsed that the rule's kind_map does not map, in order of appearance.
func unmappedKindCodes(parsed []calendar.Parsed) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range parsed {
		if p.KindCode == "" || p.Kind != "" || seen[p.KindCode] {
			continue
		}
		seen[p.KindCode] = true
		out = append(out, p.KindCode)
	}
	return out
}

// ---------- renumbering by an automatic kind (lead ruling on K2's report) ----------

// KVRenumberedAutoPrefix: renumbered_auto:{course_id} → JSON
// []RenumberNotice, oldest first (at most maxRenumberNotices): the
// renumberings caused by kinds the title task set automatically in a
// course whose citations were exported (P13.6). The course view shows them
// as a notice until the user dismisses it (DismissRenumberNotices); "" =
// none.
const KVRenumberedAutoPrefix = "renumbered_auto:"

// CodeRenumberedAuto is the code of a RenumberNotice (front end:
// notes.calendar.renumbered_auto). Params: lecture (the lecture whose kind
// was set), n (number of changes), changes ([{lecture_id, from, to}]: the
// other lectures whose citation label changed, from → to, e.g. W02L2 →
// W02L1; "W??-…" for an unnumbered one).
const CodeRenumberedAuto = "calendar.renumbered_auto"

// maxRenumberNotices bounds the kept notices (the newest are kept).
const maxRenumberNotices = 20

// RenumberNotice is one automatic renumbering: the ErrorInfo (code
// CodeRenumberedAuto, params, the Chinese text as detail) and when it
// happened (UTC RFC 3339).
type RenumberNotice struct {
	model.ErrorInfo
	At string `json:"at"`
}

// kindWrite is what a kind write of the title task changed, announced after
// its transaction commits (kindEvents).
type kindWrite struct {
	courseID   string   // "" = no kind was written
	renumbered []string // other lectures whose citation label changed
	notified   bool     // a RenumberNotice was recorded
}

// kindEvents announces a kind write: the course (its numbering changed) and
// every other lecture whose label changed (SSE changed).
func (d Deps) kindEvents(kw kindWrite) {
	if kw.courseID == "" {
		return
	}
	for _, id := range kw.renumbered {
		d.events().Changed("lecture", id)
	}
	d.events().Changed("course", kw.courseID)
}

// renumberedOthers are the lectures other than self whose citation label
// differs between before and after (CourseLabels results), ordered by the
// new label, then id.
func renumberedOthers(before, after map[string]calendar.Numbered, self string) []string {
	var out []string
	for id, a := range after {
		if id == self {
			continue
		}
		if b, ok := before[id]; ok && b.CiteLabel != a.CiteLabel {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if li, lj := after[out[i]].CiteLabel, after[out[j]].CiteLabel; li != lj {
			return li < lj
		}
		return out[i] < out[j]
	})
	return out
}

// recordRenumberNotice appends a RenumberNotice for the lectures ids
// (renumberedOthers) to kv renumbered_auto:{course} inside tx, when the
// course's citations were exported (kv cite_exported:{course}, P13.6);
// otherwise nothing is recorded (recorded = false).
func (d Deps) recordRenumberNotice(ctx context.Context, tx db.Store, courseID, lectureID string, before, after map[string]calendar.Numbered, ids []string) (recorded bool, err error) {
	if v, ok, err := tx.GetKV(ctx, model.KVCiteExportPrefix+courseID); err != nil {
		return false, err
	} else if !ok || strings.TrimSpace(v) == "" {
		return false, nil
	}
	changes := make([]map[string]any, len(ids))
	texts := make([]string, len(ids))
	for i, id := range ids {
		from, to := before[id].CiteLabel, after[id].CiteLabel
		changes[i] = map[string]any{"lecture_id": id, "from": from, "to": to}
		texts[i] = from + " → " + to
	}
	n := RenumberNotice{
		ErrorInfo: model.ErrorInfo{
			Code:   CodeRenumberedAuto,
			Params: map[string]any{"lecture": lectureID, "n": len(ids), "changes": changes},
			Detail: fmt.Sprintf("自动识别讲座类型后，另有 %d 节讲座的编号发生变化（%s），之前导出的引用将与新编号不一致",
				len(ids), strings.Join(texts, "、")),
		},
		At: d.now().UTC().Format(time.RFC3339),
	}
	list, err := RenumberNotices(ctx, tx, courseID)
	if err != nil {
		return false, err
	}
	list = append(list, n)
	if len(list) > maxRenumberNotices {
		list = list[len(list)-maxRenumberNotices:]
	}
	b, err := json.Marshal(list)
	if err != nil {
		return false, err
	}
	return true, tx.SetKV(ctx, KVRenumberedAutoPrefix+courseID, string(b))
}

// RenumberNotices returns the course's automatic renumbering notices
// (kv renumbered_auto:{course}), oldest first; empty when none or
// unreadable.
func RenumberNotices(ctx context.Context, st db.Store, courseID string) ([]RenumberNotice, error) {
	v, ok, err := st.GetKV(ctx, KVRenumberedAutoPrefix+courseID)
	if err != nil || !ok || strings.TrimSpace(v) == "" {
		return []RenumberNotice{}, err
	}
	var out []RenumberNotice
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		slog.Warn("renumbering notices unreadable", "course", courseID, "err", err)
		return []RenumberNotice{}, nil
	}
	return out, nil
}

// DismissRenumberNotices clears the course's automatic renumbering notices
// (the user read them; a new export makes them moot too).
func DismissRenumberNotices(ctx context.Context, st db.Store, courseID string) error {
	return st.SetKV(ctx, KVRenumberedAutoPrefix+courseID, "")
}

// recordPromptVersion writes the prompt version of an auxiliary task's run
// into its job's config_snapshot (SPEC-v2 G5.5 applied to the organizer
// prompts changed for lecture kinds): the LLM target of role as the run
// uses it (auxiliary tasks run with the role's current setting) with
// prompt_version, or only {"prompt_version": N} when the target is not
// known (legacy config). Auxiliary jobs are created with an empty snapshot
// and nothing compares or reuses theirs (the scheduler and the rerun
// decision skip auxiliary stages), so this is a record of what ran. A
// failure is only logged; a run outside the scheduler (no job in ctx)
// writes nothing.
func (d Deps) recordPromptVersion(ctx context.Context, role string, version int) {
	j, ok := sched.JobFrom(ctx)
	if !ok || j.ID == "" {
		return
	}
	snap := fmt.Sprintf(`{"prompt_version":%d}`, version)
	if d.Settings != nil {
		if t, err := d.Settings.RoleTarget(role); err == nil {
			t.PromptVersion = version
			snap = model.MarshalSnapshot(t)
		}
	}
	if err := d.Store.SetJobSnapshot(context.WithoutCancel(ctx), j.ID, snap); err != nil {
		slog.Warn("record prompt version", "job", j.ID, "stage", j.Stage, "err", err)
	}
}
