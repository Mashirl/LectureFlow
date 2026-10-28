// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package agent

// Downloads of the kit's files: resumable (HTTP Range on the .part file),
// size-limited, SHA-256 checked; https only (http to a loopback host in the
// tests, remote.KitSpec.Validate). The finished file stays in downloads/
// until the caller moves it.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lectureflow/internal/remote"
)

// httpClient is the downloads' client (tests replace it).
var httpClient = &http.Client{
	Timeout: 0, // large files: the per-read idle timeout below bounds a stall
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	},
}

const downloadTries = 4

// download fetches f into downloads/<sha256>.part and returns the path of
// the checked file (downloads/<sha256>).
func (e *env) download(ctx context.Context, f remote.KitDownload, progress func(float64)) (string, error) {
	dir := filepath.Join(e.dirs.Root, dirDownloads)
	final, err := inside(dir, f.SHA256)
	if err != nil {
		return "", err
	}
	if n, ok := regularFile(final); ok && n == f.Size {
		if sum, err := fileSHA256(final); err == nil && sum == f.SHA256 {
			return final, nil
		}
		_ = os.Remove(final)
	}
	part := final + ".part"
	var last error
	for try := 0; try < downloadTries; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(try*try) * 2 * time.Second):
			}
		}
		last = e.fetch(ctx, f, part, progress)
		if last == nil {
			break
		}
		if errors.Is(last, errHashMismatch) {
			_ = os.Remove(part)
		}
	}
	if last != nil {
		if errors.Is(last, errHashMismatch) {
			return "", coded(remote.AgentHashMismatch, last)
		}
		return "", coded(remote.AgentDownloadFailed, last)
	}
	if err := os.Rename(part, final); err != nil {
		return "", err
	}
	return final, nil
}

var errHashMismatch = errors.New("SHA-256 mismatch")

func (e *env) fetch(ctx context.Context, f remote.KitDownload, part string, progress func(float64)) error {
	have, _ := regularFile(part)
	if have > f.Size {
		_ = os.Remove(part)
		have = 0
	}
	if have < f.Size {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "lectureflow-agent/"+version())
		if have > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		switch {
		case have > 0 && resp.StatusCode == http.StatusPartialContent:
		case resp.StatusCode == http.StatusOK:
			have = 0 // the server ignored the range: start over
		default:
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		flags := os.O_WRONLY | os.O_CREATE
		if have == 0 {
			flags |= os.O_TRUNC
		} else {
			flags |= os.O_APPEND
		}
		fh, err := os.OpenFile(part, flags, 0o600)
		if err != nil {
			return err
		}
		w := &progressWriter{w: fh, n: have, total: f.Size, report: progress}
		_, err = io.Copy(w, &idleReader{r: io.LimitReader(resp.Body, f.Size-have+1), idle: 2 * time.Minute})
		if cerr := fh.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	n, _ := regularFile(part)
	if n != f.Size {
		if n > f.Size {
			_ = os.Remove(part)
		}
		return fmt.Errorf("size %d, want %d", n, f.Size)
	}
	sum, err := fileSHA256(part)
	if err != nil {
		return err
	}
	if sum != f.SHA256 {
		return errHashMismatch
	}
	return nil
}

type progressWriter struct {
	w        io.Writer
	n, total int64
	report   func(float64)
	last     time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.n += int64(n)
	if p.report != nil && p.total > 0 && time.Since(p.last) > time.Second {
		p.last = time.Now()
		p.report(min(float64(p.n)/float64(p.total), 1))
	}
	return n, err
}

// idleReader fails a read that takes longer than idle (a stalled download).
type idleReader struct {
	r    io.Reader
	idle time.Duration
}

func (i *idleReader) Read(b []byte) (int, error) {
	type res struct {
		n   int
		err error
	}
	ch := make(chan res, 1)
	go func() {
		n, err := i.r.Read(b)
		ch <- res{n, err}
	}()
	select {
	case r := <-ch:
		return r.n, r.err
	case <-time.After(i.idle):
		return 0, errors.New("download stalled")
	}
}

// untarGz unpacks a .tar.gz into dst: regular files, directories and
// symbolic links whose target stays inside dst; no absolute paths, no "..",
// no devices. Modes keep their execute bits, never set-id bits.
func untarGz(archive, dst string) error {
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
	type link struct{ path, target string }
	var hard []link
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(strings.TrimPrefix(h.Name, "./"))
		if name == "" || name == "." {
			continue
		}
		if filepath.IsAbs(name) || strings.HasPrefix(name, `\`) || hasDotDot(name) {
			return fmt.Errorf("unsafe path in archive")
		}
		p := filepath.Join(dst, name)
		if !within(dst, p) {
			return fmt.Errorf("unsafe path in archive")
		}
		mode := os.FileMode(h.Mode).Perm() & 0o755
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return err
			}
			out, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, io.LimitReader(tr, h.Size))
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			t := h.Linkname
			if filepath.IsAbs(t) || strings.HasPrefix(t, "/") {
				return fmt.Errorf("unsafe link in archive")
			}
			if !within(dst, filepath.Join(filepath.Dir(p), filepath.FromSlash(t))) {
				return fmt.Errorf("unsafe link in archive")
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return err
			}
			if err := os.Symlink(t, p); err != nil {
				return err
			}
		case tar.TypeLink:
			hard = append(hard, link{p, filepath.Join(dst, filepath.FromSlash(h.Linkname))})
		default:
			// devices, fifos and the like: skipped
		}
	}
	for _, l := range hard {
		if !within(dst, l.target) {
			return fmt.Errorf("unsafe link in archive")
		}
		b, err := os.ReadFile(l.target)
		if err != nil {
			return err
		}
		fi, _ := os.Lstat(l.target)
		mode := os.FileMode(0o600)
		if fi != nil {
			mode = fi.Mode().Perm()
		}
		if err := os.WriteFile(l.path, b, mode); err != nil {
			return err
		}
	}
	return nil
}

func hasDotDot(p string) bool {
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return true
		}
	}
	return false
}

// within reports whether p is dst or below it (lexically).
func within(dst, p string) bool {
	rel, err := filepath.Rel(dst, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
