// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"regexp"
	"strings"
)

// License text recognition. A text is normalised (lower case, quotes
// unified, runs of white space and markup collapsed) and matched against
// signature phrases of each license. The checks run in priority order, so
// that e.g. the LGPL (whose text mentions the GPL) wins over the GPL.
//
// The GNU texts carry no "only"/"or later" information; they are reported
// as "-only", the conservative reading (GPL-2.0-only then fails and needs an
// allowlist entry that cites the "or later" notice of the source files).

type signature struct {
	id  string
	all []string // every phrase must occur
	any []string // at least one must occur (when non-empty)
	not []string // none may occur
}

var signatures = []signature{
	{id: "SSPL-1.0", any: []string{"server side public license"}},
	{id: "BUSL-1.1", any: []string{"business source license"}},
	{id: "CC-BY-NC-4.0", any: []string{"attribution-noncommercial 4.0", "attribution-noncommercial-sharealike 4.0", "attribution-noncommercial-noderivatives 4.0"}},
	{id: "LicenseRef-NonCommercial", any: []string{"attribution-noncommercial", "polyform noncommercial", "for non-commercial use only", "for noncommercial use only"}},
	{id: "AGPL-3.0-only", all: []string{"gnu affero general public license version 3"}},
	{id: "LGPL-3.0-only", all: []string{"gnu lesser general public license version 3, 29 june 2007"}},
	{id: "LGPL-2.1-only", all: []string{"gnu lesser general public license version 2.1, february 1999"}},
	{id: "LGPL-2.0-only", all: []string{"gnu library general public license version 2, june 1991"}},
	{id: "GPL-3.0-only", all: []string{"gnu general public license version 3, 29 june 2007"}},
	{id: "GPL-2.0-only", all: []string{"gnu general public license version 2, june 1991"}},
	{id: "MPL-2.0", any: []string{"mozilla public license version 2.0", "mozilla public license, version 2.0", "mozilla public license, v. 2.0"}},
	{id: "Apache-2.0", any: []string{"apache license version 2.0, january 2004", "licensed under the apache license, version 2.0"}},
	{id: "BSL-1.0", any: []string{"boost software license - version 1.0"}},
	{id: "Zlib", all: []string{"this software is provided 'as-is', without any express or implied warranty", "altered source versions must be plainly marked as such"}},
	{id: "Unlicense", any: []string{"this is free and unencumbered software released into the public domain"}},
	{id: "CC0-1.0", any: []string{"cc0 1.0 universal"}},
	{id: "Python-2.0", any: []string{"python software foundation license version 2"}},
	{id: "blessing", any: []string{"sqlite is public domain", "may you do good and not evil"}},
	{id: "ISC", all: []string{"with or without fee is hereby granted, provided that the above copyright notice and this permission notice appear in all copies"}, any: []string{"permission to use, copy, modify, and/or distribute this software", "permission to use, copy, modify, and distribute this software"}},
	{id: "0BSD", all: []string{"permission to use, copy, modify, and/or distribute this software for any purpose with or without fee is hereby granted"}},
	{id: "BSD-4-Clause", all: []string{"redistribution and use in source and binary forms", "all advertising materials mentioning features or use of this software"}},
	{id: "BSD-3-Clause", all: []string{"redistribution and use in source and binary forms"}, any: []string{"neither the name", "may not be used to endorse or promote", "neither the names"}},
	{id: "BSD-2-Clause", all: []string{"redistribution and use in source and binary forms", "redistributions in binary form must reproduce"}},
	{id: "MIT", all: []string{"permission is hereby granted, free of charge, to any person obtaining a copy", "the above copyright notice and this permission notice shall be included"}},
	{id: "MIT-0", all: []string{"permission is hereby granted, free of charge, to any person obtaining a copy"}},
	{id: "CC-BY-4.0", all: []string{"creative commons attribution 4.0 international"}},
}

var (
	reSpace  = regexp.MustCompile(`\s+`)
	reMarkup = regexp.MustCompile("[*#>`|]+")
)

func normalizeText(s string) string {
	s = strings.ToLower(s)
	r := strings.NewReplacer("‘", "'", "’", "'", "“", "\"", "”", "\"", "–", "-", "—", "-", " ", " ")
	s = r.Replace(s)
	s = reMarkup.ReplaceAllString(s, " ")
	return reSpace.ReplaceAllString(s, " ")
}

func (sg signature) match(n string) bool {
	for _, p := range sg.all {
		if !strings.Contains(n, p) {
			return false
		}
	}
	if len(sg.any) > 0 {
		hit := false
		for _, p := range sg.any {
			if strings.Contains(n, p) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	for _, p := range sg.not {
		if strings.Contains(n, p) {
			return false
		}
	}
	return true
}

// Identify returns the license of a single license file, or "" when the
// text is not recognised.
//
// When a file bundles several licenses (a package's own license followed by
// those of vendored libraries), the license that starts first wins; ties go
// to the earlier signature (LGPL before GPL, BSD-3 before BSD-2, ...).
func Identify(text string) string {
	n := normalizeText(text)
	best, bestPos := "", -1
	for _, sg := range signatures {
		if !sg.match(n) {
			continue
		}
		if pos := sg.position(n); bestPos < 0 || pos < bestPos {
			best, bestPos = sg.id, pos
		}
	}
	return best
}

// position is where the first matched phrase of the signature starts.
func (sg signature) position(n string) int {
	pos := -1
	for _, ps := range [][]string{sg.all, sg.any} {
		for _, p := range ps {
			if i := strings.Index(n, p); i >= 0 && (pos < 0 || i < pos) {
				pos = i
			}
		}
	}
	return pos
}

// IdentifyAll returns every license recognised in a text that bundles
// several (third-party notice files). Families are reduced: a BSD-3 match
// suppresses BSD-2, MIT suppresses MIT-0, ISC suppresses 0BSD.
func IdentifyAll(text string) []string {
	n := normalizeText(text)
	seen := map[string]bool{}
	var out []string
	for _, sg := range signatures {
		if sg.match(n) {
			seen[sg.id] = true
		}
	}
	suppress := map[string]string{"BSD-2-Clause": "BSD-3-Clause", "MIT-0": "MIT", "0BSD": "ISC", "LicenseRef-NonCommercial": "CC-BY-NC-4.0"}
	for _, sg := range signatures {
		if !seen[sg.id] {
			continue
		}
		if s, ok := suppress[sg.id]; ok && seen[s] {
			continue
		}
		if sg.id == "BSD-3-Clause" && seen["BSD-4-Clause"] {
			continue
		}
		out = append(out, sg.id)
	}
	return out
}

// joinAnd combines license ids into one AND expression without duplicates,
// keeping the first-seen order.
func joinAnd(ids ...string) string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		for _, p := range splitAnd(id) {
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	return strings.Join(out, " AND ")
}

// splitAnd splits a top-level AND chain without OR or parentheses; anything
// more complex is kept whole (parenthesised) so the meaning is preserved.
func splitAnd(expr string) []string {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil
	}
	toks := tokenize(expr)
	for _, t := range toks {
		if t == "(" || strings.EqualFold(t, "OR") {
			return []string{"(" + expr + ")"}
		}
	}
	var out []string
	cur := []string{}
	for _, t := range toks {
		if strings.EqualFold(t, "AND") {
			out = append(out, strings.Join(cur, " "))
			cur = cur[:0]
			continue
		}
		cur = append(cur, t)
	}
	return append(out, strings.Join(cur, " "))
}
