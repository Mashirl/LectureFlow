// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// unpack extracts a tar.gz or zip archive into dst. Entries that would land
// outside dst (absolute paths, "..", links pointing out) are refused.
func unpack(ctx context.Context, archive, kind, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return asCoded(err, nil)
	}
	var err error
	switch kind {
	case "tar.gz":
		err = untarGz(ctx, archive, dst)
	case "zip":
		err = unzipAll(ctx, archive, dst)
	default:
		err = fmt.Errorf("unknown archive type %q", kind)
	}
	if err != nil {
		if c := context.Cause(ctx); c != nil {
			return c
		}
		if CodeOf(err) != "" {
			return err
		}
		if isDiskFull(err) {
			return cerr(CodeDiskFull, map[string]any{"required_mb": -1, "free_mb": -1}, err)
		}
		return cerr(CodeUnpackFailed, map[string]any{"file": filepath.Base(archive)}, err)
	}
	return nil
}

// entryPath maps an archive entry name to a path under dst.
func entryPath(dst, name string) (string, error) {
	name = strings.ReplaceAll(name, `\`, "/")
	clean := path.Clean("/" + name)
	if clean == "/" {
		return "", nil
	}
	rel := strings.TrimPrefix(clean, "/")
	if !safeRelPath(rel) || strings.HasPrefix(name, "/") || strings.Contains(name, ":") {
		return "", fmt.Errorf("unsafe archive entry %q", name)
	}
	return filepath.Join(dst, filepath.FromSlash(rel)), nil
}

type deferredLink struct{ target, at string }

func untarGz(ctx context.Context, archive, dst string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var links []deferredLink
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		p, err := entryPath(dst, h.Name)
		if err != nil {
			return err
		}
		if p == "" {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := writeEntry(p, tr, h.FileInfo().Mode()); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// the target must stay inside dst
			target := path.Join(path.Dir(strings.ReplaceAll(h.Name, `\`, "/")), h.Linkname)
			if strings.HasPrefix(h.Linkname, "/") || strings.Contains(h.Linkname, ":") ||
				target == ".." || strings.HasPrefix(target, "../") || target == "." {
				return fmt.Errorf("unsafe link %q -> %q", h.Name, h.Linkname)
			}
			links = append(links, deferredLink{target: filepath.FromSlash(h.Linkname), at: p})
		case tar.TypeLink:
			src, err := entryPath(dst, h.Linkname)
			if err != nil || src == "" {
				return fmt.Errorf("unsafe hard link %q -> %q", h.Name, h.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := linkOrCopy(src, p); err != nil {
				return err
			}
		default:
			// devices, fifos: not part of a runtime archive
		}
	}
	return makeLinks(links)
}

// makeLinks creates symbolic links after the files exist; where the system
// refuses symlinks (Windows without the privilege) the target is copied.
func makeLinks(links []deferredLink) error {
	for _, l := range links {
		if err := os.MkdirAll(filepath.Dir(l.at), 0o755); err != nil {
			return err
		}
		os.Remove(l.at)
		if err := os.Symlink(l.target, l.at); err == nil {
			continue
		} else if runtime.GOOS != "windows" {
			return err
		}
		src := filepath.Join(filepath.Dir(l.at), l.target)
		fi, err := os.Stat(src)
		if err != nil {
			return fmt.Errorf("link target %s: %w", l.target, err)
		}
		if fi.IsDir() {
			continue // directory links are not needed by a runtime on Windows
		}
		if err := linkOrCopy(src, l.at); err != nil {
			return err
		}
	}
	return nil
}

func writeEntry(p string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func unzipAll(ctx context.Context, archive, dst string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := extractZipEntry(zf, dst, zf.Name); err != nil {
			return err
		}
	}
	return nil
}

// extractZipEntry writes one zip entry to dst/rel (rel "" = the entry name).
func extractZipEntry(zf *zip.File, dst, name string) error {
	p, err := entryPath(dst, name)
	if err != nil {
		return err
	}
	if p == "" {
		return nil
	}
	if zf.FileInfo().IsDir() {
		return os.MkdirAll(p, 0o755)
	}
	if zf.Mode()&os.ModeSymlink != 0 {
		return errors.New("symbolic links are not supported in zip archives")
	}
	rc, err := zf.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	return writeEntry(p, rc, zf.Mode())
}
