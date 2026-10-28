// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"errors"
	"fmt"
	"io"

	"lectureflow/internal/selfupdate"
)

// -selfupdate-verify DIR (SPEC-ext 应用内更新, hardening): the desktop shell
// runs it right before it installs an update. It checks the pending update
// of the updates directory DIR again with the public key built into this
// program (selfupdate.VerifyPending: the signature of update.json, the
// package's size and SHA-256, pending.json's claims, a newer version than
// this one), prints one line on stdout and exits:
//
//	0  ok version=X.Y.Z kind=K file=<path relative to DIR> sha256=<hex>[ copy=<absolute path>]
//	1  invalid <code>: <reason>
//	2  not_configured: this build has no update public key
//	3  untrusted_dir: <reason>   another account of this computer can
//	   change the data directory (or DIR): nothing in it is installed or
//	   rolled back (sec-update M2)
//
// -selfupdate-previous checks the package of previous.json instead (the
// version a rollback returns to; selfupdate.VerifyPrevious): older than
// this version. -selfupdate-copy PARENT copies the package, while it is
// hashed, into a new private directory in PARENT and names the copy on the
// ok line; the shell runs that copy (M3). Nothing else is written, no log
// file. The data directory's permissions are checked first.

// Exit codes of -selfupdate-verify besides exitOK.
const (
	exitVerifyInvalid       = 1
	exitVerifyNotConfigured = 2
	exitVerifyUntrusted     = 3
)

// selfupdateVerifyKey is the public key -selfupdate-verify checks with
// (tests replace it).
var selfupdateVerifyKey = selfupdate.BuiltinPublicKey

// selfUpdateVerifyArgs are the flags of -selfupdate-verify.
type selfUpdateVerifyArgs struct {
	Dir      string // the updates directory
	Current  string // this program's version
	Previous bool   // previous.json instead of pending.json
	CopyDir  string // where the private copy goes ("" = none)
}

// runSelfUpdateVerify checks a's package.
func runSelfUpdateVerify(a selfUpdateVerifyArgs, stdout io.Writer) int {
	if err := selfupdate.CheckTrust(a.Dir); err != nil {
		fmt.Fprintf(stdout, "untrusted_dir: %s\n", oneLine(err.Error()))
		return exitVerifyUntrusted
	}
	o := selfupdate.VerifyOptions{PublicKey: selfupdateVerifyKey(), Current: a.Current, CopyDir: a.CopyDir}
	verify := selfupdate.VerifyPending
	if a.Previous {
		verify = selfupdate.VerifyPrevious
	}
	v, err := verify(a.Dir, o)
	switch {
	case errors.Is(err, selfupdate.ErrNotConfigured):
		fmt.Fprintln(stdout, "not_configured: this build has no update public key")
		return exitVerifyNotConfigured
	case err != nil:
		fmt.Fprintf(stdout, "invalid %s: %s\n", selfupdate.CodeOf(err), oneLine(err.Error()))
		return exitVerifyInvalid
	}
	line := fmt.Sprintf("ok version=%s kind=%s file=%s sha256=%s", v.Version, v.Kind, v.File, v.SHA256)
	if v.Copy != "" {
		line += " copy=" + v.Copy
	}
	fmt.Fprintln(stdout, line)
	return exitOK
}
