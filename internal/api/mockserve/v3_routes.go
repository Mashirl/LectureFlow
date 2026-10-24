// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import "net/http"

// mockV3Routes lets the v3 mock files register their routes from an init
// function in their own file (.claude/v3-brief.md §7.1), so parallel tasks
// do not all edit main.go. Wrap handlers with m.api like main.go does.
var mockV3Routes []func(m *mock, mux *http.ServeMux)
