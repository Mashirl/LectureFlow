// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package cite builds the citation JSON of SPEC-v1.1 A11, in the format of
// SPEC-v2 P13.6: a lecture (or a whole course) in a form another LLM can
// quote precisely, with a "{course} W02L1@12:34" reference on every sentence.
//
// The package is pure like ctxbuild: no database, files, network, clock or
// randomness. Callers load the rows, compute the calendar numbers of the
// course's lectures and pass the generation time.
package cite

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode"

	"lectureflow/internal/calendar"
	"lectureflow/internal/model"
)

// Schema is the "schema" value of every document. v2: P13.6 replaced the
// v1.1 sequence labels (L03) by calendar numbers (W02L1) and removed "seq",
// so documents of the two versions cannot be mixed. v3 (lecture kinds,
// .claude/kinds-brief.md 1.4): "label" is numbered per kind — a taught
// session per teaching week and kind ("W02L1", "W02T1", "W03S1"), teaching
// material per course and kind ("P1", "M2"), "W??-{id prefix}" without a
// number — and every lecture has "kind" and "kind_code"; the "kinds" table
// names the kinds of the document's lectures (review item 7). A course of
// lectures only (none recorded more than 30 days before the term start)
// gets the labels, ids and refs of v2 unchanged.
const Schema = "lectureflow.cite/v3"

// howToCite is the "how_to_cite" text of A11.3; the example reference is
// filled with the document's own course part and first lecture label.
const howToCite = "Cite statements with the ref field in square brackets, e.g. [%s]. Use the sentence id when an exact reference is needed."

// exampleTime is the time of the how_to_cite example (A11.3).
const exampleTime = "12:34"

// exampleLabel is the label of the how_to_cite example of a document
// without lectures.
const exampleLabel = "W01L1"

// fallbackCoursePart is used when neither the course code nor the course
// name yields any letter or digit.
const fallbackCoursePart = "COURSE"

// Doc is the citation document (A11.3). Field order is the JSON order.
type Doc struct {
	Schema      string    `json:"schema"`
	HowToCite   string    `json:"how_to_cite"`
	Course      Course    `json:"course"`
	Kinds       []Kind    `json:"kinds"`
	Lectures    []Lecture `json:"lectures"`
	Glossary    []Term    `json:"glossary"`
	GeneratedAt string    `json:"generated_at"`
}

// Course is the "course" object; Code is null when the course has no code.
type Course struct {
	ID   string  `json:"id"`
	Code *string `json:"code"`
	Name string  `json:"name"`
}

// Kind is one element of "kinds" (v3): a lecture kind of the document's
// lectures, so that a reader can tell what the letters in a label mean.
// Name is the English name of a built-in kind (BuiltinKindNames: the
// interface translates them, the document is read by another LLM) and the
// user's own name of a custom kind.
type Kind struct {
	ID   string `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
}

// BuiltinKindNames are the "kinds" names of the built-in kinds (the full
// names of the English interface when schema 3 got the table).
var BuiltinKindNames = map[string]string{
	calendar.KindLecture:     "Lecture",
	calendar.KindTutorial:    "Tutorial",
	calendar.KindSeminar:     "Seminar",
	calendar.KindPastLecture: "Past lecture (teaching material)",
	calendar.KindMaterial:    "Additional material",
}

// Lecture is one element of "lectures" (A11.3 with the P13.6 changes:
// label is the calendar number, week/n/recorded_at/title_zh/blurb_en/
// blurb_zh are added, seq is gone; v3: kind, kind_code). Kind is the
// lecture's kind id (lecture, tutorial, seminar, past_lecture, material, or
// a custom kind's id) and KindCode its code (the letters of label; given
// for "W??-…" too); both are in "kinds". Week is the teaching week the lecture was recorded in, for any kind
// (null when it cannot be computed); N is the number in label (the
// position among the same kind in that week for a taught session, in the
// course for teaching material; null without a number, label "W??-…").
// The text fields are null when unset.
type Lecture struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	Kind       string     `json:"kind"`
	KindCode   string     `json:"kind_code"`
	Week       *int       `json:"week"`
	N          *int       `json:"n"`
	RecordedAt *string    `json:"recorded_at"`
	Title      string     `json:"title"`
	TitleZh    *string    `json:"title_zh"`
	BlurbEn    *string    `json:"blurb_en"`
	BlurbZh    *string    `json:"blurb_zh"`
	DurationMS int64      `json:"duration_ms"`
	Summary    string     `json:"summary"`
	Sentences  []Sentence `json:"sentences"`
}

// Sentence is one element of a lecture's "sentences". Zh is null unless the
// sentence's zh_status is ok.
type Sentence struct {
	ID      string  `json:"id"`
	Ref     string  `json:"ref"`
	StartMS int64   `json:"start_ms"`
	EndMS   int64   `json:"end_ms"`
	En      string  `json:"en"`
	Zh      *string `json:"zh"`
	Edited  bool    `json:"edited"`
}

// Term is one element of "glossary" (enabled terms only). Zh is "" for terms
// kept in English (SPEC §13.1).
type Term struct {
	En string `json:"en"`
	Zh string `json:"zh"`
}

// LectureInput is one lecture with what its citation entry needs.
// MediaMS is the length of the lecture's media (audio.wav, read by the
// caller: the package does no I/O); 0 = unknown.
type LectureInput struct {
	Lecture   model.Lecture
	Summary   string
	Sentences []model.Sentence
	MediaMS   int64
}

// Build assembles the document (A11.3, P13.6, lecture kinds):
//   - numbers are the calendar numbers (calendar.LabelKinds, through
//     state.NumberCourse) of EVERY lecture
//     of the course, keyed by lecture id, not only of the exported ones: the
//     "W??-{id prefix}" labels of unnumbered lectures are made unique across
//     the course (calendar.CiteLabels), so a lecture carries the same label
//     in its own file and in the course file. A lecture missing from numbers
//     counts as unnumbered;
//   - label = the number ("W02L1", "W02T1", "P1") or "W??-{id prefix}";
//     kind = the effective kind (Numbered.Kind; "lecture" for a lecture
//     missing from numbers), kind_code = its code (Numbered.Code, else the
//     catalog's); week = the teaching week (null when 0); n = the number in
//     label (null without a number);
//   - kinds = the kinds of the exported lectures, each once, in catalog
//     order (kinds: the lecture_kind rows; nil = the built-in kinds): a
//     built-in kind named in English (BuiltinKindNames), a custom kind by
//     its user-given name; a kind missing from the catalog (deleted in
//     between) is listed with the lectures' code and its id as the name;
//   - lectures ordered by recorded_at, lectures without one last; ties by
//     original file name (P13.3.4), created_at, id; sentences by idx;
//   - recorded_at = "YYYY-MM-DDTHH:MM", or "YYYY-MM-DD" when only the date is
//     known (stored as 00:00, calendar.HasClock);
//   - the course part of ref and id is the course code, or, without a code,
//     the course name abbreviation (CoursePart);
//   - ref = "{course} {label}@{time of start_ms}" (mm:ss, h:mm:ss from one
//     hour on); several sentences in the same second share a ref;
//   - id = "{course}.{label}.s{sentence number, 4 digits}", the sentence
//     number being idx+1 as shown in the UI ("第 n 句");
//   - en = sentence.text; zh = sentence.zh only when zh_status is ok;
//     edited = human_text is not empty;
//   - duration_ms = the media length (LectureInput.MediaMS, the same value
//     the lecture page shows as Lecture.duration_ms), else, while it is
//     unknown, end_ms of the last sentence (0 without sentences);
//   - glossary = enabled terms in en_key order;
//   - generated_at = generatedAt in UTC, RFC 3339.
func Build(course model.Course, kinds []model.LectureKind, numbers map[string]calendar.Numbered, lectures []LectureInput, terms []model.Term, generatedAt time.Time) Doc {
	part := CoursePart(course)
	doc := Doc{
		Schema:      Schema,
		Course:      Course{ID: course.ID, Code: courseCode(course), Name: course.Name},
		Kinds:       []Kind{},
		Lectures:    make([]Lecture, 0, len(lectures)),
		Glossary:    []Term{},
		GeneratedAt: generatedAt.UTC().Format(time.RFC3339),
	}
	labels := citeLabels(numbers, lectures)
	ls := append([]LectureInput(nil), lectures...)
	sort.SliceStable(ls, func(i, j int) bool { return lectureLess(ls[i].Lecture, ls[j].Lecture) })
	cat := newKindCatalog(kinds)
	for _, in := range ls {
		doc.Lectures = append(doc.Lectures, buildLecture(part, labels[in.Lecture.ID], numbers[in.Lecture.ID], cat, in))
	}
	doc.Kinds = cat.used(doc.Lectures)
	example := part + " " + exampleLabel + "@" + exampleTime
	if len(doc.Lectures) > 0 {
		example = part + " " + doc.Lectures[0].Label + "@" + exampleTime
	}
	doc.HowToCite = fmt.Sprintf(howToCite, example)

	ts := make([]model.Term, 0, len(terms))
	for _, t := range terms {
		if t.Enabled {
			ts = append(ts, t)
		}
	}
	sort.SliceStable(ts, func(i, j int) bool {
		if ts[i].EnKey != ts[j].EnKey {
			return ts[i].EnKey < ts[j].EnKey
		}
		return ts[i].En < ts[j].En
	})
	for _, t := range ts {
		doc.Glossary = append(doc.Glossary, Term{En: t.En, Zh: t.Zh})
	}
	return doc
}

// citeLabels returns lecture id → cite label over every lecture of numbers
// plus the exported lectures missing from it (as unnumbered).
func citeLabels(numbers map[string]calendar.Numbered, lectures []LectureInput) map[string]string {
	nums := make([]calendar.Numbered, 0, len(numbers)+len(lectures))
	for id, n := range numbers {
		n.ID = id // the key is authoritative
		nums = append(nums, n)
	}
	for _, in := range lectures {
		if _, ok := numbers[in.Lecture.ID]; !ok {
			nums = append(nums, calendar.Numbered{ID: in.Lecture.ID, Label: calendar.Unknown})
		}
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i].ID < nums[j].ID }) // deterministic
	labels := calendar.CiteLabels(nums)
	out := make(map[string]string, len(nums))
	for i, n := range nums {
		out[n.ID] = labels[i]
	}
	return out
}

// recordedAt parses lecture.recorded_at as a wall-clock time (the location
// does not matter: only calendar fields are compared). ok is false when it
// is unset or unparsable.
func recordedAt(l model.Lecture) (time.Time, bool) {
	if l.RecordedAt == nil || strings.TrimSpace(*l.RecordedAt) == "" {
		return time.Time{}, false
	}
	t, err := calendar.ParseRecordedAt(*l.RecordedAt, time.UTC)
	return t, err == nil
}

// lectureLess is the course export order (P13.6): by recorded_at, lectures
// without one last; then original file name, created_at, id.
func lectureLess(a, b model.Lecture) bool {
	ta, oka := recordedAt(a)
	tb, okb := recordedAt(b)
	if oka != okb {
		return oka
	}
	if oka && !ta.Equal(tb) {
		return ta.Before(tb)
	}
	if a.OriginalFilename != b.OriginalFilename {
		return a.OriginalFilename < b.OriginalFilename
	}
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt < b.CreatedAt
	}
	return a.ID < b.ID
}

// kindCatalog is the kind catalog given to Build, by id and in order.
type kindCatalog struct {
	byID  map[string]model.LectureKind
	order []string // ord, then id
}

func newKindCatalog(kinds []model.LectureKind) kindCatalog {
	if kinds == nil {
		for i, k := range calendar.BuiltinKinds() {
			kinds = append(kinds, model.LectureKind{ID: k.ID, Builtin: true, Code: k.Code, Group: k.Group, Order: (i + 1) * 10})
		}
	}
	ks := append([]model.LectureKind(nil), kinds...)
	sort.SliceStable(ks, func(i, j int) bool {
		if ks[i].Order != ks[j].Order {
			return ks[i].Order < ks[j].Order
		}
		return ks[i].ID < ks[j].ID
	})
	c := kindCatalog{byID: make(map[string]model.LectureKind, len(ks))}
	for _, k := range ks {
		if _, dup := c.byID[k.ID]; k.ID == "" || dup {
			continue
		}
		c.byID[k.ID] = k
		c.order = append(c.order, k.ID)
	}
	return c
}

// code is the code of kind id: the catalog's, else the built-in kind's.
func (c kindCatalog) code(id string) string {
	if k, ok := c.byID[id]; ok {
		return k.Code
	}
	for _, k := range calendar.BuiltinKinds() {
		if k.ID == id {
			return k.Code
		}
	}
	return ""
}

// entry is the "kinds" element of kind id; code is the lectures' code for
// it, used when the catalog lacks the kind.
func (c kindCatalog) entry(id, code string) Kind {
	k, ok := c.byID[id]
	if !ok {
		if name, builtin := BuiltinKindNames[id]; builtin {
			return Kind{ID: id, Code: code, Name: name}
		}
		return Kind{ID: id, Code: code, Name: id}
	}
	name := ""
	if k.Name != nil {
		name = strings.TrimSpace(*k.Name)
	}
	if n, builtin := BuiltinKindNames[k.ID]; builtin && (k.Builtin || name == "") {
		name = n
	}
	if name == "" {
		name = k.Code
	}
	return Kind{ID: k.ID, Code: k.Code, Name: name}
}

// used lists the kinds of lectures, each once: in catalog order, then the
// kinds the catalog lacks, by id.
func (c kindCatalog) used(lectures []Lecture) []Kind {
	codes := map[string]string{}
	for _, l := range lectures {
		if _, ok := codes[l.Kind]; !ok {
			codes[l.Kind] = l.KindCode
		}
	}
	out := []Kind{}
	for _, id := range c.order {
		if code, ok := codes[id]; ok {
			out = append(out, c.entry(id, code))
			delete(codes, id)
		}
	}
	rest := make([]string, 0, len(codes))
	for id := range codes {
		rest = append(rest, id)
	}
	sort.Strings(rest)
	for _, id := range rest {
		out = append(out, c.entry(id, codes[id]))
	}
	return out
}

func buildLecture(part, label string, num calendar.Numbered, cat kindCatalog, in LectureInput) Lecture {
	l := in.Lecture
	out := Lecture{
		ID: l.ID, Label: label, Kind: num.Kind, Title: l.Title, Summary: in.Summary,
		TitleZh: nonEmpty(l.TitleZh), BlurbEn: nonEmpty(l.BlurbEn), BlurbZh: nonEmpty(l.BlurbZh),
		Sentences: make([]Sentence, 0, len(in.Sentences)),
	}
	if out.Kind == "" {
		out.Kind = calendar.KindLecture
	}
	out.KindCode = num.Code
	if out.KindCode == "" || num.Kind == "" {
		out.KindCode = cat.code(out.Kind)
	}
	// v3: the week of any kind (a material kind is numbered without one);
	// for a taught session both come together, as in v2.
	if num.Week > 0 {
		w := num.Week
		out.Week = &w
	}
	if num.OK && num.Label != calendar.Unknown && num.N > 0 {
		n := num.N
		out.N = &n
	}
	if t, ok := recordedAt(l); ok {
		v := t.Format(calendar.RecordedAtLayout)
		if !calendar.HasClock(t) {
			v = t.Format(calendar.DateLayout)
		}
		out.RecordedAt = &v
	}
	ss := append([]model.Sentence(nil), in.Sentences...)
	sort.SliceStable(ss, func(i, j int) bool { return ss[i].Idx < ss[j].Idx })
	for _, s := range ss {
		var zh *string
		if s.ZhStatus == model.ZhOK && s.Zh != nil {
			v := *s.Zh
			zh = &v
		}
		out.Sentences = append(out.Sentences, Sentence{
			ID:      SentenceID(part, label, s.Idx),
			Ref:     part + " " + label + "@" + RefTime(s.StartMS),
			StartMS: s.StartMS,
			EndMS:   s.EndMS,
			En:      s.Text,
			Zh:      zh,
			Edited:  s.HumanText != nil && *s.HumanText != "",
		})
	}
	if in.MediaMS > 0 {
		out.DurationMS = in.MediaMS
	} else if n := len(ss); n > 0 {
		out.DurationMS = ss[n-1].EndMS
	}
	return out
}

// nonEmpty returns a copy of *p, or nil when p is nil or blank.
func nonEmpty(p *string) *string {
	if p == nil || strings.TrimSpace(*p) == "" {
		return nil
	}
	v := *p
	return &v
}

// SentenceID is the id of a sentence (P13.6): "{course}.{label}.s{idx+1,
// 4 digits}", e.g. CSC1003.W02L1.s0123.
func SentenceID(part, label string, idx int) string {
	return fmt.Sprintf("%s.%s.s%04d", part, label, idx+1)
}

// Label is the v1.1 sequence label "L" + seq with at least two digits (L03,
// L12, L105).
//
// Deprecated: SPEC-v2 P13.6 replaces it with the calendar number
// (calendar.LabelAll); the citation JSON no longer uses it. It remains only
// for API views that still show the v1.1 label.
func Label(seq int) string { return fmt.Sprintf("L%02d", seq) }

// RefTime formats the start of a sentence for its ref: mm:ss below one hour,
// h:mm:ss from one hour on (A11.3). Sub-second parts are dropped.
func RefTime(ms int64) string {
	sec := max(ms, 0) / 1000
	if sec >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", sec/3600, sec/60%60, sec%60)
	}
	return fmt.Sprintf("%02d:%02d", sec/60, sec%60)
}

func courseCode(c model.Course) *string {
	if c.Code == nil {
		return nil
	}
	code := strings.TrimSpace(*c.Code)
	if code == "" {
		return nil
	}
	return &code
}

// CoursePart is the course part of every ref and id: the course code when
// set, otherwise the abbreviation of the course name — the first character
// of each word, upper-cased ("Computer Architecture" → "CA"; a non-ASCII
// word contributes its first character, "计算机 体系结构" → "计体"). Words
// are runs of letters and digits (an apostrophe inside a word does not end
// it: "Women's Studies" → "WS"), so punctuation never enters an id.
func CoursePart(c model.Course) string {
	if code := courseCode(c); code != nil {
		return *code
	}
	var b strings.Builder
	inWord := false
	for _, r := range c.Name {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if !inWord {
				b.WriteRune(unicode.ToUpper(r))
			}
			inWord = true
		case inWord && (r == '\'' || r == '’'):
			// still inside the word
		default:
			inWord = false
		}
	}
	if b.Len() == 0 {
		return fallbackCoursePart
	}
	return b.String()
}

// Encode writes d as JSON: two-space indentation, no HTML escaping, a final
// newline, fields in the order of A11.3.
func Encode(w io.Writer, d Doc) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(d)
}
