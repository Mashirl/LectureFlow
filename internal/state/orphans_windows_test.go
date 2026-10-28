// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package state

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"lectureflow/internal/model"
)

// makeDirLink makes link a directory junction to target (no privilege
// needed, unlike a symbolic link).
func makeDirLink(target, link string) error {
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mklink /J: %v: %s", err, out)
	}
	return nil
}

// holdOpen opens path the way Go's os.Open and most programs do (no
// FILE_SHARE_DELETE): Windows then refuses to delete it.
func holdOpen(t *testing.T, path string) func() {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	done := false
	release := func() {
		if !done {
			done = true
			f.Close()
		}
	}
	t.Cleanup(release)
	return release
}

// test-chaos 1: a .tmp that an orphaned ffmpeg still holds open does not
// stop the start: Recover succeeds (the jobs are recovered), the other
// temporary files are removed and the held one is left; the next start
// removes it once it is closed.
func TestRecoverWithHeldTmp(t *testing.T) {
	f := newFixture(t)
	l := f.lecture()
	f.drive(model.ScopeLecture, l.ID, model.StagePrep, model.StatusQueued, model.StatusRunning)
	dir := model.LectureDir(f.dataDir, l.CourseID, l.ID)
	held := filepath.Join(dir, "audio.wav.tmp")
	other := filepath.Join(dir, model.ExportDirName, "x.vtt.tmp")
	mkfile(t, held)
	mkfile(t, other)
	release := holdOpen(t, held)
	if err := os.Remove(held); err == nil {
		t.Fatal("setup: the held file could be removed")
	}

	if err := Recover(f.ctx, f.st, f.dataDir); err != nil {
		t.Fatalf("Recover with a held .tmp: %v", err)
	}
	if j := f.lj(l.ID, model.StagePrep); j.Status != model.StatusQueued || j.Attempt != 1 {
		t.Fatalf("prep not recovered: %+v", j)
	}
	if exists(other) || !exists(held) {
		t.Fatalf("other removed %v, held kept %v", !exists(other), exists(held))
	}
	release()
	if err := Recover(f.ctx, f.st, f.dataDir); err != nil {
		t.Fatal(err)
	}
	if exists(held) {
		t.Fatal("the released .tmp survived the next start")
	}
}

// test-chaos 2, cases A and B: a file of a deleted lecture still open
// (a player, an external program) keeps its directory, and its tombstone
// stays with it (removed last); the sweep goes on and removes the
// directory at a later start.
func TestSweepLectureDirsHeldFile(t *testing.T) {
	f := newFixture(t)
	l := f.lecture()
	dir := model.LectureDir(f.dataDir, l.CourseID, l.ID)
	mkfile(t, filepath.Join(dir, model.AudioFile))
	tomb(t, dir, l.ID)
	if err := f.st.DeleteLecture(f.ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	release := holdOpen(t, filepath.Join(dir, "media.mp4"))
	if err := Recover(f.ctx, f.st, f.dataDir); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, "media.mp4")) || !Tombstoned(dir, l.ID) {
		t.Fatal("the held file or the tombstone is gone")
	}
	if exists(filepath.Join(dir, model.AudioFile)) {
		t.Fatal("what could be removed was not")
	}
	release()
	if n := SweepLectureDirs(f.ctx, f.st, f.dataDir); n != 1 || exists(dir) {
		t.Fatalf("after release: removed %d, dir exists %v", n, exists(dir))
	}
}
