// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package main

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"golang.org/x/sys/windows"
)

// bindOnly binds a TCP socket to ip:port with one socket option (0: none)
// and does not listen: a socket that never listens on a wildcard address
// does not make the Windows firewall ask the user about the test binary.
// dualStack: an IPv6 socket that also takes IPv4 (IPV6_V6ONLY off).
func bindOnly(ip string, port, opt int, dualStack bool) (windows.Handle, error) {
	addr := net.ParseIP(ip)
	family := windows.AF_INET
	if addr.To4() == nil {
		family = windows.AF_INET6
	}
	h, err := windows.Socket(family, windows.SOCK_STREAM, windows.IPPROTO_TCP)
	if err != nil {
		return 0, fmt.Errorf("socket: %w", err)
	}
	if opt != 0 {
		if err := windows.SetsockoptInt(h, windows.SOL_SOCKET, opt, 1); err != nil {
			windows.Closesocket(h)
			return 0, fmt.Errorf("setsockopt: %w", err)
		}
	}
	var sa windows.Sockaddr
	if family == windows.AF_INET {
		s := &windows.SockaddrInet4{Port: port}
		copy(s.Addr[:], addr.To4())
		sa = s
	} else {
		v6only := 1
		if dualStack {
			v6only = 0
		}
		if err := windows.SetsockoptInt(h, windows.IPPROTO_IPV6, windows.IPV6_V6ONLY, v6only); err != nil {
			windows.Closesocket(h)
			return 0, fmt.Errorf("setsockopt IPV6_V6ONLY: %w", err)
		}
		s := &windows.SockaddrInet6{Port: port}
		copy(s.Addr[:], addr.To16())
		sa = s
	}
	if err := windows.Bind(h, sa); err != nil {
		windows.Closesocket(h)
		return 0, err
	}
	return h, nil
}

func refusedBind(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE) || errors.Is(err, windows.WSAEACCES)
}

// On Windows the service's socket is exclusive (sec2-app M1): once it is
// bound, no other socket binds 127.0.0.1 on its port, whatever its options,
// and none binds the port exclusively on 0.0.0.0 or :: (Microsoft's
// "Using SO_REUSEADDR and SO_EXCLUSIVEADDRUSE"). A plain bind of 0.0.0.0
// still succeeds there by design and receives every address but 127.0.0.1;
// it is not attempted here (see listen_windows.go).
func TestListenTCPIsExclusiveOnWindows(t *testing.T) {
	ln, err := listenTCP("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	rc, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var v int
	var gerr error
	if err := rc.Control(func(fd uintptr) {
		v, gerr = windows.GetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, soExclusiveAddrUse)
	}); err != nil || gerr != nil || v == 0 {
		t.Fatalf("SO_EXCLUSIVEADDRUSE on the listener: %d (%v, %v)", v, err, gerr)
	}

	for _, c := range []struct {
		name, ip  string
		opt       int
		dualStack bool
	}{
		{"127.0.0.1", "127.0.0.1", 0, false},
		{"127.0.0.1 with SO_REUSEADDR", "127.0.0.1", windows.SO_REUSEADDR, false},
		{"127.0.0.1 exclusive", "127.0.0.1", soExclusiveAddrUse, false},
		{"0.0.0.0 exclusive", "0.0.0.0", soExclusiveAddrUse, false},
		{":: dual-stack exclusive", "::", soExclusiveAddrUse, true},
	} {
		h, err := bindOnly(c.ip, port, c.opt, c.dualStack)
		if err == nil {
			windows.Closesocket(h)
			t.Errorf("%s: bound to port %d next to the service's socket", c.name, port)
			continue
		}
		if !refusedBind(err) {
			t.Errorf("%s: %v, want WSAEADDRINUSE or WSAEACCES", c.name, err)
		}
	}
}

// A port another socket holds makes the exclusive listen fail with an error
// the desktop shell reads as "port in use" (it then tries another port).
func TestListenTCPTakenPortOnWindows(t *testing.T) {
	other, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	ln, err := listenTCP(other.Addr().String())
	if err == nil {
		ln.Close()
		t.Fatal("listened on a taken port")
	}
	if !refusedBind(err) {
		t.Fatalf("%v, want WSAEADDRINUSE or WSAEACCES", err)
	}
}
