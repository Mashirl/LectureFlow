// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

// Credentials sealed at rest (cred-enc, user decision 2026-10-27; security
// review 2 "数据库明文凭据", L5). provider.api_key and the kv rows of
// sealedKVKeys hold secrets.Seal values (prefix enc:v1:) in the same TEXT
// columns (no DDL change). The Store seals on every write and opens on
// every read, so its callers keep seeing the plain credential:
//
//   - a value already sealed is stored as it is (never sealed twice);
//   - without a data key (tests, or a failed key setup: the diagnostics
//     show an error) values are stored as they come, as before;
//   - a stored plain value (written before this, or without a key) reads
//     as it is; SealSecrets seals it at the next start;
//   - a sealed value no key opens (the data directory came from another
//     computer or account) reads as "no credential": a provider has an
//     empty APIKey and UnreadableSecrets lists it ("需要重新输入"), a kv row
//     reads as absent (Colab: authorise again). The sealed text itself never
//     leaves this package, and nothing here logs a value.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"lectureflow/internal/secrets"
)

// sealedKVKeys are the kv rows kept sealed: the Colab credential
// (colab.KVCredential: a refresh_token and a client_secret).
var sealedKVKeys = []string{"colab.credential"}

// SealedKVKeys lists the kv keys whose values the Store keeps sealed.
func SealedKVKeys() []string { return slices.Clone(sealedKVKeys) }

func isSealedKV(key string) bool { return slices.Contains(sealedKVKeys, key) }

// sealStored returns what is stored for the credential plain.
func sealStored(plain string) (string, error) {
	if plain == "" || secrets.IsSealed(plain) || !secrets.HasKey() {
		return plain, nil
	}
	return secrets.Seal([]byte(plain))
}

// openStored returns the credential a stored value holds; unreadable when
// it is sealed and no key opens it (plain is then "").
func openStored(stored string) (plain string, unreadable bool) {
	if !secrets.IsSealed(stored) {
		return stored, false
	}
	b, _, err := secrets.OpenAny(stored)
	if err != nil {
		return "", true
	}
	return string(b), false
}

// warned: the unreadable credentials already logged in this process.
var warned sync.Map

// warnUnreadable logs once per credential that it cannot be decrypted
// (what: "provider" with its id, or "kv" with its key; never a value).
func warnUnreadable(what, id string) {
	if _, dup := warned.LoadOrStore(what+"\x00"+id, true); dup {
		return
	}
	slog.Warn("db: a stored credential cannot be decrypted with this installation's key (the data directory came from another computer or account?); it has to be entered again",
		what, id)
}

// openKV is GetKV's step for a sealed kv key: ok false when unreadable.
func openKV(key, stored string) (string, bool) {
	plain, unreadable := openStored(stored)
	if unreadable {
		warnUnreadable("kv", key)
		return "", false
	}
	return plain, true
}

// openProviderKey is scanProvider's step for provider.api_key.
func openProviderKey(id, stored string) string {
	plain, unreadable := openStored(stored)
	if unreadable {
		warnUnreadable("provider", id)
	}
	return plain
}

// UnreadableSecrets lists the stored credentials no key opens: provider ids
// (their key has to be entered again) and kv keys. Not part of the Store
// interface (frozen): settings and the diagnostics reach it with a type
// assertion (SecretInspector).
func (s *sqlStore) UnreadableSecrets(ctx context.Context) (providers, kv []string, err error) {
	rows, err := s.q.QueryContext(ctx, `SELECT id, api_key FROM provider WHERE api_key LIKE ? ORDER BY id`, secrets.Prefix+"%")
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id, v string
		if err := rows.Scan(&id, &v); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if _, bad := openStored(v); bad {
			providers = append(providers, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for _, k := range sealedKVKeys {
		var v string
		err := s.q.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, k).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if _, bad := openStored(v); bad {
			kv = append(kv, k)
		}
	}
	return providers, kv, nil
}

// SecretInspector is the optional Store method UnreadableSecrets.
type SecretInspector interface {
	UnreadableSecrets(ctx context.Context) (providers, kv []string, err error)
}

// UnsealedSecrets lists the credentials stored in the clear: provider ids
// with a plain API key and sealedKVKeys rows with a plain value (written
// while there was no data key; sealed at the next start that has one).
// sec3 L2: the settings page and the diagnostics show them.
func (s *sqlStore) UnsealedSecrets(ctx context.Context) (providers, kv []string, err error) {
	rows, err := s.q.QueryContext(ctx, `SELECT id, api_key FROM provider WHERE api_key <> '' ORDER BY id`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id, v string
		if err := rows.Scan(&id, &v); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if !secrets.IsSealed(v) {
			providers = append(providers, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for _, k := range sealedKVKeys {
		var v string
		err := s.q.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, k).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if v != "" && !secrets.IsSealed(v) {
			kv = append(kv, k)
		}
	}
	return providers, kv, nil
}

// SecretPlainInspector is the optional Store method UnsealedSecrets.
type SecretPlainInspector interface {
	UnsealedSecrets(ctx context.Context) (providers, kv []string, err error)
}

// SealResult counts what SealSecrets changed.
type SealResult struct {
	Sealed     int // plain values sealed
	Resealed   int // values opened with an earlier key, sealed with the current one
	Unreadable int // sealed values no key opens (left as they are)
	// Scrubbed: an earlier secure write had not finished its clean-up
	// (kvScrubPending); the VACUUM and the checkpoint ran now (sec3 I3).
	Scrubbed bool
}

// Changed reports whether anything was rewritten.
func (r SealResult) Changed() bool { return r.Sealed+r.Resealed > 0 }

type sealUpdate struct {
	query string
	args  []any
}

// SealSecrets brings the stored credentials of the database d to the
// current data key (provider.api_key, the sealedKVKeys rows, and any other
// kv row whose owner sealed it, such as ssh-mvp's private keys, which are
// only sealed again): plain values are sealed, values only an earlier key
// (secrets.SetFallbackKeys, or one of extraKeys: an imported data
// directory's key) opens are sealed again. The rewrite runs on the secure
// path (secure_delete, VACUUM, TRUNCATE checkpoint), so neither the plain
// values nor the old ciphertexts stay in free pages or the WAL. Nothing is
// written when everything is sealed already (every start but the first),
// nor without a data key. Values no key opens are left as they are and
// counted. A database without the provider or kv table is fine.
func SealSecrets(ctx context.Context, d *sql.DB, extraKeys ...[]byte) (SealResult, error) {
	var res SealResult
	if !secrets.HasKey() {
		return res, finishPendingScrub(ctx, d, &res)
	}
	tables := map[string]bool{}
	rows, err := d.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name IN ('provider', 'kv')`)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return res, err
		}
		tables[n] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}

	var updates []sealUpdate
	plan := func(stored string, update func(v string) sealUpdate) error {
		if stored == "" {
			return nil
		}
		var plain []byte
		if !secrets.IsSealed(stored) {
			plain = []byte(stored)
			res.Sealed++
		} else {
			p, current, err := secrets.OpenAny(stored)
			if err == nil && current {
				return nil
			}
			if err != nil {
				for _, k := range extraKeys {
					if p, err = secrets.OpenWith(k, stored); err == nil {
						break
					}
				}
			}
			if err != nil {
				res.Unreadable++
				return nil
			}
			plain = p
			res.Resealed++
		}
		v, err := secrets.Seal(plain)
		if err != nil {
			return err
		}
		updates = append(updates, update(v))
		return nil
	}

	if tables["provider"] {
		rows, err := d.QueryContext(ctx, `SELECT id, api_key FROM provider WHERE api_key <> ''`)
		if err != nil {
			return res, err
		}
		type row struct{ id, v string }
		var list []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.v); err != nil {
				rows.Close()
				return res, err
			}
			list = append(list, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return res, err
		}
		for _, r := range list {
			id := r.id
			if err := plan(r.v, func(v string) sealUpdate {
				return sealUpdate{`UPDATE provider SET api_key = ? WHERE id = ?`, []any{v, id}}
			}); err != nil {
				return res, err
			}
		}
	}
	if tables["kv"] {
		// the sealed kv keys, and any other row its owner sealed itself
		// (ssh-mvp's remote.ssh.key.*): those are only sealed again, a plain
		// value of another key is no credential and stays as it is
		rows, err := d.QueryContext(ctx, `SELECT key, value FROM kv WHERE value LIKE ? ORDER BY key`, secrets.Prefix+"%")
		if err != nil {
			return res, err
		}
		kv := map[string]string{}
		var keys []string
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				rows.Close()
				return res, err
			}
			if secrets.IsSealed(v) {
				kv[k] = v
				keys = append(keys, k)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return res, err
		}
		for _, k := range sealedKVKeys {
			if _, done := kv[k]; done {
				continue
			}
			var v string
			err := d.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, k).Scan(&v)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return res, err
			}
			kv[k] = v
			keys = append(keys, k)
		}
		for _, k := range keys {
			key := k
			if err := plan(kv[k], func(v string) sealUpdate {
				return sealUpdate{`UPDATE kv SET value = ? WHERE key = ?`, []any{v, key}}
			}); err != nil {
				return res, err
			}
		}
	}
	if len(updates) == 0 {
		return res, finishPendingScrub(ctx, d, &res)
	}
	err = withSecureConn(ctx, d, func(ctx context.Context, c *sql.Conn) error {
		tx, err := c.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, u := range updates {
			if _, err := tx.ExecContext(ctx, u.query, u.args...); err != nil {
				tx.Rollback()
				return err
			}
		}
		return tx.Commit()
	})
	if err != nil {
		return res, fmt.Errorf("db: sealing the stored credentials: %w", err)
	}
	return res, nil
}

// SealStoredSecrets is SealSecrets on the Store's database (startup).
func (s *sqlStore) SealStoredSecrets(ctx context.Context) (SealResult, error) {
	if s.tx != nil {
		return SealResult{}, errors.New("db: SealStoredSecrets inside a transaction")
	}
	return SealSecrets(ctx, s.db)
}

// SecretSealer is the optional Store method SealStoredSecrets.
type SecretSealer interface {
	SealStoredSecrets(ctx context.Context) (SealResult, error)
}

// finishPendingScrub runs the clean-up an earlier secure write left
// unfinished (kvScrubPending: interrupted, or the VACUUM failed): sec3 I3.
func finishPendingScrub(ctx context.Context, d *sql.DB, res *SealResult) error {
	if !scrubPending(ctx, d) {
		return nil
	}
	if err := withSecureConn(ctx, d, func(context.Context, *sql.Conn) error { return nil }); err != nil {
		return fmt.Errorf("db: finishing an interrupted clean-up of freed credentials: %w", err)
	}
	res.Scrubbed = true
	return nil
}
