// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package fetch

import (
	"encoding/binary"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The IPv4 TCP listeners with their owning processes:
// GetExtendedTcpTable(AF_INET, TCP_TABLE_OWNER_PID_LISTENER), which needs
// no administrator rights and lists every account's sockets.
var procGetExtendedTcpTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

const (
	afInet                   = 2
	tcpTableOwnerPIDListener = 3
	errInsufficientBuffer    = 122
	// tcpRowOwnerPID is sizeof(MIB_TCPROW_OWNER_PID): dwState,
	// dwLocalAddr, dwLocalPort, dwRemoteAddr, dwRemotePort, dwOwningPid.
	tcpRowOwnerPID = 24
)

func tcpListenerTable() ([]byte, error) {
	var buf []byte
	size := uint32(0)
	for i := 0; i < 8; i++ {
		var p unsafe.Pointer
		if len(buf) > 0 {
			p = unsafe.Pointer(&buf[0])
		}
		r, _, _ := procGetExtendedTcpTable.Call(uintptr(p), uintptr(unsafe.Pointer(&size)), 0, afInet,
			tcpTableOwnerPIDListener, 0)
		switch r {
		case 0:
			return buf, nil
		case errInsufficientBuffer:
			buf = make([]byte, size+16*tcpRowOwnerPID) // room for a few new sockets meanwhile
			size = uint32(len(buf))
		default:
			return nil, fmt.Errorf("GetExtendedTcpTable: %w", syscall.Errno(r))
		}
	}
	return nil, fmt.Errorf("GetExtendedTcpTable: the table kept growing")
}

// rpcListener tells who listens on 127.0.0.1:port: listenerOurs when pid
// does and no other process listens on 127.0.0.1:port or 0.0.0.0:port,
// listenerForeign (with that process's id) when another one does.
func rpcListener(port, pid int) (listenerState, int, error) {
	buf, err := tcpListenerTable()
	if err != nil {
		return listenerNone, 0, err
	}
	if len(buf) < 4 {
		return listenerNone, 0, fmt.Errorf("GetExtendedTcpTable: short table")
	}
	n := int(binary.LittleEndian.Uint32(buf[:4]))
	if n < 0 || 4+n*tcpRowOwnerPID > len(buf) {
		return listenerNone, 0, fmt.Errorf("GetExtendedTcpTable: %d rows do not fit in %d bytes", n, len(buf))
	}
	ours, foreign, owner := false, false, 0
	for i := 0; i < n; i++ {
		row := buf[4+i*tcpRowOwnerPID : 4+(i+1)*tcpRowOwnerPID]
		addr := row[4:8]                      // in_addr, network order
		lport := int(row[8])<<8 | int(row[9]) // the low 16 bits, network order
		pidOf := int(binary.LittleEndian.Uint32(row[20:24]))
		if lport != port {
			continue
		}
		loopback := addr[0] == 127 && addr[1] == 0 && addr[2] == 0 && addr[3] == 1
		wildcard := addr[0] == 0 && addr[1] == 0 && addr[2] == 0 && addr[3] == 0
		switch {
		case !loopback && !wildcard:
		case pidOf != pid:
			foreign, owner = true, pidOf
		case loopback:
			ours = true
		}
	}
	switch {
	case foreign:
		return listenerForeign, owner, nil
	case ours:
		return listenerOurs, 0, nil
	}
	return listenerNone, 0, nil
}
