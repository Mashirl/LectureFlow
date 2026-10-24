// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// TestPrepFFmpegClassification: only real input problems are permanent
// ("file damaged / no audio"); output, disk or process problems are
// retryable and never blamed on the file (lead's real-run finding).
func TestPrepFFmpegClassification(t *testing.T) {
	cases := []struct {
		mode      string
		permanent bool
		code      string // appendix D6 class (v2 P14.3)
		want      string // in the detail (ffmpeg's output)
	}{
		{"outfail", false, MediaFailed, ""},
		{"diskfull", false, MediaFailed, "No space left on device"},
		{"corrupt", true, MediaCorrupt, ""},
		{"noaudio", true, MediaNoAudio, ""},
		{"chinese", false, MediaFailed, "最后一行：磁盘已满"},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			e := newEnv(t)
			e.useFakeFFmpeg(c.mode)
			l := e.lecture("L", "m.wav")
			e.must(os.WriteFile(filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), "m.wav"), []byte("RIFF"), 0o644))
			r := e.run(e.stage(e.deps(nil), model.StagePrep), l.ID)
			if r.err == nil || sched.IsPermanent(r.err) != c.permanent {
				t.Fatalf("err %v (permanent=%v)", r.err, sched.IsPermanent(r.err))
			}
			msg := r.err.Error()
			if codeOf(r.err) != c.code || jobErrorCode(r.job) != c.code || !strings.Contains(detailOf(r.err), c.want) {
				t.Errorf("message %q (code %s)", msg, codeOf(r.err))
			}
			if !utf8.ValidString(msg) || r.job.Error == nil || !utf8.ValidString(*r.job.Error) {
				t.Errorf("job.error is not valid UTF-8: %q", msg)
			}
			if _, err := os.Stat(filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), model.AudioFile+".tmp")); !os.IsNotExist(err) {
				t.Error("audio.wav.tmp left behind")
			}
		})
	}
}

// TestPrepAudioErrorClasses (appendix D6; lead ruling at the phase-2 gate):
// a failed audio.wav conversion is classified like the playback file
// (stream.go mediaError): a damaged file, an unsupported format and a file
// without audio are problems of the file itself and permanent (no automatic
// retry); anything else is media.failed and retryable. The raw ffmpeg
// output is the detail, and prep stops before the playback file.
func TestPrepAudioErrorClasses(t *testing.T) {
	e := newEnv(t)
	exit := errors.New("exit status 1")
	cases := []struct {
		name, stderr, code string
		permanent          bool
	}{
		{"corrupt", "[mov,mp4,m4a,3gp,3g2,mj2 @ 0000] moov atom not found\nm.mp4: Invalid data found when processing input", MediaCorrupt, true},
		{"unsupported decoder", "[aist#0:1] Decoder (codec opus) not found for input stream #0:1", MediaUnsupported, true},
		{"unknown decoder", "[aist#0:0 @ 0000] Unknown decoder 'atrac9'", MediaUnsupported, true},
		{"no audio", "[out#0/wav @ 0000] Output file does not contain any stream", MediaNoAudio, true},
		{"disk full", "av_interleaved_write_frame(): No space left on device", MediaFailed, false},
		// a warning about another stream must not make a full disk the file's fault
		{"disk full with a codec warning", "[mov,mp4 @ 0000] Could not find codec parameters for stream 2 (Unknown: none): unknown codec\nav_interleaved_write_frame(): No space left on device", MediaFailed, false},
		{"output not writable", "[out#0/wav @ 0000] Error opening output audio.wav.tmp: Permission denied", MediaFailed, false},
		{"other", "Conversion failed!", MediaFailed, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, dir := e.mediaLecture(c.name)
			fm := &fakeMedia{probe: probeMP4, fail: func(args []string) (string, error) {
				if !isStreamCall(args) {
					return c.stderr, exit
				}
				return "", nil
			}}
			r := e.run(&prepStage{d: e.deps(nil), media: fm}, l.ID)
			if r.err == nil || codeOf(r.err) != c.code || sched.IsPermanent(r.err) != c.permanent || jobErrorCode(r.job) != c.code {
				t.Fatalf("err %v (code %s, permanent %v), job.error %v; want %s permanent=%v",
					r.err, codeOf(r.err), sched.IsPermanent(r.err), ptrStr(r.job.Error), c.code, c.permanent)
			}
			if !strings.Contains(detailOf(r.err), c.stderr) {
				t.Errorf("detail %q lacks the ffmpeg output", detailOf(r.err))
			}
			for _, n := range []string{model.AudioFile, model.AudioFile + ".tmp", model.StreamFile} {
				if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
					t.Errorf("%s exists after a failed conversion", n)
				}
			}
			if n := len(fm.streamCalls()); n != 0 {
				t.Errorf("%d playback file runs after a failed conversion", n)
			}
		})
	}
}

// relDataDir makes the data directory relative to the working directory (as
// the default "./data" is) or skips when that is impossible (other volume).
func relDataDir(t *testing.T, e *env) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, e.cfg.DataDir)
	if err != nil || filepath.IsAbs(rel) {
		t.Skipf("temp dir not reachable by a relative path: %v", err)
	}
	e.cfg.DataDir = rel
}

// TestRelativeDataDirGivesAbsoluteChildPaths: with a relative data_dir every
// path handed to ffmpeg and the Python workers is absolute (the fakes fail
// on a relative output, input, --in, --out, audio or pdf path).
func TestRelativeDataDirGivesAbsoluteChildPaths(t *testing.T) {
	e := newEnv(t)
	relDataDir(t, e)
	e.useFakeFFmpeg("ok")
	e.useFakeWorker("ok", fakeASR)
	l := e.lecture("L", "m.wav")
	e.must(os.WriteFile(filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), "m.wav"), []byte("RIFF"), 0o644))
	d := e.deps(nil)
	r := e.run(e.stage(d, model.StagePrep), l.ID)
	if r.err != nil {
		t.Fatalf("prep: %v", r.err)
	}
	if r.progress.last() != 1 || len(r.progress.vals) < 2 {
		t.Errorf("prep progress %v", r.progress.vals)
	}
	if r := e.run(e.stage(d, model.StageASR), l.ID); r.err != nil {
		t.Fatalf("asr: %v", r.err)
	}
	if len(e.sentences(l.ID)) != 14 {
		t.Error("asr wrote no sentences")
	}

	pages := filepath.Join(e.root, "pages.json")
	e.must(os.WriteFile(pages, []byte(`{"pages": ["Caches"]}`), 0o644))
	e.t.Setenv("LF_FAKE_RESULT", pages)
	m := e.material(nil, "s.pdf", nil)
	mdir := model.MaterialDir(e.cfg.DataDir, m.CourseID, m.ID)
	e.must(os.MkdirAll(mdir, 0o755))
	e.must(os.WriteFile(filepath.Join(mdir, "s.pdf"), []byte("%PDF"), 0o644))
	if r := e.run(e.stage(d, model.StageMaterialExtract), m.ID); r.err != nil {
		t.Fatalf("material_extract: %v", r.err)
	}
}

func TestWorkerStderrTailIsValidUTF8(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("chinese", "")
	l := e.lecture("L", "m.mp4")
	e.writeWAV(l, "wav")
	r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
	if r.err == nil || sched.IsPermanent(r.err) {
		t.Fatalf("err %v", r.err)
	}
	// job.error is the coded error (v2 P14.3); the stderr tail is its detail.
	info, detail := state.ParseJobError(*r.job.Error)
	if info == nil || info.Code != workerASR+workerFailed || !paramsMatch(info, "exit_code", 1) {
		t.Errorf("job.error %q", *r.job.Error)
	}
	if !utf8.ValidString(detail) || !strings.HasSuffix(detail, "最后一行：磁盘已满") {
		t.Errorf("job.error detail %q", detail)
	}
	if strings.Contains(detail, "�") {
		t.Error("the cut produced a replacement character instead of moving to a rune boundary")
	}
}

func TestUTF8Truncation(t *testing.T) {
	s := strings.Repeat("中文", 700) + "\n结束" // 4206 bytes, 3-byte runes
	for _, n := range []int{2048, 2047, 2046, 10, 1, 0} {
		got := utf8Tail([]byte(s), n)
		if !utf8.ValidString(got) || len(got) > n || strings.ContainsRune(got, utf8.RuneError) {
			t.Errorf("utf8Tail(n=%d) = %q (len %d)", n, got, len(got))
		}
		if n >= 10 && !strings.HasSuffix(got, "结束") {
			t.Errorf("utf8Tail(n=%d) lost the end: %q", n, got)
		}
	}
	// invalid bytes from a non-UTF-8 console are replaced, never passed on
	if got := utf8Tail([]byte("ok \xc3\x28 \xff\xfe end"), 100); !utf8.ValidString(got) || !strings.HasSuffix(got, "end") {
		t.Errorf("invalid input: %q", got)
	}
	// tailBuffer fed in chunks that split runes
	tb := &tailBuffer{n: stderrTailBytes}
	b := []byte(s)
	for i := 0; i < len(b); i += 7 {
		tb.Write(b[i:min(i+7, len(b))])
	}
	got := tb.String()
	if !utf8.ValidString(got) || len(got) > stderrTailBytes || !strings.HasSuffix(got, "结束") || strings.ContainsRune(got, utf8.RuneError) {
		t.Errorf("tailBuffer: %q (len %d)", got, len(got))
	}
}
