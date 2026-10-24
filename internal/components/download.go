// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"lectureflow/internal/model"
)

// downloader fetches content-addressed files into dir: dir/{sha256}.part
// while downloading (resumed with an HTTP Range request), dir/{sha256} once
// the SHA-256 matched.
type downloader struct {
	client  *http.Client
	dir     string
	backoff time.Duration // first delay before retrying a broken transfer
	stall   time.Duration // no bytes for this long = the transfer broke
	log     *slog.Logger
}

// fileSpec is a file to download.
type fileSpec struct {
	Name   string
	SHA256 string
	Size   int64
	URLs   []sourceURL // tried in order (chosen mirror, then default)
}

// maxNoProgress: consecutive broken transfers without any new byte before a
// download fails (network) and the next source is tried.
const maxNoProgress = 3

// errRestart: the server's answer does not continue the partial file; it
// was truncated and the transfer starts again.
var errRestart = errors.New("the server did not resume the partial download")

type httpStatusError struct{ status int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP status %d", e.status) }

func (d *downloader) finalPath(sha string) string { return filepath.Join(d.dir, sha) }
func (d *downloader) partPath(sha string) string  { return filepath.Join(d.dir, sha+".part") }

// have reports whether the verified file exists.
func (d *downloader) have(f fileSpec) bool {
	fi, err := os.Stat(d.finalPath(f.SHA256))
	return err == nil && fi.Mode().IsRegular() && fi.Size() == f.Size
}

// partial returns the bytes of a partial download.
func (d *downloader) partial(f fileSpec) int64 {
	if fi, err := os.Stat(d.partPath(f.SHA256)); err == nil && fi.Size() <= f.Size {
		return fi.Size()
	}
	return 0
}

// remove deletes the file and its partial download.
func (d *downloader) remove(sha string) {
	os.Remove(d.finalPath(sha))
	os.Remove(d.partPath(sha))
}

// fetch makes dir/{sha256} exist with the right content and returns its
// path. onBytes receives the bytes of this file present so far. A failing
// mirror source falls back to the next URL (fallback is called with the
// reason); disk-full and cancellation stop at once.
func (d *downloader) fetch(ctx context.Context, f fileSpec, onBytes func(int64),
	fallback func(u sourceURL, reason model.ErrorInfo)) (string, error) {
	final := d.finalPath(f.SHA256)
	if d.have(f) {
		onBytes(f.Size)
		return final, nil
	}
	os.Remove(final)
	if err := os.MkdirAll(d.dir, 0o755); err != nil {
		return "", asCoded(err, nil)
	}
	var lastErr error
	for i, u := range f.URLs {
		err := d.fetchFrom(ctx, u.URL, f, onBytes)
		if err == nil {
			return final, nil
		}
		if c := context.Cause(ctx); c != nil {
			return "", c
		}
		lastErr = asCoded(err, map[string]any{"file": f.Name})
		if CodeOf(lastErr) == CodeDiskFull || i == len(f.URLs)-1 {
			break
		}
		d.log.Warn("components: download source failed; trying the next one",
			"file", f.Name, "url", u.URL, "error", err)
		if fallback != nil {
			fallback(u, ErrorInfoOf(lastErr))
		}
	}
	if lastErr == nil {
		lastErr = cerr(CodeNetwork, map[string]any{"file": f.Name}, errors.New("no download URL"))
	}
	return "", lastErr
}

// fetchFrom downloads from one URL and verifies the SHA-256; on a mismatch
// the file is deleted and downloaded once more (V3.3).
func (d *downloader) fetchFrom(ctx context.Context, url string, f fileSpec, onBytes func(int64)) error {
	part := d.partPath(f.SHA256)
	var got string
	for attempt := 0; attempt < 2; attempt++ {
		h, err := d.download(ctx, url, part, f.Size, onBytes)
		if err != nil {
			return err
		}
		got = hex.EncodeToString(h.Sum(nil))
		if got == f.SHA256 {
			if err := renameRetry(part, d.finalPath(f.SHA256)); err != nil {
				return cerr(CodeIO, nil, err)
			}
			return nil
		}
		d.log.Warn("components: SHA-256 mismatch; deleting the file", "file", f.Name, "url", url,
			"got", got, "want", f.SHA256, "attempt", attempt+1)
		os.Remove(part)
		onBytes(0)
	}
	return cerr(CodeChecksum, map[string]any{"file": f.Name},
		fmt.Errorf("%s: SHA-256 %s, expected %s", f.Name, got, f.SHA256))
}

// download completes part to size bytes (resuming what is there) and returns
// the SHA-256 state of its content.
func (d *downloader) download(ctx context.Context, url, part string, size int64, onBytes func(int64)) (hash.Hash, error) {
	h := sha256.New()
	n, err := hashExisting(part, size, h)
	if err != nil {
		return nil, err
	}
	onBytes(n)
	noProgress := 0
	for {
		if n == size {
			return h, nil
		}
		before := n
		n, err = d.once(ctx, url, part, n, size, h, onBytes)
		if err == nil {
			continue
		}
		if c := context.Cause(ctx); c != nil {
			return nil, c
		}
		if !transient(err) {
			return nil, err
		}
		if n > before {
			noProgress = 0
		} else {
			noProgress++
		}
		if noProgress >= maxNoProgress {
			return nil, err
		}
		d.log.Info("components: download interrupted; resuming", "url", url, "at", n, "error", err)
		delay := d.backoff << uint(noProgress)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
}

// hashExisting hashes a partial file (truncating one longer than size) and
// returns its length.
func hashExisting(part string, size int64, h hash.Hash) (int64, error) {
	f, err := os.Open(part)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, asCoded(err, nil)
	}
	n, err := io.Copy(h, f)
	f.Close()
	if err != nil {
		return 0, asCoded(err, nil)
	}
	if n > size {
		h.Reset()
		if err := os.Truncate(part, 0); err != nil {
			return 0, asCoded(err, nil)
		}
		return 0, nil
	}
	return n, nil
}

func transient(err error) bool {
	var hs *httpStatusError
	if errors.As(err, &hs) {
		return hs.status >= 500 || hs.status == http.StatusTooManyRequests || hs.status == http.StatusRequestTimeout
	}
	if errors.Is(err, errRestart) {
		return true
	}
	return classify(err) == CodeNetwork
}

// once performs one request from offset n and returns the new length.
func (d *downloader) once(ctx context.Context, url, part string, n, size int64, h hash.Hash, onBytes func(int64)) (int64, error) {
	rctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
	if err != nil {
		return n, cerr(CodeNetwork, nil, err)
	}
	req.Header.Set("User-Agent", "LectureFlow-components")
	req.Header.Set("Accept-Encoding", "identity")
	if n > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", n))
	}
	stall := time.AfterFunc(d.stall, func() { cancel(errStalled) })
	defer stall.Stop()
	resp, err := d.client.Do(req)
	if err != nil {
		if context.Cause(rctx) == errStalled && ctx.Err() == nil {
			return n, errStalled
		}
		return n, err
	}
	defer resp.Body.Close()

	restart := func() (int64, error) {
		h.Reset()
		if err := os.Truncate(part, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
			return n, asCoded(err, nil)
		}
		onBytes(0)
		return 0, nil
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		if n > 0 { // the server ignored Range: start over
			if n, err = restart(); err != nil {
				return n, err
			}
		}
	case resp.StatusCode == http.StatusPartialContent:
		start, ok := contentRangeStart(resp.Header.Get("Content-Range"))
		if !ok || start != n {
			if _, err := restart(); err != nil {
				return n, err
			}
			return 0, errRestart
		}
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		if _, err := restart(); err != nil {
			return n, err
		}
		return 0, errRestart
	default:
		return n, cerr(CodeHTTPStatus, map[string]any{"status": resp.StatusCode}, &httpStatusError{resp.StatusCode})
	}

	out, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return n, asCoded(err, nil)
	}
	buf := make([]byte, 256<<10)
	var werr error
	for {
		k, rerr := resp.Body.Read(buf)
		if k > 0 {
			stall.Reset(d.stall)
			if n+int64(k) > size {
				werr = cerr(CodeChecksum, nil, fmt.Errorf("the server sent more than the expected %d bytes", size))
				out.Close()
				h.Reset()
				os.Truncate(part, 0)
				onBytes(0)
				return 0, werr
			}
			if _, err := out.Write(buf[:k]); err != nil {
				out.Close()
				os.Truncate(part, n) // keep the file equal to what was hashed
				return n, asCoded(err, nil)
			}
			h.Write(buf[:k])
			n += int64(k)
			onBytes(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			werr = rerr
			if context.Cause(rctx) == errStalled && ctx.Err() == nil {
				werr = errStalled
			}
			break
		}
	}
	if err := out.Sync(); err != nil && werr == nil {
		werr = asCoded(err, nil)
	}
	if err := out.Close(); err != nil && werr == nil {
		werr = asCoded(err, nil)
	}
	if werr == nil && n < size {
		werr = io.ErrUnexpectedEOF // the body ended early: resume
	}
	return n, werr
}

// errStalled: no bytes arrived for the stall timeout.
var errStalled = &net.OpError{Op: "read", Err: errors.New("download stalled")}

func contentRangeStart(v string) (int64, bool) {
	// "bytes 100-199/200"
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "bytes ") {
		return 0, false
	}
	v = strings.TrimPrefix(v, "bytes ")
	dash := strings.IndexByte(v, '-')
	if dash <= 0 {
		return 0, false
	}
	s, err := strconv.ParseInt(v[:dash], 10, 64)
	return s, err == nil
}

// sha256File hashes a file.
func sha256File(path string, onBytes func(int64)) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	var n int64
	for {
		k, err := f.Read(buf)
		if k > 0 {
			h.Write(buf[:k])
			n += int64(k)
			if onBytes != nil {
				onBytes(n)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", n, err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func defaultHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 60 * time.Second
	tr.DisableCompression = true
	return &http.Client{Transport: tr}
}
