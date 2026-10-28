// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package nmhost

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"syscall"
)

var errNoPipes = errors.New("nmhost: named pipes exist only on Windows")

func currentUserSID() (string, error) { return "", errNoPipes }

func listenPipe(string) (net.Listener, error) { return nil, errNoPipes }

func dialPipe(context.Context, string) (net.Conn, error) { return nil, errNoPipes }

// ownedByCurrentUser refuses a socket file another user created (the
// directory is 0700, so this only matters for an overridden endpoint).
func ownedByCurrentUser(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	return int(st.Uid) == os.Getuid()
}
