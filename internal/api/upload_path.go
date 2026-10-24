// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Native file handling (SPEC-v3 V1.4): in desktop mode a file the user
// dropped or picked in the system dialog is not sent over HTTP. The page
// posts {"path": "<absolute local path>", ...text fields} as JSON to the
// same upload URL, and the server copies the file into the data directory
// itself. The result is the one an HTTP upload of that file gives: same
// file-name and extension checks, size limit (§17.4), free-space check
// (§17.5), original file name, .tmp file renamed on success. In web mode a
// JSON body keeps getting the old answer (upload.not_multipart).

// maxPathBody bounds the JSON body of a path upload.
const maxPathBody = maxFieldBytes * 16

// copyBufSize is the buffer of the local copy (large files, few syscalls).
const copyBufSize = 1 << 20

// isJSONBody reports whether the request body is declared as JSON.
func isJSONBody(r *http.Request) bool {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return ct == "application/json"
}

// nativeUpload reports whether r is a desktop-mode path upload.
func (s *Server) nativeUpload(r *http.Request) bool {
	return s.desktop != nil && isJSONBody(r)
}

// pathRequest is the decoded body of a path upload.
type pathRequest struct {
	path   string
	fields map[string]string
}

// parsePathRequest decodes {"path": "...", "<field>": "<text>", ...}. Text
// fields are what the multipart form would carry (title, lecture_id); null
// fields are left out. Every upload endpoint takes one file, so "paths"
// is refused like a second file part.
func parsePathRequest(b []byte) (pathRequest, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil || raw == nil {
		return pathRequest{}, apiErr(http.StatusBadRequest, "request.bad_json", nil)
	}
	pr := pathRequest{fields: map[string]string{}}
	for k, v := range raw {
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			continue
		}
		if k == "paths" {
			return pr, apiErr(http.StatusBadRequest, "upload.multiple_files", nil)
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return pr, apiErr(http.StatusBadRequest, "request.bad_param", map[string]any{"name": k})
		}
		if k == "path" {
			pr.path = s
			continue
		}
		if len(s) > maxFieldBytes {
			return pr, apiErr(http.StatusBadRequest, "upload.field_too_long", map[string]any{"field": k})
		}
		pr.fields[k] = s
	}
	if pr.path == "" {
		return pr, apiErr(http.StatusBadRequest, "upload.no_file", nil)
	}
	return pr, nil
}

// readPathRequest reads and decodes the JSON body of a path upload.
func readPathRequest(w http.ResponseWriter, r *http.Request) (pathRequest, error) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPathBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return pathRequest{}, apiErr(http.StatusBadRequest, "request.bad_json", nil)
		}
		return pathRequest{}, uploadReadErr(err)
	}
	return parsePathRequest(b)
}

// jsonHasPath reports whether a JSON object body carries a "path" string
// (POST /syllabus/raw takes JSON {text} as well).
func jsonHasPath(b []byte) bool {
	var v struct {
		Path *string `json:"path"`
	}
	return json.Unmarshal(b, &v) == nil && v.Path != nil
}

// receivePath is receiveUpload for a desktop-mode path upload.
func (s *Server) receivePath(w http.ResponseWriter, r *http.Request, dir, base string, accept func(name string) error) (uploadResult, error) {
	pr, err := readPathRequest(w, r)
	if err != nil {
		return uploadResult{}, err
	}
	return s.copyFromPath(r.Context(), pr, dir, base, accept)
}

// copyFromPath copies the file at pr.path to dir/base+ext after the checks
// of an HTTP upload. A cancelled ctx stops the copy within one buffer; no
// file is left behind on any error.
func (s *Server) copyFromPath(ctx context.Context, pr pathRequest, dir, base string, accept func(name string) error) (uploadResult, error) {
	res := uploadResult{fields: pr.fields}
	src, info, err := s.checkSourcePath(pr.path)
	if err != nil {
		return res, err
	}
	name := filepath.Base(pr.path)
	if accept != nil {
		if err := accept(name); err != nil {
			return res, err
		}
	}
	if info.Size() > s.maxUpload {
		return res, apiErr(http.StatusRequestEntityTooLarge, "upload.too_large", map[string]any{"max_bytes": s.maxUpload})
	}
	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		return res, err
	}
	if free, err := s.freeSpace(s.dataDir); err == nil && free < s.minFree+info.Size() {
		return res, apiErr(http.StatusInsufficientStorage, "upload.no_space", map[string]any{"free_bytes": free, "keep_bytes": s.minFree})
	}
	f, err := os.Open(src)
	if err != nil {
		return res, sourceOpenErr(pr.path, err)
	}
	defer f.Close()
	// The file may have been swapped since the checks: check what was opened.
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return res, apiErr(http.StatusBadRequest, "upload.path_not_file", map[string]any{"path": pr.path})
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, err
	}
	res.filename = name
	res.path = filepath.Join(dir, base+strings.ToLower(filepath.Ext(name)))
	tmp := res.path + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return res, err
	}
	// writerOnly hides os.File.ReadFrom so that the copy goes through the
	// buffer and the context check below.
	n, err := io.CopyBuffer(writerOnly{out}, ctxReader{ctx, io.LimitReader(f, s.maxUpload+1)}, make([]byte, copyBufSize))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > s.maxUpload {
		err = apiErr(http.StatusRequestEntityTooLarge, "upload.too_large", map[string]any{"max_bytes": s.maxUpload})
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = os.Rename(tmp, res.path)
	}
	if err != nil {
		os.Remove(tmp)
		var he *httpError
		if errors.As(err, &he) || ctx.Err() != nil {
			return res, err
		}
		return res, apiErrDetail(http.StatusInternalServerError, "upload.copy_failed", map[string]any{"path": pr.path}, err.Error())
	}
	res.size = n
	return res, nil
}

// checkSourcePath validates a path sent by the desktop page: absolute, no
// device or extended-length prefix (UNC network paths are fine), an
// existing regular file outside the data directory (also after following
// links). It returns the resolved path and its info.
func (s *Server) checkSourcePath(p string) (string, fs.FileInfo, error) {
	bad := func(code string, status int) error {
		return apiErr(status, code, map[string]any{"path": p})
	}
	if p == "" || strings.ContainsRune(p, 0) || !filepath.IsAbs(p) || weirdPathPrefix(p) {
		return "", nil, bad("upload.path_invalid", http.StatusBadRequest)
	}
	clean := filepath.Clean(p)
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", nil, sourceOpenErr(p, err)
	}
	if !filepath.IsAbs(resolved) {
		resolved, _ = filepath.Abs(resolved)
	}
	if s.underDataDir(clean) || s.underDataDir(resolved) {
		return "", nil, bad("upload.path_in_data_dir", http.StatusBadRequest)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", nil, sourceOpenErr(p, err)
	}
	if !info.Mode().IsRegular() {
		return "", nil, bad("upload.path_not_file", http.StatusBadRequest)
	}
	return resolved, info, nil
}

// weirdPathPrefix reports Windows device (\\.\) and extended-length (\\?\,
// \??\) paths, in either slash direction. UNC paths (\\server\share) pass.
func weirdPathPrefix(p string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	q := strings.ReplaceAll(p, "/", `\`)
	return strings.HasPrefix(q, `\\.\`) || strings.HasPrefix(q, `\\?\`) || strings.HasPrefix(q, `\??\`) ||
		q == `\\.` || q == `\\?`
}

// underDataDir reports whether p is the data directory or lies inside it
// (with the data directory's links followed as well). Copying such a file
// would copy the app's own data onto itself.
func (s *Server) underDataDir(p string) bool {
	if s.dataDir == "" {
		return false
	}
	roots := []string{}
	if abs, err := filepath.Abs(s.dataDir); err == nil {
		roots = append(roots, abs)
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			roots = append(roots, real)
		}
	}
	for _, root := range roots {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)) {
			return true
		}
	}
	return false
}

// sourceOpenErr maps a failure to find or open the source file.
func sourceOpenErr(p string, err error) error {
	params := map[string]any{"path": p}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return apiErr(http.StatusNotFound, "upload.path_not_found", params)
	case errors.Is(err, fs.ErrPermission):
		return apiErrDetail(http.StatusForbidden, "upload.path_unreadable", params, err.Error())
	}
	// e.g. a sharing violation (the file is open in another program) or a
	// network share that went away
	return apiErrDetail(http.StatusForbidden, "upload.path_unreadable", params, err.Error())
}

// ctxReader stops reading once ctx is done (the client went away).
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

type writerOnly struct{ w io.Writer }

func (w writerOnly) Write(p []byte) (int, error) { return w.w.Write(p) }
