// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// "修复断句" (textrepair-int; NOTES "决定（textrepair-int：worker-builder…）"):
// an asr job with model.ASRModeTextRepair repairs the lecture's stored
// transcript with worker/textrepair.py instead of transcribing it. The
// scheduler runs it in the CPU pool; the interpreter is the python
// component's base environment (the one extract_pdf.py uses: an installation
// that only uses Colab has it too), never a transcription environment, and
// without it the job fails with worker.python_not_installed. The result is
// stored like a transcription (one transaction: patches, summary and
// sentences deleted, the new sentences inserted, text recomputed by
// InsertSentences); a new sentence with the same (start_ms, end_ms,
// asr_text) as an old one keeps its human_text.

// Job errors of a "修复断句" run (kind error).
const (
	CodeTextRepairNoSentences = "textrepair.no_sentences"      // the lecture has no sentences to repair
	CodeTextRepairNoWords     = "textrepair.no_words"          // stored before word timestamps were kept (P4.2)
	CodeTextRepairUnparsable  = "textrepair.result_unparsable" //
	// The API's refusal of the rerun with text_only for a lecture that is
	// not English (the repair is validated on English only).
	CodeTextRepairLanguage = "textrepair.language" // language
)

// workerTextRepairCode is the error code prefix of worker/textrepair.py
// (textrepair.worker_*).
const workerTextRepairCode = "textrepair"

// textRepairJob is job.json of worker/textrepair.py --in/--out: the
// lecture's sentence rows in idx order (keep = a person reviewed it:
// human_text), the course's whole term list, the lecture's language and the
// prompt_terms_used to pass through.
type textRepairJob struct {
	Sentences       []textRepairSentence `json:"sentences"`
	Glossary        []string             `json:"glossary"`
	Language        string               `json:"language"`
	PromptTermsUsed int                  `json:"prompt_terms_used"`
}

type textRepairSentence struct {
	StartMS      int64               `json:"start_ms"`
	EndMS        int64               `json:"end_ms"`
	ASRText      string              `json:"asr_text"`
	LowConfWords []model.LowConfWord `json:"low_conf_words"`
	Words        []model.Word        `json:"words"`
	Keep         bool                `json:"keep,omitempty"`
}

// textRepairResult is its result.json: asr.py's sentences plus the repair's
// stats ("ms" included; "skipped": "language" for a lecture that is not
// English, the sentences then unchanged).
type textRepairResult struct {
	PromptTermsUsed int `json:"prompt_terms_used"`
	Sentences       []struct {
		StartMS      int64               `json:"start_ms"`
		EndMS        int64               `json:"end_ms"`
		Text         string              `json:"text"`
		LowConfWords []model.LowConfWord `json:"low_conf_words"`
		Words        []model.Word        `json:"words"`
	} `json:"sentences"`
	TextRepair *workerTextRepair `json:"textrepair"`
}

// promptTerms are the items of an initial_prompt (", " separated, blank ones
// dropped), as the workers split it.
func promptTerms(prompt string) []string {
	out := []string{}
	for _, it := range strings.Split(prompt, ", ") {
		if it = strings.TrimSpace(it); it != "" {
			out = append(out, it)
		}
	}
	return out
}

// sentenceKey identifies a sentence across the repair: a new sentence with
// an old one's boundaries and ASR text is that sentence.
type sentenceKey struct {
	start, end int64
	text       string
}

// textRepairRun is the run of a "修复断句" asr job.
func (s *asrStage) textRepairRun(ctx context.Context, job model.Job, lectureID string, progress func(float64)) error {
	d := s.d
	snap, ok := model.ParseASRSnapshot(job.ConfigSnapshot)
	if !ok {
		return sched.Permanent(model.Errorf(CodeSnapshotMissing, nil, errors.New(MissingSnapshot)))
	}
	run := model.NormalizeASRSnapshot(snap)
	l, err := d.Store.GetLecture(ctx, lectureID)
	if err != nil {
		return err
	}
	dir, err := filepath.Abs(d.lectureDir(l))
	if err != nil {
		return err
	}
	old, err := d.Store.ListSentences(ctx, lectureID, 0, 0)
	if err != nil {
		return err
	}
	if len(old) == 0 {
		return sched.Permanent(model.Errorf(CodeTextRepairNoSentences, nil,
			errors.New("这节讲座没有转写句子，无法修复断句：请重跑转写")))
	}
	in := textRepairJob{Language: run.Language, PromptTermsUsed: job.Stats.PromptTermsUsed}
	human := map[sentenceKey]string{}
	for _, x := range old {
		if len(x.Words) == 0 {
			return sched.Permanent(model.Errorf(CodeTextRepairNoWords, nil,
				fmt.Errorf("第 %d 句没有逐词时间（在保存逐词时间之前转写）：无法只修复断句，请重跑转写", x.Idx+1)))
		}
		lc := x.LowConfWords
		if lc == nil {
			lc = []model.LowConfWord{}
		}
		in.Sentences = append(in.Sentences, textRepairSentence{StartMS: x.StartMS, EndMS: x.EndMS, ASRText: x.ASRText,
			LowConfWords: lc, Words: x.Words, Keep: x.HumanText != nil})
		if x.HumanText != nil {
			human[sentenceKey{x.StartMS, x.EndMS, x.ASRText}] = *x.HumanText
		}
	}
	prompt, _, err := s.asrPrompt(ctx, l)
	if err != nil {
		return err
	}
	in.Glossary = promptTerms(prompt)

	raw, err := d.runWorker(ctx, workerCall{
		name: "textrepair", label: "断句修复进程（textrepair.py）", code: workerTextRepairCode, dir: dir,
		job: in, out: "result.json", progress: progress,
		// the python component's base environment (config.Config.PDFPython)
		python: d.Cfg.PDFPython(), pythonKey: "python",
	})
	if err != nil {
		return err
	}
	var res textRepairResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return model.Errorf(CodeTextRepairUnparsable, nil, fmt.Errorf("断句修复结果 result.json 无法解析：%w", err))
	}
	ss := make([]model.Sentence, 0, len(res.Sentences))
	kept := 0
	for _, r := range res.Sentences {
		text := strings.TrimSpace(r.Text)
		if text == "" {
			continue
		}
		var words []model.Word
		if len(r.Words) > 0 {
			words = r.Words
		}
		x := model.Sentence{
			LectureID: lectureID, Idx: len(ss), StartMS: r.StartMS, EndMS: max(r.EndMS, r.StartMS),
			ASRText: text, LowConfWords: r.LowConfWords, RepairedText: text, ZhStatus: model.ZhNone, Words: words,
		}
		if h, ok := human[sentenceKey{x.StartMS, x.EndMS, x.ASRText}]; ok {
			x.HumanText = &h
			kept++
		}
		ss = append(ss, x)
	}
	if len(ss) == 0 {
		return model.Errorf(CodeTextRepairUnparsable, nil, errors.New("断句修复结果 result.json 没有句子"))
	}
	if kept < len(human) {
		// never expected (textrepair.py returns the kept sentences as they
		// are): nothing is written rather than a person's edit lost
		return model.Errorf(CodeTextRepairUnparsable, nil,
			fmt.Errorf("断句修复结果缺少 %d 句人工审阅过的句子，未写入", len(human)-kept))
	}
	err = d.Store.Tx(ctx, func(tx db.Store) error {
		if err := tx.DeletePatches(ctx, lectureID); err != nil {
			return err
		}
		if err := tx.DeleteSummary(ctx, lectureID); err != nil {
			return err
		}
		if err := tx.DeleteSentences(ctx, lectureID); err != nil {
			return err
		}
		return tx.InsertSentences(ctx, lectureID, ss) // runs RecomputeText in the same transaction
	})
	if err != nil {
		return err
	}
	d.events().Changed("sentences", lectureID)
	d.events().Changed("patches", lectureID)

	// The job keeps what describes the transcription it repaired (the rerun
	// copied it: prompt, terms, glossary version, timing); the repair's
	// counters go next to the transcription's own steps.
	tr := res.TextRepair.stats()
	sched.UpdateStats(ctx, func(st *model.JobStats) {
		st.Status = ""
		var t model.ASRTiming
		if st.ASRTiming != nil {
			t = *st.ASRTiming
		}
		var p model.ASRPostprocess
		if t.Postprocess != nil {
			p = *t.Postprocess
		}
		p.TextRepair = tr
		t.Postprocess = &p
		st.ASRTiming = &t
	})
	progress(1)
	return nil
}

// ---------------------------------------------------------------------------
// Whether a stored transcript is worth repairing (the offer of "修复断句")
// ---------------------------------------------------------------------------

// Thresholds of MeasureTextDegeneration and Worthwhile, as worker/textrepair.py detects the
// spans it repairs (RUN_MIN_WORDS, CAPS_MIN_ORDINARY, CAPS_ORDINARY_SHARE).
const (
	textRepairRunMinWords  = 25   // an unpunctuated stretch of this many words or more
	textRepairUnpunctShare = 0.10 // ... holding this share of the words makes it worth it
	textRepairCapsMinOrd   = 3    // or an all-caps run with this many ordinary words
	textRepairCapsOrdShare = 0.5  // ... making up at least this share of its words
	textRepairClosers      = "\"')]}”’»"
	textRepairMarks        = ".,?!;:"
)

// TextDegeneration is what TextRepairWorthwhile measures in a stored
// transcript: its words, the words inside unpunctuated stretches of 25 words
// or more (no . , ? ! ; : at a word's end), and the all-caps runs that
// textrepair.py would re-case.
type TextDegeneration struct {
	Words             int
	UnpunctuatedWords int
	CapsSegments      int
}

// UnpunctuatedShare is UnpunctuatedWords / Words (0 without words).
func (t TextDegeneration) UnpunctuatedShare() float64 {
	if t.Words == 0 {
		return 0
	}
	return float64(t.UnpunctuatedWords) / float64(t.Words)
}

// Worthwhile: an unpunctuated share of 10% or more, or an all-caps span
// (lead's brief for textrepair-int).
func (t TextDegeneration) Worthwhile() bool {
	return t.UnpunctuatedShare() >= textRepairUnpunctShare || t.CapsSegments > 0
}

// MeasureTextDegeneration measures the sentences' ASR text (idx order) the
// way worker/textrepair.py finds degenerate spans, on the words of asr_text
// (whitespace separated): unpunctuated stretches (as
// textrepair.unpunctuated_stretches: the words before the next one that ends
// with a mark), and runs of words without a lower-case letter holding at
// least three "ordinary" words (two letters or more, written elsewhere in
// the lecture with a lower-case letter) that make up half of the run's
// all-caps words or more. The repair itself learns the usual forms from the
// lecture's normal text only; "written elsewhere" is the close enough
// measure that needs no model.
func MeasureTextDegeneration(ss []model.Sentence) TextDegeneration {
	var toks []string
	for _, x := range ss {
		toks = append(toks, strings.Fields(x.ASRText)...)
	}
	out := TextDegeneration{Words: len(toks)}
	n := len(toks)
	for i := 0; i < n; {
		j := i
		for j < n-1 && !textRepairHasMark(toks[j]) {
			j++
		}
		if j-i+1 >= textRepairRunMinWords {
			out.UnpunctuatedWords += j - i
		}
		i = j + 1
	}
	lower := map[string]bool{}
	cores := make([]string, n)
	for i, t := range toks {
		cores[i] = textRepairCore(t)
		if hasLower(cores[i]) {
			lower[strings.ToLower(cores[i])] = true
		}
	}
	for i := 0; i < n; {
		if hasLower(cores[i]) || !hasUpper(cores[i]) {
			i++
			continue
		}
		j := i
		for j+1 < n && !hasLower(cores[j+1]) {
			j++
		}
		caps, ordinary := 0, 0
		for k := i; k <= j; k++ {
			if !hasUpper(cores[k]) {
				continue
			}
			caps++
			if letters(cores[k]) >= 2 && lower[strings.ToLower(cores[k])] {
				ordinary++
			}
		}
		if ordinary >= textRepairCapsMinOrd && float64(ordinary) >= textRepairCapsOrdShare*float64(caps) {
			out.CapsSegments++
		}
		i = j + 1
	}
	return out
}

// textRepairHasMark: the word ends with . , ? ! ; or : (closing quotes or
// brackets after it allowed), as textrepair.has_mark.
func textRepairHasMark(w string) bool {
	core := strings.TrimRight(strings.TrimSpace(w), textRepairClosers)
	return core != "" && strings.ContainsRune(textRepairMarks, []rune(core)[len([]rune(core))-1])
}

// textRepairCore is the word from its first to its last letter or digit.
func textRepairCore(w string) string {
	isAlnum := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
	a := strings.IndexFunc(w, isAlnum)
	if a < 0 {
		return ""
	}
	b := strings.LastIndexFunc(w, isAlnum)
	_, size := utf8.DecodeRuneInString(w[b:])
	return w[a : b+size]
}

func hasLower(s string) bool { return strings.IndexFunc(s, unicode.IsLower) >= 0 }
func hasUpper(s string) bool { return strings.IndexFunc(s, unicode.IsUpper) >= 0 }

func letters(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) {
			n++
		}
	}
	return n
}
