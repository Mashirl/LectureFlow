// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The SSH inference servers (migrations/014_remote_server.sql, ssh-mvp).
// These methods are not part of Store: internal/sshremote declares the
// narrow interface it needs and cmd hands it the store (like the download
// tasks).

// RemoteServer is one row of remote_server. It never holds a secret.
type RemoteServer struct {
	ID           string
	Name         string
	Host         string
	Port         int
	Username     string
	JumpJSON     string
	HostKey      string
	KeyRef       string
	PublicKey    string
	Platform     string
	Engine       string
	AgentVersion string
	KitHash      string
	ProbeJSON    string
	SettingsJSON string
	CreatedAt    string
	UpdatedAt    string
}

const remoteServerCols = `id, name, host, port, username, jump_json, host_key, key_ref, public_key, platform, engine,
	agent_version, kit_hash, probe_json, settings_json, created_at, updated_at`

func scanRemoteServer(sc interface{ Scan(...any) error }) (RemoteServer, error) {
	var r RemoteServer
	err := sc.Scan(&r.ID, &r.Name, &r.Host, &r.Port, &r.Username, &r.JumpJSON, &r.HostKey, &r.KeyRef, &r.PublicKey,
		&r.Platform, &r.Engine, &r.AgentVersion, &r.KitHash, &r.ProbeJSON, &r.SettingsJSON, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

func (r *RemoteServer) defaults() {
	if r.JumpJSON == "" {
		r.JumpJSON = "[]"
	}
	if r.ProbeJSON == "" {
		r.ProbeJSON = "{}"
	}
	if r.SettingsJSON == "" {
		r.SettingsJSON = "{}"
	}
}

// InsertRemoteServer adds a server (ErrDuplicate when the id exists).
func (s *sqlStore) InsertRemoteServer(ctx context.Context, r RemoteServer) error {
	r.defaults()
	_, err := s.q.ExecContext(ctx, `INSERT INTO remote_server (`+remoteServerCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Name, r.Host, r.Port, r.Username, r.JumpJSON, r.HostKey, r.KeyRef, r.PublicKey, r.Platform, r.Engine,
		r.AgentVersion, r.KitHash, r.ProbeJSON, r.SettingsJSON, r.CreatedAt, r.UpdatedAt)
	return duplicateErr(err)
}

// UpdateRemoteServer replaces a server's row (ErrNotFound when it is gone).
func (s *sqlStore) UpdateRemoteServer(ctx context.Context, r RemoteServer) error {
	r.defaults()
	res, err := s.q.ExecContext(ctx, `UPDATE remote_server SET name=?, host=?, port=?, username=?, jump_json=?, host_key=?,
		key_ref=?, public_key=?, platform=?, engine=?, agent_version=?, kit_hash=?, probe_json=?, settings_json=?, updated_at=?
		WHERE id=?`,
		r.Name, r.Host, r.Port, r.Username, r.JumpJSON, r.HostKey, r.KeyRef, r.PublicKey, r.Platform, r.Engine,
		r.AgentVersion, r.KitHash, r.ProbeJSON, r.SettingsJSON, r.UpdatedAt, r.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetRemoteServer returns one server (ErrNotFound).
func (s *sqlStore) GetRemoteServer(ctx context.Context, id string) (RemoteServer, error) {
	r, err := scanRemoteServer(s.q.QueryRowContext(ctx, `SELECT `+remoteServerCols+` FROM remote_server WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// ListRemoteServers returns every server by name.
func (s *sqlStore) ListRemoteServers(ctx context.Context) ([]RemoteServer, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+remoteServerCols+` FROM remote_server ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RemoteServer
	for rows.Next() {
		r, err := scanRemoteServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRemoteServer removes a server and the kv row of its key in one
// transaction (its device row goes through devices.Registry). A missing
// server is not an error.
func (s *sqlStore) DeleteRemoteServer(ctx context.Context, id, keyRef string) error {
	return s.Tx(ctx, func(tx Store) error {
		t, ok := tx.(*sqlStore)
		if !ok {
			return fmt.Errorf("db: unexpected transaction store %T", tx)
		}
		if _, err := t.q.ExecContext(ctx, `DELETE FROM remote_server WHERE id=?`, id); err != nil {
			return err
		}
		if keyRef != "" {
			if _, err := t.q.ExecContext(ctx, `DELETE FROM kv WHERE key=?`, keyRef); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteASRDevice removes one asr_device row (an SSH server's, when the
// server is removed; devices.Registry.RemoveSSH). A missing row is not an
// error. Nothing references asr_device (migrations/009).
func (s *sqlStore) DeleteASRDevice(ctx context.Context, id string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM asr_device WHERE id=?`, id)
	return err
}
