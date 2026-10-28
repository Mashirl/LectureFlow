// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"fmt"
	"regexp"
	"strings"

	"lectureflow/internal/components"
)

// PublicKey is the maintainer's minisign public key (the base64 line of the
// .pub file, "RWQ…"), compiled into the application. Every update.json is
// checked against it; the matching secret key is a password-protected file
// on the maintainer's computer only (SPEC-ext "应用内更新"). Empty = updates
// are "not configured": the updater reports state not_configured and does
// nothing else.
//
// The maintainer's key pair was made with minisign on 2026-10-27 (given to
// lead by the user that day; regenerated that evening after the first key's
// password was lost, before any release used it); its key id, as minisign
// prints it, is 0FAC4F1B6C17636D (TestBuiltinPublicKey). Changing the key:
// see scripts/release/README.md ("Changing the key").
const PublicKey = "RWRtYxdsG0+sD5yxDvxA4MOrUrK3WGrkwpejK2VfGHH3GibjDpES6nTX"

// PublicKeyID is the key id of PublicKey as minisign prints it ("" while
// PublicKey is empty); the release documentation names it, and
// TestBuiltinPublicKey checks that it belongs to PublicKey.
const PublicKeyID = "0FAC4F1B6C17636D"

// builtinPublicKey is PublicKey (a variable so that tests can cover an
// application built without a key, and give a test key to the code that
// reads the built-in one).
var builtinPublicKey = PublicKey

// Repo is the GitHub repository whose Releases carry update.json
// ("owner/name"): the public repository (CLAUDE.md, lead 2026-10-27).
// Empty = no default source (only the extra sources of the settings are
// asked). Without PublicKey the updater is still not_configured and asks
// no one (TestNotConfiguredMakesNoRequest).
const Repo = "ShouldBeQuincy/LectureFlow"

// UserAgent is the only identifying header of every request the updater
// makes (no cookies, no referrer, no other header that names the user or
// the installation).
const UserAgent = "LectureFlow"

// versionRe is SemVer's grammar: the identifiers of the pre-release and the
// build are ASCII letters, digits and "-" (a version names a directory,
// updates/<version>/, so no "/" or "\" may pass; fuzz, NOTES).
var versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

// parseVersion parses a version as written in update.json and in the
// trusted comment: MAJOR.MINOR.PATCH[-pre][+build], no "v", no spaces.
func parseVersion(s string) (components.Version, error) {
	if s == "" || strings.HasPrefix(s, "v") || strings.TrimSpace(s) != s || !versionRe.MatchString(s) {
		return components.Version{}, fmt.Errorf("invalid version %q", s)
	}
	return components.ParseVersion(s)
}
