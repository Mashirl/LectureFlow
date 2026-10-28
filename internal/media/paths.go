// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package media

import (
	"regexp"
	"runtime"
	"strings"
)

// schemeRe is a URL scheme of two or more characters ("file", "smb"; a
// drive letter "C" is one).
var schemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]+$`)

// UnsafeStoredPath reports whether p, a file path stored in the database
// (lecture.media_path, material.path, syllabus.path), names something other
// than a plain local file: a UNC path (\\server\share\…, //server/share/…),
// a Win32 device or extended-length path (\\.\…, \\?\…, \??\…, including
// \\?\UNC\server\…; \\?\C:\… on a local drive is fine) or a URL
// (file://server/…, smb://…). The app stats such a path whenever it lists
// the lecture and ffmpeg opens it; on Windows that connects to the server
// with the user's NTLM credentials. The app itself only ever stores plain
// paths. The rule is the same on every system (a database moves between
// systems; no LectureFlow path starts like this on macOS either).
func UnsafeStoredPath(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	q := strings.ReplaceAll(p, "/", `\`)
	if strings.HasPrefix(q, `\\?\`) || strings.HasPrefix(q, `\??\`) {
		rest := q[4:]
		return !(len(rest) >= 3 && isASCIILetter(rest[0]) && rest[1] == ':' && rest[2] == '\\')
	}
	if strings.HasPrefix(q, `\\`) {
		return true
	}
	if i := strings.IndexByte(p, ':'); i >= 2 && schemeRe.MatchString(p[:i]) {
		return true
	}
	return false
}

func isASCIILetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

// WithinDir reports whether p is dir or lies inside it, comparing the
// paths as text: separators unified, trailing separators ignored, case
// ignored on Windows. Nothing is resolved or touched on disk (p may be a
// network path that must not be opened).
func WithinDir(p, dir string) bool {
	norm := func(s string) string {
		s = strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(s), "/", `\`), `\`)
		if runtime.GOOS == "windows" {
			s = strings.ToLower(s)
		}
		return s
	}
	a, d := norm(p), norm(dir)
	if d == "" || a == "" {
		return false
	}
	return a == d || strings.HasPrefix(a, d+`\`)
}

// ForeignStoredPath reports whether p, a path stored in the database, names
// another computer (UnsafeStoredPath) and does not lie inside root, the
// data directory (UnderRoot). A data directory on a network share is the
// user's own choice: the app opens its database there anyway, so the paths
// inside it are not a leak; everything else on another computer is. The
// one rule for the app's own checks (state.StoredPathUnsafe, the startup
// scan) and the data import (internal/api import_paths.go). Nothing is
// resolved or touched on disk.
func ForeignStoredPath(p, root string) bool {
	return UnsafeStoredPath(p) && !UnderRoot(p, root)
}

// UnderRoot reports whether p is root or lies inside it; root is an
// absolute directory (the data directory). It decides whether a network
// path may be opened, so unlike WithinDir it is strict, and it is purely
// lexical (nothing is resolved or touched on disk):
//   - separators are unified (/ and \), repeated separators and "."
//     components dropped, a trailing separator ignored; on Windows ASCII
//     letters compare without case (non-ASCII ones must match exactly:
//     Unicode case folding is not what Windows or DNS does);
//   - \\?\UNC\server\share\… is the same as \\server\share\…, and
//     \\?\C:\… the same as C:\…;
//   - a ".." component anywhere means not inside: Windows resolves it
//     differently per prefix (not at all after \\?\, up to the device root
//     after \\.\), so no lexical rule can follow it, and the app never
//     stores one (its paths are joined, hence clean);
//   - a path with leading or trailing white space, a relative or
//     drive-relative path, a URL and any other device path (\\.\…, \??\…,
//     \\?\GLOBALROOT\…) are never inside, and no root of that kind has
//     anything inside it.
func UnderRoot(p, root string) bool {
	pv, pp, ok := rootedPath(p)
	if !ok {
		return false
	}
	rv, rp, ok := rootedPath(root)
	if !ok || !sameName(pv, rv) || len(pp) < len(rp) {
		return false
	}
	for i := range rp {
		if !sameName(pp[i], rp[i]) {
			return false
		}
	}
	return true
}

// rootedPath splits an absolute path into its volume (\\server\share,
// C:, or \ for a path rooted without one) and its components, for
// UnderRoot; ok is false for anything UnderRoot never trusts.
func rootedPath(p string) (vol string, parts []string, ok bool) {
	if p == "" || strings.TrimSpace(p) != p {
		return "", nil, false
	}
	q := strings.ReplaceAll(p, "/", `\`)
	if strings.HasPrefix(q, `\\?\`) {
		switch rest := q[4:]; {
		case len(rest) >= 4 && strings.EqualFold(rest[:4], `UNC\`):
			q = `\\` + rest[4:]
		case isDriveRoot(rest):
			q = rest
		default:
			return "", nil, false
		}
	}
	var tail string
	switch {
	case strings.HasPrefix(q, `\\`):
		s := strings.SplitN(q[2:], `\`, 3)
		if len(s) < 2 || !plainName(s[0]) || !plainName(s[1]) {
			return "", nil, false
		}
		vol = `\\` + s[0] + `\` + s[1]
		if len(s) == 3 {
			tail = s[2]
		}
	case isDriveRoot(q):
		vol, tail = q[:2], q[3:]
	case strings.HasPrefix(q, `\??\`):
		return "", nil, false // an NT path (\??\UNC\… is a share)
	case strings.HasPrefix(q, `\`):
		vol, tail = `\`, q[1:]
	default:
		return "", nil, false
	}
	for _, c := range strings.Split(tail, `\`) {
		switch c {
		case "", ".":
		case "..":
			return "", nil, false
		default:
			parts = append(parts, c)
		}
	}
	return vol, parts, true
}

// isDriveRoot: s starts with a drive letter, a colon and a separator.
func isDriveRoot(s string) bool {
	return len(s) >= 3 && isASCIILetter(s[0]) && s[1] == ':' && s[2] == '\\'
}

// plainName: a server or share name of a UNC path, not a device prefix
// (\\.\, \\?\) or a dot component.
func plainName(s string) bool {
	return s != "" && s != "." && s != ".." && s != "?"
}

// sameName compares two path components (or volumes): ASCII case is
// ignored on Windows, nothing else is folded.
func sameName(a, b string) bool {
	if runtime.GOOS != "windows" {
		return a == b
	}
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
