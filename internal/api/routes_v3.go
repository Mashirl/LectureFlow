// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import "net/http"

// v3Routes lets the v3 handler files register their routes from an init
// function in their own file, so parallel tasks do not all edit server.go
// (.claude/v3-brief.md §7). Registration order is the order of the files'
// init functions; patterns must not overlap.
var v3Routes []func(s *Server, h func(pattern string, fn http.HandlerFunc))

func (s *Server) registerV3Routes(h func(pattern string, fn http.HandlerFunc)) {
	for _, reg := range v3Routes {
		reg(s, h)
	}
}
