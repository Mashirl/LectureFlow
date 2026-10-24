// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

// Secrets in kv that must not outlive their deletion (v3 Gate round 1
// item 9: the Colab credential's refresh_token and client_secret). SQLite
// keeps deleted or replaced content in free pages, in the unused space of
// pages and in old WAL frames; a plain UPDATE or DELETE leaves it readable
// in the file. Here the write runs with PRAGMA secure_delete=ON, then
// VACUUM rebuilds every page from the live rows only, then a TRUNCATE
// checkpoint copies the WAL into the database and cuts the WAL to zero
// bytes. NOTES "决定（v3 gate-fix-core）".

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SetSecretKV writes kv key = value, or deletes key when value is "", so
// that the previous value leaves no copy in the database file or its WAL.
// It is not part of the Store interface (frozen); callers reach it with a
// type assertion (colab.SecretKV). It is rare and slow (a VACUUM of the
// whole database), so only secrets go through it. Inside a transaction
// (a Store from Tx) it is refused: VACUUM cannot run there.
func (s *sqlStore) SetSecretKV(ctx context.Context, key, value string) error {
	if s.tx != nil {
		return errors.New("db: SetSecretKV inside a transaction")
	}
	return withSecureConn(ctx, s.db, func(ctx context.Context, c *sql.Conn) error {
		var err error
		if value == "" {
			_, err = c.ExecContext(ctx, `DELETE FROM kv WHERE key = ?`, key)
		} else {
			_, err = c.ExecContext(ctx, `INSERT INTO kv (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
		}
		return err
	})
}

// ScrubKV deletes keys from the kv table of an opened database d the same
// way (relocate: the database an import replaced, kept in backups/). A
// database without a kv table is left as it is.
func ScrubKV(ctx context.Context, d *sql.DB, keys ...string) error {
	return withSecureConn(ctx, d, func(ctx context.Context, c *sql.Conn) error {
		var n int
		if err := c.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'kv'`).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		for _, k := range keys {
			if _, err := c.ExecContext(ctx, `DELETE FROM kv WHERE key = ?`, k); err != nil {
				return err
			}
		}
		return nil
	})
}

// withSecureConn runs write on one connection of d with secure_delete on
// (restored afterwards), then VACUUM and a TRUNCATE checkpoint.
func withSecureConn(ctx context.Context, d *sql.DB, write func(context.Context, *sql.Conn) error) error {
	c, err := d.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	var prev int
	if err := c.QueryRowContext(ctx, "PRAGMA secure_delete").Scan(&prev); err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, "PRAGMA secure_delete = 1"); err != nil {
		return err
	}
	defer c.ExecContext(context.WithoutCancel(ctx), fmt.Sprintf("PRAGMA secure_delete = %d", prev))
	if err := write(ctx, c); err != nil {
		return err
	}
	// VACUUM writes a fresh copy of the live rows: no free page, no unused
	// space of a page can still hold the old value.
	if _, err := c.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("db: vacuum after deleting a secret: %w", err)
	}
	return checkpointTruncate(ctx, c)
}

// checkpointTruncate copies the WAL into the database and truncates it to
// zero bytes (old frames hold earlier versions of the pages). Another
// connection reading at the same time keeps the WAL busy: tried a few
// times, then an error (the WAL is truncated by the next checkpoint or
// deleted when the last connection closes). Not in WAL mode it does
// nothing.
func checkpointTruncate(ctx context.Context, c *sql.Conn) error {
	for attempt := 0; ; attempt++ {
		var busy, logFrames, done int
		if err := c.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &done); err != nil {
			return err
		}
		if busy == 0 {
			return nil
		}
		if attempt == 4 {
			return errors.New("db: the WAL could not be truncated: another connection is reading the database")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
		}
	}
}
