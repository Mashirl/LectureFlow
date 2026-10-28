// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// "修复断句" (textrepair-int): an asr job with model.ASRModeTextRepair runs
// worker/textrepair.py (here the fake worker) on the stored sentences and
// stores its result like a transcription; human_text survives where the
// sentence is unchanged.

func words(text string, startMS int64) []model.Word {
	var out []model.Word
	t := startMS
	for _, w := range strings.Fields(text) {
		out = append(out, model.Word{W: w, S: t, E: t + 200})
		t += 250
	}
	return out
}

func sentence(idx int, text string, startMS int64) model.Sentence {
	ws := words(text, startMS)
	return model.Sentence{Idx: idx, StartMS: startMS, EndMS: ws[len(ws)-1].E, ASRText: text, RepairedText: text,
		ZhStatus: model.ZhNone, Words: ws, LowConfWords: []model.LowConfWord{}}
}

// textRepairLecture is a launched lecture whose asr job is a pending
// "修复断句" run over four stored sentences (the second one reviewed by a
// person), with a patch and a summary, and the course's terms XOR and AES.
func (e *env) textRepairLecture() (model.Lecture, []model.Sentence) {
	e.t.Helper()
	l := e.lecture("L repair", "m.mp4")
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceCUDA0, "large-v3", "auto", "en"))
	ss := []model.Sentence{
		sentence(0, "Okay, let's start.", 0),
		sentence(1, "so the key is random", 1000),
		sentence(2, "and we xor it with the message and then the receiver does the same thing", 3000),
		sentence(3, "with the key right", 7000),
	}
	h := "So the key is random."
	ss[1].HumanText = &h
	e.must(e.st.InsertSentences(e.ctx, l.ID, ss))
	e.must(e.st.InsertPatches(e.ctx, []model.Patch{{LectureID: l.ID, SentenceIdx: 2, Before: "xor", After: "XOR",
		Class: model.ClassDecide, Review: model.ReviewRejected}}))
	e.must(e.st.UpsertSummary(e.ctx, model.Summary{LectureID: l.ID, Text: "sum", InputHash: "h"}))
	for i, en := range []string{"XOR", "AES"} {
		e.must(e.st.InsertTerm(e.ctx, model.Term{ID: db.NewID(), CourseID: l.CourseID, En: en, EnKey: strings.ToLower(en),
			Source: model.SourceManual, Enabled: true, Freq: 2 - i}))
	}
	j := e.job(model.ScopeLecture, l.ID, model.StageASR)
	gv := 3
	e.must(e.st.SetJobStats(e.ctx, j.ID, model.JobStats{ASRMode: model.ASRModeTextRepair, PromptTermsUsed: 2,
		ASRPrompt: "XOR, AES", GlossaryVersion: &gv, HashVersion: ASRHashVersion,
		ASRTiming: &model.ASRTiming{LoadMS: 2000, TranscribeMS: 60000, AudioMS: 600000}}))
	return l, ss
}

// useFakeTextRepair makes the fake worker play worker/textrepair.py with the
// result res (written to a file); returns the job log.
func (e *env) useFakeTextRepair(res any) string {
	e.t.Helper()
	p := filepath.Join(e.t.TempDir(), "textrepair_result.json")
	e.must(os.WriteFile(p, marshalT(e.t, res), 0o644))
	e.useFakeWorker("ok", p)
	e.must(os.WriteFile(filepath.Join(e.cfg.WorkerDir, "textrepair.py"), []byte("# fake\n"), 0o644))
	return e.jobLog()
}

func resultSentence(x model.Sentence) map[string]any {
	return map[string]any{"start_ms": x.StartMS, "end_ms": x.EndMS, "text": x.ASRText,
		"low_conf_words": x.LowConfWords, "words": x.Words}
}

var trStats = map[string]any{"version": 1, "words": 31, "segments": 1, "unpunctuated_segments": 1, "caps_segments": 0,
	"sentence_ends_added": 1, "commas_added": 0, "words_recased": 2, "train_sentences": 0,
	"kept_sentences": 1, "sentences_in": 4, "sentences_out": 4, "ms": 12}

// repairedResult: sentences 0 and 1 as they were, 2 and 3 split again.
func repairedResult(ss []model.Sentence) map[string]any {
	a := sentence(2, "And we XOR it with the message.", 3000)
	b := sentence(3, "And then the receiver does the same thing with the key right", 4750)
	return map[string]any{"prompt_terms_used": 2, "textrepair": trStats,
		"sentences": []any{resultSentence(ss[0]), resultSentence(ss[1]), resultSentence(a), resultSentence(b)}}
}

func TestTextRepairRunStoresTheRepairedTranscript(t *testing.T) {
	e := newEnv(t)
	l, ss := e.textRepairLecture()
	logPath := e.useFakeTextRepair(repairedResult(ss))
	// the device check of a transcription does not apply: nothing is transcribed
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot("ov:gpu-missing", "large-v3-turbo", "int8", "en"))
	r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	// job.json: the rows in idx order, the reviewed one kept, the whole term list
	b, err := os.ReadFile(logPath)
	e.must(err)
	var logged struct {
		Script string        `json:"script"`
		Job    textRepairJob `json:"job"`
	}
	e.must(json.Unmarshal([]byte(strings.SplitN(string(b), "\n", 2)[0]), &logged))
	if logged.Script != "textrepair.py" {
		t.Errorf("script %q", logged.Script)
	}
	in := logged.Job
	if in.Language != "en" || in.PromptTermsUsed != 2 || !slices.Equal(in.Glossary, []string{"XOR", "AES"}) || len(in.Sentences) != 4 {
		t.Fatalf("job.json %+v", in)
	}
	for i, x := range in.Sentences {
		if x.ASRText != ss[i].ASRText || x.StartMS != ss[i].StartMS || !slices.Equal(x.Words, ss[i].Words) || x.Keep != (i == 1) {
			t.Errorf("job sentence %d: %+v", i, x)
		}
	}
	// stored like a transcription; the reviewed sentence keeps its human_text
	got, err := e.st.ListSentences(e.ctx, l.ID, 0, 0)
	e.must(err)
	wantText := []string{"Okay, let's start.", "So the key is random.", "And we XOR it with the message.",
		"And then the receiver does the same thing with the key right"}
	if len(got) != 4 {
		t.Fatalf("%d sentences", len(got))
	}
	for i, x := range got {
		if x.Idx != i || x.Text != wantText[i] || x.RepairedText != x.ASRText || x.ZhStatus != model.ZhNone {
			t.Errorf("sentence %d: %+v", i, x)
		}
		if (x.HumanText != nil) != (i == 1) {
			t.Errorf("sentence %d human_text %v", i, x.HumanText)
		}
		if len(x.Words) == 0 {
			t.Errorf("sentence %d lost its words", i)
		}
	}
	if got[1].ASRText != "so the key is random" {
		t.Errorf("kept sentence changed: %+v", got[1])
	}
	if n, _ := e.st.CountPatches(e.ctx, l.ID, ""); n != 0 {
		t.Errorf("%d patches left", n)
	}
	if _, err := e.st.GetSummary(e.ctx, l.ID); err == nil {
		t.Error("summary left")
	}
	if !e.ev.has("sentences", l.ID) {
		t.Error("no sentences event")
	}
	// the job keeps the transcription's timing and prompt; the repair's counters join it
	st := r.job.Stats
	if st.ASRMode != model.ASRModeTextRepair || st.ASRPrompt != "XOR, AES" || st.PromptTermsUsed != 2 ||
		st.ASRTiming == nil || st.ASRTiming.TranscribeMS != 60000 || st.ASRTiming.Postprocess == nil {
		t.Fatalf("stats %+v", st)
	}
	want := model.ASRTextRepair{Version: 1, Words: 31, Segments: 1, UnpunctuatedSegments: 1, SentenceEndsAdded: 1,
		WordsRecased: 2, KeptSentences: 1, SentencesIn: 4, SentencesOut: 4, MS: 12}
	if tr := st.ASRTiming.Postprocess.TextRepair; tr == nil || *tr != want {
		t.Errorf("textrepair stats %+v, want %+v", tr, want)
	}
	if r.job.Status != model.StatusDone || r.progress.last() != 1 {
		t.Errorf("job %s, progress %v", r.job.Status, r.progress.last())
	}
}

func TestTextRepairRunKeepsTheRedecodeCounters(t *testing.T) {
	e := newEnv(t)
	l, ss := e.textRepairLecture()
	j := e.job(model.ScopeLecture, l.ID, model.StageASR)
	st := j.Stats
	st.ASRTiming.Postprocess = &model.ASRPostprocess{Redecode: &model.ASRRedecode{Windows: 100, Unpunctuated: 50}}
	e.must(e.st.SetJobStats(e.ctx, j.ID, st))
	e.useFakeTextRepair(repairedResult(ss))
	r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	p := r.job.Stats.ASRTiming.Postprocess
	if p.Redecode == nil || p.Redecode.Windows != 100 || p.TextRepair == nil || p.TextRepair.SentenceEndsAdded != 1 {
		t.Errorf("postprocess %+v", p)
	}
}

func TestTextRepairRunFailures(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *env, l model.Lecture, ss []model.Sentence)
		res   func(ss []model.Sentence) any
		code  string
		perm  bool
	}{
		{"no base python", func(e *env, l model.Lecture, ss []model.Sentence) { e.cfg.Python = "" },
			func(ss []model.Sentence) any { return repairedResult(ss) }, CodeWorkerPythonNotInstalled, true},
		{"no word timestamps", func(e *env, l model.Lecture, ss []model.Sentence) {
			e.must(e.st.DeleteSentences(e.ctx, l.ID))
			for i := range ss {
				ss[i].Words = nil
			}
			e.must(e.st.InsertSentences(e.ctx, l.ID, ss))
		}, func(ss []model.Sentence) any { return repairedResult(ss) }, CodeTextRepairNoWords, true},
		{"no sentences", func(e *env, l model.Lecture, ss []model.Sentence) { e.must(e.st.DeleteSentences(e.ctx, l.ID)) },
			func(ss []model.Sentence) any { return repairedResult(ss) }, CodeTextRepairNoSentences, true},
		{"unparsable", nil, func(ss []model.Sentence) any { return "not an object" }, CodeTextRepairUnparsable, false},
		{"the reviewed sentence missing", nil, func(ss []model.Sentence) any {
			r := repairedResult(ss)
			x := sentence(1, "so the key is random.", 1000)
			r["sentences"].([]any)[1] = resultSentence(x)
			return r
		}, CodeTextRepairUnparsable, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			l, ss := e.textRepairLecture()
			e.useFakeTextRepair(c.res(slices.Clone(ss)))
			if c.setup != nil {
				c.setup(e, l, ss)
			}
			before, err := e.st.ListSentences(e.ctx, l.ID, 0, 0)
			e.must(err)
			r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
			if r.err == nil {
				t.Fatal("no error")
			}
			if got := codeOf(r.err); got != c.code {
				t.Errorf("code %q, want %q (%v)", got, c.code, r.err)
			}
			if jobErrorCode(r.job) != c.code {
				t.Errorf("job error %v", r.job.Error)
			}
			if sched.IsPermanent(r.err) != c.perm {
				t.Errorf("permanent = %v, want %v", sched.IsPermanent(r.err), c.perm)
			}
			// nothing written: the stored transcript, patches and summary stay
			after, err := e.st.ListSentences(e.ctx, l.ID, 0, 0)
			e.must(err)
			if len(after) != len(before) {
				t.Errorf("%d sentences after, %d before", len(after), len(before))
			}
			if n, _ := e.st.CountPatches(e.ctx, l.ID, ""); n != 1 {
				t.Errorf("patches %d", n)
			}
		})
	}
}

func TestTextRepairStatsParsed(t *testing.T) {
	var tm workerTiming
	e := json.Unmarshal([]byte(`{"load_s":1,"transcribe_s":2,"audio_s":3,"postprocess":{"textrepair":`+
		`{"version":1,"words":7000,"segments":40,"unpunctuated_segments":35,"caps_segments":5,"sentence_ends_added":300,`+
		`"commas_added":4,"words_recased":900,"train_sentences":250,"ms":38}}}`), &tm)
	if e != nil {
		t.Fatal(e)
	}
	got := string(marshalT(t, tm.stats()))
	want := `{"load_ms":1000,"transcribe_ms":2000,"audio_ms":3000,"postprocess":{"textrepair":{"version":1,"words":7000,` +
		`"segments":40,"unpunctuated_segments":35,"caps_segments":5,"sentence_ends_added":300,"commas_added":4,` +
		`"words_recased":900,"train_sentences":250,"ms":38}}}`
	if got != want {
		t.Errorf("%s\nwant %s", got, want)
	}
	// both steps; a skipped language
	tm = workerTiming{}
	_ = json.Unmarshal([]byte(`{"postprocess":{"redecode":{"windows":2,"unpunctuated":1,"uppercase":0,"redecoded":1,`+
		`"accepted":1,"redecode_s":1.5},"textrepair":{"version":1,"skipped":"language","words":10,"ms":0}}}`), &tm)
	p := tm.stats().Postprocess
	if p.Redecode == nil || p.Redecode.RedecodeMS != 1500 || p.TextRepair == nil || p.TextRepair.Skipped != "language" {
		t.Errorf("%+v", p)
	}
	// none
	tm = workerTiming{}
	_ = json.Unmarshal([]byte(`{"load_s":1}`), &tm)
	if tm.stats().Postprocess != nil {
		t.Error("postprocess without steps")
	}
}

func TestMeasureTextDegeneration(t *testing.T) {
	mk := func(texts ...string) []model.Sentence {
		var out []model.Sentence
		for i, x := range texts {
			out = append(out, model.Sentence{Idx: i, ASRText: x})
		}
		return out
	}
	normal := "So the key is random. We use XOR for this step, okay. The receiver does the same thing."
	run := strings.Repeat("and then we take the key ", 6) // 36 words, no mark
	cases := []struct {
		name  string
		ss    []model.Sentence
		unp   int
		caps  int
		worth bool
	}{
		{"normal", mk(normal, normal, normal), 0, 0, false},
		// a stretch counts its words up to the next mark (the marked word ends it)
		{"unpunctuated", mk(normal, run+"right.", normal), 36, 0, true},
		{"short stretches", mk(strings.Repeat("a b c d e f g h i j k l m n o p q r s t u v w x. ", 2)), 0, 0, false},
		// an all-caps run of ordinary words (written elsewhere in lower case)
		{"caps", mk(normal, "SO THE RECEIVER DOES THE SAME THING WITH THE KEY.", normal), 0, 1, true},
		// acronyms only, or capitals never seen in lower case: no span
		{"acronyms", mk(normal, "AES DES XOR CBC CTR IV NIST FIPS.", normal), 0, 0, false},
		// fewer than 10% of the words
		{"a little", mk(strings.Repeat(normal+" ", 30), run+"done."), 36, 0, false},
	}
	for _, c := range cases {
		m := MeasureTextDegeneration(c.ss)
		if m.UnpunctuatedWords != c.unp || m.CapsSegments != c.caps || m.Worthwhile() != c.worth {
			t.Errorf("%s: %+v (share %.3f), want unpunctuated %d caps %d worth %v", c.name, m, m.UnpunctuatedShare(),
				c.unp, c.caps, c.worth)
		}
	}
	if m := MeasureTextDegeneration(nil); m.Words != 0 || m.Worthwhile() {
		t.Errorf("empty: %+v", m)
	}
}

func TestPromptTerms(t *testing.T) {
	if got := promptTerms("XOR, AES,  , Shannon cipher, "); !slices.Equal(got, []string{"XOR", "AES", "Shannon cipher"}) {
		t.Errorf("%q", got)
	}
	if got := promptTerms(""); len(got) != 0 || got == nil {
		t.Errorf("%#v", got)
	}
}
