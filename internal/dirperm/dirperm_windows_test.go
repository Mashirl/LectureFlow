// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package dirperm

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// sddlOf is the DACL of p in SDDL, without the "automatically inherited"
// flag (Windows sets it when a DACL is written back; it changes nothing).
// sddlToken is how SDDL strings name sid: a well-known alias where Windows
// has one (the built-in Administrator, which CI runners run as, is "LA"),
// else the SID itself.
func sddlToken(t *testing.T, sid string) string {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString("D:(A;;FA;;;" + sid + ")")
	if err != nil {
		t.Fatal(err)
	}
	s := sd.String()
	return strings.TrimSuffix(s[strings.LastIndex(s, ";")+1:], ")")
}

func sddlOf(t *testing.T, p string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	s := sd.String()
	if i := strings.Index(s, "D:"); i >= 0 {
		s = s[i:]
	}
	return autoInherited.ReplaceAllString(s, "D:$1")
}

var autoInherited = regexp.MustCompile(`^D:(P?)AI`)

// looseDir makes a directory like one under D:\ (Authenticated Users may
// modify it and what it holds; Users may read).
func looseDir(t *testing.T, parent string) string {
	t.Helper()
	me, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + me.String() + ")(A;OICI;0x1301bf;;;AU)(A;OICI;0x1200a9;;;BU)")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "loose")
	if err := createPrivate(dir, sd); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The private directory: a protected DACL with exactly the user, SYSTEM
// and the administrators, inherited by what is created below it.
func TestPrivateDACL(t *testing.T) {
	me, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "data")
	if _, err := MkdirPrivate(dir); err != nil {
		t.Fatal(err)
	}
	want := "D:P(A;OICI;FA;;;" + sddlToken(t, me.String()) + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	if got := sddlOf(t, dir); got != want {
		t.Fatalf("DACL %s, want %s", got, want)
	}
	f := filepath.Join(dir, "lectureflow.db")
	os.WriteFile(f, []byte("x"), 0o644)
	got := sddlOf(t, f)
	if !strings.Contains(got, "(A;ID;FA;;;"+sddlToken(t, me.String())+")") || strings.Contains(got, ";AU)") || strings.Contains(got, ";BU)") || strings.Contains(got, ";WD)") {
		t.Fatalf("a file inside: %s", got)
	}
}

// A directory other accounts may change is reported with their names;
// Restrict makes it private, and what it already holds inherits that;
// Save puts the old permissions back.
func TestCheckRestrictSave(t *testing.T) {
	dir := looseDir(t, t.TempDir())
	inner := filepath.Join(dir, "updates")
	os.Mkdir(inner, 0o755)
	os.WriteFile(filepath.Join(inner, "pending.json"), []byte("{}"), 0o644)

	e, err := Check(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !e.Write || !e.Read || len(e.Who) < 2 {
		t.Fatalf("loose directory: %+v", e)
	}
	if e, _ := Check(inner); !e.Write {
		t.Fatalf("what it holds inherits the loose permissions: %+v", e)
	}
	before := sddlOf(t, dir)
	restore, err := Save(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Restrict(dir); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, inner, filepath.Join(inner, "pending.json")} {
		if e, err := Check(p); err != nil || e.Exposed() {
			t.Fatalf("%s after Restrict: %+v %v (%s)", p, e, err, sddlOf(t, p))
		}
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	if got := sddlOf(t, dir); got != before {
		t.Fatalf("restored %s, was %s", got, before)
	}
	if e, _ := Check(inner); !e.Write {
		t.Fatalf("restored permissions reach the inside again: %+v", e)
	}

	// a directory that inherits from its parent: Save and the restore
	// keep it inheriting
	plain := filepath.Join(t.TempDir(), "plain")
	os.Mkdir(plain, 0o755)
	was := sddlOf(t, plain)
	restore, err = Save(plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := Restrict(plain); err != nil {
		t.Fatal(err)
	}
	if got := sddlOf(t, plain); !strings.HasPrefix(got, "D:P") {
		t.Fatalf("not protected: %s", got)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	if got := sddlOf(t, plain); got != was {
		t.Fatalf("restored %s, was %s", got, was)
	}
}

// A foreign owner can always change the permissions: exposed.
func TestCheckEveryoneAndNullDACL(t *testing.T) {
	me, _ := currentUserSID()
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + me.String() + ")(A;OICI;GA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "everyone")
	if err := createPrivate(dir, sd); err != nil {
		t.Fatal(err)
	}
	e, err := Check(dir)
	if err != nil || !e.Write || !e.Read || len(e.Who) != 1 {
		t.Fatalf("Everyone: %+v %v", e, err)
	}
	// an inherit-only entry for the files below counts as well
	sd, _ = windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + me.String() + ")(A;OIIO;FW;;;BU)")
	dir2 := filepath.Join(t.TempDir(), "inherit-only")
	if err := createPrivate(dir2, sd); err != nil {
		t.Fatal(err)
	}
	if e, err := Check(dir2); err != nil || !e.Write || e.Read {
		t.Fatalf("inherit-only write: %+v %v", e, err)
	}
	// a deny entry is not a grant; app container SIDs are not accounts
	sd, _ = windows.SecurityDescriptorFromString("D:P(D;OICI;FA;;;WD)(A;OICI;FA;;;" + me.String() + ")(A;OICI;FR;;;S-1-15-2-1)")
	dir3 := filepath.Join(t.TempDir(), "deny")
	if err := createPrivate(dir3, sd); err != nil {
		t.Fatal(err)
	}
	if e, err := Check(dir3); err != nil || e.Exposed() {
		t.Fatalf("deny / app packages: %+v %v", e, err)
	}
}
