// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"path/filepath"
	"testing"
)

// The connection keeps the three pragmas of SPEC §3.3 and gets the 32 MiB
// page cache (perf-fix), on its only connection.
func TestConnectionPragmas(t *testing.T) {
	d, err := OpenDB(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var mode string
	var busy, fk, cache int
	for _, q := range []struct {
		sql string
		dst any
	}{
		{"PRAGMA journal_mode", &mode}, {"PRAGMA busy_timeout", &busy}, {"PRAGMA foreign_keys", &fk}, {"PRAGMA cache_size", &cache},
	} {
		if err := d.QueryRow(q.sql).Scan(q.dst); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	if mode != "wal" || busy != 5000 || fk != 1 || cache != -32768 {
		t.Fatalf("journal_mode %q busy_timeout %d foreign_keys %d cache_size %d", mode, busy, fk, cache)
	}
	if n := d.Stats().MaxOpenConnections; n != 1 {
		t.Fatalf("max open connections %d, want 1 (SPEC §3.3)", n)
	}
}
