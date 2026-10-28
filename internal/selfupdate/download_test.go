// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPDownloaderResumeAndFallback(t *testing.T) {
	content := pkgContent("dl")
	var plain atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(content))
		case "/norange": // ignores Range
			plain.Add(1)
			w.Write(content)
		case "/broken": // breaks off after 1000 bytes, then resumes properly
			if r.Header.Get("Range") == "" {
				// declares the whole file, sends 1000 bytes: the connection breaks
				w.Header().Set("Content-Length", strconv.Itoa(len(content)))
				w.WriteHeader(http.StatusOK)
				w.Write(content[:1000])
				return
			}
			http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(content))
		case "/other":
			w.Header().Set("Content-Length", strconv.Itoa(len(content)-5))
			w.Write(content[:len(content)-5])
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	d := &HTTPDownloader{Backoff: time.Millisecond}
	run := func(name string, urls ...string) (string, error) {
		p := filepath.Join(dir, name)
		var last int64
		err := d.Download(context.Background(), DownloadRequest{URLs: urls, Path: p, Size: int64(len(content)),
			Progress: func(n int64) { last = n }})
		if err == nil && last != int64(len(content)) {
			t.Errorf("%s: progress ended at %d", name, last)
		}
		return p, err
	}
	check := func(p string) {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(b, content) {
			t.Fatalf("%s: wrong content (%v)", p, err)
		}
	}
	p, err := run("a", srv.URL+"/missing", srv.URL+"/ok")
	if err != nil {
		t.Fatal(err)
	}
	check(p)
	// resume from a partial file
	p = filepath.Join(dir, "b")
	os.WriteFile(p, content[:5000], 0o644)
	if _, err := run("b", srv.URL+"/ok"); err != nil {
		t.Fatal(err)
	}
	check(p)
	// a partial file the server cannot resume: start over
	p = filepath.Join(dir, "c")
	os.WriteFile(p, []byte("garbage"), 0o644)
	if _, err := run("c", srv.URL+"/norange"); err != nil {
		t.Fatal(err)
	}
	check(p)
	// a transfer that breaks off is resumed
	if p, err = run("d", srv.URL+"/broken"); err != nil {
		t.Fatal(err)
	}
	check(p)
	// a file of another size is not taken; the next URL is
	if p, err = run("e", srv.URL+"/other", srv.URL+"/ok"); err != nil {
		t.Fatal(err)
	}
	check(p)
	// every URL failing
	if _, err := run("f", srv.URL+"/missing", srv.URL+"/other"); err == nil {
		t.Fatal("no error")
	}
	// a partial file longer than the package is emptied first
	p = filepath.Join(dir, "g")
	os.WriteFile(p, append(bytes.Clone(content), 1, 2, 3), 0o644)
	if _, err := run("g", srv.URL+"/ok"); err != nil {
		t.Fatal(err)
	}
	check(p)
	// cancellation
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.Download(ctx, DownloadRequest{URLs: []string{srv.URL + "/ok"}, Path: filepath.Join(dir, "h"),
		Size: int64(len(content))}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	if err := d.Download(context.Background(), DownloadRequest{Path: filepath.Join(dir, "i"), Size: 1}); err == nil {
		t.Fatal("no URL accepted")
	}
}

func TestHTTPClientDropsRefererAndCookies(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.SetCookie(w, &http.Cookie{Name: "track", Value: "me"})
			http.Redirect(w, r, "/end", http.StatusFound)
			return
		}
		got = r.Header.Clone()
	}))
	defer srv.Close()
	req, _ := newRequest(context.Background(), srv.URL+"/start")
	resp, err := NewHTTPClient(time.Second).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.Get("Referer") != "" || got.Get("Cookie") != "" || got.Get("User-Agent") != UserAgent {
		t.Fatalf("headers after a redirect: %v", got)
	}
}

// Redirects keep to the addresses a source may have: https, or plain http
// on this computer; never from https down to http (also on the same host).
// A refused redirect is not retried.
func TestHTTPClientRefusesPlainRedirects(t *testing.T) {
	content := pkgContent("redir")
	var hits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(content))
		case "/to-tls":
			http.Redirect(w, r, r.URL.Query().Get("to"), http.StatusFound)
		case "/to-ok":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/to-elsewhere":
			hits.Add(1)
			http.Redirect(w, r, "http://update-mirror.invalid/f", http.StatusFound)
		}
	}))
	defer plain.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(content))
		case "/down":
			hits.Add(1)
			http.Redirect(w, r, plain.URL+"/ok", http.StatusFound)
		}
	}))
	defer tls.Close()
	client := NewHTTPClient(10 * time.Second)
	client.Transport.(*http.Transport).TLSClientConfig = tls.Client().Transport.(*http.Transport).TLSClientConfig
	get := func(u string) error {
		req, _ := newRequest(context.Background(), u)
		res, err := client.Do(req)
		if err == nil {
			res.Body.Close()
		}
		return err
	}
	for _, u := range []string{plain.URL + "/to-ok", plain.URL + "/to-tls?to=" + tls.URL + "/ok"} {
		if err := get(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	for _, u := range []string{tls.URL + "/down", plain.URL + "/to-elsewhere"} {
		if err := get(u); !errors.Is(err, errInsecureRedirect) {
			t.Errorf("%s: %v, want the redirect refused", u, err)
		}
	}
	// the downloader gives such a URL up at once and takes the next
	hits.Store(0)
	path := filepath.Join(t.TempDir(), "f.part")
	d := &HTTPDownloader{Client: client, Backoff: time.Millisecond}
	err := d.Download(context.Background(), DownloadRequest{URLs: []string{tls.URL + "/down", plain.URL + "/to-elsewhere", tls.URL + "/ok"},
		Path: path, Size: int64(len(content))})
	if err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Fatalf("%d requests to the redirecting URLs, want 2 (no retry)", hits.Load())
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, content) {
		t.Fatal("wrong content")
	}
}

// The detail under "查看详情" has no Windows line ends (aria2c) and no
// blank lines.
func TestInfoOfTidiesDetail(t *testing.T) {
	err := cerr(CodeDownloadFailed, nil, errors.New("https://x.invalid/a.exe: Network problem\r\nretry later \r\n\r\n"))
	if got := InfoOf(err).Detail; got != "https://x.invalid/a.exe: Network problem\nretry later" {
		t.Fatalf("detail %q", got)
	}
	if got := InfoOf(errors.New("disk full\r\n")); got.Code != CodeIO || got.Detail != "disk full" {
		t.Fatalf("plain error %+v", got)
	}
	// an update.json that is too large names its URL once
	f := newFakeGH(t)
	k := newTestKey(t)
	f.set(latestPath, bytes.Repeat([]byte(" "), maxManifestBytes+1))
	u := newTestUpdater(t, f, k, nil)
	st, _ := u.Check(bg)
	if st.Error == nil || st.Error.Code != CodeUnreachable || strings.Count(st.Error.Detail, latestPath) != 1 ||
		!strings.Contains(st.Error.Detail, "more than") {
		t.Fatalf("too large: %+v", st.Error)
	}
}
