// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/wav"
)

// SPEC-v3 V4.3 (v5-integration-a): without a lecture that gives a clip,
// the self-test uses the whole bundled sample (fake worker, real ffmpeg —
// skipped without ffmpeg as SPEC §14 allows).

func (e *env) useRepoSample() string {
	e.t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "assets", "sample"))
	if err != nil {
		e.t.Fatal(err)
	}
	e.cfg.SampleDir = dir
	return dir
}

func TestDeviceTestBundledSample(t *testing.T) {
	e := newEnv(t)
	ffmpegOrSkip(t, e.cfg)
	e.useFakeWorker("selftest", "")
	log := e.jobLog()
	_, ref, ok := devices.BundledClip(e.useRepoSample())
	if !ok {
		t.Fatal("assets/sample missing")
	}
	e.selfTestScript(map[string]fakeCandidate{
		"large-v3-turbo/fp16": {Text: ref, TranscribeS: 10},
		"large-v3-turbo/int8": {Text: dropWords(ref, 4), TranscribeS: 8},
	})
	clip := filepath.Join(e.absDataDir(), "selftest", "ov_GPU", model.AudioFile)
	var clipMS int64
	var clipOK bool
	te := &testEnv{before: func(ctx context.Context, n int) error {
		if n == 1 { // the decoded clip exists while the test runs
			clipMS, clipOK = wav.DurationMS(clip)
		}
		return ctx.Err()
	}}
	b, err := e.deps(nil).DeviceTest(e.ctx, gpuDevice, te.env())
	if err != nil || b.Status != model.BenchmarkDone {
		t.Fatalf("%+v %v", b, err)
	}
	if !clipOK || clipMS < 65000 || clipMS > 66000 {
		t.Fatalf("decoded clip %d ms (%v)", clipMS, clipOK)
	}
	want := model.BenchmarkAudio{Source: model.BenchmarkSourceBundled, StartMS: 0, EndMS: clipMS, ReferenceWords: 174}
	if b.Audio != want {
		t.Errorf("audio %+v, want %+v", b.Audio, want)
	}
	if len(b.Results) != 2 || b.Results[0].WER == nil || *b.Results[0].WER != 0 || b.Results[1].WER == nil || *b.Results[1].WER == 0 {
		t.Errorf("results %+v", b.Results)
	}
	for i, j := range fakeJobs(t, log) {
		if j.Job["audio"] != clip || j.Job["language"] != "en" || j.Job["initial_prompt"] != "" {
			t.Errorf("run %d: %v", i, j.Job)
		}
	}
	if _, err := os.Stat(clip); !os.IsNotExist(err) {
		t.Error("the decoded sample was not removed")
	}
	// The sample itself is untouched.
	if _, _, ok := devices.BundledClip(e.cfg.SampleDir); !ok {
		t.Error("sample gone")
	}
}

// A lecture without a usable clip falls back to the sample too.
func TestDeviceTestBundledWhenNoClip(t *testing.T) {
	e := newEnv(t)
	ffmpegOrSkip(t, e.cfg)
	e.useFakeWorker("selftest", "")
	e.useRepoSample()
	l := e.lecture("One sentence", "m.mp4")
	writePCM(t, filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), model.AudioFile), 600)
	e.insertSentences(l.ID, []model.Sentence{{LectureID: l.ID, Idx: 0, StartMS: 0, EndMS: 600000, ASRText: "x", RepairedText: "x", ZhStatus: model.ZhNone}})
	e.markDone(model.ScopeLecture, l.ID, model.StageRepair)
	e.selfTestScript(map[string]fakeCandidate{"large-v3-turbo/fp16": {Text: "x", TranscribeS: 1}, "large-v3-turbo/int8": {Text: "x", TranscribeS: 1}})
	b, err := e.deps(nil).DeviceTest(e.ctx, gpuDevice, (&testEnv{}).env())
	if err != nil || b.Status != model.BenchmarkDone || b.Audio.Source != model.BenchmarkSourceBundled || b.Audio.LectureID != "" {
		t.Fatalf("%+v %v", b, err)
	}
}

// Neither a lecture nor the sample: selftest.no_audio as before, no worker,
// nothing left in the self-test directory.
func TestDeviceTestBundledMissing(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("selftest", "")
	log := e.jobLog()
	e.cfg.SampleDir = t.TempDir() // no sample-lecture.flac
	b, err := e.deps(nil).DeviceTest(e.ctx, gpuDevice, (&testEnv{}).env())
	if err != nil || b.Status != model.BenchmarkFailed || infoCode(b.Error) != CodeSelfTestNoAudio || len(b.Results) != 0 {
		t.Fatalf("%+v %v", b, err)
	}
	if !strings.Contains(b.Error.Detail, "示例音频") {
		t.Errorf("detail %q", b.Error.Detail)
	}
	if n := len(fakeJobs(t, log)); n != 0 {
		t.Errorf("%d worker runs", n)
	}
}

// A failed decode returns the error and leaves no partial clip.
func TestDeviceTestBundledDecodeFails(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("selftest", "")
	e.useRepoSample()
	e.useFakeFFmpeg("corrupt")
	_, err := e.deps(nil).DeviceTest(e.ctx, gpuDevice, (&testEnv{}).env())
	if err == nil || !strings.Contains(err.Error(), "bundled sample") {
		t.Fatalf("err %v", err)
	}
	clip := filepath.Join(e.absDataDir(), "selftest", "ov_GPU", model.AudioFile)
	if _, err := os.Stat(clip); !os.IsNotExist(err) {
		t.Error("partial clip left behind")
	}
}
