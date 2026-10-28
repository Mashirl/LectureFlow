// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package nmhost

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Named pipes are opened for overlapped I/O and handed to os.NewFile, which
// (Go 1.25+) associates such handles with the runtime's completion port:
// reads and writes do not block OS threads, deadlines work, and Close
// cancels pending I/O. Only ConnectNamedPipe is driven here, before the
// handle is handed over.

const pipeBuffer = 64 << 10

func currentUserSID() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}

// pipeSecurity allows the current user only (ext-brief §4: D:P(A;;GA;;;SID)).
func pipeSecurity() (*windows.SecurityAttributes, error) {
	sid, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + sid + ")")
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}, nil
}

func createPipeInstance(name string, sa *windows.SecurityAttributes, first bool) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	flags := uint32(windows.PIPE_ACCESS_DUPLEX | windows.FILE_FLAG_OVERLAPPED)
	if first {
		flags |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
	}
	mode := uint32(windows.PIPE_TYPE_BYTE | windows.PIPE_READMODE_BYTE | windows.PIPE_WAIT | windows.PIPE_REJECT_REMOTE_CLIENTS)
	return windows.CreateNamedPipe(p, flags, mode, windows.PIPE_UNLIMITED_INSTANCES, pipeBuffer, pipeBuffer, 0, sa)
}

type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

// pipeConn is one end of a pipe connection.
type pipeConn struct {
	*os.File
	addr pipeAddr
}

func (c *pipeConn) LocalAddr() net.Addr  { return c.addr }
func (c *pipeConn) RemoteAddr() net.Addr { return c.addr }

func newPipeConn(h windows.Handle, name string) *pipeConn {
	return &pipeConn{File: os.NewFile(uintptr(h), name), addr: pipeAddr(name)}
}

type pipeListener struct {
	name    string
	sa      *windows.SecurityAttributes
	closeEv windows.Handle
	closed  atomic.Bool

	acceptMu sync.Mutex // one Accept at a time; guards next, ov, ev
	next     windows.Handle
	// ov is on the heap (a field of the listener) because the kernel keeps
	// its address while ConnectNamedPipe is pending.
	ov *windows.Overlapped
	ev windows.Handle
}

func listenPipe(name string) (net.Listener, error) {
	sa, err := pipeSecurity()
	if err != nil {
		return nil, fmt.Errorf("nmhost: pipe security: %w", err)
	}
	h, err := createPipeInstance(name, sa, true)
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return nil, ErrInUse
		}
		return nil, fmt.Errorf("nmhost: create pipe: %w", err)
	}
	closeEv, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(h)
		windows.CloseHandle(closeEv)
		return nil, err
	}
	return &pipeListener{name: name, sa: sa, closeEv: closeEv, next: h, ov: new(windows.Overlapped), ev: ev}, nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr(l.name) }

func (l *pipeListener) Accept() (net.Conn, error) {
	l.acceptMu.Lock()
	defer l.acceptMu.Unlock()
	for {
		if l.closed.Load() {
			return nil, net.ErrClosed
		}
		h := l.next
		l.next = windows.InvalidHandle
		if h == windows.InvalidHandle || h == 0 {
			var err error
			if h, err = createPipeInstance(l.name, l.sa, false); err != nil {
				return nil, fmt.Errorf("nmhost: create pipe: %w", err)
			}
		}
		ok, err := l.connect(h)
		if err != nil {
			windows.CloseHandle(h)
			if errors.Is(err, net.ErrClosed) {
				return nil, err
			}
			if errors.Is(err, windows.ERROR_NO_DATA) || errors.Is(err, windows.ERROR_BROKEN_PIPE) {
				continue // the client came and went
			}
			return nil, fmt.Errorf("nmhost: connect pipe: %w", err)
		}
		if !ok {
			windows.CloseHandle(h)
			continue
		}
		// The next instance exists before this one is handed over, so the
		// name never disappears while the app runs (a client then sees
		// ERROR_PIPE_BUSY at worst, and retries).
		if nh, err := createPipeInstance(l.name, l.sa, false); err == nil {
			l.next = nh
		}
		return newPipeConn(h, l.name), nil
	}
}

// connect waits for a client on h, or for Close.
func (l *pipeListener) connect(h windows.Handle) (bool, error) {
	*l.ov = windows.Overlapped{HEvent: l.ev}
	if err := windows.ResetEvent(l.ev); err != nil {
		return false, err
	}
	err := windows.ConnectNamedPipe(h, l.ov)
	switch {
	case err == nil, errors.Is(err, windows.ERROR_PIPE_CONNECTED):
		return true, nil
	case errors.Is(err, windows.ERROR_IO_PENDING):
	default:
		return false, err
	}
	idx, err := windows.WaitForMultipleObjects([]windows.Handle{l.ev, l.closeEv}, false, windows.INFINITE)
	if err != nil || idx != windows.WAIT_OBJECT_0 {
		windows.CancelIoEx(h, l.ov)
		var n uint32
		windows.GetOverlappedResult(h, l.ov, &n, true)
		if err != nil {
			return false, err
		}
		return false, net.ErrClosed
	}
	var n uint32
	if err := windows.GetOverlappedResult(h, l.ov, &n, false); err != nil {
		return false, err
	}
	return true, nil
}

func (l *pipeListener) Close() error {
	if l.closed.Swap(true) {
		return nil
	}
	windows.SetEvent(l.closeEv)
	l.acceptMu.Lock() // Accept has returned
	defer l.acceptMu.Unlock()
	if l.next != windows.InvalidHandle && l.next != 0 {
		windows.CloseHandle(l.next)
		l.next = windows.InvalidHandle
	}
	windows.CloseHandle(l.ev)
	windows.CloseHandle(l.closeEv)
	return nil
}

func dialPipe(ctx context.Context, name string) (net.Conn, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	for {
		// SECURITY_IDENTIFICATION: the server may learn who we are, never
		// act as us.
		h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_OVERLAPPED|windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
		if err == nil {
			if err := checkPipeServer(h); err != nil {
				windows.CloseHandle(h)
				return nil, err
			}
			return newPipeConn(h, name), nil
		}
		switch {
		case errors.Is(err, windows.ERROR_FILE_NOT_FOUND):
			return nil, ErrNotListening
		case errors.Is(err, windows.ERROR_PIPE_BUSY):
			// Every instance is taken for a moment: the server is creating
			// the next one.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
		case errors.Is(err, windows.ERROR_ACCESS_DENIED):
			// A pipe of that name whose DACL shuts us out: not ours.
			return nil, ErrForeign
		default:
			return nil, &fs.PathError{Op: "dial", Path: name, Err: err}
		}
	}
}

// checkPipeServer refuses a pipe whose server the host must not hand
// cookies to (security review L5): another user's process, or one of ours
// that runs below medium integrity or in an AppContainer (a sandboxed
// process may create a pipe of our name before LectureFlow starts). Any
// step that fails counts as untrusted: ErrForeign.
func checkPipeServer(h windows.Handle) error {
	theirs, err := serverTokenOf(h)
	if err != nil {
		return ErrForeign
	}
	ours, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return ErrForeign
	}
	return trustPipeServer(theirs, ours.User.Sid)
}

// pipeServerToken is what a client learns about the process serving a
// pipe.
type pipeServerToken struct {
	user *windows.SID
	// integrity is the RID of the token's mandatory label
	// (mandatoryMediumRID and so on).
	integrity    uint32
	appContainer bool
}

// Mandatory label RIDs (winnt.h SECURITY_MANDATORY_*_RID) and the token
// information class TokenIsAppContainer, which golang.org/x/sys/windows
// does not name.
const (
	mandatoryUntrustedRID = 0x0000
	mandatoryLowRID       = 0x1000
	mandatoryMediumRID    = 0x2000
	mandatoryHighRID      = 0x3000
	tokenIsAppContainer   = windows.TokenLogonSid + 1 // 29
)

// trustPipeServer: the server runs as ourUser, at medium integrity or
// above, outside any AppContainer.
func trustPipeServer(theirs pipeServerToken, ourUser *windows.SID) error {
	switch {
	case theirs.user == nil || ourUser == nil || !windows.EqualSid(theirs.user, ourUser):
		return ErrForeign
	case theirs.integrity < mandatoryMediumRID:
		return ErrForeign
	case theirs.appContainer:
		return ErrForeign
	}
	return nil
}

// serverTokenOf reads the token of the process serving pipe h:
// GetNamedPipeServerProcessId, OpenProcess with
// PROCESS_QUERY_LIMITED_INFORMATION, OpenProcessToken. A variable so tests
// can stand in for the server.
var serverTokenOf = func(h windows.Handle) (pipeServerToken, error) {
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(h, &pid); err != nil {
		return pipeServerToken{}, fmt.Errorf("nmhost: pipe server: %w", err)
	}
	ph, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return pipeServerToken{}, err
	}
	defer windows.CloseHandle(ph)
	var tok windows.Token
	if err := windows.OpenProcessToken(ph, windows.TOKEN_QUERY, &tok); err != nil {
		return pipeServerToken{}, err
	}
	defer tok.Close()
	return readPipeServerToken(tok)
}

// readPipeServerToken reads the user, the integrity level and whether tok
// is an AppContainer token.
func readPipeServerToken(tok windows.Token) (pipeServerToken, error) {
	var out pipeServerToken
	u, err := tok.GetTokenUser()
	if err != nil {
		return out, err
	}
	out.user = u.User.Sid
	b, err := tokenInformation(tok, windows.TokenIntegrityLevel, 64)
	if err != nil {
		return out, err
	}
	label := (*windows.Tokenmandatorylabel)(unsafe.Pointer(&b[0]))
	sid := label.Label.Sid
	if sid == nil || !sid.IsValid() || sid.SubAuthorityCount() == 0 {
		return out, errors.New("nmhost: token without an integrity level")
	}
	out.integrity = sid.SubAuthority(uint32(sid.SubAuthorityCount()) - 1)
	var ac uint32
	var n uint32
	if err := windows.GetTokenInformation(tok, tokenIsAppContainer, (*byte)(unsafe.Pointer(&ac)), uint32(unsafe.Sizeof(ac)), &n); err != nil {
		return out, err
	}
	out.appContainer = ac != 0
	return out, nil
}

// tokenInformation is GetTokenInformation into a buffer that grows as
// needed (the returned slice keeps the SIDs it points to alive).
func tokenInformation(tok windows.Token, class uint32, size int) ([]byte, error) {
	n := uint32(size)
	for {
		b := make([]byte, n)
		err := windows.GetTokenInformation(tok, class, &b[0], uint32(len(b)), &n)
		if err == nil {
			return b, nil
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || n <= uint32(len(b)) {
			return nil, err
		}
	}
}

// ownedByCurrentUser: Unix sockets on Windows are only used by tests (the
// app uses the named pipe); their directory is private to the test.
func ownedByCurrentUser(fs.FileInfo) bool { return true }
