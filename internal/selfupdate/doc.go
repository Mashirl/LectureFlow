// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package selfupdate is the Go side of the in-app update of SPEC-ext
// "应用内更新（不依赖自有服务器）": it checks GitHub Releases (and extra
// mirror sources) for a minisign-signed update.json, downloads this
// installation's package, accepts offline updates from files, and leaves a
// verified package in {data_dir}/updates for the desktop shell (Electron)
// to install at the next start. README.md in this directory is the frozen
// description of update.json, the signature, the updates directory and the
// contract with Electron.
package selfupdate
