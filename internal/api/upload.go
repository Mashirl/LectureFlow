// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Streaming multipart uploads (SPEC §17.4): parts are read one by one with
// multipart.Reader and the file is copied straight to disk; the request is
// never parsed into memory (no ParseMultipartForm).

const maxFieldBytes = 64 << 10

// uploadResult is what receiveUpload read.
type uploadResult struct {
	fields   map[string]string // text fields (in any order relative to the file)
	filename string            // the client's file name (base name only)
	path     string            // final path of the stored file
	size     int64
}

// checkSpace refuses an upload when the partition of the data directory
// would have less than minFree left (SPEC §17.5: 507).
func (s *Server) checkSpace(r *http.Request) error {
	if r.ContentLength > s.maxUpload+maxFieldBytes*16 {
		return apiErr(http.StatusRequestEntityTooLarge, "upload.too_large", map[string]any{"max_bytes": s.maxUpload})
	}
	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		return err
	}
	free, err := s.freeSpace(s.dataDir)
	if err != nil {
		return nil // unknown: do not block the user
	}
	need := s.minFree
	if r.ContentLength > 0 {
		need += r.ContentLength
	}
	if free < need {
		return apiErr(http.StatusInsufficientStorage, "upload.no_space", map[string]any{"free_bytes": free, "keep_bytes": s.minFree})
	}
	return nil
}

// receiveUpload streams the multipart body. The part named "file" is
// written to dir/base+ext (ext from the client's file name, lower case) via
// a .tmp file renamed on success. accept validates the client's file name
// before anything is written. Any error leaves no file behind. In desktop
// mode a JSON body {"path": ...} is copied from that local file instead
// (SPEC-v3 V1.4, upload_path.go).
func (s *Server) receiveUpload(w http.ResponseWriter, r *http.Request, dir, base string, accept func(name string) error) (uploadResult, error) {
	if s.nativeUpload(r) {
		return s.receivePath(w, r, dir, base, accept)
	}
	var res uploadResult
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUpload+maxFieldBytes*16)
	mr, err := r.MultipartReader()
	if err != nil {
		return res, apiErr(http.StatusBadRequest, "upload.not_multipart", nil)
	}
	res.fields = map[string]string{}
	var tmp string
	cleanup := func() {
		if tmp != "" {
			os.Remove(tmp)
		}
	}
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanup()
			return res, uploadReadErr(err)
		}
		if p.FormName() != "file" {
			b, err := io.ReadAll(io.LimitReader(p, maxFieldBytes+1))
			p.Close()
			if err != nil {
				cleanup()
				return res, uploadReadErr(err)
			}
			if len(b) > maxFieldBytes {
				cleanup()
				return res, apiErr(http.StatusBadRequest, "upload.field_too_long", map[string]any{"field": p.FormName()})
			}
			res.fields[p.FormName()] = string(b)
			continue
		}
		if tmp != "" {
			p.Close()
			cleanup()
			return res, apiErr(http.StatusBadRequest, "upload.multiple_files", nil)
		}
		name := filepath.Base(strings.ReplaceAll(p.FileName(), "\\", "/"))
		if name == "" || name == "." || name == "/" {
			p.Close()
			return res, apiErr(http.StatusBadRequest, "upload.no_filename", nil)
		}
		if accept != nil {
			if err := accept(name); err != nil {
				p.Close()
				return res, err
			}
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			p.Close()
			return res, err
		}
		res.filename = name
		res.path = filepath.Join(dir, base+strings.ToLower(filepath.Ext(name)))
		tmp = res.path + ".tmp"
		f, err := os.Create(tmp)
		if err != nil {
			p.Close()
			return res, err
		}
		n, err := io.Copy(f, io.LimitReader(p, s.maxUpload+1))
		cerr := f.Close()
		p.Close()
		if err == nil {
			err = cerr
		}
		if err != nil {
			cleanup()
			return res, uploadReadErr(err)
		}
		if n > s.maxUpload {
			cleanup()
			return res, apiErr(http.StatusRequestEntityTooLarge, "upload.too_large", map[string]any{"max_bytes": s.maxUpload})
		}
		res.size = n
	}
	if tmp == "" {
		return res, apiErr(http.StatusBadRequest, "upload.no_file", nil)
	}
	if err := os.Rename(tmp, res.path); err != nil {
		cleanup()
		return res, err
	}
	return res, nil
}

func uploadReadErr(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return apiErr(http.StatusRequestEntityTooLarge, "upload.too_large", map[string]any{"max_bytes": mbe.Limit})
	}
	return apiErrDetail(http.StatusBadRequest, "upload.interrupted", nil, err.Error())
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// dirSize sums the sizes of the regular files under dir (0 if missing).
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}
