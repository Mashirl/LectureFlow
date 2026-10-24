// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

const fakeASR = "../../testdata/fake_asr.json"

func TestPrep(t *testing.T) {
	e := newEnv(t)
	ff := ffmpegOrSkip(t, e.cfg)
	l := e.lecture("L1", "")
	dir := model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID)
	media := filepath.Join(dir, "upload.wav")
	silentMedia(t, ff, media, 3)
	e.must(e.st.SetLectureMedia(e.ctx, l.ID, "upload.wav")) // relative to the lecture dir
	e.cfg.Storage.KeepOriginalMedia = boolp(false)

	r := e.run(e.stage(e.deps(nil), model.StagePrep), l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	fi, err := os.Stat(filepath.Join(dir, model.AudioFile))
	if err != nil || fi.Size() < 16000*2*2 { // ~3 s of 16 kHz 16-bit mono
		t.Fatalf("audio.wav: %v size %v", err, fi)
	}
	if _, err := os.Stat(filepath.Join(dir, model.AudioFile+".tmp")); !os.IsNotExist(err) {
		t.Error("audio.wav.tmp left behind")
	}
	if r.progress.last() != 1 {
		t.Errorf("progress %v", r.progress.vals)
	}
	if _, err := os.Stat(media); !os.IsNotExist(err) {
		t.Error("keep_original_media=false: uploaded media inside the lecture dir must be deleted")
	}
	// SPEC-v2 P8.1: a WAV upload has no video, so no playback file
	if st, ok, err := StreamStateOf(e.ctx, e.st, l.ID); err != nil || !ok || st.Status != StreamAudioOnly {
		t.Errorf("stream state %+v %v %v, want audio_only", st, ok, err)
	}
	if _, err := os.Stat(filepath.Join(dir, model.StreamFile)); !os.IsNotExist(err) {
		t.Error("stream.mp4 written for an audio-only upload")
	}
}

func TestPrepKeepsExternalMedia(t *testing.T) {
	e := newEnv(t)
	ff := ffmpegOrSkip(t, e.cfg)
	media := filepath.Join(e.root, "outside.wav") // e.g. a file the user pointed at
	silentMedia(t, ff, media, 1)
	l := e.lecture("L1", media)
	e.cfg.Storage.KeepOriginalMedia = boolp(false)
	if r := e.run(e.stage(e.deps(nil), model.StagePrep), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if _, err := os.Stat(media); err != nil {
		t.Error("media outside the lecture directory must never be deleted")
	}
}

func TestPrepInputErrors(t *testing.T) {
	e := newEnv(t)
	ffmpegOrSkip(t, e.cfg)
	d := e.deps(nil)

	l := e.lecture("missing", "nope.mp4")
	r := e.run(e.stage(d, model.StagePrep), l.ID)
	if r.err == nil || !sched.IsPermanent(r.err) || codeOf(r.err) != CodePrepMediaMissing || jobErrorCode(r.job) != CodePrepMediaMissing {
		t.Errorf("missing media: %v", r.err)
	}

	l2 := e.lecture("garbage", "bad.mp4")
	dir := model.LectureDir(e.cfg.DataDir, l2.CourseID, l2.ID)
	e.must(os.WriteFile(filepath.Join(dir, "bad.mp4"), []byte(strings.Repeat("not a media file ", 100)), 0o644))
	r = e.run(e.stage(d, model.StagePrep), l2.ID)
	if r.err == nil || !sched.IsPermanent(r.err) || codeOf(r.err) != MediaCorrupt {
		t.Errorf("garbage media: %v", r.err)
	}
	if _, err := os.Stat(filepath.Join(dir, model.AudioFile)); !os.IsNotExist(err) {
		t.Error("audio.wav written for a failed prep")
	}

	e.cfg.FFmpeg = filepath.Join(e.root, "no-ffmpeg.exe")
	l3 := e.lecture("noffmpeg", "x.wav")
	e.must(os.WriteFile(filepath.Join(model.LectureDir(e.cfg.DataDir, l3.CourseID, l3.ID), "x.wav"), []byte("x"), 0o644))
	if r := e.run(e.stage(d, model.StagePrep), l3.ID); r.err == nil || !sched.IsPermanent(r.err) || codeOf(r.err) != CodePrepFFmpegMissing {
		t.Errorf("no ffmpeg: %v", r.err)
	}
}

func boolp(b bool) *bool { return &b }

// writeWAV puts a dummy audio.wav in place (asr reads it only for the hash
// in fake mode).
func (e *env) writeWAV(l model.Lecture, content string) {
	e.t.Helper()
	e.must(os.WriteFile(filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), model.AudioFile), []byte(content), 0o644))
}

func TestASRFakeResult(t *testing.T) {
	e := newEnv(t)
	e.cfg.ASR.FakeResult = fakeASR
	e.addTerm("cache", "缓存", model.SourceSyllabus, 9)
	e.addTerm("write-back", "写回", model.SourceManual, 1)
	e.addTerm("LRU", "", model.SourceMaterial, 4)
	e.addTerm("TLB", "快表", model.SourceMaterial, 1)
	for range 2 {
		if _, err := e.st.BumpGlossaryVersion(e.ctx, e.course.ID); err != nil {
			t.Fatal(err)
		}
	}
	l := e.lecture("L1", "m.mp4")
	e.writeWAV(l, "RIFF fake wav")
	// the settings change after launch: the run keeps the snapshot (A4.1)
	e.cfg.ASR.Model, e.cfg.ASR.Language = "large-v2", "de"
	d := e.deps(nil)
	stg := e.stage(d, model.StageASR)

	r := e.run(stg, l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	dir := model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID)
	if _, err := os.Stat(filepath.Join(dir, "result.json")); err != nil {
		t.Error("result.json missing")
	}
	if _, err := os.Stat(filepath.Join(dir, "result.json.tmp")); !os.IsNotExist(err) {
		t.Error("result.json.tmp left behind")
	}
	ss := e.sentences(l.ID)
	if len(ss) != 14 {
		t.Fatalf("%d sentences", len(ss))
	}
	for i, s := range ss {
		if s.Idx != i || s.RepairedText != s.ASRText || s.Text != s.ASRText || s.HumanText != nil || s.ZhStatus != model.ZhNone {
			t.Errorf("sentence %d: %+v", i, s)
		}
	}
	if len(ss[2].LowConfWords) != 2 || ss[2].StartMS != 8200 || ss[2].EndMS != 12600 {
		t.Errorf("sentence 2: %+v", ss[2])
	}
	if len(r.progress.vals) < 3 || r.progress.last() != 1 {
		t.Errorf("progress %v", r.progress.vals)
	}
	// v1.1 A1: the prompt actually used (the worker's 3 terms), the glossary
	// version and the hash formula are recorded.
	if prompt := ctxbuild.BuildASRPrompt(e.terms(), ""); prompt != "write-back, cache, LRU, TLB" {
		t.Fatalf("prompt %q", prompt)
	}
	if r.stats.PromptTermsUsed != 3 || r.stats.ASRPrompt != "write-back, cache, LRU" ||
		r.stats.GlossaryVersion == nil || *r.stats.GlossaryVersion != 2 || r.stats.HashVersion != ASRHashVersion {
		t.Errorf("stats %+v", r.stats)
	}
	// input_hash = wav + the snapshot's language (SPEC-v2 I6 revising
	// A1, hash_version 3; phase 3b task D: formerly the v1.1 hash of wav +
	// model, compute_type, language)
	wav, _ := stg.(*asrStage).wavSHA256(filepath.Join(dir, model.AudioFile))
	want := ctxbuild.ASRInputHashV3(wav, "en")
	if r.job.InputHash == nil || *r.job.InputHash != want {
		t.Errorf("input_hash %v, want the v3 hash", r.job.InputHash)
	}
	if h, _ := stg.InputHash(e.ctx, l.ID); h != want {
		t.Errorf("InputHash after the run = %q, want %q", h, want)
	}
	// glossary changes do not touch the asr hash (A1)
	e.addTerm("Belady", "", model.SourceManual, 0)
	if h, _ := stg.InputHash(e.ctx, l.ID); h != want {
		t.Error("a new term changed the asr input hash")
	}
	if !e.ev.has("sentences", l.ID) {
		t.Error("no sentences event")
	}
}

// TestASROldJobHashIsEmpty: an asr job recorded before v1.1 (no
// hash_version) has a hash of the old formula; it is never compared (lead
// ruling 3), and neither is one without an ASR snapshot.
func TestASROldJobHashIsEmpty(t *testing.T) {
	e := newEnv(t)
	e.cfg.ASR.FakeResult = fakeASR
	l := e.lecture("L1", "m.mp4")
	e.writeWAV(l, "RIFF")
	stg := e.stage(e.deps(nil), model.StageASR)
	if r := e.run(stg, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if h, _ := stg.InputHash(e.ctx, l.ID); h == "" {
		t.Fatal("a v1.1 asr job must have an input hash")
	}
	j := e.job(model.ScopeLecture, l.ID, model.StageASR)
	old := j.Stats
	old.HashVersion = 0
	e.must(e.st.SetJobStats(e.ctx, j.ID, old))
	if h, err := stg.InputHash(e.ctx, l.ID); err != nil || h != "" {
		t.Errorf("old asr job: InputHash %q %v, want empty", h, err)
	}
	// a v1 job without hash_version is never marked stale, whatever changed
	hashers := map[string]state.Hasher{model.StageASR: stg}
	e.writeWAV(l, "RIFF other content")
	if stale, err := state.CheckStale(e.ctx, e.st, hashers, l.ID); err != nil || len(stale) != 0 {
		t.Errorf("old asr job marked stale: %v %v", staleStages(stale), err)
	}
	old.HashVersion = ASRHashVersion
	e.must(e.st.SetJobStats(e.ctx, j.ID, old))
	e.setSnapshot(l.ID, model.StageASR, model.EmptySnapshot)
	if h, err := stg.InputHash(e.ctx, l.ID); err != nil || h != "" {
		t.Errorf("asr job without snapshot: InputHash %q %v, want empty", h, err)
	}
}

func TestASRZeroSentences(t *testing.T) {
	e := newEnv(t)
	empty := filepath.Join(e.root, "empty.json")
	e.must(os.WriteFile(empty, []byte(`{"prompt_terms_used": 0, "sentences": []}`), 0o644))
	e.cfg.ASR.FakeResult = empty
	l := e.lecture("L1", "m.mp4")
	e.writeWAV(l, "x")
	r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
	if r.err == nil || !sched.IsPermanent(r.err) || codeOf(r.err) != CodeASRNoSpeech || jobErrorCode(r.job) != CodeASRNoSpeech {
		t.Errorf("err %v", r.err)
	}
	if len(e.sentences(l.ID)) != 0 {
		t.Error("sentences written")
	}
}

func TestASRMissingAudio(t *testing.T) {
	e := newEnv(t)
	e.cfg.ASR.FakeResult = fakeASR
	l := e.lecture("L1", "m.mp4")
	stg := e.stage(e.deps(nil), model.StageASR)
	if h, err := stg.InputHash(e.ctx, l.ID); err != nil || h != "" {
		t.Errorf("InputHash without audio: %q %v", h, err)
	}
	if r := e.run(stg, l.ID); r.err == nil || !sched.IsPermanent(r.err) || codeOf(r.err) != CodeASRAudioMissing {
		t.Errorf("err %v", r.err)
	}
}

func TestASRWorkerProtocol(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("ok", fakeASR)
	e.addTerm("cache", "缓存", model.SourceSyllabus, 9)
	l := e.lecture("L1", "m.mp4")
	e.writeWAV(l, "wav")
	// the run parameters come from the job's ASR snapshot, not the settings (A4.1)
	e.setSnapshot(l.ID, model.StageASR, model.MarshalSnapshot(model.ASRSnapshot{Model: "large-v2", ComputeType: "auto", Device: "cuda", Language: "de"}))
	e.cfg.ASR.Model, e.cfg.ASR.ComputeType, e.cfg.ASR.Device, e.cfg.ASR.Language = "tiny", "int8_float16", "cpu", "fr"
	d := e.deps(nil)
	r := e.run(e.stage(d, model.StageASR), l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	dir := model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID)
	var job map[string]any
	b, err := os.ReadFile(filepath.Join(dir, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.must(json.Unmarshal(b, &job))
	absDir, _ := filepath.Abs(dir)
	if job["audio"] != filepath.Join(absDir, model.AudioFile) || job["model"] != "large-v2" || job["compute_type"] != "float16" ||
		job["language"] != "de" || job["initial_prompt"] != "cache" || job["device"] != "cuda" || job["cpu_threads"] != 4.0 {
		t.Errorf("job.json %v", job)
	}
	if r.stats.ASRPrompt != "cache" {
		t.Errorf("asr_prompt %q", r.stats.ASRPrompt)
	}
	if len(e.sentences(l.ID)) != 14 || r.stats.PromptTermsUsed != 3 {
		t.Errorf("sentences %d, stats %+v", len(e.sentences(l.ID)), r.stats)
	}
	if want := []float64{0.1, 0.5, 1}; len(r.progress.vals) < 3 || r.progress.vals[0] != want[0] || r.progress.vals[1] != want[1] {
		t.Errorf("progress %v", r.progress.vals)
	}
	if _, err := os.Stat(filepath.Join(dir, "result.json")); err != nil {
		t.Error("result.json missing")
	}

	// compute_type=auto on a small GPU → int8_float16 (SPEC §17.2)
	e.gpuMB = 8000
	l2 := e.lecture("L2", "m.mp4")
	e.writeWAV(l2, "wav")
	e.setSnapshot(l2.ID, model.StageASR, model.MarshalSnapshot(model.ASRSnapshot{Model: "large-v2", ComputeType: "auto", Device: "cuda", Language: "en"}))
	if r := e.run(e.stage(e.deps(nil), model.StageASR), l2.ID); r.err != nil {
		t.Fatal(r.err)
	}
	b, _ = os.ReadFile(filepath.Join(model.LectureDir(e.cfg.DataDir, l2.CourseID, l2.ID), "job.json"))
	if !strings.Contains(string(b), `"compute_type": "int8_float16"`) {
		t.Errorf("job.json %s", b)
	}

	// device cpu in the snapshot: auto → int8, no cpu_threads (SPEC §17.3)
	l3 := e.lecture("L3", "m.mp4")
	e.writeWAV(l3, "wav")
	e.setSnapshot(l3.ID, model.StageASR, model.MarshalSnapshot(model.ASRSnapshot{Model: "small", ComputeType: "auto", Device: "cpu", Language: "en"}))
	e.cfg.ASR.Device = "cuda"
	if r := e.run(e.stage(e.deps(nil), model.StageASR), l3.ID); r.err != nil {
		t.Fatal(r.err)
	}
	job = nil
	b, _ = os.ReadFile(filepath.Join(model.LectureDir(e.cfg.DataDir, l3.CourseID, l3.ID), "job.json"))
	e.must(json.Unmarshal(b, &job))
	if job["model"] != "small" || job["compute_type"] != "int8" || job["device"] != "cpu" || job["cpu_threads"] != nil {
		t.Errorf("cpu job.json %v", job)
	}
}

// TestStagesNeedSnapshot: a lecture stage whose job has no config_snapshot
// fails permanently with the lead ruling 4 text and never falls back to the
// current settings (no LLM request, no worker run).
func TestStagesNeedSnapshot(t *testing.T) {
	e := newEnv(t)
	e.cfg.ASR.FakeResult = fakeASR
	l := e.draft("never launched", "m.mp4")
	e.writeWAV(l, "RIFF")
	e.insertSample(l.ID)
	e.setSummary(l.ID, "A lecture about caches.")
	d := e.deps(nil)
	for _, stage := range []string{model.StageASR, model.StageSummary, model.StageRepair, model.StageTranslate} {
		r := e.run(e.stage(d, stage), l.ID)
		if r.err == nil || !sched.IsPermanent(r.err) || codeOf(r.err) != CodeSnapshotMissing || detailOf(r.err) != MissingSnapshot {
			t.Errorf("%s: err %v, want permanent %s", stage, r.err, CodeSnapshotMissing)
		}
		if r.job.Status != model.StatusFailed || jobErrorCode(r.job) != CodeSnapshotMissing {
			t.Errorf("%s: status %s", stage, r.job.Status)
		}
	}
	if n := len(e.fake.Requests("")); n != 0 {
		t.Errorf("%d LLM requests without a snapshot", n)
	}
	if _, err := os.Stat(filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), "result.json")); !os.IsNotExist(err) {
		t.Error("asr ran without a snapshot")
	}
	// an unparsable snapshot is as unusable as a missing one
	l2 := e.lecture("broken snapshot", "m.mp4")
	e.insertSample(l2.ID)
	e.setSnapshot(l2.ID, model.StageSummary, `{"provider_id": "deepseek"}`)
	if r := e.run(e.stage(d, model.StageSummary), l2.ID); r.err == nil || codeOf(r.err) != CodeSnapshotMissing {
		t.Errorf("snapshot without model: %v", r.err)
	}
}

func TestASRWorkerExitCodes(t *testing.T) {
	cases := []struct {
		mode      string
		permanent bool
		code      string
		msg       string // in the detail (the worker's stderr)
	}{
		{"exit2", true, workerASR + workerInputError, "ERROR: audio file cannot be decoded"},
		{"exit1", false, workerASR + workerFailed, "ERROR: CUDA out of memory"},
		{"noout", false, workerASR + workerNoOutput, ""},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			e := newEnv(t)
			e.useFakeWorker(c.mode, fakeASR)
			l := e.lecture("L", "m.mp4")
			e.writeWAV(l, "wav")
			r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
			if r.err == nil || sched.IsPermanent(r.err) != c.permanent || codeOf(r.err) != c.code || !strings.Contains(r.err.Error(), c.msg) {
				t.Errorf("err %v (permanent=%v)", r.err, sched.IsPermanent(r.err))
			}
			if r.job.Error == nil || jobErrorCode(r.job) != c.code || !strings.Contains(*r.job.Error, c.msg) {
				t.Errorf("job.error %v", r.job.Error)
			}
			if len(e.sentences(l.ID)) != 0 {
				t.Error("sentences written for a failed run")
			}
		})
	}
}

func TestASRWorkerCancelKills(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("hang", fakeASR)
	l := e.lecture("L", "m.mp4")
	e.writeWAV(l, "wav")
	ctx, cancel := context.WithCancel(e.ctx)
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	r := e.runCtx(ctx, e.stage(e.deps(nil), model.StageASR), l.ID)
	if r.err != context.Canceled {
		t.Errorf("err %v", r.err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("cancel took %v: the worker process was not killed", d)
	}
	if r.job.Status != model.StatusCancelled {
		t.Errorf("status %s", r.job.Status)
	}
}

func TestASRDownloadingStatus(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("download", fakeASR)
	l := e.lecture("L", "m.mp4")
	e.writeWAV(l, "wav")
	dir := model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID)
	stg := e.stage(e.deps(nil), model.StageASR)

	// Drive Run directly to watch the sink while the worker waits.
	rec := &sched.StatsRecorder{}
	j := e.job(model.ScopeLecture, l.ID, model.StageASR)
	ctx := sched.WithStatsSink(sched.WithJob(e.ctx, j), rec)
	done := make(chan error, 1)
	go func() { done <- stg.Run(ctx, l.ID, func(float64) {}) }()
	deadline := time.Now().Add(10 * time.Second)
	for rec.Snapshot().Status != "downloading_model" {
		if time.Now().After(deadline) {
			t.Fatal("stats.status never became downloading_model")
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.must(os.WriteFile(filepath.Join(dir, "go.flag"), nil, 0o644))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s := rec.Snapshot(); s.Status != "" || s.PromptTermsUsed != 3 {
		t.Errorf("stats after run %+v", s)
	}
}

// TestASRSavesWords (SPEC-v2 P4.2): the word timestamps of the worker's
// result are stored with each sentence; sentences without them (v1 results,
// part of the fake file) keep words NULL, and so does an empty list.
func TestASRSavesWords(t *testing.T) {
	e := newEnv(t)
	e.cfg.ASR.FakeResult = fakeASR
	l := e.lecture("L1", "m.mp4")
	e.writeWAV(l, "RIFF")
	if r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	var file struct {
		Sentences []struct {
			Words []model.Word `json:"words"`
		} `json:"sentences"`
	}
	b, err := os.ReadFile(fakeASR)
	if err != nil {
		t.Fatal(err)
	}
	e.must(json.Unmarshal(b, &file))
	ss := e.sentences(l.ID) // read back from the database
	if len(ss) != len(file.Sentences) {
		t.Fatalf("%d sentences, file has %d", len(ss), len(file.Sentences))
	}
	with := 0
	for i, s := range ss {
		want := file.Sentences[i].Words
		if len(want) == 0 {
			if s.Words != nil {
				t.Errorf("sentence %d: words %v, want NULL", i, s.Words)
			}
			continue
		}
		with++
		if !slices.Equal(s.Words, want) {
			t.Errorf("sentence %d: words %v, want %v", i, s.Words, want)
		}
	}
	if with != 5 || ss[0].Words[0] != (model.Word{W: "Okay,", S: 0, E: 440}) {
		t.Errorf("%d sentences with words; first word %+v", with, ss[0].Words)
	}

	// the worker protocol path keeps them too; an empty list is no words
	res := filepath.Join(e.root, "words.json")
	e.must(os.WriteFile(res, []byte(`{"prompt_terms_used": 0, "sentences": [
		{"start_ms": 0, "end_ms": 900, "text": " Hello world.", "low_conf_words": [], "words": [{"w": "Hello", "s": 0, "e": 400}, {"w": "world.", "s": 450, "e": 900}]},
		{"start_ms": 900, "end_ms": 2000, "text": "Second one.", "low_conf_words": [], "words": []}]}`), 0o644))
	e.useFakeWorker("ok", res)
	l2 := e.lecture("L2", "m.mp4")
	e.writeWAV(l2, "RIFF")
	e.cfg.ASR.FakeResult = ""
	if r := e.run(e.stage(e.deps(nil), model.StageASR), l2.ID); r.err != nil {
		t.Fatal(r.err)
	}
	ss = e.sentences(l2.ID)
	if len(ss) != 2 || !slices.Equal(ss[0].Words, []model.Word{{W: "Hello", S: 0, E: 400}, {W: "world.", S: 450, E: 900}}) || ss[1].Words != nil {
		t.Errorf("worker words: %+v", ss)
	}
}
