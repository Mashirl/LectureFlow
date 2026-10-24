// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestMigrateCreatesTables(t *testing.T) {
	d, err := OpenDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, tbl := range []string{"course", "syllabus", "lecture", "material", "term", "sentence", "patch", "summary", "job", "kv"} {
		var n int
		if err := d.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", tbl).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s missing (err=%v)", tbl, err)
		}
	}
	var mode string
	d.QueryRow("PRAGMA journal_mode").Scan(&mode)
	if mode != "wal" {
		t.Fatalf("journal_mode = %q", mode)
	}
	var fk int
	d.QueryRow("PRAGMA foreign_keys").Scan(&fk)
	if fk != 1 {
		t.Fatal("foreign_keys off")
	}
	// Re-running is a no-op.
	if err := Migrate(context.Background(), d); err != nil {
		t.Fatal(err)
	}
}
