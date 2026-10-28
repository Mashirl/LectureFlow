// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// aria2c's RPC port must be aria2c's (security review H1, NOTES "安全审查
// sec-review"). --rpc-listen-all=false binds 127.0.0.1:P, then [::1]:P.
// When another program (another account of this computer) holds
// 127.0.0.1:P first, aria2c only prints "IPv4 RPC: failed to bind TCP port
// P" and goes on listening on [::1]:P; the client, which uses 127.0.0.1,
// would hand that program the RPC secret and every download's cookies and
// signed URL, and with the secret the program could make the real aria2c
// write any file. So:
//
//   - nothing is sent to 127.0.0.1:P before the system's socket table shows
//     the aria2c child listening there and no other process (rpcListener,
//     per system: the TCP table on Windows, lsof on macOS, /proc on Linux);
//   - aria2c's console output is read for its bind failure, which ends that
//     start at once (the output itself is dropped: it may quote a URL);
//   - every new connection of the RPC client checks the table again before
//     its first byte, so a port aria2c lost (it exited) and someone took is
//     never written to;
//   - a taken port means a new aria2c on another, random port (acquire).
//
// TLS on the RPC (--rpc-secure with a fresh self-signed certificate pinned
// by the client) was weighed and left out (NOTES "sec-fetch"): with the
// listener checked on every connection a program in between cannot exist,
// and the macOS build of aria2c (AppleTLS) takes the certificate only from
// the keychain or as PKCS#12, which Go's standard library cannot write.

// listenerState is what the socket table says about 127.0.0.1:port.
type listenerState int

const (
	// listenerNone: nobody listens there (aria2c has not bound it yet).
	listenerNone listenerState = iota
	// listenerOurs: the aria2c child listens there and no other process.
	listenerOurs
	// listenerForeign: another process holds 127.0.0.1:port (or 0.0.0.0:port).
	listenerForeign
)

func (s listenerState) String() string {
	switch s {
	case listenerOurs:
		return "ours"
	case listenerForeign:
		return "foreign"
	}
	return "none"
}

// errPortTaken: another program holds aria2c's RPC port of 127.0.0.1, or
// aria2c reported that it could not bind it. Nothing was sent there.
var errPortTaken = errors.New("aria2c's RPC port of 127.0.0.1 is held by another program")

// ariaBindFailure is what aria2c prints when 127.0.0.1:P is taken
// ("[ERROR] IPv4 RPC: failed to bind TCP port P", aria2c 1.37.0). Its IPv6
// counterpart does not count: the client never uses [::1], and on a
// computer without IPv6 aria2c prints it at every start.
var ariaBindFailure = []byte("IPv4 RPC: failed to bind")

// bindWatch is aria2c's stdout and stderr: it notices ariaBindFailure and
// drops everything else.
type bindWatch struct {
	mu   sync.Mutex
	tail []byte // the end of the output so far, for a message split across writes
	hit  chan struct{}
	once sync.Once
}

func newBindWatch() *bindWatch { return &bindWatch{hit: make(chan struct{})} }

func (w *bindWatch) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	buf := append(append([]byte(nil), w.tail...), p...)
	if bytes.Contains(buf, ariaBindFailure) {
		w.once.Do(func() { close(w.hit) })
	}
	if keep := len(ariaBindFailure) - 1; len(buf) > keep {
		buf = buf[len(buf)-keep:]
	}
	w.tail = buf
	return len(p), nil
}

// failed reports whether aria2c said it could not bind 127.0.0.1.
func (w *bindWatch) failed() bool {
	select {
	case <-w.hit:
		return true
	default:
		return false
	}
}

// freePort returns a random port of 127.0.0.1 that was free a moment ago:
// random rather than the system's next ephemeral port, which another
// account could predict and take first (a taken port is noticed anyway;
// this only makes it rare).
func freePort() (int, error) {
	for i := 0; i < 50; i++ {
		p := 20000 + rand.IntN(40000)
		l, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(p))
		if err != nil {
			continue // in use, or reserved (Windows' excluded port ranges)
		}
		l.Close()
		return p, nil
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// awaitListener waits until the aria2c of in listens on 127.0.0.1:port,
// sending nothing there: errPortTaken when another process holds the port
// or aria2c reported it could not bind it.
func (e *engine) awaitListener(ctx context.Context, in *instance, port int, watch *bindWatch, deadline time.Time) error {
	pid := in.proc.Pid()
	taken := func(owner int, how string) error {
		e.log.Warn("downloads: aria2c's RPC port is held by another program; nothing was sent to it",
			"port", port, "owner_pid", owner, "seen_by", how)
		return errPortTaken
	}
	for {
		if watch.failed() {
			return taken(0, "aria2c output")
		}
		st, owner, err := rpcListener(port, pid)
		if err != nil {
			return fmt.Errorf("aria2c's RPC listener cannot be verified: %w", err)
		}
		switch st {
		case listenerOurs:
			return nil
		case listenerForeign:
			return taken(owner, "socket table")
		}
		select {
		case <-in.exited:
			if watch.failed() {
				return taken(0, "aria2c output")
			}
			return errExitedEarly
		case <-watch.hit:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return errors.New("aria2c did not listen on its RPC port in time")
		}
	}
}

// rpcTransport is the RPC client's transport for the aria2c pid listening
// on 127.0.0.1:port: every new connection goes to that address only, and
// is used only once the socket table still shows aria2c there (aria2c
// binds once, so a connection made while it still listens reached it).
func (e *engine) rpcTransport(port, pid int) *http.Transport {
	addr := "127.0.0.1:" + strconv.Itoa(port)
	d := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Transport{
		Proxy:               nil,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			c, err := d.DialContext(ctx, "tcp4", addr)
			if err != nil {
				return nil, err
			}
			st, owner, err := rpcListener(port, pid)
			if err != nil || st != listenerOurs {
				c.Close()
				e.log.Warn("downloads: aria2c's RPC port is no longer aria2c's; nothing was sent to it",
					"port", port, "state", st.String(), "owner_pid", owner, "err", err)
				return nil, errPortTaken
			}
			return c, nil
		},
	}
}

// parseLsof reads "lsof -Fn" for a process's TCP listeners on port:
// listenerOurs when 127.0.0.1:port is one of them; listenerForeign when
// only [::1]:port is (aria2c binds 127.0.0.1 first, so listening on [::1]
// alone means another program held 127.0.0.1:port); listenerNone
// otherwise.
func parseLsof(out []byte, port int) listenerState {
	p := ":" + strconv.Itoa(port)
	v4, v6 := false, false
	for _, line := range strings.Split(string(out), "\n") {
		name, ok := strings.CutPrefix(strings.TrimSpace(line), "n")
		if !ok {
			continue
		}
		switch name {
		case "127.0.0.1" + p:
			v4 = true
		case "[::1]" + p:
			v6 = true
		}
	}
	switch {
	case v4:
		return listenerOurs
	case v6:
		return listenerForeign
	}
	return listenerNone
}
