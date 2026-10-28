// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Where update.json comes from (README.md "来源与镜像"). In order:
//
//  1. GitHub (when Repo is set). Stable channel:
//     https://github.com/<repo>/releases/latest/download/update.json (and
//     .minisig). Beta channel: first the newest release with an
//     update.json found through the public releases API
//     (https://api.github.com/repos/<repo>/releases), then the stable URL.
//  2. The extra sources of the settings, in their order. Each is one of
//     - a proxy template containing {url}: "https://proxy.example/{url}";
//       {url} is replaced with the GitHub URL (also for the packages);
//     - a URL ending in .json: that update.json, its signature next to it
//       (+".minisig"), the packages in the same directory;
//     - any other URL: a directory with update.json, update.json.minisig
//       and the packages.
//
// Every source is asked (at the same time) and checked against the same
// built-in key, so a mirror cannot change anything; of the update.json
// files that verify, the newest one this channel may offer wins (security
// review sec-update: a mirror that serves an old release, or none, cannot
// hide a newer one another source has). Sources are https; plain http only
// to this computer (127.0.0.1, ::1, localhost), since on the way anyone
// could see what is fetched and hold back newer releases. Packages are
// downloaded from the source that delivered update.json first, then from
// the URL in update.json, the other sources and update.json's own mirror
// rules (https, or http to this computer, too).

const (
	srcGitHub    = "github"     // releases/latest/download
	srcGitHubAPI = "github-api" // the releases API (beta channel)
	srcTemplate  = "template"   // a {url} proxy
	srcDir       = "dir"        // a directory (or an update.json URL)

	maxSources   = 8
	maxSourceLen = 512
	urlToken     = "{url}"
)

type source struct {
	kind     string
	label    string // the user's text, or "github"
	manifest string // update.json URL (not for srcGitHubAPI)
	sig      string
	tmpl     string // srcTemplate
	base     string // srcDir: the packages' directory, ending in "/"
	api      string // srcGitHubAPI: the releases list URL
}

// assetURL is this source's URL of a package ("" = none).
func (s *source) assetURL(a Asset) string {
	switch s.kind {
	case srcTemplate:
		return strings.Replace(s.tmpl, urlToken, a.URL, 1)
	case srcDir:
		return s.base + url.PathEscape(a.Name)
	}
	return a.URL
}

// parseSource validates one extra source of the settings. githubLatest is
// the default update.json URL a template wraps ("" = no repository: a
// template source has nothing to fetch).
func parseSource(raw, githubLatest string) (*source, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxSourceLen || strings.ContainsAny(raw, " \t\r\n") {
		return nil, errors.New("empty, too long or contains spaces")
	}
	if n := strings.Count(raw, urlToken); n > 0 {
		if n > 1 {
			return nil, errors.New("{url} more than once")
		}
		// the proxy's own address comes first: "https://proxy.example/{url}"
		prefix := raw[:strings.Index(raw, urlToken)]
		probe := strings.Replace(raw, urlToken, "https://github.com/o/r/releases/download/v1.0.0/f.exe", 1)
		if pu, err := url.Parse(prefix + "x"); err != nil || !isHTTPURL(probe, false) || !secureURL(prefix+"x") || pu.Host == "" {
			return nil, errors.New("not an https URL")
		}
		s := &source{kind: srcTemplate, label: raw, tmpl: raw}
		if githubLatest != "" {
			s.manifest = strings.Replace(raw, urlToken, githubLatest, 1)
			s.sig = strings.Replace(raw, urlToken, githubLatest+".minisig", 1)
		}
		return s, nil
	}
	if !secureURL(raw) {
		return nil, errors.New("not an https URL")
	}
	u, _ := url.Parse(raw)
	// (a bare "?" or "#" leaves the query or fragment empty but would still
	// be part of every URL made from the source)
	if u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || strings.ContainsAny(raw, "?#") {
		return nil, errors.New("a directory source cannot have a query or fragment")
	}
	s := &source{kind: srcDir, label: raw}
	if strings.HasSuffix(strings.ToLower(u.Path), ".json") {
		s.manifest = raw
		s.sig = raw + ".minisig"
		s.base = raw[:strings.LastIndex(raw, "/")+1]
		return s, nil
	}
	if !strings.HasSuffix(raw, "/") {
		raw += "/"
	}
	s.base = raw
	s.manifest = raw + manifestFile
	s.sig = raw + signatureFile
	return s, nil
}

// secureURL: an https URL, or an http URL of this computer (loopback
// address or localhost; a mirror or cache on the same machine). Plain http
// to anywhere else would let anyone on the way see which release is
// fetched and serve an older update.json (still signed) to hold back a
// newer one.
func secureURL(s string) bool {
	if !isHTTPURL(s, false) {
		return false
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	h := u.Hostname()
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// normalizeSources validates and de-duplicates the extra sources; the
// error carries the index of the first bad one.
func normalizeSources(in []string, githubLatest string) ([]string, error) {
	if len(in) > maxSources {
		return nil, cerr(CodeSourcesInvalid, map[string]any{"index": maxSources}, fmt.Errorf("at most %d sources", maxSources))
	}
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for i, s := range in {
		s = strings.TrimSpace(s)
		if _, err := parseSource(s, githubLatest); err != nil {
			return nil, cerr(CodeSourcesInvalid, map[string]any{"index": i}, err)
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out, nil
}

// githubLatest is the stable update.json URL ("" without a repository).
func (u *Updater) githubLatest() string {
	if u.repo == "" {
		return ""
	}
	return u.web + "/" + u.repo + "/releases/latest/download/" + manifestFile
}

// sources are the sources to ask, in order.
func (u *Updater) sources(extra []string, channel string) []*source {
	var out []*source
	latest := u.githubLatest()
	if latest != "" {
		if channel == ChannelBeta {
			out = append(out, &source{kind: srcGitHubAPI, label: "github-api",
				api: u.api + "/repos/" + u.repo + "/releases?per_page=20"})
		}
		out = append(out, &source{kind: srcGitHub, label: "github", manifest: latest, sig: latest + ".minisig"})
	}
	for _, raw := range extra {
		s, err := parseSource(raw, latest)
		if err != nil || s.manifest == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// fetch gets a small file (at most limit bytes).
func (u *Updater) fetch(ctx context.Context, rawURL string, limit int64, accept string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, u.reqTimeout)
	defer cancel()
	req, err := newRequest(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &httpStatusError{resp.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("more than %d bytes", limit) // (the caller names the URL)
	}
	return b, nil
}

// fetchErr classifies a failed request for update.json or its signature:
// the server answered that there is no such file (404, 410: the latest
// release has no update.json or no signature yet; typically a release
// whose signature the maintainer has not uploaded) is
// selfupdate.no_manifest, anything else (no connection, a timeout, a server
// error, a refused redirect) selfupdate.unreachable.
func fetchErr(rawURL string, err error) error {
	err = fmt.Errorf("%s: %w", redactURL(rawURL), err)
	var hs *httpStatusError
	if errors.As(err, &hs) && (hs.status == http.StatusNotFound || hs.status == http.StatusGone) {
		return cerr(CodeNoManifest, nil, err)
	}
	return cerr(CodeUnreachable, nil, err)
}

// fetchFrom gets update.json and its signature from one source and
// verifies them.
func (u *Updater) fetchFrom(ctx context.Context, s *source) (*Verified, error) {
	manifestURL, sigURL := s.manifest, s.sig
	if s.kind == srcGitHubAPI {
		var err error
		if manifestURL, sigURL, err = u.newestRelease(ctx, s.api); err != nil {
			return nil, err
		}
	}
	raw, err := u.fetch(ctx, manifestURL, maxManifestBytes, "")
	if err != nil {
		return nil, fetchErr(manifestURL, err)
	}
	sig, err := u.fetch(ctx, sigURL, maxSignatureBytes, "")
	if err != nil {
		return nil, fetchErr(sigURL, err)
	}
	m, err := verifyManifest(u.pk, raw, sig)
	if err != nil {
		return nil, err
	}
	return &Verified{Manifest: m, Raw: raw, Signature: sig, Source: s.label, source: s}, nil
}

// newestRelease finds, through the public releases API, the release with
// the highest version that has update.json and its signature, and returns
// their download URLs (which must be this repository's release assets).
func (u *Updater) newestRelease(ctx context.Context, api string) (string, string, error) {
	b, err := u.fetch(ctx, api, 4<<20, "application/vnd.github+json")
	if err != nil {
		return "", "", fetchErr(api, err)
	}
	var rels []struct {
		Tag    string `json:"tag_name"`
		Draft  bool   `json:"draft"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(b, &rels); err != nil {
		return "", "", cerr(CodeUnreachable, nil, fmt.Errorf("releases API: %w", err))
	}
	prefix := u.web + "/" + u.repo + "/releases/download/"
	best, bestM, bestS := "", "", ""
	for _, r := range rels {
		if r.Draft {
			continue
		}
		m, s := "", ""
		for _, a := range r.Assets {
			if !strings.HasPrefix(a.URL, prefix) {
				continue
			}
			switch a.Name {
			case manifestFile:
				m = a.URL
			case signatureFile:
				s = a.URL
			}
		}
		if m == "" || s == "" {
			continue
		}
		v := strings.TrimPrefix(r.Tag, "v")
		if _, err := parseVersion(v); err != nil {
			continue
		}
		if best == "" || compareVersions(v, best) > 0 {
			best, bestM, bestS = v, m, s
		}
	}
	if best == "" {
		return "", "", cerr(CodeNoManifest, nil, errors.New("releases API: no published release with update.json and update.json.minisig"))
	}
	return bestM, bestS, nil
}

// compareVersions compares two valid versions.
func compareVersions(a, b string) int {
	va, _ := parseVersion(a)
	vb, _ := parseVersion(b)
	return va.Compare(vb)
}

// checkSources asks every source at once and returns, of the update.json
// files that verify, the newest one channel may offer (the first source in
// order among equal versions); when none may be offered, the first that
// verified (the check then finds nothing newer). When none verifies, the
// error is the most telling one: a bad signature or version binding
// (possible tampering) before a bad update.json before a release without
// update.json or signature (no_manifest) before an unreachable source; its
// detail lists what every source answered.
func (u *Updater) checkSources(ctx context.Context, srcs []*source, channel string) (*Verified, error) {
	type result struct {
		v       *Verified
		err     error
		elapsed time.Duration
	}
	results := make([]result, len(srcs))
	var wg sync.WaitGroup
	for i, s := range srcs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			v, err := u.fetchFrom(ctx, s)
			results[i] = result{v, err, time.Since(start)}
		}()
	}
	wg.Wait()
	if c := context.Cause(ctx); c != nil {
		return nil, c
	}
	rank := map[string]int{CodeSignatureInvalid: 5, CodeVersionMismatch: 4, CodeManifestInvalid: 3, CodeNoManifest: 2, CodeUnreachable: 1}
	var bestErr error
	var failed sourcesError
	var first, best *Verified
	for i, r := range results {
		if r.err != nil {
			u.log.Warn("selfupdate: source failed", "source", redactURL(srcs[i].label), "error", r.err,
				"elapsed", r.elapsed.Round(time.Millisecond))
			if bestErr == nil || rank[CodeOf(r.err)] > rank[CodeOf(bestErr)] {
				bestErr = r.err
			}
			failed.add(srcs[i].label, r.err)
			continue
		}
		if first == nil {
			first = r.v
		}
		if r.v.Manifest.Channel == ChannelBeta && channel != ChannelBeta {
			continue
		}
		if best == nil || compareVersions(r.v.Manifest.Version, best.Manifest.Version) > 0 {
			best = r.v
		}
	}
	if best != nil {
		for i, r := range results {
			if r.err == nil && compareVersions(r.v.Manifest.Version, best.Manifest.Version) < 0 {
				u.log.Info("selfupdate: a source offers an older release than another", "source", redactURL(srcs[i].label),
					"version", r.v.Manifest.Version, "newest", best.Manifest.Version)
			}
		}
		return best, nil
	}
	if first != nil {
		return first, nil
	}
	if bestErr == nil {
		return nil, cerr(CodeUnreachable, nil, errors.New("no source"))
	}
	info := InfoOf(bestErr)
	return nil, cerr(info.Code, info.Params, &failed)
}

// sourcesError is what every source of a failed check answered, one line
// each ("github: selfupdate.no_manifest: https://…/update.json: HTTP status
// 404"): the detail of the check's error, so that a mirror's answer is not
// hidden behind GitHub's.
type sourcesError struct {
	lines []string
	errs  []error
}

func (e *sourcesError) add(label string, err error) {
	e.lines = append(e.lines, redactURL(label)+": "+tidyDetail(err.Error()))
	e.errs = append(e.errs, err)
}

func (e *sourcesError) Error() string   { return strings.Join(e.lines, "\n") }
func (e *sourcesError) Unwrap() []error { return e.errs }
