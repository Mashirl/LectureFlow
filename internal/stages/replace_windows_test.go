// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package stages

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// hold opens path the way most programs (and Go's os.Open) do: sharing
// reading and writing, not deletion. Windows then refuses to remove,
// rename or replace it. The returned func closes it (also at cleanup).
func hold(t *testing.T, path string) func() {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	release := func() {
		if !closed {
			closed = true
			f.Close()
		}
	}
	t.Cleanup(release)
	return release
}

// holdShareDelete opens path allowing deletion (FILE_SHARE_DELETE), as
// some players and viewers do.
func holdShareDelete(t *testing.T, path string) windows.Handle {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { windows.CloseHandle(h) })
	return h
}

// test-chaos 1: a .tmp that another program (an orphaned ffmpeg) still
// holds is never reused: the next free name is handed out, and the held
// file is left alone.
func TestTmpForSkipsHeldTmp(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "audio.wav")
	write(t, out+".tmp", "the orphan's half-written file")
	release := hold(t, out+".tmp")
	tmp, err := tmpFor(out)
	if err != nil || tmp != out+".1.tmp" {
		t.Fatalf("tmpFor = %q, %v; want audio.wav.1.tmp", tmp, err)
	}
	if b, err := os.ReadFile(out + ".tmp"); err != nil || string(b) != "the orphan's half-written file" {
		t.Fatalf("held file: %q %v", b, err)
	}
	// every name held: a fileInUseError naming the product
	for i := 1; i < tmpNames; i++ {
		p := tmp
		if i > 1 {
			p = strings.Replace(tmp, ".1.tmp", "."+string(rune('0'+i))+".tmp", 1)
		}
		write(t, p, "x")
		hold(t, p)
	}
	_, err = tmpFor(out)
	if fe, ok := asFileInUse(err); !ok || fe.name != "audio.wav" {
		t.Fatalf("all names held: %v", err)
	}
	release()
	if tmp, err := tmpFor(out); err != nil || tmp != out+".tmp" {
		t.Fatalf("after release: %q, %v", tmp, err)
	}
}

// test-chaos 3: one final held open stops the replacement before anything
// changed: every old file (also the obsolete one) is as it was, nothing is
// left moved aside, and the error names the held file only.
func TestReplaceFilesHeldFinal(t *testing.T) {
	dir := t.TempDir()
	finals := []string{filepath.Join(dir, "a.md"), filepath.Join(dir, "b.vtt"), filepath.Join(dir, "c.txt")}
	for _, f := range finals {
		write(t, f, "old "+filepath.Base(f))
	}
	obsolete := filepath.Join(dir, "Old title.md")
	write(t, obsolete, "old name")
	var pairs []filePair
	for _, f := range finals {
		tmp, err := tmpFor(f)
		if err != nil {
			t.Fatal(err)
		}
		write(t, tmp, "new "+filepath.Base(f))
		pairs = append(pairs, filePair{tmp: tmp, final: f})
	}
	before := readAll(t, dir)
	for _, held := range []string{finals[1], obsolete} {
		release := hold(t, held)
		err := replaceFiles(pairs, []string{obsolete})
		fe, ok := asFileInUse(err)
		if !ok || fe.name != filepath.Base(held) || strings.Contains(fe.Error(), dir) {
			t.Fatalf("held %s: %v", held, err)
		}
		if got := readAll(t, dir); names(got) != names(before) {
			t.Fatalf("held %s: files %s, want %s", held, names(got), names(before))
		} else {
			for k, v := range before {
				if got[k] != v {
					t.Fatalf("held %s: %s = %q, want %q", held, k, got[k], v)
				}
			}
		}
		release()
	}
	if err := replaceFiles(pairs, []string{obsolete}); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, dir); names(got) != "a.md,b.vtt,c.txt" || got["b.vtt"] != "new b.vtt" {
		t.Fatalf("after release: %v", got)
	}
}

// A program that allows deletion does not stop the replacement: it keeps
// reading the old file.
func TestReplaceFileHeldWithShareDelete(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "stream.mp4")
	write(t, out, "old")
	h := holdShareDelete(t, out)
	write(t, out+".tmp", "new")
	if err := replaceFile(out+".tmp", out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != "new" {
		t.Fatalf("stream.mp4 = %q", b)
	}
	buf := make([]byte, 8)
	var n uint32
	if err := windows.ReadFile(h, buf, &n, nil); err != nil || string(buf[:n]) != "old" {
		t.Fatalf("the reader reads %q %v, want the old file", buf[:n], err)
	}
}

// test-chaos 3: an export file open in another program (Word, a download
// in progress) fails the export with export.file_in_use {name}, retryable,
// and the previous export stays complete and unchanged; once the file is
// closed the export goes through.
func TestExportFileInUse(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil)
	l := e.lecture("Stream Ciphers", "a.mp4")
	e.insertSentences(l.ID, []model.Sentence{{LectureID: l.ID, Idx: 0, StartMS: 0, EndMS: 1000, ASRText: "Hi.", RepairedText: "Hi.", ZhStatus: model.ZhNone}})
	export := func() error { return (&exportStage{d: d}).Run(e.ctx, l.ID, func(float64) {}) }
	e.must(export())
	dir := filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), model.ExportDirName)
	before := readAll(t, dir)
	if len(before) != len(ExportSuffixes) {
		t.Fatalf("first export: %v", names(before))
	}
	zh := "你好。"
	e.must(e.st.SetTranslation(e.ctx, l.ID, 0, db.SentenceTr{Zh: &zh, ZhStatus: model.ZhOK}))
	md := e.exportNames(l)[0]
	release := hold(t, filepath.Join(dir, md))

	err := export()
	if codeOf(err) != CodeExportFileInUse || sched.IsPermanent(err) {
		t.Fatalf("export with %s held: %v", md, err)
	}
	if info := errInfo(err); info.Params["name"] != md || strings.Contains(detailOf(err), dir) {
		t.Fatalf("params %v, detail %q", info.Params, detailOf(err))
	}
	got := readAll(t, dir)
	if names(got) != names(before) {
		t.Fatalf("export dir after the failure: %s, want %s (no .tmp left, nothing removed)", names(got), names(before))
	}
	for k, v := range before {
		if got[k] != v {
			t.Errorf("%s changed although the export failed", k)
		}
	}
	release()
	e.must(export())
	after := readAll(t, dir)
	if names(after) != names(before) || !strings.Contains(after[md], "你好。") {
		t.Fatalf("after release: %s\n%s", names(after), after[md])
	}
}

// test-chaos 1 + 3 for prep: an audio.wav.tmp still held by an orphaned
// ffmpeg is skipped (audio.wav.1.tmp is written instead), and an
// audio.wav that a program has open fails the rerun with prep.file_in_use
// {name: audio.wav} and stays as it was.
func TestPrepFilesInUse(t *testing.T) {
	e := newEnv(t)
	l, dir := e.mediaLecture("held")
	wav := filepath.Join(dir, model.AudioFile)
	write(t, wav+".tmp", "the orphan's half-written file")
	release := hold(t, wav+".tmp")
	fm := &fakeMedia{probe: probeM4A}
	prep := func() error { return (&prepStage{d: e.deps(nil), media: fm}).Run(e.ctx, l.ID, func(float64) {}) }
	e.must(prep())
	calls := fm.ffmpegCalls()
	if out := calls[0][len(calls[0])-1]; !strings.HasSuffix(out, "audio.wav.1.tmp") {
		t.Fatalf("ffmpeg wrote %s, want audio.wav.1.tmp", out)
	}
	if fi, err := os.Stat(wav); err != nil || fi.Size() != 1000 {
		t.Fatalf("audio.wav: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(wav + ".tmp"); string(b) != "the orphan's half-written file" {
		t.Fatalf("held tmp changed: %q", b)
	}
	release()

	write(t, wav, "the audio being played")
	release = hold(t, wav)
	err := prep()
	if codeOf(err) != CodePrepFileInUse || sched.IsPermanent(err) || errInfo(err).Params["name"] != model.AudioFile {
		t.Fatalf("prep with audio.wav held: %v", err)
	}
	if b, _ := os.ReadFile(wav); string(b) != "the audio being played" {
		t.Fatalf("audio.wav changed: %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
	release()
	e.must(prep())
	if fi, err := os.Stat(wav); err != nil || fi.Size() != 1000 {
		t.Fatalf("audio.wav after release: %v %v", fi, err)
	}
}

// test-chaos 1 for the Python workers: a result.json.tmp still held by a
// worker that outlived an earlier LectureFlow is not reused; the worker
// writes result.json.1.tmp and the run completes.
func TestWorkerSkipsHeldTmp(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("ok", fakeASR)
	l := e.lecture("L1", "m.mp4")
	e.writeWAV(l, "wav")
	dir := model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID)
	held := filepath.Join(dir, "result.json.tmp")
	write(t, held, "{ the orphan's half")
	hold(t, held)
	if r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if len(e.sentences(l.ID)) != 14 {
		t.Fatalf("sentences %d", len(e.sentences(l.ID)))
	}
	if b, _ := os.ReadFile(held); string(b) != "{ the orphan's half" {
		t.Fatalf("held tmp changed: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "result.json.1.tmp")); !os.IsNotExist(err) {
		t.Fatalf("result.json.1.tmp left behind: %v", err)
	}
}

// A stream.mp4 being played (held) is a prep.file_in_use failure of the
// playback file, recorded with its name only; the old file stays.
func TestStreamFileInUse(t *testing.T) {
	e := newEnv(t)
	l, dir := e.mediaLecture("playing")
	out := filepath.Join(dir, model.StreamFile)
	write(t, out, "the playback file being played")
	hold(t, out)
	fm := &fakeMedia{probe: probeMP4} // a remux, done by prep
	e.must((&prepStage{d: e.deps(nil), media: fm}).Run(e.ctx, l.ID, func(float64) {}))
	st := e.streamState(l.ID)
	if info := streamError(t, st); st.Status != StreamFailed || info.Code != CodePrepFileInUse || info.Params["name"] != model.StreamFile {
		t.Fatalf("stream state %+v", st)
	}
	if b, _ := os.ReadFile(out); string(b) != "the playback file being played" {
		t.Fatalf("stream.mp4 changed: %q", b)
	}
}
