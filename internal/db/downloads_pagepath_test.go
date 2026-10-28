// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"path/filepath"
	"testing"
)

// Migration 013 (security review L3): the page paths stored before are
// cleared, the hosts stay; new and updated rows never get one.
func TestMigration013PagePath(t *testing.T) {
	ctx := context.Background()
	d, err := openRaw(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := migrateUpTo(ctx, d, 12); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO course (id, name, created_at) VALUES ('c1','CSC1003','2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO download_task (platform, session_id, profile_id, course_id, title, page_host,
		page_path, tracks_json, state, created_at, updated_at) VALUES ('panopto','old','panopto','c1','x','lms.example.edu',
		'/Panopto/Pages/Viewer.aspx','[]','done','t','t')`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, d); err != nil {
		t.Fatal(err)
	}
	var v int
	d.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v < 13 {
		t.Fatalf("user_version = %d, want >= 13", v)
	}
	pagePath := func(session string) (host, path string) {
		t.Helper()
		if err := d.QueryRowContext(ctx, `SELECT page_host, page_path FROM download_task WHERE session_id = ?`, session).Scan(&host, &path); err != nil {
			t.Fatal(err)
		}
		return
	}
	if host, path := pagePath("old"); host != "lms.example.edu" || path != "" {
		t.Fatalf("a row of 012: host %q path %q", host, path)
	}
	st := New(d).(*sqlStore)
	id, err := st.InsertDownloadTask(ctx, sampleTask("c1", "s1"))
	if err != nil {
		t.Fatal(err)
	}
	if host, path := pagePath("s1"); host != "x.example.edu" || path != "" {
		t.Fatalf("inserted: host %q path %q", host, path)
	}
	if _, err := d.ExecContext(ctx, `UPDATE download_task SET page_path = '/left/over' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetDownloadTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateDownloadTask(ctx, got); err != nil {
		t.Fatal(err)
	}
	if _, path := pagePath("s1"); path != "" {
		t.Fatalf("updated: path %q", path)
	}
}
