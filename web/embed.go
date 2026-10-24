// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package web holds the browser front end (SPEC §1, §9): one index.html,
// plain ES modules and one style sheet, with no framework and no build step.
// It is embedded into the binary and served at "/" by internal/api.
package web

import "embed"

// FS contains index.html, the *.js modules, style.css and favicon.svg at its
// root, the UI text catalogs in i18n/ (SPEC-v2 P14.2) and third-party code
// in vendor/ with its licence files (opencc-js for P14.6, loaded on demand).
//
//go:embed index.html *.js *.css *.svg i18n/*.json vendor
var FS embed.FS
