// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a semantic version (semver.org 2.0): MAJOR.MINOR.PATCH with an
// optional pre-release; build metadata is kept for display but ignored by
// Compare.
type Version struct {
	Major, Minor, Patch int
	Pre                 []string
	Build               string
}

// ParseVersion parses "1.2.3", "1.2.3-beta.1", "1.2.3+build" or "v1.2.3".
func ParseVersion(s string) (Version, error) {
	var v Version
	in := strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(in, '+'); i >= 0 {
		v.Build = in[i+1:]
		in = in[:i]
		if v.Build == "" {
			return Version{}, fmt.Errorf("invalid version %q: empty build metadata", s)
		}
	}
	if i := strings.IndexByte(in, '-'); i >= 0 {
		pre := in[i+1:]
		in = in[:i]
		if pre == "" {
			return Version{}, fmt.Errorf("invalid version %q: empty pre-release", s)
		}
		v.Pre = strings.Split(pre, ".")
		for _, p := range v.Pre {
			if p == "" {
				return Version{}, fmt.Errorf("invalid version %q: empty pre-release identifier", s)
			}
		}
	}
	parts := strings.Split(in, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("invalid version %q: want MAJOR.MINOR.PATCH", s)
	}
	nums := make([]int, 3)
	for i, p := range parts {
		if p == "" || (len(p) > 1 && p[0] == '0') {
			return Version{}, fmt.Errorf("invalid version %q", s)
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("invalid version %q", s)
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	return v, nil
}

// String formats the version.
func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		s += "-" + strings.Join(v.Pre, ".")
	}
	if v.Build != "" {
		s += "+" + v.Build
	}
	return s
}

// Compare returns -1, 0 or 1 (semver precedence; build metadata ignored).
func (v Version) Compare(o Version) int {
	for _, d := range [][2]int{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(v.Pre) == 0 && len(o.Pre) == 0:
		return 0
	case len(v.Pre) == 0:
		return 1
	case len(o.Pre) == 0:
		return -1
	}
	for i := 0; i < len(v.Pre) && i < len(o.Pre); i++ {
		a, b := v.Pre[i], o.Pre[i]
		an, aerr := strconv.Atoi(a)
		bn, berr := strconv.Atoi(b)
		switch {
		case aerr == nil && berr == nil:
			if an != bn {
				if an < bn {
					return -1
				}
				return 1
			}
		case aerr == nil:
			return -1 // numeric identifiers have lower precedence
		case berr == nil:
			return 1
		default:
			if c := strings.Compare(a, b); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(v.Pre) < len(o.Pre):
		return -1
	case len(v.Pre) > len(o.Pre):
		return 1
	}
	return 0
}

// CompareVersions compares two version strings; an unparsable version sorts
// before every valid one and two unparsable ones compare as strings.
func CompareVersions(a, b string) int {
	va, ea := ParseVersion(a)
	vb, eb := ParseVersion(b)
	switch {
	case ea == nil && eb == nil:
		return va.Compare(vb)
	case ea != nil && eb != nil:
		return strings.Compare(a, b)
	case ea != nil:
		return -1
	}
	return 1
}

// VersionRange is a set of versions: comparator sets joined by "||", each a
// space-separated list of ">=1.2.3", ">1.2.3", "<=1.2.3", "<1.2.3", "=1.2.3"
// or "1.2.3" that all must hold; "*" or "" matches everything.
type VersionRange struct {
	raw  string
	sets [][]comparator
}

type comparator struct {
	op string
	v  Version
}

// ParseRange parses a VersionRange.
func ParseRange(s string) (VersionRange, error) {
	r := VersionRange{raw: strings.TrimSpace(s)}
	if r.raw == "" || r.raw == "*" {
		return r, nil
	}
	for _, alt := range strings.Split(r.raw, "||") {
		var set []comparator
		for _, f := range strings.Fields(alt) {
			op := ""
			for _, o := range []string{">=", "<=", ">", "<", "="} {
				if strings.HasPrefix(f, o) {
					op = o
					break
				}
			}
			v, err := ParseVersion(strings.TrimPrefix(f, op))
			if err != nil {
				return VersionRange{}, fmt.Errorf("invalid version range %q: %w", s, err)
			}
			if op == "" {
				op = "="
			}
			set = append(set, comparator{op, v})
		}
		if len(set) == 0 {
			return VersionRange{}, fmt.Errorf("invalid version range %q: empty alternative", s)
		}
		r.sets = append(r.sets, set)
	}
	return r, nil
}

// Contains reports whether v is in the range.
func (r VersionRange) Contains(v Version) bool {
	if len(r.sets) == 0 {
		return true
	}
	for _, set := range r.sets {
		ok := true
		for _, c := range set {
			d := v.Compare(c.v)
			switch c.op {
			case ">=":
				ok = d >= 0
			case ">":
				ok = d > 0
			case "<=":
				ok = d <= 0
			case "<":
				ok = d < 0
			default:
				ok = d == 0
			}
			if !ok {
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// String returns the range as written.
func (r VersionRange) String() string { return r.raw }
