// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bufio"
	"fmt"
	"net"
	"testing"
	"time"
)

// The service's listener serves on 127.0.0.1 and no second listener can take
// the same address while it is open (sec2-app M1). Only loopback addresses
// here: a listener on a wildcard address would make the Windows and macOS
// firewalls ask the user about the test binary.
func TestListenTCPHoldsTheLoopbackPort(t *testing.T) {
	ln, err := listenTCP("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			fmt.Fprintln(c, "ours")
			c.Close()
		}
	}()

	// Go's default listener (SO_REUSEADDR outside Windows) cannot join it.
	if l2, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
		l2.Close()
		t.Fatalf("a second listener took 127.0.0.1:%d", port)
	}

	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	got, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || got != "ours\n" {
		t.Fatalf("answer %q, %v", got, err)
	}
}
