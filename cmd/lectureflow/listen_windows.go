// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package main

import (
	"context"
	"net"
	"os"
	"syscall"
)

// soExclusiveAddrUse is SO_EXCLUSIVEADDRUSE, ((int)(~SO_REUSEADDR)) in
// winsock2.h.
const soExclusiveAddrUse = ^syscall.SO_REUSEADDR

// listenTCP opens the service's listener. On Windows its socket is bound
// with SO_EXCLUSIVEADDRUSE (sec2-app M1), as Microsoft asks of every server
// ("Using SO_REUSEADDR and SO_EXCLUSIVEADDRUSE"): while it is open no other
// socket, with or without SO_REUSEADDR, of this or another account, can
// bind the same address and port, and none can bind the port exclusively
// on a wildcard address. A plain bind of 0.0.0.0 or :: on the same port
// still succeeds, but such a socket gets the traffic of every address
// except ours, so the desktop shell's requests to 127.0.0.1 keep reaching
// us. A socket another account has already bound to the port on a wildcard
// address makes this bind fail (WSAEACCES: the desktop shell tries another
// port); one of this account may stay, and gets no traffic for 127.0.0.1
// once we are bound.
func listenTCP(addr string) (net.Listener, error) {
	lc := net.ListenConfig{Control: exclusiveAddrUse}
	return lc.Listen(context.Background(), "tcp", addr)
}

// exclusiveAddrUse sets SO_EXCLUSIVEADDRUSE before the bind.
func exclusiveAddrUse(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, soExclusiveAddrUse, 1)
	}); err != nil {
		return err
	}
	if serr != nil {
		return os.NewSyscallError("setsockopt SO_EXCLUSIVEADDRUSE", serr)
	}
	return nil
}
