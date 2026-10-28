// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package agent

import "io/fs"

// ownedByMe: %LOCALAPPDATA% belongs to the user and its ACL is inherited
// (a Windows remote is plan step 5; here the agent only runs in tests).
func ownedByMe(fs.FileInfo) bool { return true }

func setUmask() {}
