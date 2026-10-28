// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"net/http"
	"strconv"
)

// Visible front ends (perf-fix, NOTES.md). SPEC-v1.1 A10 stops the resource
// sampling "without a front end"; a window hidden to the tray or a
// background tab keeps its event stream (the completion notices come
// through it) but nobody looks at the resource bar, so only visible pages
// count:
//
//	GET  /api/events?client=ID&visible=0|1&seq=N   the stream; the query
//	     says whether the page is shown when it connects (no query: shown,
//	     as before — other clients and old pages count as looking).
//	POST /api/events/visibility {"client", "visible", "seq"}   a change
//	     while connected; 204 also when no stream has that id (the next
//	     connect sends the state again).
//
// seq orders the reports of one page (the query's and the posts'): a report
// with a seq not above the one in effect is ignored, so a late post cannot
// undo a newer state. The resource watcher gets the visible count
// (Hub.notifyClients → resources.Service.SetClients): 0 stops sampling, a
// rise samples at once and broadcasts the snapshot to every client.

// clientIDMax bounds the client id (the front end sends a UUID).
const clientIDMax = 64

// clientVisibility is one visibility report of a front end.
type clientVisibility struct {
	id      string
	visible bool
	seq     int64
}

// visibilityOf reads the report of GET /api/events: visible unless
// visible=0 or visible=false; an invalid client id is dropped (that stream
// cannot be changed later) and an invalid seq is 0.
func visibilityOf(r *http.Request) clientVisibility {
	q := r.URL.Query()
	v := clientVisibility{visible: true}
	if id := q.Get("client"); validClientID(id) {
		v.id = id
	}
	switch q.Get("visible") {
	case "0", "false":
		v.visible = false
	}
	if n, err := strconv.ParseInt(q.Get("seq"), 10, 64); err == nil && n > 0 {
		v.seq = n
	}
	return v
}

// validClientID: 1–clientIDMax characters of [A-Za-z0-9_-].
func validClientID(id string) bool {
	if id == "" || len(id) > clientIDMax {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// VisibleClients returns the number of connected SSE clients whose page is
// shown.
func (h *Hub) VisibleClients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for c := range h.clients {
		if c.visible {
			n++
		}
	}
	return n
}

// setVisibility applies a report to the streams with its id (normally one;
// two while a reconnect overlaps the old stream). It reports whether a
// stream took it; the hook is told when the visible count changed.
func (h *Hub) setVisibility(v clientVisibility) bool {
	if v.id == "" {
		return false
	}
	h.mu.Lock()
	applied, changed := false, false
	for c := range h.clients {
		if c.id != v.id || v.seq <= c.visSeq {
			continue
		}
		applied = true
		c.visSeq = v.seq
		if c.visible != v.visible {
			c.visible = v.visible
			changed = true
		}
	}
	h.mu.Unlock()
	if changed {
		h.notifyClients()
	}
	return applied
}

// eventsVisibilityBody is the body of POST /api/events/visibility.
type eventsVisibilityBody struct {
	Client  string `json:"client"`
	Visible *bool  `json:"visible"`
	Seq     int64  `json:"seq"`
}

// postEventsVisibility is POST /api/events/visibility: 204, or 400
// request.invalid without a valid client id, visible or a positive seq.
func (s *Server) postEventsVisibility(w http.ResponseWriter, r *http.Request) {
	var b eventsVisibilityBody
	if err := decodeJSON(r, &b); err != nil {
		s.fail(w, r, err)
		return
	}
	if !validClientID(b.Client) || b.Visible == nil || b.Seq <= 0 {
		s.fail(w, r, apiErr(http.StatusBadRequest, "request.invalid", nil))
		return
	}
	s.hub.setVisibility(clientVisibility{id: b.Client, visible: *b.Visible, seq: b.Seq})
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
