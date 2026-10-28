// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package state

import "os"

// makeDirLink makes link a symbolic link to the directory target.
func makeDirLink(target, link string) error { return os.Symlink(target, link) }
