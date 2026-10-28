// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package sched

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// heldLectureDir makes the directory of draft l with a file that is held
// open (Go's os.Open, like most programs, does not allow deletion).
func heldLectureDir(t *testing.T, e *env, l model.Lecture, name string) (dir string, release func()) {
	t.Helper()
	dir = model.LectureDir(e.dataDir, l.CourseID, l.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, model.AudioFile), []byte("wav"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return dir, func() { f.Close() }
}

func shortRetries(o *Options) {
	o.RemoveRetry = []time.Duration{20 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, time.Second, 2 * time.Second}
}

// test-chaos 2: a file of the deleted lecture is still open (the player
// streaming stream.mp4), so its directory cannot be removed at the delete.
// The delete succeeds (rows first, §4.6), the directory keeps its tombstone
// (written before the rows were deleted, removed last), and a later try
// removes it once the file is closed.
func TestDeleteLectureRetriesHeldDirectory(t *testing.T) {
	e := newEnvOpts(t, nil, shortRetries)
	l := e.draft()
	ldir, release := heldLectureDir(t, e, l, model.StreamFile)
	e.start()

	if err := e.s.DeleteLecture(e.ctx, l.ID, false); err != nil {
		t.Fatalf("delete with a held file: %v", err)
	}
	if _, err := e.st.GetLecture(e.ctx, l.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("lecture row: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if !exists(filepath.Join(ldir, model.StreamFile)) || !state.Tombstoned(ldir, l.ID) {
		t.Fatal("setup: the held file or the tombstone is gone")
	}
	if exists(filepath.Join(ldir, model.AudioFile)) {
		t.Fatal("what could be removed was not")
	}
	release() // the player stops
	deadline := time.Now().Add(10 * time.Second)
	for exists(ldir) {
		if time.Now().After(deadline) {
			t.Fatal("the directory was not removed after the file was closed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The tries stop when the lecture exists again (a database restored meanwhile):
// its directory and files stay.
func TestDeleteLectureRetryKeepsLectureThatCameBack(t *testing.T) {
	e := newEnvOpts(t, nil, shortRetries)
	l := e.draft()
	ldir, release := heldLectureDir(t, e, l, "media.mp4")
	e.start()
	if err := e.s.DeleteLecture(e.ctx, l.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := e.st.CreateLecture(e.ctx, l); err != nil { // back, same id
		t.Fatal(err)
	}
	release()
	time.Sleep(4 * time.Second) // longer than every retry together
	if !exists(filepath.Join(ldir, "media.mp4")) {
		t.Fatal("the directory of a lecture that exists was removed")
	}
}

// A scheduler that is not running leaves the directory, with its
// tombstone, to the next start's sweep (no goroutine outlives it).
func TestDeleteLectureHeldDirectoryNotStarted(t *testing.T) {
	e := newEnvOpts(t, nil, func(o *Options) { o.RemoveRetry = []time.Duration{time.Millisecond} })
	l := e.draft()
	ldir, release := heldLectureDir(t, e, l, "media.mp4")
	if err := e.s.DeleteLecture(e.ctx, l.ID, false); err != nil {
		t.Fatal(err)
	}
	release()
	time.Sleep(50 * time.Millisecond)
	if !exists(filepath.Join(ldir, "media.mp4")) {
		t.Fatal("removed without a running scheduler")
	}
	if n := state.SweepLectureDirs(e.ctx, e.st, e.dataDir); n != 1 || exists(ldir) {
		t.Fatalf("next start's sweep: %d, exists %v", n, exists(ldir))
	}
}
