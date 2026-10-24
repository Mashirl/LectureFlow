// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Bundled media (packaging/licenses/media.json), upstreams reimplemented
// from their protocol (packaging/licenses/references.json) and repository
// copies of license texts (packaging/licenses/texts/).

// extraTableFor returns the additional accepted ids of a component kind:
// public-domain media are accepted only as media.
func extraTableFor(kind string) map[string]string {
	if kind == KindMedia {
		return mediaCompatible
	}
	return nil
}

// MediaEntry is one third-party media file shipped with LectureFlow.
type MediaEntry struct {
	Path         string   `json:"path"`
	Title        string   `json:"title"`
	Creator      string   `json:"creator"`
	Published    string   `json:"published,omitempty"`
	License      string   `json:"license"`
	LicenseBasis string   `json:"license_basis"`
	LicenseURL   string   `json:"license_url,omitempty"`
	Sources      []string `json:"sources"`
	SHA256       string   `json:"sha256"`
	Note         string   `json:"note,omitempty"`
}

// OwnFile is a file under assets/ written by the LectureFlow contributors.
type OwnFile struct {
	Path    string `json:"path"`
	License string `json:"license"`
	Note    string `json:"note,omitempty"`
}

type mediaTable struct {
	Media []MediaEntry `json:"media"`
	Own   []OwnFile    `json:"own"`
}

// fileSHA256 hashes a repository file; text files (no NUL byte) are hashed
// with CRLF normalised to LF, so a checkout with autocrlf gives the same sum.
func fileSHA256(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !bytes.Contains(b, []byte{0}) {
		b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}

// auditMedia judges media.json and checks that every file under assets/ is
// listed in it.
func auditMedia(opt Options, r *Report, al *allowlist) error {
	var mt mediaTable
	b, err := os.ReadFile(filepath.Join(opt.Root, mediaFile))
	haveTable := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if haveTable {
		if err := json.Unmarshal(b, &mt); err != nil {
			return fmt.Errorf("%s: %w", mediaFile, err)
		}
	}
	listed := map[string]bool{}
	for i := range mt.Media {
		m := mt.Media[i]
		name := filepath.ToSlash(strings.TrimSpace(m.Path))
		var extra []string
		if name == "" {
			name = fmt.Sprintf("%s entry %d", mediaFile, i+1)
			extra = append(extra, "path missing")
		} else {
			if listed[name] {
				extra = append(extra, "listed twice in "+mediaFile)
			}
			listed[name] = true
		}
		var src []string
		for _, s := range m.Sources {
			if s = strings.TrimSpace(s); s != "" {
				if !isURL(s) {
					extra = append(extra, fmt.Sprintf("source %q is not an http(s) URL", s))
				}
				src = append(src, s)
			}
		}
		if len(src) == 0 {
			extra = append(extra, "source missing")
		}
		if strings.TrimSpace(m.License) == "" {
			extra = append(extra, "license missing")
		}
		if strings.TrimSpace(m.LicenseBasis) == "" {
			extra = append(extra, "license_basis missing (why the file is under this license)")
		}
		if strings.TrimSpace(m.Title) == "" || strings.TrimSpace(m.Creator) == "" {
			extra = append(extra, "title and creator are required")
		}
		if m.LicenseURL != "" && !isURL(m.LicenseURL) {
			extra = append(extra, "license_url is not an http(s) URL")
		}
		if m.Path != "" {
			sum, err := fileSHA256(filepath.Join(opt.Root, filepath.FromSlash(name)))
			switch {
			case err != nil:
				extra = append(extra, "file not found in the repository")
			case m.SHA256 == "":
				extra = append(extra, "sha256 missing (actual "+sum+")")
			case !strings.EqualFold(m.SHA256, sum):
				extra = append(extra, "sha256 mismatch: the file changed since it was audited (actual "+sum+")")
			}
		}
		mm := m
		judge(r, al, Component{Kind: KindMedia, Name: name, License: strings.TrimSpace(m.License), Source: strings.Join(src, " "), Note: m.Note, Media: &mm}, extra...)
	}
	for i, o := range mt.Own {
		name := filepath.ToSlash(strings.TrimSpace(o.Path))
		if name == "" {
			r.fail(KindMedia, fmt.Sprintf("%s own entry %d", mediaFile, i+1), "", o.License, "path missing")
			continue
		}
		if listed[name] {
			r.fail(KindMedia, name, "", o.License, "listed twice in "+mediaFile)
		}
		listed[name] = true
		if _, err := os.Stat(filepath.Join(opt.Root, filepath.FromSlash(name))); err != nil {
			r.fail(KindMedia, name, "", o.License, "file not found in the repository")
		}
		if ok, rs := Evaluate(o.License); !ok {
			for _, s := range rs {
				r.fail(KindMedia, name, "", o.License, s)
			}
		}
	}

	base := filepath.Join(opt.Root, assetsDir)
	if _, err := os.Stat(base); os.IsNotExist(err) {
		if !haveTable {
			r.Skipped = append(r.Skipped, mediaFile+" (not present, no "+assetsDir+"/ directory)")
		}
		return nil
	}
	var unlisted []string
	err = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(opt.Root, p)
		rel = filepath.ToSlash(rel)
		if !listed[rel] {
			unlisted = append(unlisted, rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(unlisted)
	for _, u := range unlisted {
		r.fail(KindMedia, u, "", "", "shipped under "+assetsDir+"/ but not listed in "+mediaFile+" (add it with its source and license)")
	}
	return nil
}

// ReferenceEntry is one upstream whose protocol LectureFlow reimplemented
// without copying code (SPEC-v3 V14.6).
type ReferenceEntry struct {
	Name              string   `json:"name"`
	Version           string   `json:"version"`
	License           string   `json:"license"`
	Copyright         []string `json:"copyright"`
	Source            string   `json:"source"`
	Artifact          string   `json:"artifact,omitempty"`
	Used              string   `json:"used"`
	UsedBy            string   `json:"used_by"`
	Changes           string   `json:"changes"`
	Details           string   `json:"details,omitempty"`
	LicenseText       string   `json:"license_text"`
	LicenseTextSHA256 string   `json:"license_text_sha256"`
	LicenseTextSource string   `json:"license_text_source,omitempty"`
}

func auditReferences(opt Options, r *Report, al *allowlist) error {
	b, err := os.ReadFile(filepath.Join(opt.Root, refsFile))
	if os.IsNotExist(err) {
		r.Skipped = append(r.Skipped, refsFile+" (not present)")
		return nil
	}
	if err != nil {
		return err
	}
	var rf struct {
		References []ReferenceEntry `json:"references"`
	}
	if err := json.Unmarshal(b, &rf); err != nil {
		return fmt.Errorf("%s: %w", refsFile, err)
	}
	for i := range rf.References {
		e := rf.References[i]
		var extra []string
		name := strings.TrimSpace(e.Name)
		if name == "" {
			name = fmt.Sprintf("%s entry %d", refsFile, i+1)
			extra = append(extra, "name missing")
		}
		for _, f := range []struct{ v, what string }{
			{e.Version, "version"}, {e.Source, "source"}, {e.Used, "used (what was referenced)"},
			{e.UsedBy, "used_by"}, {e.Changes, "changes"}, {e.LicenseText, "license_text"},
		} {
			if strings.TrimSpace(f.v) == "" {
				extra = append(extra, f.what+" missing")
			}
		}
		hasCopyright := false
		for _, c := range e.Copyright {
			if strings.TrimSpace(c) != "" {
				hasCopyright = true
			}
		}
		if !hasCopyright {
			extra = append(extra, "copyright missing (keep the upstream copyright notice)")
		}
		if e.Source != "" && !isURL(e.Source) {
			extra = append(extra, "source is not an http(s) URL")
		}
		c := Component{Kind: KindReference, Name: name, Version: e.Version, License: strings.TrimSpace(e.License), Source: e.Source}
		if e.LicenseText != "" {
			lt, why := repoLicenseText(opt.Root, e.LicenseText, e.LicenseTextSHA256, e.License)
			if why != "" {
				extra = append(extra, why)
			} else {
				c.Texts = append(c.Texts, lt)
			}
		}
		if e.Details != "" && e.Name != "" && e.Version != "" {
			db, err := os.ReadFile(filepath.Join(opt.Root, filepath.FromSlash(e.Details)))
			switch {
			case err != nil:
				extra = append(extra, "details file "+e.Details+" not found")
			case !strings.Contains(strings.ToLower(string(db)), strings.ToLower(e.Name+" "+e.Version)):
				extra = append(extra, fmt.Sprintf("details file %s does not mention %q (version out of step?)", e.Details, e.Name+" "+e.Version))
			}
		}
		ee := e
		c.Ref = &ee
		judge(r, al, c, extra...)
	}
	return nil
}

// licenseIDs returns the license ids of an SPDX expression (canonical).
func licenseIDs(expr string) map[string]bool {
	ids := map[string]bool{}
	for _, t := range tokenize(expr) {
		if t == "(" || t == ")" || isOperator(t) {
			continue
		}
		ids[strings.ToLower(canonical(t))] = true
	}
	return ids
}

// repoLicenseText loads a repository copy of a license file, checks its
// sha256 and that the recognised license is one of the declared ones.
func repoLicenseText(root, rel, wantSum, declared string) (LicenseText, string) {
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	p := filepath.Join(root, filepath.FromSlash(rel))
	b, err := os.ReadFile(p)
	if err != nil {
		return LicenseText{}, "license text " + rel + " not found"
	}
	sum, _ := fileSHA256(p)
	if wantSum == "" {
		return LicenseText{}, "license text " + rel + ": sha256 missing (actual " + sum + ")"
	}
	if !strings.EqualFold(wantSum, sum) {
		return LicenseText{}, "license text " + rel + ": sha256 mismatch (actual " + sum + ")"
	}
	id := Identify(string(b))
	if id == "" {
		return LicenseText{}, "license text " + rel + " not recognised"
	}
	if !licenseIDs(declared)[strings.ToLower(canonical(id))] {
		return LicenseText{}, fmt.Sprintf("license text %s is %s, but the declared license is %q", rel, id, declared)
	}
	return LicenseText{File: "LICENSE, copy in " + rel, Text: string(b)}, ""
}
