// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"fmt"
	"strings"
)

// Source groups selectable by the user (V3.3: Python packages and models
// independently).
const (
	GroupPyPI   = "pypi"
	GroupModels = "models"
)

// SourceDefault selects a group's default source.
const SourceDefault = "default"

// Sources is the user's download source choice per group: "" or "default",
// the id of one of the manifest's mirrors (e.g. "tuna", "hf-mirror"), or a
// custom http(s) URL that replaces the group's primary placeholder ({pypi}:
// a package index, {models}: a Hugging Face endpoint).
type Sources struct {
	PyPI   string `json:"pypi,omitempty"`
	Models string `json:"models,omitempty"`
}

func (s Sources) choice(group string) string {
	switch group {
	case GroupPyPI:
		return s.PyPI
	case GroupModels:
		return s.Models
	}
	return ""
}

// ValidateSources checks the choices against the manifest's sources.
func (m *Manifest) ValidateSources(s Sources) error {
	for _, g := range []string{GroupPyPI, GroupModels} {
		c := s.choice(g)
		if c == "" || c == SourceDefault || isHTTPURL(c) {
			continue
		}
		sg, ok := m.Sources[g]
		found := false
		if ok {
			for _, mr := range sg.Mirrors {
				found = found || mr.ID == c
			}
		}
		if !found {
			return cerr(CodeManifestInvalid, map[string]any{"group": g, "source": c},
				fmt.Errorf("unknown %s source %q", g, c))
		}
	}
	return nil
}

// sourceURL is one URL to try for a download.
type sourceURL struct {
	URL    string
	Group  string // "" when the template has no placeholder
	Mirror string // the mirror id or custom URL; "" = the default source
}

// resolveURL expands a template for the chosen sources: the chosen mirror
// first (when one is chosen), then the default source.
func (m *Manifest) resolveURL(tmpl string, s Sources) ([]sourceURL, error) {
	ph := placeholderOf(tmpl)
	if ph == "" {
		return []sourceURL{{URL: tmpl}}, nil
	}
	name, g := m.groupOf(ph)
	if g == nil {
		return nil, fmt.Errorf("unknown placeholder {%s}", ph)
	}
	expand := func(base string) string {
		return strings.TrimRight(base, "/") + tmpl[len(ph)+2:]
	}
	var out []sourceURL
	if c := s.choice(name); c != "" && c != SourceDefault {
		base := ""
		if isHTTPURL(c) {
			if ph == g.Primary {
				base = c
			}
		} else {
			for _, mr := range g.Mirrors {
				if mr.ID == c {
					base = mr.Values[ph]
				}
			}
		}
		if base != "" {
			out = append(out, sourceURL{URL: expand(base), Group: name, Mirror: c})
		}
	}
	def := g.Default.Values[ph]
	if def == "" {
		if len(out) == 0 {
			return nil, fmt.Errorf("no source for {%s}", ph)
		}
		return out, nil
	}
	if len(out) == 0 || out[0].URL != expand(def) {
		out = append(out, sourceURL{URL: expand(def), Group: name})
	}
	return out, nil
}

// indexURLs are the package indexes for uv: the chosen one first, then the
// default.
func (m *Manifest) indexURLs(s Sources) ([]sourceURL, error) {
	g, ok := m.Sources[GroupPyPI]
	if !ok {
		return nil, fmt.Errorf("the manifest has no %q source group", GroupPyPI)
	}
	return m.resolveURL("{"+g.Primary+"}", s)
}
