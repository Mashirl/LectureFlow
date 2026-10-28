// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

func TestIsRecordID(t *testing.T) {
	for name, want := range map[string]bool{
		db.NewID():                   true,
		"01JB2AXYZ0ABCDEFGHJKMNPQRS": true,
		strings.ToLower(db.NewID()):  false, // not how ids are spelled
		"01JB2AXYZ0ABCDEFGHJKMNPQR":  false, // 25 characters
		"01JB2AXYZ0ABCDEFGHJKMNPQRI": false, // I is not Crockford base 32
		"81JB2AXYZ0ABCDEFGHJKMNPQRS": false, // overflows 128 bits
		"export":                     false,
		"":                           false,
	} {
		if got := IsRecordID(name); got != want {
			t.Errorf("IsRecordID(%q) = %v", name, got)
		}
	}
}

func mkfile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// tomb writes the tombstone of lecture id into dir (creating dir).
func tomb(t *testing.T, dir, id string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteTombstone(dir, id, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestTombstone(t *testing.T) {
	dir := t.TempDir()
	id := db.NewID()
	if Tombstoned(dir, id) {
		t.Fatal("tombstoned without a tombstone")
	}
	tomb(t, dir, id)
	if !Tombstoned(dir, id) || Tombstoned(dir, db.NewID()) {
		t.Fatal("the tombstone must name its lecture")
	}
	b, _ := os.ReadFile(filepath.Join(dir, TombstoneFile))
	if !strings.Contains(string(b), `"lecture_id":"`+id+`"`) || !strings.Contains(string(b), `"deleted_at":"`) {
		t.Fatalf("tombstone %s", b)
	}
	if err := RemoveTombstone(dir); err != nil || Tombstoned(dir, id) {
		t.Fatalf("remove: %v", err)
	}
	if err := RemoveTombstone(dir); err != nil {
		t.Fatalf("removing a missing tombstone: %v", err)
	}
	if err := WriteTombstone(filepath.Join(dir, "missing"), id, time.Now()); err == nil {
		t.Fatal("a tombstone in a missing directory")
	}
	// garbage is no tombstone
	if err := os.WriteFile(filepath.Join(dir, TombstoneFile), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Tombstoned(dir, id) {
		t.Fatal("an unreadable tombstone counts")
	}
}

// test-chaos 2 with the lead's ruling: at the start only a lecture
// directory with its tombstone whose lecture the database does not have is
// removed. Everything else stays: a directory without a tombstone even
// when the database has no such lecture (an older backup restored; an
// upload killed before its row), a tombstoned lecture that exists (a newer
// backup restored; its stale tombstone goes), a tombstone naming another
// lecture, tombstones outside lectures/{id} (non-id names, a lower-case id,
// materials, syllabus, backups, anything outside courses/) and a file with
// an id's name.
func TestSweepLectureDirs(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	kept := f.lecture()
	keptDir := model.LectureDir(f.dataDir, kept.CourseID, kept.ID)
	mkfile(t, filepath.Join(keptDir, model.AudioFile))

	gone := f.lecture() // deleted: tombstone, then the row; the directory stayed
	goneDir := model.LectureDir(f.dataDir, gone.CourseID, gone.ID)
	mkfile(t, filepath.Join(goneDir, model.ExportDirName, "x.bilingual.md"))
	tomb(t, goneDir, gone.ID)
	if err := f.st.DeleteLecture(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	// a deleted lecture of a course this database does not know (the
	// course went too, or another backup): the tombstone is enough
	otherCourse := db.NewID()
	goneElsewhere := model.LectureDir(f.dataDir, otherCourse, db.NewID())
	mkfile(t, filepath.Join(goneElsewhere, "media.mp4"))
	tomb(t, goneElsewhere, filepath.Base(goneElsewhere))

	// no tombstone, no row: an older backup restored, or an upload killed
	// before its row
	restored := f.lecture()
	restoredDir := model.LectureDir(f.dataDir, restored.CourseID, restored.ID)
	if err := f.st.DeleteLecture(ctx, restored.ID); err != nil {
		t.Fatal(err)
	}
	crashed := model.LectureDir(f.dataDir, f.course.ID, db.NewID())
	mkfile(t, filepath.Join(crashed, "media.mp4"))

	// a tombstoned lecture the database has (a newer backup restored)
	back := f.lecture()
	backDir := model.LectureDir(f.dataDir, back.CourseID, back.ID)
	tomb(t, backDir, back.ID)

	lectures := filepath.Dir(keptDir)
	wrongName := filepath.Join(lectures, db.NewID()) // its tombstone names another lecture
	tomb(t, wrongName, db.NewID())
	keep := []string{
		keptDir, restoredDir, crashed, backDir, wrongName,
		filepath.Join(lectures, "notes"),                     // not an id
		filepath.Join(lectures, strings.ToLower(db.NewID())), // not an id's spelling
		model.MaterialDir(f.dataDir, f.course.ID, db.NewID()),
		model.SyllabusDir(f.dataDir, f.course.ID),
		filepath.Join(f.dataDir, "backups", "lectures", db.NewID()),
		filepath.Join(f.dataDir, "courses", "lectures", db.NewID()),
		filepath.Join(f.dataDir, db.NewID()),
	}
	for _, d := range keep[5:] {
		tomb(t, d, filepath.Base(d))
	}
	idFile := filepath.Join(lectures, db.NewID()) // a file, not a directory
	mkfile(t, idFile)

	if n := SweepLectureDirs(ctx, f.st, f.dataDir); n != 2 {
		t.Errorf("removed %d, want 2", n)
	}
	for _, d := range []string{goneDir, goneElsewhere} {
		if exists(d) {
			t.Errorf("%s not removed", d)
		}
	}
	for _, p := range append(keep, idFile, filepath.Join(keptDir, model.AudioFile), filepath.Join(restoredDir, "media.mp4"),
		filepath.Join(crashed, "media.mp4"), filepath.Join(backDir, "media.mp4")) {
		if !exists(p) {
			t.Errorf("%s removed", p)
		}
	}
	if exists(filepath.Join(backDir, TombstoneFile)) {
		t.Error("the stale tombstone of an existing lecture stayed")
	}
	if !exists(filepath.Join(wrongName, TombstoneFile)) {
		t.Error("a tombstone naming another lecture was touched")
	}
	// nothing to do the second time; a missing data_dir is fine
	if n := SweepLectureDirs(ctx, f.st, f.dataDir); n != 0 {
		t.Errorf("second sweep removed %d", n)
	}
	if n := SweepLectureDirs(ctx, f.st, filepath.Join(t.TempDir(), "missing")); n != 0 {
		t.Errorf("missing data_dir: %d", n)
	}
}

// Startup recovery sweeps: a lecture deleted by a run that was killed
// between the row and the directory (test-chaos 2, case C) is gone after
// the next start.
func TestRecoverSweepsDeletedLecture(t *testing.T) {
	f := newFixture(t)
	l := f.lecture()
	dir := model.LectureDir(f.dataDir, l.CourseID, l.ID)
	tomb(t, dir, l.ID)
	if err := f.st.DeleteLecture(f.ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	if err := Recover(f.ctx, f.st, f.dataDir); err != nil {
		t.Fatal(err)
	}
	if exists(dir) {
		t.Fatal("the deleted lecture's directory survived the start")
	}
}

// The delete's retries (RemoveDeletedLectureDir): only while the lecture
// does not exist and its tombstone is there; a missing directory is done.
func TestRemoveDeletedLectureDir(t *testing.T) {
	f := newFixture(t)
	l := f.lecture()
	dir := model.LectureDir(f.dataDir, l.CourseID, l.ID)
	tomb(t, dir, l.ID)
	if err := RemoveDeletedLectureDir(f.ctx, f.st, dir, l.ID, true); !errors.Is(err, ErrKeepDir) || !exists(dir) {
		t.Fatalf("lecture exists: %v", err)
	}
	if err := f.st.DeleteLecture(f.ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTombstone(dir); err != nil {
		t.Fatal(err)
	}
	if err := RemoveDeletedLectureDir(f.ctx, f.st, dir, l.ID, true); !errors.Is(err, ErrKeepDir) || !exists(dir) {
		t.Fatalf("tombstone gone: %v", err)
	}
	tomb(t, dir, l.ID)
	if err := RemoveDeletedLectureDir(f.ctx, f.st, dir, l.ID, true); err != nil || exists(dir) {
		t.Fatalf("tombstoned: %v, exists %v", err, exists(dir))
	}
	if err := RemoveDeletedLectureDir(f.ctx, f.st, dir, l.ID, true); err != nil {
		t.Fatalf("already gone: %v", err)
	}
	// no tombstone could be written: the delete's own tries still remove it
	l2 := f.lecture()
	dir2 := model.LectureDir(f.dataDir, l2.CourseID, l2.ID)
	if err := f.st.DeleteLecture(f.ctx, l2.ID); err != nil {
		t.Fatal(err)
	}
	if err := RemoveDeletedLectureDir(f.ctx, f.st, dir2, l2.ID, false); err != nil || exists(dir2) {
		t.Fatalf("untombstoned: %v", err)
	}
}

// A link in place of a lecture directory or of the lectures directory is
// never followed: what it points to stays, tombstone or not.
func TestSweepLectureDirsSkipsLinks(t *testing.T) {
	f := newFixture(t)
	outside := t.TempDir()
	mkfile(t, filepath.Join(outside, "precious.txt"))
	lectures := filepath.Join(model.CourseDir(f.dataDir, f.course.ID), "lectures")
	if err := os.MkdirAll(lectures, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(lectures, db.NewID())
	tomb(t, outside, filepath.Base(link))
	if err := makeDirLink(outside, link); err != nil {
		t.Fatalf("cannot create a directory link here: %v", err)
	}
	SweepLectureDirs(f.ctx, f.st, f.dataDir)
	if !exists(filepath.Join(outside, "precious.txt")) {
		t.Fatal("the sweep removed what a link points to")
	}

	// the lectures directory itself a link
	other := model.Course{ID: db.NewID(), Name: "Linked"}
	if err := f.st.CreateCourse(f.ctx, other); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	id := db.NewID()
	mkfile(t, filepath.Join(target, id, "precious.txt"))
	tomb(t, filepath.Join(target, id), id)
	if err := os.MkdirAll(model.CourseDir(f.dataDir, other.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := makeDirLink(target, filepath.Join(model.CourseDir(f.dataDir, other.ID), "lectures")); err != nil {
		t.Fatal(err)
	}
	if n := SweepLectureDirs(f.ctx, f.st, f.dataDir); n != 0 {
		t.Fatalf("removed %d through a linked lectures directory", n)
	}
	if !exists(filepath.Join(target, id, "precious.txt")) {
		t.Fatal("target changed")
	}
}
