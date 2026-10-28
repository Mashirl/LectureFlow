// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// OpenDB opens the SQLite database with the connection parameters of SPEC §3.3
// and applies pending migrations.
func OpenDB(path string) (*sql.DB, error) {
	d, err := openRaw(path)
	if err != nil {
		return nil, err
	}
	if err := Migrate(context.Background(), d); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// connPragmas are the connection parameters: the three of SPEC §3.3 and
// (perf-fix, NOTES.md) a 32 MiB page cache instead of SQLite's 2 MB, so the
// lecture lists' reads stay in memory (the sentence rows with their word
// timestamps are several MB). Still one connection (SetMaxOpenConns).
const connPragmas = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)" +
	"&_pragma=cache_size(-32768)"

// openRaw opens the database with the SPEC §3.3 parameters, without migrating.
func openRaw(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := "file:" + (&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath() + "?" + connPragmas
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(1)
	return d, nil
}

// Migrate applies migrations/NNN_*.sql in order, tracking the applied
// number in PRAGMA user_version. A database whose user_version is above
// SchemaVersion (written by a newer LectureFlow) is refused with
// *NewerSchemaError and left as it is (test-upgrade F2).
func Migrate(ctx context.Context, d *sql.DB) error {
	return migrateUpTo(ctx, d, 0)
}

// migrateUpTo is Migrate stopping after migration upTo (0 = all); the
// upgrade tests build a database of an older version with it.
func migrateUpTo(ctx context.Context, d *sql.DB, upTo int) error {
	var cur int
	if err := d.QueryRowContext(ctx, "PRAGMA user_version").Scan(&cur); err != nil {
		return err
	}
	if known := SchemaVersion(); cur > known {
		return &NewerSchemaError{Found: cur, Known: known}
	}
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		n, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("bad migration name %s", name)
		}
		if n <= cur {
			continue
		}
		if upTo > 0 && n > upTo {
			break
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", n)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		cur = n
	}
	return nil
}
