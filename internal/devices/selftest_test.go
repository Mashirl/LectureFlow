// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// lectureSentences builds n sentences of durMS each, separated by gaps
// gap(i) (the gap before sentence i, i >= 1); sentence i says "s{i} …".
func lectureSentences(n int, durMS int64, gap func(i int) int64) []model.Sentence {
	ss := make([]model.Sentence, n)
	t := int64(800)
	for i := range ss {
		if i > 0 {
			t += gap(i)
		}
		txt := fmt.Sprintf("s%d says something.", i)
		ss[i] = model.Sentence{Idx: i, StartMS: t, EndMS: t + durMS, ASRText: txt, RepairedText: txt, Text: txt}
		t += durMS
	}
	return ss
}

// checkClip asserts the planner's invariants: no sentence crosses a cut,
// ref is the text of exactly the sentences inside, the clip is 3/4..5/4 of
// target long.
func checkClip(t *testing.T, ss []model.Sentence, start, end int64, ref string, target time.Duration) []int {
	t.Helper()
	var inside []int
	var parts []string
	for i, s := range ss {
		switch {
		case s.StartMS >= start && s.EndMS <= end:
			inside = append(inside, i)
			if txt := strings.Join(strings.Fields(s.Text), " "); txt != "" {
				parts = append(parts, txt)
			}
		case s.EndMS <= start || s.StartMS >= end:
		default:
			t.Errorf("sentence %d (%d–%d) crosses the clip %d–%d", i, s.StartMS, s.EndMS, start, end)
		}
	}
	if want := strings.Join(parts, " "); ref != want {
		t.Errorf("ref %q, want %q", ref, want)
	}
	tg := target.Milliseconds()
	if l := end - start; l < tg*3/4 || l > tg*5/4 {
		t.Errorf("clip %d–%d is %d ms long, target %d", start, end, l, tg)
	}
	return inside
}

func TestPlanClipCentred(t *testing.T) {
	// 10 minutes: 5 s sentences with 0.5 s pauses, every 7th pause 3 s.
	ss := lectureSentences(100, 5000, func(i int) int64 {
		if i%7 == 0 {
			return 3000
		}
		return 500
	})
	total := ss[len(ss)-1].EndMS
	start, end, ref, ok := devices.PlanClip(ss, devices.SelfTestClip)
	if !ok {
		t.Fatal("no clip")
	}
	inside := checkClip(t, ss, start, end, ref, devices.SelfTestClip)
	if mid := (start + end) / 2; mid < total/2-10000 || mid > total/2+10000 {
		t.Errorf("clip %d–%d is not centred on %d", start, end, total/2)
	}
	if len(inside) < 15 {
		t.Errorf("only %d sentences inside", len(inside))
	}
	// the cut keeps half of a short pause next to the speech
	first, last := ss[inside[0]], ss[inside[len(inside)-1]]
	if first.StartMS-start != 250 && first.StartMS-start != 1000 {
		t.Errorf("start cut %d ms before the first sentence", first.StartMS-start)
	}
	if end-last.EndMS != 250 && end-last.EndMS != 1000 {
		t.Errorf("end cut %d ms after the last sentence", end-last.EndMS)
	}
	// deterministic
	s2, e2, r2, _ := devices.PlanClip(ss, devices.SelfTestClip)
	if s2 != start || e2 != end || r2 != ref {
		t.Error("PlanClip is not deterministic")
	}
}

// TestPlanClipPrefersPauses: sentences touching each other are cut only
// where there is a pause, and a long pause contributes at most clipPadMS.
func TestPlanClipPrefersPauses(t *testing.T) {
	// 6 minutes of 4 s sentences without gaps, except a 14 s pause before
	// sentence 30 (≈ 2:00) and a 6 s pause before sentence 60 (≈ 4:14).
	ss := lectureSentences(90, 4000, func(i int) int64 {
		switch i {
		case 30:
			return 14000
		case 60:
			return 6000
		}
		return 0
	})
	start, end, ref, ok := devices.PlanClip(ss, devices.SelfTestClip)
	if !ok {
		t.Fatal("no clip")
	}
	checkClip(t, ss, start, end, ref, devices.SelfTestClip)
	if start != ss[30].StartMS-1000 {
		t.Errorf("start %d, want 1 s before sentence 30 (%d)", start, ss[30].StartMS)
	}
	if end != ss[59].EndMS+1000 {
		t.Errorf("end %d, want 1 s after sentence 59 (%d)", end, ss[59].EndMS)
	}
	if !strings.HasPrefix(ref, "s30 says") || !strings.HasSuffix(ref, "s59 says something.") {
		t.Errorf("ref %q…", ref[:min(len(ref), 40)])
	}

	// without any pause, sentence boundaries are used
	flat := lectureSentences(90, 4000, func(int) int64 { return 0 })
	start, end, ref, ok = devices.PlanClip(flat, devices.SelfTestClip)
	if !ok {
		t.Fatal("no clip without pauses")
	}
	checkClip(t, flat, start, end, ref, devices.SelfTestClip)
	if (end-start)%4000 != 0 {
		t.Errorf("clip %d–%d does not lie on sentence boundaries", start, end)
	}
}

func TestPlanClipText(t *testing.T) {
	ss := lectureSentences(60, 4000, func(int) int64 { return 400 })
	human := "  A  human\tedit.  "
	for i := range ss {
		switch i % 10 {
		case 3:
			ss[i].Text = human
		case 4:
			ss[i].Text = "   " // an emptied sentence adds nothing
		}
	}
	start, end, ref, ok := devices.PlanClip(ss, devices.SelfTestClip)
	if !ok {
		t.Fatal("no clip")
	}
	checkClip(t, ss, start, end, ref, devices.SelfTestClip)
	if !strings.Contains(ref, "A human edit.") || strings.Contains(ref, "  ") {
		t.Errorf("ref %q", ref)
	}
}

func TestPlanClipUnusable(t *testing.T) {
	cases := map[string][]model.Sentence{
		"empty":             nil,
		"short":             lectureSentences(20, 5000, func(int) int64 { return 500 }), // 1:50
		"one long sentence": {{Idx: 0, StartMS: 0, EndMS: 600000, Text: "x"}},
		// every sentence overlaps the next: no boundary inside the lecture
		"overlapping": func() []model.Sentence {
			var ss []model.Sentence
			for i := range 100 {
				s := int64(i) * 5000
				ss = append(ss, model.Sentence{Idx: i, StartMS: s, EndMS: s + 6000, Text: "x"})
			}
			return ss
		}(),
	}
	for name, ss := range cases {
		if s, e, ref, ok := devices.PlanClip(ss, devices.SelfTestClip); ok {
			t.Errorf("%s: clip %d–%d %q", name, s, e, ref)
		}
	}
	// a lecture just over the target long: the clip starts with its first
	// sentence and is about target long
	whole := lectureSentences(24, 4600, func(int) int64 { return 600 }) // 0.8 s … 2:05
	s, e, ref, ok := devices.PlanClip(whole, devices.SelfTestClip)
	if !ok {
		t.Fatal("lecture of 2:05: no clip")
	}
	checkClip(t, whole, s, e, ref, devices.SelfTestClip)
	if s != whole[0].StartMS-400 || !strings.HasPrefix(ref, "s0 ") {
		t.Errorf("lecture of 2:05: clip %d–%d %q…", s, e, ref[:10])
	}
}

func f64(v float64) *float64 { return &v }

func TestPick(t *testing.T) {
	r := func(m, p string, ok bool, rtf float64, wer *float64) model.BenchmarkResult {
		return model.BenchmarkResult{Model: m, Precision: p, OK: ok, RTF: rtf, WER: wer}
	}
	cases := []struct {
		name string
		rs   []model.BenchmarkResult
		want model.BenchmarkPick
		ok   bool
	}{
		{"none", nil, model.BenchmarkPick{}, false},
		{"all failed", []model.BenchmarkResult{r("a", "fp16", false, 0.1, f64(0.01))}, model.BenchmarkPick{}, false},
		{"lowest wer under limit", []model.BenchmarkResult{
			r("turbo", "fp16", true, 0.3, f64(0.06)), r("turbo", "int8", true, 0.2, f64(0.05)), r("small", "int8", true, 0.1, f64(0.09)),
		}, model.BenchmarkPick{Model: "turbo", Precision: "int8", Rule: model.PickRuleBest}, true},
		{"limit is inclusive, too slow excluded", []model.BenchmarkResult{
			r("large", "fp16", true, 0.51, f64(0.01)), r("turbo", "fp16", true, 0.5, f64(0.07)),
		}, model.BenchmarkPick{Model: "turbo", Precision: "fp16", Rule: model.PickRuleBest}, true},
		{"equal wer: lower rtf", []model.BenchmarkResult{
			r("turbo", "fp16", true, 0.3, f64(0.05)), r("turbo", "int8", true, 0.2, f64(0.05)),
		}, model.BenchmarkPick{Model: "turbo", Precision: "int8", Rule: model.PickRuleBest}, true},
		{"equal wer and rtf: earlier", []model.BenchmarkResult{
			r("turbo", "fp16", true, 0.2, f64(0.05)), r("turbo", "int8", true, 0.2, f64(0.05)),
		}, model.BenchmarkPick{Model: "turbo", Precision: "fp16", Rule: model.PickRuleBest}, true},
		{"no reference: first ok under limit", []model.BenchmarkResult{
			r("turbo", "fp16", false, 0, nil), r("turbo", "int8", true, 0.4, nil), r("small", "int8", true, 0.1, nil),
		}, model.BenchmarkPick{Model: "turbo", Precision: "int8", Rule: model.PickRuleBest}, true},
		{"failed result ignored", []model.BenchmarkResult{
			r("turbo", "fp16", false, 0.1, f64(0.0)), r("turbo", "int8", true, 0.3, f64(0.08)),
		}, model.BenchmarkPick{Model: "turbo", Precision: "int8", Rule: model.PickRuleBest}, true},
		{"none under limit: lowest rtf", []model.BenchmarkResult{
			r("turbo", "fp16", true, 1.2, f64(0.05)), r("turbo", "int8", true, 0.9, f64(0.06)), r("small", "int8", true, 0.7, f64(0.12)),
			r("base", "int8", false, 0.2, nil),
		}, model.BenchmarkPick{Model: "small", Precision: "int8", Rule: model.PickRuleLowestRTF}, true},
	}
	for _, c := range cases {
		got, ok := devices.Pick(c.rs)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: %+v %v, want %+v %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// TestPickFor (power management, lead addition after I12): when the
// PickRuleBest winner and the device's recommended combination tie — WERs
// at most one reference word apart and RTFs less than 10 % apart — the
// recommended one is kept; otherwise PickFor is Pick.
func TestPickFor(t *testing.T) {
	r := func(m, p string, ok bool, rtf float64, wer *float64) model.BenchmarkResult {
		return model.BenchmarkResult{Model: m, Precision: p, OK: ok, RTF: rtf, WER: wer}
	}
	const turbo = "large-v3-turbo"
	errs := func(n, words int) *float64 { return f64(float64(n) / float64(words)) }
	// I12 (docs/benchmarks/intel-openvino.md §11.1): the NPU on a 280-word
	// clip of 119.1 s, 16 errors with both precisions, 9.85 s against 9.77 s.
	clip := 119.1
	i12 := []model.BenchmarkResult{
		r(turbo, "fp16", true, 9.85/clip, errs(16, 280)), r(turbo, "int8", true, 9.77/clip, errs(16, 280)),
	}
	if p, _ := devices.Pick(i12); p.Precision != "int8" {
		t.Fatalf("Pick of the I12 NPU results: %+v (the case PickFor corrects)", p)
	}
	best := func(m, p string) model.BenchmarkPick {
		return model.BenchmarkPick{Model: m, Precision: p, Rule: model.PickRuleBest}
	}
	cases := []struct {
		name     string
		id       string
		refWords int
		rs       []model.BenchmarkResult
		want     model.BenchmarkPick
		ok       bool
	}{
		{"I12 NPU: same WER, RTF 0.8 % apart → the recommended fp16", model.DeviceOVNPU, 280, i12, best(turbo, "fp16"), true},
		{"same WER, int8 5 % faster → fp16", model.DeviceOVNPU, 280, []model.BenchmarkResult{
			r(turbo, "fp16", true, 0.0800, errs(16, 280)), r(turbo, "int8", true, 0.0760, errs(16, 280)),
		}, best(turbo, "fp16"), true},
		{"fp16 one word worse, int8 4 % faster → fp16", model.DeviceOVNPU, 280, []model.BenchmarkResult{
			r(turbo, "fp16", true, 0.0800, errs(17, 280)), r(turbo, "int8", true, 0.0768, errs(16, 280)),
		}, best(turbo, "fp16"), true},
		{"fp16 two words worse → int8", model.DeviceOVNPU, 280, []model.BenchmarkResult{
			r(turbo, "fp16", true, 0.0800, errs(18, 280)), r(turbo, "int8", true, 0.0790, errs(16, 280)),
		}, best(turbo, "int8"), true},
		{"RTF 9.9 % apart → fp16", model.DeviceOVGPU, 280, []model.BenchmarkResult{
			r(turbo, "fp16", true, 0.0400, errs(16, 280)), r(turbo, "int8", true, 0.03604, errs(16, 280)),
		}, best(turbo, "fp16"), true},
		{"RTF exactly 10 % apart → int8", model.DeviceOVGPU, 280, []model.BenchmarkResult{
			r(turbo, "fp16", true, 0.3125, errs(16, 280)), r(turbo, "int8", true, 0.28125, errs(16, 280)),
		}, best(turbo, "int8"), true},
		{"I12 Intel GPU: same WER, RTF 16 % apart → int8", model.DeviceOVGPU, 280, []model.BenchmarkResult{
			r(turbo, "fp16", true, 5.26/clip, errs(16, 280)), r(turbo, "int8", true, 4.35/clip, errs(16, 280)),
		}, best(turbo, "int8"), true},
		{"the recommended one wins anyway", model.DeviceOVNPU, 280, []model.BenchmarkResult{
			r(turbo, "fp16", true, 0.0800, errs(15, 280)), r(turbo, "int8", true, 0.0790, errs(16, 280)),
		}, best(turbo, "fp16"), true},
		{"recommended above the RTF limit → int8", model.DeviceOVNPU, 280, []model.BenchmarkResult{
			r(turbo, "fp16", true, 0.52, errs(16, 280)), r(turbo, "int8", true, 0.49, errs(16, 280)),
		}, best(turbo, "int8"), true},
		{"recommended failed → int8", model.DeviceOVNPU, 280, []model.BenchmarkResult{
			r(turbo, "fp16", false, 0, nil), r(turbo, "int8", true, 0.0790, errs(16, 280)),
		}, best(turbo, "int8"), true},
		{"no reference → Pick", model.DeviceOVNPU, 0, []model.BenchmarkResult{
			r(turbo, "fp16", true, 0.0800, nil), r(turbo, "int8", true, 0.0790, nil),
		}, best(turbo, "fp16"), true},
		{"no reference words, equal WER → Pick (lower RTF)", model.DeviceOVNPU, 0, i12, best(turbo, "int8"), true},
		{"no recommendation (fw:cpu) → Pick", model.DeviceFWCPU, 280, []model.BenchmarkResult{
			r("small", "auto", true, 0.30, errs(20, 280)), r("base", "auto", true, 0.29, errs(20, 280)),
		}, best("base", "auto"), true},
		{"unknown device → Pick", "tpu:0", 280, i12, best(turbo, "int8"), true},
		{"CUDA: large-v3 kept over a 5 % faster turbo", model.DeviceCUDA0, 280, []model.BenchmarkResult{
			r("large-v3", "auto", true, 0.0500, errs(10, 280)), r(turbo, "auto", true, 0.0475, errs(10, 280)),
		}, best("large-v3", "auto"), true},
		{"CUDA: turbo twice as fast → turbo", model.DeviceCUDA0, 280, []model.BenchmarkResult{
			r("large-v3", "auto", true, 0.0500, errs(10, 280)), r(turbo, "auto", true, 0.0250, errs(10, 280)),
		}, best(turbo, "auto"), true},
		{"nothing under the limit: lowest RTF stays", model.DeviceOVNPU, 280, []model.BenchmarkResult{
			r(turbo, "fp16", true, 0.90, errs(16, 280)), r(turbo, "int8", true, 0.88, errs(16, 280)),
		}, model.BenchmarkPick{Model: turbo, Precision: "int8", Rule: model.PickRuleLowestRTF}, true},
		{"no OK result", model.DeviceOVNPU, 280, []model.BenchmarkResult{r(turbo, "fp16", false, 0, nil)}, model.BenchmarkPick{}, false},
	}
	for _, c := range cases {
		got, ok := devices.PickFor(c.id, c.refWords, c.rs)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: %+v %v, want %+v %v", c.name, got, ok, c.want, c.ok)
		}
	}
	if devices.PickTieWords != 1 || devices.PickTieRTF != 0.10 {
		t.Errorf("tie rule %d words / %v", devices.PickTieWords, devices.PickTieRTF)
	}
}

// writeWAVSeconds writes a valid PCM WAV header whose data lasts sec
// seconds at a tiny byte rate (100 bytes/s, 8-bit mono).
func writeWAVSeconds(t *testing.T, path string, sec int) {
	t.Helper()
	data := make([]byte, sec*100)
	b := []byte("RIFF")
	b = binary.LittleEndian.AppendUint32(b, uint32(36+len(data)))
	b = append(b, "WAVEfmt "...)
	b = binary.LittleEndian.AppendUint32(b, 16)
	b = binary.LittleEndian.AppendUint16(b, 1)   // PCM
	b = binary.LittleEndian.AppendUint16(b, 1)   // mono
	b = binary.LittleEndian.AppendUint32(b, 100) // sample rate
	b = binary.LittleEndian.AppendUint32(b, 100) // byte rate
	b = binary.LittleEndian.AppendUint16(b, 1)   // block align
	b = binary.LittleEndian.AppendUint16(b, 8)
	b = append(b, "data"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(data)))
	b = append(b, data...)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindClipSource(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := db.Open(filepath.Join(root, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dataDir := filepath.Join(root, "data")
	course := model.Course{ID: "C1", Name: "Course", CreatedAt: db.Now()}
	if err := st.CreateCourse(ctx, course); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := devices.FindClipSource(ctx, st, dataDir); ok || err != nil {
		t.Fatalf("empty database: ok %v err %v", ok, err)
	}
	// lecture adds a lecture whose repair finished at finished ("" = repair
	// not done) with audio of sec seconds (0 = no audio.wav).
	lecture := func(id, finished string, sec int) {
		t.Helper()
		l := model.Lecture{ID: id, CourseID: course.ID, Title: "Lecture " + id, MediaPath: "m.mp4", CreatedAt: db.Now()}
		if err := st.CreateLecture(ctx, l); err != nil {
			t.Fatal(err)
		}
		if sec > 0 {
			writeWAVSeconds(t, filepath.Join(model.LectureDir(dataDir, course.ID, id), model.AudioFile), sec)
		}
		if finished == "" {
			return
		}
		j, err := st.GetCurrentJob(ctx, model.ScopeLecture, id, model.StageRepair)
		if err != nil {
			t.Fatal(err)
		}
		for _, step := range [][2]string{{model.StatusPending, model.StatusQueued}, {model.StatusQueued, model.StatusRunning}} {
			if err := state.TransitionJob(ctx, st, j.ID, step[0], step[1], db.JobFinish{}); err != nil {
				t.Fatal(err)
			}
		}
		if err := state.TransitionJob(ctx, st, j.ID, model.StatusRunning, model.StatusDone, db.JobFinish{FinishedAt: &finished}); err != nil {
			t.Fatal(err)
		}
	}
	lecture("L-norepair", "", 600)                  // repair not done
	lecture("L-noaudio", "2026-10-20T10:00:00Z", 0) // no audio.wav
	lecture("L-short", "2026-10-21T10:00:00Z", 110) // shorter than the clip
	lecture("L-old", "2026-10-01T10:00:00Z", 600)   // usable, older
	if src, ok, err := devices.FindClipSource(ctx, st, dataDir); err != nil || !ok || src.LectureID != "L-old" {
		t.Fatalf("got %+v %v %v, want L-old", src, ok, err)
	}
	lecture("L-b", "2026-10-15T10:00:00Z", 3000)
	lecture("L-a", "2026-10-15T10:00:00Z", 3000) // same finish: by lecture id
	src, ok, err := devices.FindClipSource(ctx, st, dataDir)
	if err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	wantAudio, _ := filepath.Abs(filepath.Join(model.LectureDir(dataDir, course.ID, "L-a"), model.AudioFile))
	if src != (devices.ClipSource{LectureID: "L-a", CourseID: course.ID, Title: "Lecture L-a", Audio: wantAudio}) {
		t.Errorf("source %+v", src)
	}
	if !filepath.IsAbs(src.Audio) {
		t.Errorf("audio %q is not absolute", src.Audio)
	}
	// a stale repair (the text changed since) is not "done"
	j, _ := st.GetCurrentJob(ctx, model.ScopeLecture, "L-a", model.StageRepair)
	if err := state.TransitionJob(ctx, st, j.ID, model.StatusDone, model.StatusStale, db.JobFinish{}); err != nil {
		t.Fatal(err)
	}
	if src, _, _ := devices.FindClipSource(ctx, st, dataDir); src.LectureID != "L-b" {
		t.Errorf("after L-a went stale: %q, want L-b", src.LectureID)
	}
}
