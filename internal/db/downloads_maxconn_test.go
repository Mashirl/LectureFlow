// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"path/filepath"
	"testing"
)

// Migration 012: download_task.max_connections, the profile's connection
// limit; rows of 011 read 0 (no limit).
func TestMigration012MaxConnections(t *testing.T) {
	ctx := context.Background()
	d, err := openRaw(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := migrateUpTo(ctx, d, 11); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO course (id, name, created_at) VALUES ('c1','CSC1003','2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO download_task (platform, session_id, profile_id, course_id, title, tracks_json,
		state, created_at, updated_at) VALUES ('panopto','old','panopto','c1','x','[]','failed','t','t')`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, d); err != nil {
		t.Fatal(err)
	}
	st := New(d).(*sqlStore)
	old, err := st.FindDownloadTask(ctx, "panopto", "old")
	if err != nil || old.MaxConnections != 0 {
		t.Fatalf("a row of 011: %+v %v", old, err)
	}
	in := sampleTask("c1", "s1")
	in.MaxConnections = 2
	id, err := st.InsertDownloadTask(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetDownloadTask(ctx, id)
	if got.MaxConnections != 2 {
		t.Fatalf("inserted %d", got.MaxConnections)
	}
	got.MaxConnections = 3
	if err := st.UpdateDownloadTask(ctx, got); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.GetDownloadTask(ctx, id); got.MaxConnections != 3 {
		t.Fatalf("updated %d", got.MaxConnections)
	}
}
