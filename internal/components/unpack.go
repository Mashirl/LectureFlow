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
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// layout is how an archive's entries map to the destination: Strip
// (File.Strip) is a top-level directory whose contents land in the
// destination, an entry outside it is refused; Exclude (File.Exclude) are
// paths below the destination that are not unpacked.
type layout struct {
	Strip   string
	Exclude []string
}

func fileLayout(f File) layout { return layout{Strip: f.Strip, Exclude: f.Exclude} }

// excluded reports whether rel (relative to the destination) is left out:
// an Exclude entry ending in "/" is a directory, else one file.
func (l layout) excluded(rel string) bool {
	for _, e := range l.Exclude {
		if dir, ok := strings.CutSuffix(e, "/"); ok {
			if rel == dir || strings.HasPrefix(rel, e) {
				return true
			}
		} else if rel == e {
			return true
		}
	}
	return false
}

// unpack extracts a tar.gz or zip archive into dst. Entries that would land
// outside dst (absolute paths, "..", links pointing out) are refused; see
// layout for strip and exclude.
func unpack(ctx context.Context, archive, kind string, lay layout, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return asCoded(err, nil)
	}
	var err error
	switch kind {
	case "tar.gz":
		err = untarGz(ctx, archive, lay, dst)
	case "zip":
		err = unzipAll(ctx, archive, lay, dst)
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
	p, _, err := entryRel(dst, name, layout{})
	return p, err
}

// entryRel maps an archive entry name to a path under dst and returns its
// "/"-separated path relative to dst too; with Strip, the name must be
// inside that top-level directory, which is dropped. "" = nothing to write
// (the root, the strip directory itself, an excluded path).
func entryRel(dst, name string, lay layout) (string, string, error) {
	strip := lay.Strip
	name = strings.ReplaceAll(name, `\`, "/")
	clean := path.Clean("/" + name)
	if clean == "/" {
		return "", "", nil
	}
	rel := strings.TrimPrefix(clean, "/")
	if !safeRelPath(rel) || strings.HasPrefix(name, "/") || strings.Contains(name, ":") {
		return "", "", fmt.Errorf("unsafe archive entry %q", name)
	}
	if strip != "" {
		if rel == strip {
			return "", "", nil
		}
		r, ok := strings.CutPrefix(rel, strip+"/")
		if !ok {
			return "", "", fmt.Errorf("archive entry %q is outside %s/", name, strip)
		}
		rel = r
	}
	if lay.excluded(rel) {
		return "", "", nil
	}
	return filepath.Join(dst, filepath.FromSlash(rel)), rel, nil
}

type deferredLink struct{ target, at string }

// linkTarget checks a symbolic link's target: relative, and inside the
// destination when resolved from the entry's place (rel: the entry's path
// relative to the destination, after strip).
func linkTarget(rel, entry, linkname string) (string, error) {
	linkname = strings.ReplaceAll(linkname, `\`, "/")
	target := path.Join(path.Dir(rel), linkname)
	if linkname == "" || strings.HasPrefix(linkname, "/") || strings.Contains(linkname, ":") ||
		target == ".." || strings.HasPrefix(target, "../") || target == "." {
		return "", fmt.Errorf("unsafe link %q -> %q", entry, linkname)
	}
	return filepath.FromSlash(linkname), nil
}

func untarGz(ctx context.Context, archive string, lay layout, dst string) error {
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
		p, rel, err := entryRel(dst, h.Name, lay)
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
			target, err := linkTarget(rel, h.Name, h.Linkname)
			if err != nil {
				return err
			}
			links = append(links, deferredLink{target: target, at: p})
		case tar.TypeLink:
			src, srcRel, err := entryRel(dst, h.Linkname, layout{Strip: lay.Strip})
			if err != nil || src == "" {
				return fmt.Errorf("unsafe hard link %q -> %q", h.Name, h.Linkname)
			}
			if lay.excluded(srcRel) {
				continue // its file was left out
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
// refuses symlinks (Windows without the privilege) the target file is
// copied, a link to a directory or to nothing is left out.
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
		if errors.Is(err, fs.ErrNotExist) {
			continue // a dangling link: nothing can use it
		}
		if err != nil {
			return fmt.Errorf("link target %s: %w", l.target, err)
		}
		if fi.IsDir() {
			continue // directory links are not needed by a runtime or tool on Windows
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

// unzipAll extracts a zip archive. Symbolic links (Unix mode in the
// external attributes; the entry's data is the target) are checked like the
// ones of a tar archive and made after the files.
func unzipAll(ctx context.Context, archive string, lay layout, dst string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	var links []deferredLink
	for _, zf := range zr.File {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p, rel, err := entryRel(dst, zf.Name, lay)
		if err != nil {
			return err
		}
		if p == "" {
			continue
		}
		if zf.Mode()&os.ModeSymlink != 0 {
			target, err := readZipLink(zf)
			if err != nil {
				return err
			}
			t, err := linkTarget(rel, zf.Name, target)
			if err != nil {
				return err
			}
			links = append(links, deferredLink{target: t, at: p})
			continue
		}
		if err := writeZipEntry(zf, p); err != nil {
			return err
		}
	}
	return makeLinks(links)
}

// readZipLink reads the target of a symbolic link entry.
func readZipLink(zf *zip.File) (string, error) {
	rc, err := zf.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 4097))
	if err != nil {
		return "", err
	}
	if len(b) > 4096 {
		return "", fmt.Errorf("link %q: target too long", zf.Name)
	}
	return string(b), nil
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
	if zf.Mode()&os.ModeSymlink != 0 {
		return errors.New("symbolic links are not supported in zip archives")
	}
	return writeZipEntry(zf, p)
}

// writeZipEntry writes a directory or file entry to p.
func writeZipEntry(zf *zip.File, p string) error {
	if zf.FileInfo().IsDir() {
		return os.MkdirAll(p, 0o755)
	}
	rc, err := zf.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	return writeEntry(p, rc, zf.Mode())
}
