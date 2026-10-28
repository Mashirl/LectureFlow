// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// The SSH inference servers (ssh-mvp; .claude/research/remote-ssh-plan.md
// §6): add, edit, remove, connect (shows the host key), trust (pins it),
// install the environment, test, the public key to authorize, and "改用
// <服务器> 处理" for a lecture. The servers come from the scheduler
// (sched.Scheduler.SSH, an *sshremote.Manager wired by cmd only with
// LECTUREFLOW_EXPERIMENTAL_SSH=1; security review 3, L6); without them every
// endpoint answers 404 api.not_found, as if it did not exist. No body holds
// a private key or any secret: a server's view has the public key and
// fingerprints only.

import (
	"context"
	"errors"
	"net/http"

	"lectureflow/internal/sched"
	"lectureflow/internal/sshremote"
)

// remoteServers is what the endpoints need from the SSH servers.
type remoteServers interface {
	List() []sshremote.ServerView
	Get(id string) (sshremote.ServerView, bool)
	Add(ctx context.Context, req sshremote.AddRequest) (sshremote.ServerView, error)
	Update(ctx context.Context, id string, p sshremote.Patch) (sshremote.ServerView, error)
	Remove(ctx context.Context, id string, uninstall bool) error
	Connect(ctx context.Context, id string) (sshremote.ConnectResult, error)
	Trust(ctx context.Context, id string, hop int, fingerprint string) (sshremote.ConnectResult, error)
	Install(id string) error
	Test(ctx context.Context, id string) (sshremote.ServerView, error)
	PublicKeyOf(id string) (sshremote.PublicKey, error)
}

// API codes of these endpoints (the servers' own codes, remote.*, are the
// back end's: error_codes_backend.json).
const (
	codeRemoteServerNotFound = "remote_server.not_found"
	codeRemoteServerInvalid  = "remote_server.invalid"
	codeRemoteServerExists   = "remote_server.exists"
	codeRemoteServerBusy     = "remote_server.busy"
	codeRemoteServerNoKey    = "remote_server.credentials_unavailable"
)

var remoteServerCodes = map[string]errDef{
	codeRemoteServerNotFound: {"找不到这台远程服务器（可能已被移除），请刷新页面", nil},
	codeRemoteServerInvalid:  {"远程服务器的设置无效（{field}）", []string{"field"}},
	codeRemoteServerExists:   {"已经有一台同名（{id}）的远程服务器", []string{"id"}},
	codeRemoteServerBusy:     {"这台远程服务器正在转写，请等它结束或先取消再改", nil},
	codeRemoteServerNoKey:    {"本机的凭据存储不可用，无法为远程服务器生成或读取密钥。请查看“诊断”页", nil},
}

func init() {
	for code, def := range remoteServerCodes {
		if _, dup := errorCatalog[code]; dup {
			panic("remote server code defined twice: " + code)
		}
		errorCatalog[code] = def
	}
	v3Routes = append(v3Routes, func(s *Server, h func(string, http.HandlerFunc)) {
		h("GET /api/remote/servers", s.listRemoteServers)
		h("POST /api/remote/servers", s.addRemoteServer)
		h("GET /api/remote/servers/{id}", s.getRemoteServer)
		h("PATCH /api/remote/servers/{id}", s.patchRemoteServer)
		h("DELETE /api/remote/servers/{id}", s.deleteRemoteServer)
		h("POST /api/remote/servers/{id}/connect", s.connectRemoteServer)
		h("POST /api/remote/servers/{id}/trust", s.trustRemoteServer)
		h("POST /api/remote/servers/{id}/install", s.installRemoteServer)
		h("POST /api/remote/servers/{id}/test", s.testRemoteServer)
		h("GET /api/remote/servers/{id}/pubkey", s.remoteServerKey)
		h("POST /api/lectures/{id}/use-remote", s.useRemoteServer)
	})
}

// remoteSrv returns the servers, or answers 404 api.not_found (the SSH
// servers are off: the endpoints do not exist).
func (s *Server) remoteSrv(w http.ResponseWriter, r *http.Request) (remoteServers, bool) {
	if s.sc != nil {
		if rs, ok := s.sc.SSH().(remoteServers); ok && rs != nil {
			return rs, true
		}
	}
	writeError(w, http.StatusNotFound, "api.not_found", map[string]any{"method": r.Method, "path": r.URL.Path})
	return nil, false
}

// failRemote maps the servers' errors.
func (s *Server) failRemote(w http.ResponseWriter, r *http.Request, id string, err error) {
	var fe *sshremote.FieldError
	switch {
	case errors.Is(err, sshremote.ErrNotFound):
		writeError(w, http.StatusNotFound, codeRemoteServerNotFound, nil)
	case errors.As(err, &fe):
		writeError(w, http.StatusBadRequest, codeRemoteServerInvalid, map[string]any{"field": fe.Field})
	case errors.Is(err, sshremote.ErrExists):
		writeError(w, http.StatusConflict, codeRemoteServerExists, map[string]any{"id": id})
	case errors.Is(err, sshremote.ErrBusy):
		writeError(w, http.StatusConflict, codeRemoteServerBusy, nil)
	case errors.Is(err, sshremote.ErrNoDataKey):
		writeError(w, http.StatusServiceUnavailable, codeRemoteServerNoKey, nil)
	case r.Context().Err() != nil:
	default:
		// A connection's failure: the server's own code (remote.*), 502.
		name := id
		if rs, ok := s.sc.SSH().(remoteServers); ok {
			if v, ok := rs.Get(id); ok {
				name = v.Name
			}
		}
		writeError(w, http.StatusBadGateway, sshremote.ConnErrorCode(err), map[string]any{"device": name})
	}
}

func (s *Server) listRemoteServers(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteSrv(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": rs.List()})
}

func (s *Server) addRemoteServer(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteSrv(w, r)
	if !ok {
		return
	}
	var req sshremote.AddRequest
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := rs.Add(r.Context(), req)
	if err != nil {
		s.failRemote(w, r, req.ID, err)
		return
	}
	s.hub.Changed("devices", "")
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) getRemoteServer(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteSrv(w, r)
	if !ok {
		return
	}
	v, found := rs.Get(r.PathValue("id"))
	if !found {
		writeError(w, http.StatusNotFound, codeRemoteServerNotFound, nil)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) patchRemoteServer(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteSrv(w, r)
	if !ok {
		return
	}
	var p sshremote.Patch
	if err := decodeJSON(r, &p); err != nil {
		s.fail(w, r, err)
		return
	}
	id := r.PathValue("id")
	v, err := rs.Update(r.Context(), id, p)
	if err != nil {
		s.failRemote(w, r, id, err)
		return
	}
	s.hub.Changed("devices", "")
	writeJSON(w, http.StatusOK, v)
}

// DELETE /api/remote/servers/{id}?uninstall=1: with uninstall, lf-agent
// removes its directories (environment, models, jobs) on the server first.
func (s *Server) deleteRemoteServer(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteSrv(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	uninstall := r.URL.Query().Get("uninstall") == "1"
	if err := rs.Remove(r.Context(), id, uninstall); err != nil {
		s.failRemote(w, r, id, err)
		return
	}
	s.hub.Changed("devices", "")
	w.WriteHeader(http.StatusNoContent)
}

// POST …/connect: without a confirmed host key the answer holds the
// presented key (host_key: algorithm, fingerprint, the command that shows
// it on the server, the user's known_hosts verdict); nothing else is sent.
func (s *Server) connectRemoteServer(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteSrv(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	res, err := rs.Connect(r.Context(), id)
	if err != nil {
		s.failRemote(w, r, id, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// TrustRequest confirms a presented host key.
type TrustRequest struct {
	Hop         int    `json:"hop"`
	Fingerprint string `json:"fingerprint"`
}

func (s *Server) trustRemoteServer(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteSrv(w, r)
	if !ok {
		return
	}
	var req TrustRequest
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	id := r.PathValue("id")
	res, err := rs.Trust(r.Context(), id, req.Hop, req.Fingerprint)
	if err != nil {
		s.failRemote(w, r, id, err)
		return
	}
	s.hub.Changed("devices", "")
	writeJSON(w, http.StatusOK, res)
}

// POST …/install: installs the environment in the background (202); the
// server's view shows the steps (install) and its state.
func (s *Server) installRemoteServer(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteSrv(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if err := rs.Install(id); err != nil {
		s.failRemote(w, r, id, err)
		return
	}
	v, _ := rs.Get(id)
	writeJSON(w, http.StatusAccepted, v)
}

func (s *Server) testRemoteServer(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteSrv(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	v, err := rs.Test(r.Context(), id)
	if err != nil {
		s.failRemote(w, r, id, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) remoteServerKey(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteSrv(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	k, err := rs.PublicKeyOf(id)
	if err != nil {
		s.failRemote(w, r, id, err)
		return
	}
	writeJSON(w, http.StatusOK, k)
}

// UseRemoteRequest is "改用 <服务器> 处理".
type UseRemoteRequest struct {
	Device string `json:"device"` // "ssh:<id>"
}

func (s *Server) useRemoteServer(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.remoteSrv(w, r); !ok {
		return
	}
	var req UseRemoteRequest
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	id := r.PathValue("id")
	if _, err := s.getLectureRow(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.sc.UseSSH(r.Context(), id, req.Device); err != nil {
		if errors.Is(err, sched.ErrSSHNotConfigured) {
			writeError(w, http.StatusNotFound, "api.not_found", map[string]any{"method": r.Method, "path": r.URL.Path})
			return
		}
		s.fail(w, r, err)
		return
	}
	s.respondLecture(w, r, id, http.StatusOK)
}
