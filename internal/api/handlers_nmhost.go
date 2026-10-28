// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"errors"
	"net/http"
	"sync"

	"lectureflow/internal/nmhost"
)

// The browser extension connection (SPEC-ext "Native Messaging 宿主",
// .claude/ext-brief.md §4): the diagnostics page's "浏览器扩展连接" row.
//
//	GET  /api/nmhost           nmhost.Status: each browser's registration
//	                           (registered | not_registered | host_missing |
//	                           not_installed), the endpoint, the latest
//	                           extension connection
//	POST /api/nmhost/register  registers again ("重新登记") and answers the
//	                           new status; a failed run is reported in
//	                           last_register.error, still 200
//
// Both answer 503 server.unavailable {service: "nmhost"} before cmd has
// started the service; register answers it as well when this instance
// does not manage the registration (an isolated instance on its own
// endpoint; status.managed is false).

// NMHost is the Native Messaging service (*nmhost.Service).
type NMHost interface {
	Status() nmhost.Status
	Register() (nmhost.Status, error)
}

type nmHostHolder struct {
	mu sync.RWMutex
	v  NMHost
}

// SetNMHost hands the server the Native Messaging service (cmd, after the
// endpoint is up). nil removes it.
func (s *Server) SetNMHost(n NMHost) {
	if svc, ok := n.(*nmhost.Service); ok && svc == nil {
		n = nil
	}
	s.nm.mu.Lock()
	s.nm.v = n
	s.nm.mu.Unlock()
}

func (s *Server) nmHost() NMHost {
	s.nm.mu.RLock()
	defer s.nm.mu.RUnlock()
	return s.nm.v
}

func init() {
	v3Routes = append(v3Routes, func(s *Server, h func(string, http.HandlerFunc)) {
		h("GET /api/nmhost", s.getNMHost)
		h("POST /api/nmhost/register", s.postNMHostRegister)
	})
}

func (s *Server) getNMHost(w http.ResponseWriter, r *http.Request) {
	n := s.nmHost()
	if n == nil {
		writeError(w, http.StatusServiceUnavailable, "server.unavailable", map[string]any{"service": "nmhost"})
		return
	}
	writeJSON(w, http.StatusOK, n.Status())
}

func (s *Server) postNMHostRegister(w http.ResponseWriter, r *http.Request) {
	if crossSite(w, r) {
		return
	}
	n := s.nmHost()
	if n == nil {
		writeError(w, http.StatusServiceUnavailable, "server.unavailable", map[string]any{"service": "nmhost"})
		return
	}
	st, err := n.Register()
	s.forgetBrowserExtRow() // the diagnostics row shows the new registration
	if errors.Is(err, nmhost.ErrNotManaged) {
		writeError(w, http.StatusServiceUnavailable, "server.unavailable", map[string]any{"service": "nmhost"})
		return
	}
	// Any other failure is in st.LastRegister (the page shows it).
	writeJSON(w, http.StatusOK, st)
}
