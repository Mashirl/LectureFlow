// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build linux

package fetch

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// rpcListener reads /proc/net/tcp for the listening sockets on
// 127.0.0.1:port and 0.0.0.0:port and /proc/<pid>/fd for pid's sockets: a
// listener whose socket pid does not hold belongs to another process
// (its id is not looked up: 0).
func rpcListener(port, pid int) (listenerState, int, error) {
	b, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return listenerNone, 0, err
	}
	type sock struct {
		inode    string
		loopback bool
	}
	var socks []sock
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 10 || f[3] != "0A" { // st 0A = LISTEN
			continue
		}
		ip, p, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		pv, err1 := strconv.ParseUint(p, 16, 16)
		av, err2 := strconv.ParseUint(ip, 16, 32)
		if err1 != nil || err2 != nil || int(pv) != port {
			continue
		}
		var a [4]byte
		binary.NativeEndian.PutUint32(a[:], uint32(av))
		loopback := a == [4]byte{127, 0, 0, 1}
		if loopback || a == [4]byte{} {
			socks = append(socks, sock{inode: f[9], loopback: loopback})
		}
	}
	if len(socks) == 0 {
		return listenerNone, 0, nil
	}
	held := map[string]bool{}
	fds, _ := filepath.Glob(filepath.Join("/proc", strconv.Itoa(pid), "fd", "*"))
	for _, fd := range fds {
		if l, err := os.Readlink(fd); err == nil {
			if in, ok := strings.CutPrefix(l, "socket:["); ok {
				held[strings.TrimSuffix(in, "]")] = true
			}
		}
	}
	ours := false
	for _, s := range socks {
		if !held[s.inode] {
			return listenerForeign, 0, nil
		}
		ours = ours || s.loopback
	}
	if ours {
		return listenerOurs, 0, nil
	}
	return listenerNone, 0, nil
}
