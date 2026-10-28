// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package media

import (
	"runtime"
	"testing"
)

type rootCase struct {
	p, root string
	want    bool
}

// UnderRoot (netpath-datadir): a data directory on a share trusts what lies
// inside it, compared strictly and as text; \\?\UNC\ is the same as \\;
// ".." never counts as inside.
func TestUnderRoot(t *testing.T) {
	const share = `\\nas\share\LF`
	cases := []rootCase{
		{`\\nas\share\LF\courses\c1\lectures\l1\media.mp4`, share, true},
		{`//nas/share/LF/courses/c1/media.mp4`, share, true},
		{`\\nas\share\LF`, share, true},
		{`\\nas\share\LF\`, share, true},
		{`\\nas\share\LF\courses\x.mp4`, share + `\`, true},
		{`\\nas\share\LF\courses\x.mp4`, `//nas/share/LF/`, true},
		{`\\nas\share\LF\\courses\x.mp4`, share, true}, // repeated separator
		{`\\nas\share\LF\.\courses\x.mp4`, share, true},
		{`\\nas\share\LF\courses\x.mp4`, `\\nas\share\LF\.`, true},
		{`\\nas\share`, share, false},
		{`\\nas\share\LF2\x.mp4`, share, false},
		{`\\nas\share\x.mp4`, share, false},
		{`\\other\share\LF\x.mp4`, share, false},
		{`\\nas\other\LF\x.mp4`, share, false},
		{`\\nas\share\other\LF\x.mp4`, share, false},
		// ".." escapes, and ".." that would stay inside: never inside
		{`\\nas\share\LF\..\..\evil\s\x.mp4`, share, false},
		{`\\nas\share\LF\..\LF2\x.mp4`, share, false},
		{`\\nas\share\LF\..\LF\x.mp4`, share, false},
		{`\\nas\share\LF\courses\..\x.mp4`, share, false},
		{`\\nas\share\LF\courses\..`, share, false},
		{`//nas/share/LF/../../evil/s/x.mp4`, share, false},
		// \\?\UNC\ is the same as \\ (either side)
		{`\\?\UNC\nas\share\LF\courses\x.mp4`, share, true},
		{`//?/UNC/nas/share/LF/courses/x.mp4`, share, true},
		{`\\?\unc\nas\share\LF\x.mp4`, share, true},
		{`\\nas\share\LF\courses\x.mp4`, `\\?\UNC\nas\share\LF`, true},
		{`\\?\UNC\nas\share\LF\x.mp4`, `\\?\UNC\nas\share\LF`, true},
		{`\\?\UNC\nas\share\LF\..\..\..\evil\s\x.mp4`, share, false},
		{`\\?\UNC\other\share\LF\x.mp4`, share, false},
		{`\\?\UNC\nas`, share, false},
		// other device paths, URLs and odd spellings: never inside
		{`\\.\UNC\nas\share\LF\x.mp4`, share, false},
		{`\\.\UNC\nas\share\LF\..\..\..\evil\s\x.mp4`, share, false},
		{`\??\UNC\nas\share\LF\x.mp4`, share, false},
		{`\\?\GLOBALROOT\Device\Mup\nas\share\LF\x.mp4`, share, false},
		{`\\?\UNC\?\nas\share\LF\x.mp4`, share, false},
		{`\\\nas\share\LF\x.mp4`, share, false},
		{`file://nas/share/LF/x.mp4`, share, false},
		{`file:\\nas\share\LF\x.mp4`, share, false},
		{`smb://nas/share/LF/x.mp4`, share, false},
		{`  \\nas\share\LF\x.mp4`, share, false},
		{`\\nas\share\LF\x.mp4 `, share, false},
		{"\\\\nas\\share\\LF\\x.mp4\n", share, false},
		{`LF\x.mp4`, share, false},
		{``, share, false},
		// roots that trust nothing
		{`\\nas\share\LF\x.mp4`, ``, false},
		{`\\nas\share\LF\x.mp4`, `LF`, false},
		{`\\nas\share\LF\x.mp4`, `\\nas\share\LF\..\LF`, false},
		{`\\.\UNC\nas\share\LF\x.mp4`, `\\.\UNC\nas\share\LF`, false},
		{`file://nas/share/LF/x.mp4`, `file://nas/share/LF`, false},
		// non-ASCII letters are not folded (nor is the Kelvin sign a k)
		{`\\nas\share\Ü\x.mp4`, `\\nas\share\ü`, false},
		{"\\\\nas\u212a\\share\\LF\\x.mp4", `\\nask\share\LF`, false},
		{`\\nas\share\Vorlesung-ü\x.mp4`, `\\nas\share\Vorlesung-ü`, true},
		// local roots
		{`C:\data\courses\x.mp4`, `C:\data`, true},
		{`C:/data/courses/x.mp4`, `C:\data\`, true},
		{`\\?\C:\data\courses\x.mp4`, `C:\data`, true},
		{`C:\data\courses\x.mp4`, `\\?\C:\data`, true},
		{`C:\data2\x.mp4`, `C:\data`, false},
		{`D:\data\x.mp4`, `C:\data`, false},
		{`C:\data\..\x.mp4`, `C:\data`, false},
		{`C:data\x.mp4`, `C:\`, false},
		{`\\nas\share\LF\x.mp4`, `C:\`, false},
		{`/home/u/data/courses/x.mp4`, `/home/u/data`, true},
		{`/home/u/data2/x.mp4`, `/home/u/data`, false},
		{`//nas/share/x.mp4`, `/`, false},
		{`\??\UNC\nas\share\x.mp4`, `/`, false},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases,
			rootCase{`\\NAS\Share\lf\Courses\x.mp4`, share, true},
			rootCase{`\\?\UNC\NAS\SHARE\LF\x.mp4`, `\\nas\share\lf`, true},
			rootCase{`c:\DATA\x.mp4`, `C:\data`, true})
	} else {
		cases = append(cases, rootCase{`\\NAS\Share\lf\x.mp4`, share, false})
	}
	for _, tc := range cases {
		if got := UnderRoot(tc.p, tc.root); got != tc.want {
			t.Errorf("UnderRoot(%q, %q) = %v, want %v", tc.p, tc.root, got, tc.want)
		}
	}
}

// ForeignStoredPath: unsafe (UnsafeStoredPath) and not inside the data
// directory. Without a data directory it is UnsafeStoredPath; a local data
// directory changes nothing; a data directory on a share lets only its own
// paths through.
func TestForeignStoredPath(t *testing.T) {
	paths := []string{
		`\\nas\share\LF\courses\x.mp4`, `//nas/share/LF/x.mp4`, `\\?\UNC\nas\share\LF\x.mp4`,
		`\\nas\share\LF\..\..\evil\s\x.mp4`, `\\evil\share\x.mp4`, `\\.\UNC\nas\share\LF\x.mp4`,
		`\??\UNC\nas\share\LF\x.mp4`, `file://nas/share/LF/x.mp4`, `smb://nas/share/LF/x.mp4`,
		`concat:a.mp4|b.mp4`, `\\?\C:\data\x.mp4`, `C:\data\x.mp4`, `media.mp4`, `/Users/me/x.mp4`, ``,
	}
	for _, root := range []string{``, `C:\data`, `/Users/me`} {
		for _, p := range paths {
			if got, want := ForeignStoredPath(p, root), UnsafeStoredPath(p); got != want {
				t.Errorf("ForeignStoredPath(%q, %q) = %v, want %v (UnsafeStoredPath)", p, root, got, want)
			}
		}
	}
	inside := map[string]bool{
		`\\nas\share\LF\courses\x.mp4`: true, `//nas/share/LF/x.mp4`: true, `\\?\UNC\nas\share\LF\x.mp4`: true,
	}
	for _, root := range []string{`\\nas\share\LF`, `\\?\UNC\nas\share\LF`, `//nas/share/LF/`} {
		for _, p := range paths {
			want := UnsafeStoredPath(p) && !inside[p]
			if got := ForeignStoredPath(p, root); got != want {
				t.Errorf("ForeignStoredPath(%q, %q) = %v, want %v", p, root, got, want)
			}
		}
	}
}
