// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package dirperm

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Access rights that let another account change a directory or what it
// holds (including its permissions), and those that let it read.
const (
	rightsWrite = 0x2 | 0x4 | 0x40 | // FILE_WRITE_DATA / ADD_FILE, FILE_APPEND_DATA / ADD_SUBDIRECTORY, FILE_DELETE_CHILD
		windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_WRITE | windows.GENERIC_ALL
	rightsRead = 0x1 | // FILE_READ_DATA / LIST_DIRECTORY
		windows.GENERIC_READ | windows.GENERIC_ALL
	aceAllowedCallback = 9 // ACCESS_ALLOWED_CALLBACK_ACE_TYPE: the same layout as ACCESS_ALLOWED_ACE
)

// Accounts that are not "other accounts": besides the user, SYSTEM, the
// administrators and TrustedInstaller (who can take any file anyway), and
// the placeholders CREATOR OWNER / OWNER RIGHTS (they stand for whoever
// creates or owns an entry, who already needs access of their own).
var trustedSIDs = []string{
	"S-1-5-18",     // SYSTEM
	"S-1-5-32-544", // Administrators
	"S-1-3-0",      // CREATOR OWNER
	"S-1-3-4",      // OWNER RIGHTS
	"S-1-5-80-956008885-3425870976-2393997262-3005939297", // NT SERVICE\TrustedInstaller
}

func currentUserSID() (*windows.SID, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return u.User.Sid.Copy()
}

// privateSDDL is the security descriptor of a private directory.
func privateSDDL(user *windows.SID) string {
	return "D:P(A;OICI;FA;;;" + user.String() + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
}

func privateDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	sid, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString(privateSDDL(sid))
}

// winPath is an absolute path that CreateDirectory accepts at any length.
func winPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if len(abs) < 248 || strings.HasPrefix(abs, `\\?\`) {
		return abs
	}
	if strings.HasPrefix(abs, `\\`) {
		return `\\?\UNC\` + abs[2:]
	}
	return `\\?\` + abs
}

func createPrivate(dir string, sd *windows.SECURITY_DESCRIPTOR) error {
	p, err := windows.UTF16PtrFromString(winPath(dir))
	if err != nil {
		return &os.PathError{Op: "mkdir", Path: dir, Err: err}
	}
	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	if err := windows.CreateDirectory(p, sa); err != nil {
		return &os.PathError{Op: "mkdir", Path: dir, Err: err}
	}
	return nil
}

// mkdirPrivate creates dir with the private DACL already in place (no
// moment in which it has the parent's permissions).
func mkdirPrivate(dir string) error {
	sd, err := privateDescriptor()
	if err != nil {
		return err
	}
	return createPrivate(dir, sd)
}

func mkdirTempPrivate(parent, pattern string) (string, error) {
	if strings.ContainsAny(pattern, `/\`) {
		return "", errors.New("dirperm: pattern contains a path separator")
	}
	prefix, suffix := pattern, ""
	if i := strings.LastIndex(pattern, "*"); i >= 0 {
		prefix, suffix = pattern[:i], pattern[i+1:]
	}
	sd, err := privateDescriptor()
	if err != nil {
		return "", err
	}
	for range 10000 {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		name := prefix + strconv.FormatUint(uint64(binary.LittleEndian.Uint32(b[:])), 10) + suffix
		dir := filepath.Join(parent, name)
		err := createPrivate(dir, sd)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", &os.PathError{Op: "mkdirtemp", Path: filepath.Join(parent, pattern), Err: os.ErrExist}
}

func restrict(dir string) error {
	sd, err := privateDescriptor()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	// Setting a protected DACL drops the inherited entries; Windows then
	// passes the new inheritable ones down to everything below dir.
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return &os.PathError{Op: "set permissions", Path: dir, Err: err}
	}
	return nil
}

func check(dir string) (Exposure, error) {
	var e Exposure
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return e, &os.PathError{Op: "read permissions", Path: dir, Err: err}
	}
	me, err := currentUserSID()
	if err != nil {
		return e, err
	}
	trusted := func(sid *windows.SID) bool {
		if sid.Equals(me) {
			return true
		}
		s := sid.String()
		// app container and capability SIDs (S-1-15-…) only narrow what a
		// sandboxed program of an account with access may do
		if strings.HasPrefix(s, "S-1-15-") {
			return true
		}
		for _, t := range trustedSIDs {
			if s == t {
				return true
			}
		}
		return false
	}
	if owner, _, err := sd.Owner(); err == nil && owner != nil && !trusted(owner) {
		// the owner may always change the permissions
		e.Write = true
		e.add(accountName(owner))
	}
	dacl, _, err := sd.DACL()
	if err != nil && !errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
		return e, err
	}
	if dacl == nil {
		// no DACL (e.g. FAT or exFAT): everyone may do everything
		e.Write, e.Read = true, true
		e.add("Everyone")
		return e, nil
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return e, err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE && ace.Header.AceType != aceAllowedCallback {
			continue // deny entries can only take away (counting without them errs on the safe side)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if trusted(sid) {
			continue
		}
		// Inherit-only entries count too: they apply to what lies below.
		m := uint32(ace.Mask)
		if m&rightsWrite != 0 {
			e.Write = true
			e.add(accountName(sid))
		}
		if m&rightsRead != 0 {
			e.Read = true
			e.add(accountName(sid))
		}
	}
	return e, nil
}

// accountName is DOMAIN\name of a SID (localised by Windows), or the SID
// when it cannot be looked up within half a second (a domain account while
// the domain controller is unreachable).
func accountName(sid *windows.SID) string {
	s := sid.String()
	c, err := sid.Copy()
	if err != nil {
		return s
	}
	done := make(chan string, 1)
	go func() {
		acc, dom, _, err := c.LookupAccount("")
		switch {
		case err != nil || acc == "":
			done <- s
		case dom == "" || strings.EqualFold(dom, "BUILTIN") || strings.EqualFold(dom, "NT AUTHORITY") || dom == acc:
			done <- acc
		default:
			done <- dom + `\` + acc
		}
	}()
	select {
	case n := <-done:
		return n
	case <-time.After(500 * time.Millisecond):
		return s
	}
}

func save(dir string) (func() error, error) {
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, &os.PathError{Op: "read permissions", Path: dir, Err: err}
	}
	ctl, _, err := sd.Control()
	if err != nil {
		return nil, err
	}
	dacl, _, err := sd.DACL()
	if err != nil && !errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
		return nil, err
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if ctl&windows.SE_DACL_PROTECTED != 0 {
		info = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	return func() error {
		// sd (Go memory) holds dacl until this runs
		_ = sd
		if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, info, nil, nil, dacl, nil); err != nil {
			return &os.PathError{Op: "set permissions", Path: dir, Err: err}
		}
		return nil
	}, nil
}
