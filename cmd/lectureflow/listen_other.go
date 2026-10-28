// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package main

import "net"

// listenTCP opens the service's listener. On macOS and Linux Go's default
// listener (SO_REUSEADDR, no SO_REUSEPORT) is enough against another
// account's socket on the same port (sec2-app M1):
//   - Linux refuses our bind while another socket listens on 0.0.0.0, ::
//     or 127.0.0.1 on the port (SO_REUSEADDR does not override a listening
//     socket; SO_REUSEPORT would need both sockets and the same user), and
//     refuses theirs while we listen.
//   - macOS refuses a non-root bind of 127.0.0.1:port while a socket of
//     another user is bound to the port on a wildcard or the same address
//     (in_pcbbind's port-hijacking check), and the reverse. A socket of the
//     same user on 0.0.0.0 or :: may coexist, but connections to 127.0.0.1
//     go to the socket bound to that exact address: ours.
//
// The desktop shell sends the token only after the ready line, printed once
// this listener exists.
func listenTCP(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}
