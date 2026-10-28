// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"log/slog"
	"path"
	"strings"
	"sync"
)

// Static asset ETags (perf-fix): the front end is embedded, so its files
// never change while the program runs, but embed.FS has no modification
// times and the files were sent in full on every load. Each file's ETag is
// its content's SHA-256 (first 16 bytes, hex), computed once, on the first
// static request; with "Cache-Control: no-cache" the browser revalidates
// every time and gets 304 until the binary (and so the content) changes.

// staticETags returns the ETag of every regular file of fsys by its path
// (as fs.FS names it: "app.js", "i18n/zh-CN.json"), computed on the first
// call. A file that cannot be read gets none (it is served without).
func staticETags(fsys fs.FS) func() map[string]string {
	return sync.OnceValue(func() map[string]string {
		out := map[string]string{}
		err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return nil
			}
			b, err := fs.ReadFile(fsys, name)
			if err != nil {
				return nil
			}
			sum := sha256.Sum256(b)
			out[name] = `"` + hex.EncodeToString(sum[:16]) + `"`
			return nil
		})
		if err != nil {
			slog.Warn("static: hashing the front end", "err", err)
		}
		return out
	})
}

// staticName is the file a static request serves, as staticETags keys it:
// "/" is index.html (http.FileServer serves a directory's index.html), and
// "" for a path http.FileServer redirects (".../index.html", a directory
// without its trailing slash) or lists.
func staticName(urlPath string) string {
	if strings.HasSuffix(urlPath, "/index.html") {
		return ""
	}
	name := strings.TrimPrefix(path.Clean("/"+urlPath), "/")
	switch {
	case name == "":
		return "index.html"
	case strings.HasSuffix(urlPath, "/"):
		return name + "/index.html" // a directory: its index.html, if any
	}
	return name
}
