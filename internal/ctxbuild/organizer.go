// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"lectureflow/internal/model"
)

// ChaptersSystem is the chapters prompt of SPEC-v2 P7, verbatim.
const ChaptersSystem = `You divide a university lecture into chapters for a video player. You are given a summary and the full transcript, one sentence per line as "[idx] sentence".
Create between 3 and 12 chapters that follow the lecture's actual structure. Each chapter starts at the sentence where a new topic genuinely begins.
Titles: short and specific (at most 8 English words; Chinese title at most 16 characters), using the correct technical terms.
Respond with JSON only: {"chapters": [{"start_idx": 0, "title_en": "...", "title_zh": "..."}]}`

// FilenameRuleSystem is the organizer prompt of SPEC-v2 P13.3.2 (the
// filename_rule task) as amended for lecture kinds (user request
// 2026-10-23, lead freeze .claude/kinds-brief.md 1.3): the SPEC-v2 text
// with the optional (?P<kind>…) group, a rule for it and kind_map in the
// answer; "Do not capture week or lecture numbers" keeps its meaning
// (numbers are still not captured) without forbidding the kind code.
// TestOrganizerPromptsVerbatim checks it is the SPEC-v2 block with exactly
// these edits. Version FilenameRulePromptVersion.
const FilenameRuleSystem = `You write one regular expression that extracts the recording date and time from lecture video file names for one university course.
The file names come from a lecture-capture system; their format is consistent within the course but unknown to you. Study the examples carefully.
Rules:
- Use Go RE2 syntax with named groups: (?P<year>…), (?P<month>…), (?P<day>…), and optionally (?P<hour>…), (?P<minute>…), (?P<ampm>…), (?P<title>…), (?P<kind>…). month may capture a number or an English month name/abbreviation.
- No lookbehind, no backreferences.
- The pattern should match as many of the given file names as possible; if some names clearly follow a different format, ignore them rather than making the pattern vague.
- If a part of the name looks like a human-readable lecture title, capture it as title; otherwise omit the title group.
- If a part of the name is a code for the type of class activity (for example L and T in "CSC1002-L01-01 …" and "CSC1002-T01-02 …", meaning lecture and tutorial), capture only that code as kind, and in kind_map map every code the group captures to the id of one of the LECTURE KINDS given. If the names carry no such code, or you cannot tell what a code means, omit the kind group and give an empty kind_map.
- Write the kind group so that it accepts any such code (for example (?P<kind>[A-Z]+)), not only the codes in the examples, and let the digits around it vary, so that later files of other activity types or streams still match; a code missing from kind_map is simply left unmapped.
- Do not capture week numbers or lecture sequence numbers (in "L01-01" the kind is "L"; the digits are not captured).
Respond with JSON only: {"regex": "...", "kind_map": {"<code>": "<kind id>"}, "explanation": "one short sentence"}`

// TitleSystem is the organizer prompt of SPEC-v2 P13.5 (the title task) as
// amended for lecture kinds (user request 2026-10-23, lead freeze
// .claude/kinds-brief.md 1.3): the SPEC-v2 text with a Kind line and "kind"
// in the answer. TestOrganizerPromptsVerbatim checks it is the SPEC-v2
// block with exactly these edits. Version TitlePromptVersion.
const TitleSystem = `You write a title and a one-sentence description for one university lecture, based on its summary.
If the syllabus entry for this lecture's week is provided and matches the content, prefer its wording for the title.
Title: at most 8 English words; name the main topics in the order taught; no "Lecture", "Week", course code, or date. Chinese title: at most 18 characters, using standard Chinese textbook terms.
Description: one sentence, at most 25 English words, saying what a student will learn; Chinese description: one sentence, at most 50 characters.
Kind: the id of the one entry of LECTURE KINDS that fits this recording best, judged from the summary: a lecture, a tutorial or problem class, a seminar, a recording from an earlier year, supplementary material, or one of the user-defined kinds. If unsure, answer "lecture".
Respond with JSON only: {"title_en": "...", "title_zh": "...", "blurb_en": "...", "blurb_zh": "...", "kind": "..."}`

// Prompt versions of the organizer prompts (SPEC-v2 G5.5 applied to the
// auxiliary tasks; the stages record them in the job's config_snapshot as
// prompt_version): 1 = SPEC-v2 P13.3.2 / P13.5 as written; 2 = with lecture
// kinds; 3 (filename rule) = the kind group must accept any code and the
// digits around it vary (lead, after the real DeepSeek check: a rule with
// "(?P<kind>[LT])01-" did not match a later "-L02-01" or "-S01-" file).
const (
	FilenameRulePromptVersion = 3
	TitlePromptVersion        = 2
)

// Limits stated in the P7 and P13.5 prompts; the stages check the answers
// against them (P7: "校验失败重试一次").
const (
	ChaptersMin        = 3
	ChaptersMax        = 12
	ChapterTitleWords  = 8  // title_en, English words
	ChapterTitleZhLen  = 16 // title_zh, ZhLen
	TitleWords         = 8  // title_en, English words
	TitleZhLen         = 18 // title_zh, ZhLen
	BlurbWords         = 25 // blurb_en, English words
	BlurbZhLen         = 50 // blurb_zh, ZhLen
	FilenameMaxSamples = 50 // P13.3.2: at most the 50 newest file names
	// SyllabusEntryMaxChars bounds the syllabus entry of the title request.
	SyllabusEntryMaxChars = 2000
)

// Labels of the user messages of the organizer and chapters requests.
const (
	labelSummary       = "SUMMARY:"
	labelTranscript    = "TRANSCRIPT:"
	labelSyllabusEntry = "SYLLABUS ENTRY FOR THIS WEEK:"
	labelOriginalFile  = "ORIGINAL FILE NAME:"
	// The kind catalog (lecture kinds): the title request lists "id |
	// meaning", the filename_rule request "id | code | meaning".
	labelTitleKinds    = "LECTURE KINDS (answer one id), one per line as: id | meaning"
	labelFilenameKinds = "LECTURE KINDS (the ids for kind_map), one per line as: id | code | meaning"
)

// ---------- lecture kinds (the catalog shown to the organizer model) ----------

// builtinKindMeanings describe the built-in kinds to the organizer model
// (the UI translates their names; the model gets these English meanings).
var builtinKindMeanings = map[string]string{
	model.KindLecture:     "a regular lecture of the course",
	model.KindTutorial:    "a tutorial, problem class or exercise session",
	model.KindSeminar:     "a seminar or discussion session",
	model.KindPastLecture: "a recording of this course from an earlier year",
	model.KindMaterial:    "supplementary material, not a class session",
}

// KindMeaning describes one kind of the catalog to the organizer model: the
// English meaning of a built-in kind; for a user-defined kind the name the
// user wrote and its group ("user-defined kind named "Lab" (a class
// session)").
func KindMeaning(k model.LectureKind) string {
	if m, ok := builtinKindMeanings[k.ID]; ok && k.Builtin {
		return m
	}
	name := ""
	if k.Name != nil {
		name = strings.Join(strings.Fields(*k.Name), " ")
	}
	if name == "" {
		name = k.Code
	}
	group := "a class session"
	if k.Group == model.KindGroupMaterial {
		group = "supplementary material"
	}
	return `user-defined kind named "` + name + `" (` + group + `)`
}

// kindLines lists the catalog, in the order given, as "id | meaning" or,
// withCode, "id | code | meaning".
func kindLines(kinds []model.LectureKind, withCode bool) string {
	var b strings.Builder
	for i, k := range kinds {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(oneLine(k.ID))
		if withCode {
			b.WriteString(" | " + oneLine(k.Code))
		}
		b.WriteString(" | " + KindMeaning(k))
	}
	return b.String()
}

// ---------- chapters (P7) ----------

// ChaptersRequest builds the chapters request (P7): the verbatim prompt as
// system, and the summary followed by every sentence as "[idx] text" (the
// current sentence.text) as user.
func ChaptersRequest(summary string, ss []Sentence) Request {
	var b strings.Builder
	b.WriteString(labelSummary + "\n" + summaryOrNone(strings.TrimSpace(summary)) + "\n\n" + labelTranscript + "\n")
	for i, s := range ss {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("[" + strconv.Itoa(s.Idx) + "] " + oneLine(s.Text))
	}
	return Request{System: ChaptersSystem, User: b.String()}
}

// ChaptersInputHash is the input hash stored with generated chapters (P7,
// lead ruling 9 of phase 2): SHA-256 of the summary and every sentence.text.
// A different hash later means the chapters may be out of date.
func ChaptersInputHash(summary string, ss []Sentence) string {
	parts := []string{"chapters", summary, strconv.Itoa(len(ss))}
	for _, s := range ss {
		parts = append(parts, s.Text)
	}
	return hashParts(parts...)
}

// ---------- title and blurb (P13.5) ----------

// TitleInput is what the title request depends on.
type TitleInput struct {
	Summary string
	// SyllabusEntry is the Weekly Topics entry of the lecture's teaching
	// week (SyllabusWeekEntry); "" = not known. Only this week's entry is
	// given, never the whole syllabus or other lectures' summaries (G6 rule 5).
	SyllabusEntry string
	// OriginalFilename is the uploaded file's name; "" = unknown.
	OriginalFilename string
	// Kinds is the kind catalog (ListLectureKinds order) the answer's kind
	// is chosen from (lecture kinds); empty = the section is left out.
	Kinds []model.LectureKind
}

// TitleRequest builds the title request (P13.5). Sections without content
// (no syllabus entry, no file name, no kind catalog) are left out.
func TitleRequest(in TitleInput) Request {
	var b strings.Builder
	b.WriteString(labelSummary + "\n" + summaryOrNone(strings.TrimSpace(in.Summary)))
	if e := strings.TrimSpace(in.SyllabusEntry); e != "" {
		b.WriteString("\n\n" + labelSyllabusEntry + "\n" + e)
	}
	if f := strings.TrimSpace(in.OriginalFilename); f != "" {
		b.WriteString("\n\n" + labelOriginalFile + "\n" + oneLine(f))
	}
	if len(in.Kinds) > 0 {
		b.WriteString("\n\n" + labelTitleKinds + "\n" + kindLines(in.Kinds, false))
	}
	return Request{System: TitleSystem, User: b.String()}
}

// ---------- filename rule (P13.3.2) ----------

// FilenameSample is one lecture file of the filename_rule request. Times are
// local wall-clock times formatted by the caller as SampleTimeLayout (this
// package does not touch time values).
type FilenameSample struct {
	Filename     string
	UploadedAt   string
	CreationTime string // media creation_time; "" = unknown
}

// SampleTimeLayout is the time.Format layout of FilenameSample times.
const SampleTimeLayout = "2006-01-02 15:04"

// FilenameRuleInput is what the filename_rule request depends on.
type FilenameRuleInput struct {
	CourseCode string // "" = none
	TermStart  string // course.term_start "YYYY-MM-DD"; "" = not set
	// Samples in the order given (callers pass upload order, at most
	// FilenameMaxSamples).
	Samples []FilenameSample
	// Kinds is the kind catalog (ListLectureKinds order) whose ids kind_map
	// may use (lecture kinds); empty = the section is left out.
	Kinds []model.LectureKind
}

// FilenameRuleRequest builds the filename_rule request (P13.3.2): the
// prompt as system; the course code, term start, the kind catalog (id |
// code | meaning) and one line per file ("name | upload time |
// creation_time") as user. creation_time is marked as possibly a download
// or transcoding time.
func FilenameRuleRequest(in FilenameRuleInput) Request {
	var b strings.Builder
	b.WriteString("COURSE CODE: " + orNone(in.CourseCode) + "\n")
	b.WriteString("TERM START (Monday of teaching week 1): " + orNone(in.TermStart) + "\n\n")
	if len(in.Kinds) > 0 {
		b.WriteString(labelFilenameKinds + "\n" + kindLines(in.Kinds, true) + "\n\n")
	}
	b.WriteString("FILES (" + strconv.Itoa(len(in.Samples)) + "), one per line as: file name | upload time | media creation_time (local times).\n")
	b.WriteString("The media creation_time may be a download or transcoding time, not necessarily the recording time; \"-\" = none.\n")
	for _, s := range in.Samples {
		ct := strings.TrimSpace(s.CreationTime)
		if ct == "" {
			ct = "-"
		}
		b.WriteString(oneLine(s.Filename) + " | " + strings.TrimSpace(s.UploadedAt) + " | " + ct + "\n")
	}
	return Request{System: FilenameRuleSystem, User: strings.TrimSuffix(b.String(), "\n")}
}

func orNone(s string) string {
	if s = strings.TrimSpace(s); s == "" {
		return None
	}
	return s
}

// ---------- answer checks shared by the organizer and chapters stages ----------

// EnglishWords counts the words of an English title or sentence (runs of
// non-space characters).
func EnglishWords(s string) int { return len(strings.Fields(s)) }

// ZhLen is the length of a Chinese title or sentence as the P7/P13.5 limits
// count it ("16 个汉字", "at most 18 characters"): every Han character or
// punctuation mark counts one, and so does each run of Latin letters or
// digits (an English term or acronym such as "ElGamal" or "IND-CPA" inside a
// Chinese title is one unit, not one per letter). Spaces do not count.
func ZhLen(s string) int {
	n := 0
	inWord := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			inWord = false
		case r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' || r == '+'):
			if !inWord {
				n++
				inWord = true
			}
		default:
			n++
			inWord = false
		}
	}
	return n
}

// ---------- syllabus: the entry of one teaching week (P13.5, G6 rule 5) ----------

var (
	mdHeadingRE   = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	tableSepRE    = regexp.MustCompile(`^\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$`)
	listItemRE    = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+(.*)$`)
	weekPrefixRE  = regexp.MustCompile(`(?i)^(?:weeks?|wk|w)\s*\.?\s*(\d{1,3})(?:\s*(?:-|–|—|~|to|&|and|,|/)\s*(?:weeks?\s*|w\s*)?(\d{1,3}))?(?:\D|$)`)
	weekWordRE    = regexp.MustCompile(`(?i)\b(?:weeks?|wk)\b\.?|\bw(\d)`)
	weekNumbersRE = regexp.MustCompile(`(\d{1,3})(?:\s*(?:-|–|—|~|to)\s*(\d{1,3}))?`)
)

// SyllabusWeekEntry returns the entry for teaching week `week` in the
// "Weekly Topics" section of SYLLABUS.md (SPEC-v1.1 A8.3 format), or "" when
// there is none or the weeks cannot be identified. Recognised forms:
//   - a table whose week column (the header cell containing "week"; a
//     table without one is not used) holds "3", "Week 3", "W3" or a range
//     "3–4": the header row and every matching row are returned;
//   - list items or sub-headings that start with "Week 3", "W3", "Weeks 3–4"
//     (a sub-heading brings the lines under it).
//
// A plain list without week numbers is not used: A8.3 writes a list exactly
// when the weeks are not identifiable, and guessing by position could give
// the model a wrong topic. The result is cut to SyllabusEntryMaxChars.
func SyllabusWeekEntry(syllabus string, week int) string {
	if week < 1 {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(syllabus, "\r\n", "\n"), "\n")
	start, level := -1, 0
	for i, l := range lines {
		if m := mdHeadingRE.FindStringSubmatch(l); m != nil && strings.HasPrefix(strings.ToLower(m[2]), "weekly topics") {
			start, level = i+1, len(m[1])
			break
		}
	}
	if start < 0 {
		return ""
	}
	end := len(lines)
	for i := start; i < len(lines); i++ {
		if m := mdHeadingRE.FindStringSubmatch(lines[i]); m != nil && len(m[1]) <= level {
			end = i
			break
		}
	}
	section := lines[start:end]

	var out []string
	weekCol, header := -1, "" // weekCol: the table column whose header names the week
	inBlock := false          // inside a matching sub-heading
	for _, l := range section {
		t := strings.TrimSpace(l)
		if m := mdHeadingRE.FindStringSubmatch(t); m != nil {
			inBlock = weekTextMatches(m[2], week)
			if inBlock {
				out = append(out, t)
			}
			continue
		}
		if inBlock {
			if t != "" {
				out = append(out, t)
			}
			continue
		}
		switch {
		case strings.HasPrefix(t, "|"):
			if tableSepRE.MatchString(t) {
				continue
			}
			cells := tableCells(t)
			if header == "" {
				header = t
				for k, c := range cells {
					if strings.Contains(strings.ToLower(c), "week") {
						weekCol = k
						break
					}
				}
				continue
			}
			if weekCol >= 0 && weekCol < len(cells) && weekSpecMatches(cells[weekCol], week) {
				if len(out) == 0 || out[0] != header {
					out = append([]string{header}, out...)
				}
				out = append(out, t)
			}
		case listItemRE.MatchString(t):
			item := listItemRE.FindStringSubmatch(t)[1]
			if weekTextMatches(item, week) {
				out = append(out, t)
			}
		}
	}
	return truncateRunes(strings.Join(out, "\n"), SyllabusEntryMaxChars)
}

// tableCells splits a Markdown table row into trimmed cells.
func tableCells(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "|")
	row = strings.TrimSuffix(row, "|")
	cells := strings.Split(row, "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

// weekTextMatches: text starts with "Week N", "W N", "Weeks N–M" (Markdown
// emphasis ignored) and week is N or within N–M.
func weekTextMatches(text string, week int) bool {
	text = strings.TrimLeft(text, "*_ ")
	m := weekPrefixRE.FindStringSubmatch(text)
	if m == nil {
		return false
	}
	return inWeekRange(m[1], m[2], week)
}

// weekSpecMatches: a table cell of the week column ("3", "Week 3", "W3",
// "3–4", "3, 4") names week.
func weekSpecMatches(cell string, week int) bool {
	c := weekWordRE.ReplaceAllString(cell, "$1")
	for _, m := range weekNumbersRE.FindAllStringSubmatch(c, -1) {
		if inWeekRange(m[1], m[2], week) {
			return true
		}
	}
	return false
}

func inWeekRange(a, b string, week int) bool {
	lo, err := strconv.Atoi(a)
	if err != nil {
		return false
	}
	if b == "" {
		return lo == week
	}
	hi, err := strconv.Atoi(b)
	if err != nil || hi < lo {
		return lo == week
	}
	return week >= lo && week <= hi
}

// ---------- retry feedback (lead ruling after the phase 2 acceptance) ----------

// An auxiliary answer that fails its check is asked again once (organizer-p2
// decision 3). Sending the identical request again let the model repeat the
// same rejected regex (filename_rule failed 4 times on deepseek-chat in the
// phase 2 acceptance), so the retry carries feedback: the system prompt and
// the original user message are unchanged (the prompts stay verbatim) and
// the problems of the rejected answer are appended to the user message.

// labelRetryFeedback heads the appended feedback.
const labelRetryFeedback = "YOUR PREVIOUS ANSWER WAS REJECTED. Fix the following and answer again in the same JSON format:"

// retryListMax bounds each list of file names in the feedback.
const retryListMax = 15

// WithRetryFeedback returns the user message of a retried request: user,
// then the problems of the rejected answer, one per line ("- …"; a problem
// may span several lines). Without problems user is returned unchanged.
func WithRetryFeedback(user string, problems []string) string {
	if len(problems) == 0 {
		return user
	}
	var b strings.Builder
	b.WriteString(user + "\n\n" + labelRetryFeedback)
	for _, p := range problems {
		b.WriteString("\n- " + strings.TrimSpace(p))
	}
	return b.String()
}

// NotJSONProblem is the feedback for an answer that is not the requested
// JSON object; detail is the parse error (cut to 200 characters).
func NotJSONProblem(detail string) string {
	return "The answer is not a valid JSON object (" + truncateRunes(oneLine(detail), 200) + "). Respond with the JSON object only."
}

// FilenameRuleRejection is what the deterministic check of P13.3.2 found
// wrong with a generated rule. The stage fills it in English from the
// validation result; FilenameRuleProblems turns it into feedback.
type FilenameRuleRejection struct {
	Regex           string
	CompileError    string   // the Go regexp error; "" = the pattern compiles
	MissingGroups   []string // required named groups the pattern lacks (month, day)
	Matched, Total  int
	MinMatchPercent int      // rule 2: the share of names that must match
	Unmatched       []string // file names the pattern does not match
	// Invalid lists the names the pattern matches without a usable date
	// (rules 2–3; a date long before the term start is a recording from an
	// earlier year since lecture kinds, not a failure).
	Invalid []FilenameRuleInvalidDate
	// KindMap lists why entries of the answer's kind_map were dropped
	// (lecture kinds; FilenameKindMapProblem texts). Dropping them does not
	// reject a rule; they are told when the rule is asked again anyway.
	KindMap []string
}

// Reasons why an entry of kind_map is dropped (FilenameRuleRejection.KindMap).

// FilenameKindMapNoGroup: kind_map without a (?P<kind>…) group.
func FilenameKindMapNoGroup() string {
	return "kind_map was ignored: the regex has no (?P<kind>…) group."
}

// FilenameKindMapNotObject: kind_map is not a JSON object of strings.
func FilenameKindMapNotObject() string {
	return `kind_map was ignored: it must be a JSON object mapping each captured code to a kind id, e.g. {"L": "lecture"}.`
}

// FilenameKindMapEmptyCode: an entry whose code is empty.
func FilenameKindMapEmptyCode(kind string) string {
	return "kind_map entry \"\" → " + strconv.Quote(oneLine(kind)) + " was dropped: the code is empty."
}

// FilenameKindMapUnknownKind: an entry whose kind is not a catalog id.
func FilenameKindMapUnknownKind(code, kind string) string {
	return "kind_map entry " + strconv.Quote(oneLine(code)) + " → " + strconv.Quote(oneLine(kind)) +
		" was dropped: " + strconv.Quote(oneLine(kind)) + " is not one of the LECTURE KINDS ids."
}

// FilenameKindMapUnmapped: codes the kind group captures that kind_map
// does not map (those lectures get no kind from their file names).
func FilenameKindMapUnmapped(codes []string) string {
	q := make([]string, len(codes))
	for i, c := range codes {
		q[i] = strconv.Quote(oneLine(c))
	}
	return "kind_map does not map the captured code(s) " + strings.Join(q, ", ") + "."
}

// FilenameRuleInvalidDate is one matched file name without a usable date.
type FilenameRuleInvalidDate struct {
	Filename string
	Groups   []string // the date and time groups as captured, "name=value"
	Reason   string   // English: "not a valid date", "2026-11-29 is after the upload date 2026-10-20"…
}

// FilenameRuleProblems is the retry feedback of a rejected rule: the regex
// itself, then why it was rejected — the compile error (with the RE2
// restrictions of the prompt), the missing groups, how many names it
// matched and which not, and the names whose date it got wrong with the
// captured groups and the reason. Lists are cut to retryListMax names.
func FilenameRuleProblems(r FilenameRuleRejection) []string {
	out := []string{"Your regex was: " + r.Regex}
	if r.CompileError != "" {
		out = append(out, "It does not compile as a Go RE2 regular expression: "+oneLine(r.CompileError)+
			". RE2 has no lookbehind, lookahead or backreferences.")
		return out
	}
	if len(r.MissingGroups) > 0 {
		out = append(out, "It lacks the required named group(s) "+strings.Join(r.MissingGroups, ", ")+
			"; month and day are required, written as (?P<month>…) and (?P<day>…).")
		return out
	}
	if r.Total > 0 && r.Matched*100 < r.MinMatchPercent*r.Total {
		out = append(out, "It matches only "+strconv.Itoa(r.Matched)+" of "+strconv.Itoa(r.Total)+
			" file names; at least "+strconv.Itoa(r.MinMatchPercent)+"% must match.")
	}
	if len(r.Unmatched) > 0 {
		out = append(out, "File names it does not match:"+nameList(r.Unmatched, func(s string) string { return s }))
	}
	if len(r.Invalid) > 0 {
		out = append(out, "File names it matches without a valid recording date (a date must exist and not be after the file's upload time):"+
			nameList(r.Invalid, func(d FilenameRuleInvalidDate) string {
				s := d.Filename
				if len(d.Groups) > 0 {
					s += " → " + strings.Join(d.Groups, " ")
				}
				if d.Reason != "" {
					s += ": " + d.Reason
				}
				return s
			}))
	}
	for _, k := range r.KindMap {
		out = append(out, oneLine(k))
	}
	return out
}

// nameList renders items as indented lines, at most retryListMax of them.
func nameList[T any](items []T, show func(T) string) string {
	var b strings.Builder
	for i, it := range items {
		if i == retryListMax {
			b.WriteString("\n  … and " + strconv.Itoa(len(items)-i) + " more")
			break
		}
		b.WriteString("\n  " + oneLine(show(it)))
	}
	return b.String()
}
