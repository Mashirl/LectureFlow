// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"fmt"
	"strings"
)

// The policy (SPEC-v3 V7): a third-party license is accepted only when it is
// listed below as compatible with AGPL-3.0-or-later. Everything else fails:
// GPL-2.0-only, SSPL, non-commercial terms, no license, custom
// (LicenseRef-*) and unknown identifiers. An exception needs an allowlist
// entry with a reason (packaging/licenses/allowlist.json).

// compatible lists the SPDX identifiers accepted as AGPL-3.0 compatible.
// Keys are canonical SPDX ids; the value says why (shown nowhere, kept for
// reviewers).
var compatible = map[string]string{
	"0BSD":                  "permissive",
	"AGPL-3.0-only":         "same license family",
	"AGPL-3.0-or-later":     "same license",
	"Apache-2.0":            "GPLv3-compatible (FSF)",
	"blessing":              "SQLite public-domain dedication",
	"BSD-2-Clause":          "permissive",
	"BSD-2-Clause-Patent":   "BSD-2-Clause plus an express patent grant without a termination clause; the license note says it is designed to be GPL-2.0 compatible, and GPL-3.0 accepts patent grants (https://opensource.org/license/bsdpluspatent)",
	"BSD-3-Clause":          "permissive",
	"BSD-3-Clause-Clear":    "permissive",
	"BSD-3-Clause-Open-MPI": "permissive (BSD-3 variant)",
	"BSL-1.0":               "permissive (Boost)",
	"bzip2-1.0.6":           "permissive (BSD-style)",
	"CC-BY-4.0":             "GPLv3-compatible (FSF)",
	"CC0-1.0":               "public-domain dedication",
	"GPL-2.0-or-later":      "may be used under GPL-3.0",
	"GPL-3.0-only":          "AGPL-3.0 section 13 allows combining with GPL-3.0",
	"GPL-3.0-or-later":      "AGPL-3.0 section 13 allows combining with GPL-3.0",
	"HPND":                  "permissive",
	"HPND-sell-variant":     "HPND with \"and sell\" added to the grant; FSF lists HPND as GPL-compatible (https://www.gnu.org/licenses/license-list.html#HPND, https://spdx.org/licenses/HPND-sell-variant.html)",
	"ISC":                   "permissive",
	"LGPL-2.0-only":         "section 3 allows conversion to the GPL",
	"LGPL-2.0-or-later":     "section 3 allows conversion to the GPL",
	"LGPL-2.1-only":         "section 3 allows conversion to the GPL",
	"LGPL-2.1-or-later":     "section 3 allows conversion to the GPL",
	"LGPL-3.0-only":         "GPLv3 with additional permissions",
	"LGPL-3.0-or-later":     "GPLv3 with additional permissions",
	"libpng-2.0":            "zlib-style conditions (no misrepresentation, mark altered versions, keep the notice); FSF lists the zlib license as GPL-compatible (https://www.gnu.org/licenses/license-list.html#ZLib, https://spdx.org/licenses/libpng-2.0.html)",
	"MIT":                   "permissive",
	"MIT-0":                 "permissive",
	"MIT-CMU":               "permissive (HPND family)",
	"MIT-Modern-Variant":    "OSI-approved; only condition is keeping the notice and disclaimer, as in HPND, which FSF lists as GPL-compatible (https://opensource.org/license/mit-modern-variant, https://www.gnu.org/licenses/license-list.html#HPND)",
	"MPL-2.0":               "secondary-license clause covers the GPL family",
	"PSF-2.0":               "GPL-compatible (FSF)",
	"Python-2.0":            "GPL-compatible (FSF)",
	"Unicode-3.0":           "permissive",
	"Unicode-DFS-2016":      "permissive",
	"Unlicense":             "public-domain dedication",
	"WTFPL":                 "FSF: lax permissive, compatible with the GNU GPL (https://www.gnu.org/licenses/license-list.html#WTFPL)",
	"X11":                   "permissive",
	"Zlib":                  "permissive",
	"ZPL-2.1":               "FSF: Zope Public License versions 2.0 and 2.1 are compatible with the GNU GPL (https://www.gnu.org/licenses/license-list.html#Zope2.0)",
}

// mediaCompatible are accepted for media files only (packaging/licenses/
// media.json), never for code: a bare "public domain" claim on software
// cannot be verified from a declaration, while a media entry must also give
// the basis of the claim and its source.
var mediaCompatible = map[string]string{
	"LicenseRef-Public-Domain": "no copyright, nothing to be compatible with; media entries must state the basis (e.g. LibriVox recordings and the Creative Commons Public Domain Mark 1.0, or a work published before 1900 whose author died over 100 years ago)",
}

// compatibleExceptions are the WITH exceptions that only add permissions
// (the value says why, for reviewers). An exception never makes an
// incompatible base license acceptable: the base id is judged on its own.
var compatibleExceptions = map[string]string{
	"LLVM-exception":          "adds permissions only",
	"GCC-exception-2.0":       "adds permissions only",
	"GCC-exception-3.1":       "adds permissions only",
	"Classpath-exception-2.0": "adds permissions only",
	"PCRE2-exception":         "only relaxes the BSD binary-notice condition for software that embeds a package which includes PCRE2 (https://spdx.org/licenses/PCRE2-exception.html)",
	"Bison-exception-2.2":     "only permits distributing works containing the Bison parser skeleton under terms of your choice (https://spdx.org/licenses/Bison-exception-2.2.html)",
}

// aliases maps deprecated or sloppy SPDX ids to canonical ones. The
// deprecated bare "GPL-2.0" means GPL-2.0-only, as SPDX defines it.
var aliases = map[string]string{
	"GPL-2.0":   "GPL-2.0-only",
	"GPL-2.0+":  "GPL-2.0-or-later",
	"GPL-3.0":   "GPL-3.0-only",
	"GPL-3.0+":  "GPL-3.0-or-later",
	"LGPL-2.0":  "LGPL-2.0-only",
	"LGPL-2.0+": "LGPL-2.0-or-later",
	"LGPL-2.1":  "LGPL-2.1-only",
	"LGPL-2.1+": "LGPL-2.1-or-later",
	"LGPL-3.0":  "LGPL-3.0-only",
	"LGPL-3.0+": "LGPL-3.0-or-later",
	"AGPL-3.0":  "AGPL-3.0-only",
	"AGPL-3.0+": "AGPL-3.0-or-later",
	"GPL-1.0":   "GPL-1.0-only",
	"GPL-1.0+":  "GPL-1.0-or-later",
}

// knownBad gives a precise reason for licenses that are known to be
// incompatible (anything not in compatible fails anyway).
var knownBad = map[string]string{
	"GPL-1.0-only":     "GPL-1.0 only, incompatible with AGPL-3.0",
	"GPL-1.0-or-later": "GPL-1.0 or later: needs review, not accepted automatically",
	"GPL-2.0-only":     "GPL-2.0 only (no \"or later\"), incompatible with AGPL-3.0",
	"SSPL-1.0":         "SSPL, incompatible with AGPL-3.0",
	"BUSL-1.1":         "Business Source License, not open source",
	"BSD-4-Clause":     "BSD with advertising clause, incompatible with the GPL family",
	"OpenSSL":          "OpenSSL/SSLeay license (advertising clause), incompatible with the GPL family",
	"JSON":             "JSON license (\"good, not evil\"), not free",
	"CNRI-Python":      "CNRI Python license alone is not GPL-compatible (FSF)",
	"Commons-Clause":   "Commons Clause, commercial restriction",
}

// canonical returns the canonical spelling of an SPDX id (case-insensitive
// lookup in the tables above).
func canonical(id string) string {
	if a, ok := aliases[id]; ok {
		return a
	}
	for k, v := range aliases {
		if strings.EqualFold(k, id) {
			return v
		}
	}
	for _, tbl := range []map[string]string{compatible, knownBad} {
		for k := range tbl {
			if strings.EqualFold(k, id) {
				return k
			}
		}
	}
	return id
}

// verdictForID judges a single license id; extra accepts further ids (the
// media-only table).
func verdictForID(id string, extra map[string]string) (bool, string) {
	for k := range extra {
		if strings.EqualFold(k, id) {
			return true, ""
		}
	}
	c := canonical(id)
	up := strings.ToUpper(c)
	switch {
	case c == "" || up == "NOASSERTION" || up == "NONE" || up == "UNKNOWN":
		return false, "no license"
	case strings.Contains(up, "-NC-") || strings.HasSuffix(up, "-NC") || strings.Contains(up, "NONCOMMERCIAL"):
		return false, c + ": non-commercial restriction"
	case strings.HasPrefix(up, "CC-BY-ND") || strings.HasPrefix(up, "CC-BY-SA"):
		return false, c + ": not confirmed AGPL-3.0 compatible"
	}
	if r, ok := knownBad[c]; ok {
		return false, r
	}
	if _, ok := compatible[c]; ok {
		return true, ""
	}
	if strings.HasPrefix(up, "LICENSEREF-") {
		return false, c + ": custom license, compatibility not confirmed"
	}
	return false, c + ": not in the AGPL-3.0 compatible table"
}

// Evaluate judges an SPDX license expression: OR needs one compatible
// alternative, AND needs all parts compatible, "X WITH e" needs X
// compatible and e a permission-only exception. It returns the reasons of
// the failing parts.
func Evaluate(expr string) (bool, []string) { return EvaluateWith(expr, nil) }

// EvaluateWith is Evaluate with extra accepted ids (mediaCompatible).
func EvaluateWith(expr string, extra map[string]string) (bool, []string) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return false, []string{"no license"}
	}
	p := &exprParser{toks: tokenize(expr)}
	n, err := p.parseOr()
	if err == nil && p.pos != len(p.toks) {
		err = fmt.Errorf("unexpected %q", p.toks[p.pos])
	}
	if err != nil {
		// Not an expression: judge it as one (unknown) id.
		ok, r := verdictForID(expr, extra)
		if ok {
			return true, nil
		}
		return false, []string{r}
	}
	return n.eval(extra)
}

type exprNode struct {
	op       string // "", "AND", "OR", "WITH"
	id       string
	exc      string
	children []*exprNode
}

func (n *exprNode) eval(extra map[string]string) (bool, []string) {
	switch n.op {
	case "":
		ok, r := verdictForID(n.id, extra)
		if ok {
			return true, nil
		}
		return false, []string{r}
	case "WITH":
		ok, rs := verdictForID(n.id, extra)
		if !ok {
			return false, []string{rs}
		}
		if _, ok := compatibleExceptions[n.exc]; !ok {
			return false, []string{n.id + " WITH " + n.exc + ": exception not in the compatible table"}
		}
		return true, nil
	case "AND":
		var bad []string
		for _, c := range n.children {
			if ok, r := c.eval(extra); !ok {
				bad = append(bad, r...)
			}
		}
		return len(bad) == 0, bad
	default: // OR
		var bad []string
		for _, c := range n.children {
			ok, r := c.eval(extra)
			if ok {
				return true, nil
			}
			bad = append(bad, r...)
		}
		return false, bad
	}
}

func tokenize(s string) []string {
	var toks []string
	cur := strings.Builder{}
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '(' || r == ')':
			flush()
			toks = append(toks, string(r))
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return toks
}

type exprParser struct {
	toks []string
	pos  int
}

func (p *exprParser) peekOp(op string) bool {
	return p.pos < len(p.toks) && strings.EqualFold(p.toks[p.pos], op)
}

func (p *exprParser) parseOr() (*exprNode, error) {
	n, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	if !p.peekOp("OR") {
		return n, nil
	}
	or := &exprNode{op: "OR", children: []*exprNode{n}}
	for p.peekOp("OR") {
		p.pos++
		c, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		or.children = append(or.children, c)
	}
	return or, nil
}

func (p *exprParser) parseAnd() (*exprNode, error) {
	n, err := p.parseWith()
	if err != nil {
		return nil, err
	}
	if !p.peekOp("AND") {
		return n, nil
	}
	and := &exprNode{op: "AND", children: []*exprNode{n}}
	for p.peekOp("AND") {
		p.pos++
		c, err := p.parseWith()
		if err != nil {
			return nil, err
		}
		and.children = append(and.children, c)
	}
	return and, nil
}

func (p *exprParser) parseWith() (*exprNode, error) {
	n, err := p.parseAtom()
	if err != nil {
		return nil, err
	}
	if p.peekOp("WITH") {
		if n.op != "" {
			return nil, fmt.Errorf("WITH after a compound expression")
		}
		p.pos++
		if p.pos >= len(p.toks) || isOperator(p.toks[p.pos]) {
			return nil, fmt.Errorf("WITH without an exception")
		}
		n = &exprNode{op: "WITH", id: n.id, exc: p.toks[p.pos]}
		p.pos++
	}
	return n, nil
}

func (p *exprParser) parseAtom() (*exprNode, error) {
	if p.pos >= len(p.toks) {
		return nil, fmt.Errorf("unexpected end")
	}
	t := p.toks[p.pos]
	if t == "(" {
		p.pos++
		n, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.pos >= len(p.toks) || p.toks[p.pos] != ")" {
			return nil, fmt.Errorf("missing )")
		}
		p.pos++
		return n, nil
	}
	if isOperator(t) || t == ")" {
		return nil, fmt.Errorf("unexpected %q", t)
	}
	p.pos++
	return &exprNode{id: t}, nil
}

func isOperator(t string) bool {
	return strings.EqualFold(t, "AND") || strings.EqualFold(t, "OR") || strings.EqualFold(t, "WITH")
}
