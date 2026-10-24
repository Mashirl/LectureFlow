// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/cite"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/stages -run Export -update)", err)
	}
	if w := strings.ReplaceAll(string(want), "\r\n", "\n"); got != w {
		t.Errorf("%s differs from the golden file:\n--- got ---\n%s\n--- want ---\n%s", name, got, w)
	}
}

func TestSafeFileName(t *testing.T) {
	cases := map[string]string{
		`Lec 3: Caches/Memory? <draft>`: "Lec 3_ Caches_Memory_ _draft_",
		"  spaced\tout  ":               "spaced out",
		"trailing dots...":              "trailing dots",
		"CON":                           "CON_",
		"nul.txt":                       "nul_.txt",
		"":                              "lecture",
		"...":                           "lecture",
		"a\x00b":                        "a_b",
		"第3讲 缓存":                        "第3讲 缓存",
		strings.Repeat("x", 150):        strings.Repeat("x", 100),
	}
	for in, want := range cases {
		if got := SafeFileName(in); got != want {
			t.Errorf("SafeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

// exportNames is the export file names of lecture l as the export stage
// writes them now (CourseLabels → ExportLabel → ExportNames).
func (e *env) exportNames(l model.Lecture) []string {
	e.t.Helper()
	numbers, err := CourseLabels(e.ctx, e.deps(nil), l.CourseID)
	e.must(err)
	l = e.lec(l.ID)
	return ExportNames(ExportLabel(numbers, l.ID), l.Title)
}

// TestExportNamesUseCalendarNumber (SPEC-v2 P13.6): export files are named
// "{number} {English title}"; an unnumbered lecture uses the unique
// "W??-{id prefix}" of the citation JSON ("?" → "_"), so two unnumbered
// lectures with the same title get different names.
func TestExportNamesUseCalendarNumber(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil)
	a := e.lecture("Stream Ciphers", "a.mp4")
	b := e.lecture("Stream Ciphers", "b.mp4")
	e.insertSentences(a.ID, []model.Sentence{{LectureID: a.ID, Idx: 0, StartMS: 0, EndMS: 1000, ASRText: "Hi.", RepairedText: "Hi.", ZhStatus: model.ZhNone}})
	e.insertSentences(b.ID, []model.Sentence{{LectureID: b.ID, Idx: 0, StartMS: 0, EndMS: 1000, ASRText: "Hi.", RepairedText: "Hi.", ZhStatus: model.ZhNone}})
	numbers, err := CourseLabels(e.ctx, d, e.course.ID)
	e.must(err)
	la, lb := ExportLabel(numbers, a.ID), ExportLabel(numbers, b.ID)
	if !strings.HasPrefix(la, "W??-") || !strings.HasPrefix(lb, "W??-") || la == lb {
		t.Fatalf("unnumbered labels %q %q: want distinct W??-{id prefix}", la, lb)
	}
	for _, l := range []model.Lecture{a, b} {
		if r := e.run(e.stage(d, model.StageExport), l.ID); r.err != nil {
			t.Fatal(r.err)
		}
	}
	nameOf := func(l model.Lecture) string {
		entries, err := os.ReadDir(filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), model.ExportDirName))
		e.must(err)
		for _, en := range entries {
			if strings.HasSuffix(en.Name(), ".bilingual.md") {
				return en.Name()
			}
		}
		return ""
	}
	na, nb := nameOf(a), nameOf(b)
	if na != "W__-"+strings.TrimPrefix(la, "W??-")+" Stream Ciphers.bilingual.md" || nb == na {
		t.Errorf("unnumbered names %q %q", na, nb)
	}

	// numbered: term from Monday 2026-09-28, recorded in week 2
	termStart, rec := "2026-09-28", "2026-10-06T09:00"
	e.must(e.st.SetCourseCalendar(e.ctx, e.course.ID, &termStart, nil))
	e.must(e.st.SetLectureRecordedAt(e.ctx, a.ID, &rec, model.RecordedAtUser))
	e.must((&exportStage{d: d}).Run(e.ctx, a.ID, func(float64) {})) // a rerun

	if got := nameOf(a); got != "W02L1 Stream Ciphers.bilingual.md" {
		t.Errorf("numbered name %q", got)
	}
	if got := e.exportNames(a); got[4] != "W02L1 Stream Ciphers.cite.json" {
		t.Errorf("ExportNames %v", got)
	}
	if got := ExportLabel(nil, "01JB2AXYZ"); got != "W??-01JB2A" {
		t.Errorf("lecture missing from numbers: %q", got)
	}
}

// The citation JSON's duration_ms is the media length (audio.wav), the
// value the lecture page shows, not the end of the last sentence; a
// lecture whose audio.wav cannot be read keeps the last sentence's end.
func TestExportCiteDurationIsMedia(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil)
	l := e.lecture("Stream Ciphers", "a.mp4")
	e.insertSentences(l.ID, []model.Sentence{{LectureID: l.ID, Idx: 0, StartMS: 0, EndMS: 4000, ASRText: "Hi.", RepairedText: "Hi.", ZhStatus: model.ZhNone}})
	citeDuration := func() int64 {
		t.Helper()
		e.must((&exportStage{d: d}).Run(e.ctx, l.ID, func(float64) {}))
		b, err := os.ReadFile(filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), model.ExportDirName, e.exportNames(l)[4]))
		e.must(err)
		var doc cite.Doc
		e.must(json.Unmarshal(b, &doc))
		return doc.Lectures[0].DurationMS
	}
	e.writeWAV(l, "RIFF fake wav") // not a readable header
	if got := citeDuration(); got != 4000 {
		t.Errorf("unreadable audio.wav: %d, want the last sentence's end", got)
	}
	// 16 kHz mono 16-bit, 6.5 s: the recording goes on after the last sentence
	n := 16000 * 2 * 6500 / 1000
	var b bytes.Buffer
	b.WriteString("RIFF")
	e.must(binary.Write(&b, binary.LittleEndian, uint32(36+n)))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(16000), uint32(32000), uint16(2), uint16(16)} {
		e.must(binary.Write(&b, binary.LittleEndian, v))
	}
	b.WriteString("data")
	e.must(binary.Write(&b, binary.LittleEndian, uint32(n)))
	b.Write(make([]byte, n))
	e.writeWAV(l, b.String())
	if got := citeDuration(); got != 6500 {
		t.Errorf("audio.wav of 6.5 s: %d", got)
	}
}

// TestExportDownloadName: a transcript or subtitle file downloads under the
// lecture's current "{number} {English title}"; a citation JSON and an
// unknown file keep their names.
func TestExportDownloadName(t *testing.T) {
	for name, want := range map[string]string{
		"Old title.bilingual.md":        "W03L2 New_ title.bilingual.md",
		"W02L1 Old title.zh.vtt":        "W03L2 New_ title.zh.vtt",
		"W02L1 Old title.bilingual.vtt": "W03L2 New_ title.bilingual.vtt",
		"W02L1 Old title.en.txt":        "W03L2 New_ title.en.txt",
		"W02L1 Old title.cite.json":     "W02L1 Old title.cite.json",
		"notes.pdf":                     "notes.pdf",
	} {
		if got := ExportDownloadName("W03L2", "New: title", name); got != want {
			t.Errorf("ExportDownloadName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestTimeFormats(t *testing.T) {
	if mmss(4512345) != "75:12" || mmss(0) != "00:00" || mmss(59999) != "00:59" {
		t.Errorf("mmss: %s %s %s", mmss(4512345), mmss(0), mmss(59999))
	}
	if vttTime(4512345) != "01:15:12.345" || vttTime(1) != "00:00:00.001" {
		t.Errorf("vttTime: %s %s", vttTime(4512345), vttTime(1))
	}
}

func TestExportGolden(t *testing.T) {
	e := newEnv(t)
	l := e.lecture(`Lec 3: Caches/Memory?`, "m.mp4")
	ss := []model.Sentence{
		{Idx: 0, StartMS: 0, EndMS: 3800, ASRText: "Okay, let's get started.", RepairedText: "Okay, let's get started."},
		{Idx: 1, StartMS: 3800, EndMS: 8200, ASRText: "The cash hierarchy.", RepairedText: "The cache hierarchy."},
		{Idx: 2, StartMS: 4512345, EndMS: 4512345, ASRText: "A --> B at minute 75.", RepairedText: "A --> B at minute 75."},
		{Idx: 3, StartMS: 4520000, EndMS: 4525500, ASRText: "Untranslated.", RepairedText: "Untranslated."},
		// SPEC-v2 P4.6: a long sentence becomes several cues in both VTT files
		{Idx: 4, StartMS: 4526000, EndMS: 4548000, ASRText: longEN, RepairedText: longEN},
	}
	for i := range ss {
		ss[i].LectureID = l.ID
		ss[i].ZhStatus = model.ZhNone
	}
	e.insertSentences(l.ID, ss)
	human := "A human\nedit."
	e.must(e.st.SetHumanText(e.ctx, l.ID, 0, &human))
	e.must(e.st.RecomputeText(e.ctx, l.ID, []int{0}))
	for idx, zh := range map[int]string{0: "好，我们开始。", 1: "缓存层次结构。", 2: "A --> B，在第 75 分钟。", 4: longZh} {
		z := zh
		e.must(e.st.SetTranslation(e.ctx, l.ID, idx, db.SentenceTr{Zh: &z, ZhStatus: model.ZhOK}))
	}
	dir := filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), model.ExportDirName)
	e.must(os.MkdirAll(dir, 0o755))
	e.must(os.WriteFile(filepath.Join(dir, "Old title.en.txt"), []byte("old"), 0o644))

	// citation JSON inputs (A11): course code, summary, enabled terms only;
	// P13.6: the calendar number (term from Monday 2026-09-28, recorded on
	// Tuesday of teaching week 2 → W02L1)
	code := "CSC1001"
	e.must(e.st.SetCourseCode(e.ctx, e.course.ID, &code))
	termStart, rec := "2026-09-28", "2026-10-06T09:00"
	e.must(e.st.SetCourseCalendar(e.ctx, e.course.ID, &termStart, nil))
	e.must(e.st.SetLectureRecordedAt(e.ctx, l.ID, &rec, model.RecordedAtUser))
	e.setSummary(l.ID, "Caches and memory.")
	e.addTerm("cache hierarchy", "缓存层次结构", model.SourceSyllabus, 1)
	off := e.addTerm("eviction", "驱逐", model.SourceSyllabus, 1)
	off.Enabled = false
	e.must(e.st.UpdateTerm(e.ctx, off))

	d := e.deps(nil)
	d.Now = func() time.Time { return time.Date(2026, 10, 23, 20, 0, 0, 0, time.UTC) }
	r := e.run(e.stage(d, model.StageExport), l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, en := range entries {
		names = append(names, en.Name())
	}
	// P13.6: "{number} {English title}" (the content of the golden files is
	// unchanged; only the names gained the number)
	const base = "W02L1 Lec 3_ Caches_Memory_"
	want := e.exportNames(l)
	if strings.Join(names, ",") != base+".bilingual.md,"+base+".bilingual.vtt,"+base+".cite.json,"+base+".en.txt,"+base+".zh.vtt" ||
		want[0] != base+".bilingual.md" {
		t.Errorf("export dir: %v (old files and .tmp must be gone)", names)
	}
	for _, n := range want {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		got := string(b)
		if strings.HasSuffix(n, ".cite.json") { // ids are new ULIDs on every run
			got = strings.NewReplacer(l.ID, "{lecture_id}", e.course.ID, "{course_id}").Replace(got)
		}
		golden(t, "export/"+strings.TrimPrefix(n, base+".")+".golden", got)
	}

	// the citation JSON is cite.Build/Encode of this one lecture (A11.2)
	b, err := os.ReadFile(filepath.Join(dir, want[4]))
	if err != nil {
		t.Fatal(err)
	}
	var doc cite.Doc
	e.must(json.Unmarshal(b, &doc))
	if len(doc.Lectures) != 1 || doc.Lectures[0].ID != l.ID || doc.Lectures[0].Label != "W02L1" || len(doc.Lectures[0].Sentences) != 5 {
		t.Fatalf("cite lectures %+v", doc.Lectures)
	}
	cs := doc.Lectures[0].Sentences
	if cs[0].Ref != "CSC1001 W02L1@00:00" || cs[0].ID != "CSC1001.W02L1.s0001" || !cs[0].Edited || cs[0].En != "A human\nedit." || cs[3].Zh != nil || cs[2].Ref != "CSC1001 W02L1@1:15:12" {
		t.Errorf("cite sentences %+v", cs)
	}
	if len(doc.Glossary) != 1 || doc.Glossary[0].En != "cache hierarchy" || doc.Lectures[0].Summary != "Caches and memory." {
		t.Errorf("cite glossary %+v summary %q", doc.Glossary, doc.Lectures[0].Summary)
	}
	if r.progress.last() != 1 || !e.ev.has("lecture", l.ID) {
		t.Errorf("progress %v", r.progress.vals)
	}
	// P13.6 (lead ruling): only the user's exports count for the
	// renumbering check, not the cite.json the stage writes automatically
	if v, ok, err := e.st.GetKV(e.ctx, model.KVCiteExportPrefix+e.course.ID); err != nil || ok {
		t.Errorf("the export stage recorded cite_exported = %q (%v)", v, err)
	}
	at := time.Date(2026, 10, 23, 22, 30, 0, 0, time.FixedZone("UTC+8", 8*3600))
	e.must(MarkCiteExported(e.ctx, e.st, e.course.ID, at)) // what the API calls on a download
	if v, ok, err := e.st.GetKV(e.ctx, model.KVCiteExportPrefix+e.course.ID); err != nil || !ok || v != "2026-10-23T14:30:00Z" {
		t.Errorf("kv cite_exported = %q %v %v", v, ok, err)
	}
}

// longEN / longZh: a 22-second sentence (P4.3 splits it into several cues).
const (
	longEN = "So when the processor misses in the first level cache, it asks the second level, and only if that misses too does it go all the way out to main memory, which costs hundreds of cycles."
	longZh = "所以当处理器在一级缓存中未命中时，它会访问二级缓存；只有二级缓存也未命中，才会一路访问主存，这要花费数百个周期。"
)
