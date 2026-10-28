// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// ssh-mvp: the SSH server endpoints over a real sshremote.Manager (no
// server is contacted: connecting to a closed port answers remote.offline).

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/secrets"
	"lectureflow/internal/sshremote"
)

func withSSH(t *testing.T, e *env) *sshremote.Manager {
	t.Helper()
	m, err := sshremote.New(context.Background(), sshremote.Options{Store: e.st.(sshremote.Store), AgentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	e.sc.SetSSH(m)
	return m
}

// With the SSH servers off (the default; security review 3, L6) every
// endpoint answers like a route that does not exist.
func TestRemoteServersOffAre404(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/remote/servers"},
		{http.MethodPost, "/api/remote/servers"},
		{http.MethodGet, "/api/remote/servers/x"},
		{http.MethodPatch, "/api/remote/servers/x"},
		{http.MethodDelete, "/api/remote/servers/x"},
		{http.MethodPost, "/api/remote/servers/x/connect"},
		{http.MethodPost, "/api/remote/servers/x/trust"},
		{http.MethodPost, "/api/remote/servers/x/install"},
		{http.MethodPost, "/api/remote/servers/x/test"},
		{http.MethodGet, "/api/remote/servers/x/pubkey"},
		{http.MethodPost, "/api/lectures/x/use-remote"},
	} {
		res, body := e.do(c.method, c.path, map[string]any{})
		if res.StatusCode != http.StatusNotFound || !strings.Contains(string(body), `"api.not_found"`) {
			t.Errorf("%s %s: %d %s", c.method, c.path, res.StatusCode, body)
		}
	}
	// The same body as a path that never existed.
	_, unknown := e.do(http.MethodGet, "/api/remote/nothing-here", nil)
	if !strings.Contains(string(unknown), `"api.not_found"`) {
		t.Fatalf("unknown path: %s", unknown)
	}
}

func TestRemoteServersCRUD(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	if err := secrets.SetKey(bytes.Repeat([]byte{7}, secrets.KeySize)); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	withSSH(t, e)
	// A closed port: the server is unreachable.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	res, body := e.do(http.MethodPost, "/api/remote/servers", map[string]any{"name": "Lab box", "host": "127.0.0.1", "port": port,
		"username": "q", "group": "prefer"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("add: %d %s", res.StatusCode, body)
	}
	var v sshremote.ServerView
	_ = json.Unmarshal(body, &v)
	if v.ID != "lab-box" || v.DeviceID != "ssh:lab-box" || !strings.HasPrefix(v.PublicKey, "restrict ssh-ed25519 ") ||
		strings.Contains(string(body), "PRIVATE") || strings.Contains(string(body), "enc:v1:") {
		t.Fatalf("view %s", body)
	}
	// The stored key is sealed; nothing in the row is a secret.
	if raw, ok, _ := e.st.GetKV(context.Background(), sshremote.KVKeyPrefix+"lab-box"); !ok || !secrets.IsSealed(raw) {
		t.Fatal("the private key is not sealed in kv")
	}
	rec, err := e.st.(interface {
		GetRemoteServer(context.Context, string) (db.RemoteServer, error)
	}).GetRemoteServer(context.Background(), "lab-box")
	if err != nil || strings.Contains(rec.PublicKey+rec.SettingsJSON+rec.ProbeJSON, "PRIVATE") {
		t.Fatalf("%+v %v", rec, err)
	}
	res, body = e.do(http.MethodPost, "/api/remote/servers", map[string]any{"name": "x", "host": "a b", "username": "q"})
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), codeRemoteServerInvalid) {
		t.Fatalf("invalid: %d %s", res.StatusCode, body)
	}
	res, body = e.do(http.MethodGet, "/api/remote/servers/lab-box/pubkey", nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "authorized_keys") {
		t.Fatalf("pubkey: %d %s", res.StatusCode, body)
	}
	res, body = e.do(http.MethodPost, "/api/remote/servers/lab-box/connect", nil)
	if res.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), `"remote.offline"`) {
		t.Fatalf("connect: %d %s", res.StatusCode, body)
	}
	res, body = e.do(http.MethodPatch, "/api/remote/servers/lab-box", map[string]any{"name": "Lab", "paused": true})
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"state":"paused"`) {
		t.Fatalf("patch: %d %s", res.StatusCode, body)
	}
	res, body = e.do(http.MethodGet, "/api/remote/servers", nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"name":"Lab"`) {
		t.Fatalf("list: %d %s", res.StatusCode, body)
	}
	res, body = e.do(http.MethodGet, "/api/remote/servers/nope", nil)
	if res.StatusCode != http.StatusNotFound || !strings.Contains(string(body), codeRemoteServerNotFound) {
		t.Fatalf("missing: %d %s", res.StatusCode, body)
	}
	res, _ = e.do(http.MethodDelete, "/api/remote/servers/lab-box", nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", res.StatusCode)
	}
	if _, ok, _ := e.st.GetKV(context.Background(), sshremote.KVKeyPrefix+"lab-box"); ok {
		t.Fatal("the key is left after removing the server")
	}
}

func TestRemoteServerWithoutCredentialStore(t *testing.T) {
	secrets.ResetForTest()
	e := newEnv(t)
	withSSH(t, e)
	res, body := e.do(http.MethodPost, "/api/remote/servers", map[string]any{"name": "x", "host": "h", "username": "q"})
	if res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), codeRemoteServerNoKey) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
}
