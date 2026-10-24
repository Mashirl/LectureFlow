// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package relocate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// fileEntry is a regular file to copy (relative to the source root).
type fileEntry struct {
	rel    string
	size   int64 // at the scan
	copied int64 // bytes written to the target
}

// scanTree lists the regular files and directories under root (paths
// relative to root, slash-free of root itself); skip(rel, isDir) leaves an
// entry (and, for a directory, everything under it) out. A symbolic link
// to a file is copied as the file; one to a directory is an error.
func scanTree(root string, skip func(rel string, dir bool) bool) ([]fileEntry, []string, int64, error) {
	var files []fileEntry
	var dirs []string
	var total int64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		isDir := d.IsDir()
		if d.Type()&fs.ModeSymlink != 0 {
			fi, err := os.Stat(p)
			if err != nil {
				return err
			}
			if fi.IsDir() {
				return fmt.Errorf("%s: a link to a directory is not copied", p)
			}
		}
		if skip != nil && skip(rel, isDir) {
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}
		if isDir {
			dirs = append(dirs, rel)
			return nil
		}
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s: not a regular file", p)
		}
		files = append(files, fileEntry{rel: rel, size: fi.Size()})
		total += fi.Size()
		return nil
	})
	return files, dirs, total, err
}

// treeBytes is the size of the regular files under root (skip as in scanTree).
func treeBytes(root string, skip func(rel string, dir bool) bool) (int64, error) {
	_, _, n, err := scanTree(root, skip)
	return n, err
}

// tracker records what this run created in the target, so a failure removes
// exactly that and nothing else.
type tracker struct {
	created []string
}

// mkdirAll creates dir and its missing parents, recording each one created.
func (t *tracker) mkdirAll(dir string) error {
	var missing []string
	for p := dir; ; {
		if _, err := os.Lstat(p); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		missing = append(missing, p)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		t.created = append(t.created, missing[i])
	}
	return nil
}

// file records a file about to be created.
func (t *tracker) file(p string) { t.created = append(t.created, p) }

// cleanup removes what was created, newest first; directories only when
// empty (anything else in them was not ours).
func (t *tracker) cleanup() {
	for i := len(t.created) - 1; i >= 0; i-- {
		os.Remove(t.created[i])
	}
	t.created = nil
}

// copyFileHook is a test hook run before each file copy (rel path).
var copyFileHook func(rel string) error

// copyFile copies src to dst (which must not exist), keeping the
// modification time; it reports the bytes written through add.
func copyFile(ctx context.Context, t *tracker, src, dst string, add func(int64)) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return 0, err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, err
	}
	t.file(dst)
	buf := make([]byte, 1<<20)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			out.Close()
			return n, err
		}
		r, rerr := in.Read(buf)
		if r > 0 {
			w, werr := out.Write(buf[:r])
			n += int64(w)
			if add != nil {
				add(int64(w))
			}
			if werr != nil {
				out.Close()
				return n, werr
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			return n, rerr
		}
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return n, err
	}
	if err := out.Close(); err != nil {
		return n, err
	}
	_ = os.Chtimes(dst, fi.ModTime(), fi.ModTime())
	return n, nil
}

// copyTree copies the listed directories and files from src to dst.
func copyTree(ctx context.Context, t *tracker, src, dst string, dirs []string, files []fileEntry, add func(int64)) error {
	for _, d := range dirs {
		if err := t.mkdirAll(filepath.Join(dst, d)); err != nil {
			return coded(CodeCopyFailed, map[string]any{"path": d}, err)
		}
	}
	for i := range files {
		f := &files[i]
		if copyFileHook != nil {
			if err := copyFileHook(f.rel); err != nil {
				return coded(CodeCopyFailed, map[string]any{"path": f.rel}, err)
			}
		}
		if err := t.mkdirAll(filepath.Dir(filepath.Join(dst, f.rel))); err != nil {
			return coded(CodeCopyFailed, map[string]any{"path": f.rel}, err)
		}
		n, err := copyFile(ctx, t, filepath.Join(src, f.rel), filepath.Join(dst, f.rel), add)
		if err != nil {
			if ctx.Err() != nil {
				return coded(CodeCancelled, nil, ctx.Err())
			}
			return coded(CodeCopyFailed, map[string]any{"path": f.rel}, err)
		}
		if n < f.size {
			return coded(CodeCopyFailed, map[string]any{"path": f.rel}, fmt.Errorf("copied %d of %d bytes (the file shrank)", n, f.size))
		}
		f.copied = n
	}
	return nil
}

// verifyFiles checks that every copied file exists in dst with the size
// written (a file that grew while it was copied, such as a log, is compared
// with what was copied).
func verifyFiles(dst string, files []fileEntry) error {
	for _, f := range files {
		fi, err := os.Stat(filepath.Join(dst, f.rel))
		if err != nil {
			return verifyErr("file:"+filepath.ToSlash(f.rel), err)
		}
		if fi.Size() != f.copied {
			return verifyErr("file:"+filepath.ToSlash(f.rel), fmt.Errorf("%d bytes, %d copied", fi.Size(), f.copied))
		}
	}
	return nil
}

// samePath compares two clean absolute paths (ASCII case-insensitive on
// Windows).
func samePath(a, b string) bool {
	if caseInsensitive {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// within reports whether p is inside dir (not equal).
func within(p, dir string) bool {
	if caseInsensitive {
		// filepath.Rel is case-sensitive; compare lower-cased copies.
		p, dir = strings.ToLower(p), strings.ToLower(dir)
	}
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// atomicWrite writes b to path through a temporary file and a rename.
func atomicWrite(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
