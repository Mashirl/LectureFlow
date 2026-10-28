// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBindWatch(t *testing.T) {
	w := newBindWatch()
	for _, chunk := range []string{"\n10/27 03:57:51 [ERROR] IPv6 RPC: failed to bind TCP port 1\n", "noise \r   \r"} {
		w.Write([]byte(chunk))
	}
	if w.failed() {
		t.Fatal("the IPv6 failure (a computer without IPv6) ended the start")
	}
	// split across writes, as a pipe may deliver it
	for _, chunk := range []string{"[ERROR] IPv4 RP", "C: failed to ", "bind TCP port 52568\n"} {
		w.Write([]byte(chunk))
	}
	if !w.failed() {
		t.Fatal("the IPv4 bind failure was not noticed")
	}
	if len(w.tail) >= len(ariaBindFailure) {
		t.Fatalf("the watch keeps %d bytes of output", len(w.tail))
	}
}

func TestParseLsof(t *testing.T) {
	cases := []struct {
		out  string
		want listenerState
	}{
		{"", listenerNone},
		{"p123\nf7\nn127.0.0.1:5000\n", listenerOurs},
		{"p123\nf7\nn127.0.0.1:5000\nf8\nn[::1]:5000\n", listenerOurs},
		{"p123\nf8\nn[::1]:5000\n", listenerForeign}, // 127.0.0.1 failed: aria2c binds it first
		{"p123\nf7\nn127.0.0.1:50001\n", listenerNone},
		{"p123\nf7\nn127.0.0.2:5000\n", listenerNone},
	}
	for _, c := range cases {
		if got := parseLsof([]byte(c.out), 5000); got != c.want {
			t.Errorf("%q: %v, want %v", c.out, got, c.want)
		}
	}
}

// The socket table of this system: a listener of this process is ours for
// this process's id, and not for another process's.
func TestRPCListenerThisSystem(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	st, _, err := rpcListener(port, os.Getpid())
	if err != nil || st != listenerOurs {
		t.Fatalf("own listener: %v %v", st, err)
	}
	st, owner, err := rpcListener(port, os.Getppid())
	if err != nil {
		t.Fatal(err)
	}
	switch runtime.GOOS {
	case "windows":
		if st != listenerForeign || owner != os.Getpid() {
			t.Fatalf("another process's view: %v owner %d", st, owner)
		}
	case "linux":
		if st != listenerForeign {
			t.Fatalf("another process's view: %v", st)
		}
	case "darwin":
		// lsof lists the asked process's sockets only
		if st != listenerNone {
			t.Fatalf("another process's view: %v", st)
		}
	}
	ln.Close()
	if st, _, err := rpcListener(port, os.Getpid()); err != nil || st != listenerNone {
		t.Fatalf("closed listener: %v %v", st, err)
	}
}

// squatter holds 127.0.0.1:port the way another account's program would,
// and records every connection and byte that reaches it.
type squatter struct {
	ln    net.Listener
	mu    sync.Mutex
	conns int
	got   []byte
}

func newSquatter(t *testing.T, port int) *squatter {
	t.Helper()
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ { // (a port aria2c just left may need a moment)
		if ln, err = net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port)); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	s := &squatter{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns++
			s.mu.Unlock()
			go func() {
				defer c.Close()
				b, _ := io.ReadAll(c)
				s.mu.Lock()
				s.got = append(s.got, b...)
				s.mu.Unlock()
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *squatter) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *squatter) seen() (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns, string(s.got)
}

// childEngine runs this test binary as aria2c (a separate process, which
// handles a taken port as aria2c 1.37.0 does: fakeAria.likeAria2c).
func childEngine(t *testing.T, logs io.Writer, pick func() (int, error)) *engine {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LF_FAKE_ARIA2C", "1")
	eng := &engine{exe: exe, launch: execLauncher, idle: time.Minute, startTimeout: 10 * time.Second,
		hc: &http.Client{Timeout: 5 * time.Second}, pickPort: pick,
		log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	t.Cleanup(eng.close)
	return eng
}

func rpcPort(t *testing.T, in *instance) int {
	t.Helper()
	_, rest, _ := strings.Cut(in.rpc.url, "127.0.0.1:")
	p, err := strconv.Atoi(strings.TrimSuffix(rest, "/jsonrpc"))
	if err != nil {
		t.Fatalf("rpc url %q", in.rpc.url)
	}
	return p
}

// Security review H1: another program took 127.0.0.1:P before aria2c
// (aria2c then listens on [::1]:P alone). The secret never reaches it, not
// even a connection: aria2c is started again on another port. Loud: aria2c
// prints its bind failure; quiet: only the socket table tells.
func TestRPCPortTakenIsRefused(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		t.Run(map[bool]string{false: "loud", true: "quiet"}[quiet], func(t *testing.T) {
			sq := newSquatter(t, 0)
			if quiet {
				t.Setenv("LF_FAKE_ARIA2C_QUIET", "1")
			}
			logs := &syncBuffer{}
			var mu sync.Mutex
			picks := 0
			eng := childEngine(t, logs, func() (int, error) {
				mu.Lock()
				defer mu.Unlock()
				picks++
				if picks == 1 {
					return sq.port(), nil
				}
				return freePort()
			})
			in, err := eng.acquire(context.Background())
			if err != nil {
				t.Fatalf("acquire: %v\n%s", err, logs)
			}
			defer eng.release()
			if p := rpcPort(t, in); p == sq.port() || picks != 2 {
				t.Fatalf("aria2c runs on port %d (taken %d) after %d picks", p, sq.port(), picks)
			}
			if v, err := in.rpc.version(context.Background()); err != nil || !strings.Contains(v, "fake") {
				t.Fatalf("version %q %v", v, err)
			}
			time.Sleep(100 * time.Millisecond)
			if conns, got := sq.seen(); conns != 0 || got != "" {
				t.Fatalf("the program on the taken port saw %d connections: %q", conns, got)
			}
			secret := strings.TrimPrefix(in.rpc.token, "token:")
			if l := logs.String(); !strings.Contains(l, "held by another program") || strings.Contains(l, secret) {
				t.Fatalf("log:\n%s", l)
			}
		})
	}
}

// aria2c went away and another program took its port: the RPC client's
// next connection sees it before writing a byte.
func TestRPCConnectionRechecked(t *testing.T) {
	logs := &syncBuffer{}
	eng := childEngine(t, logs, nil)
	ctx := context.Background()
	in, err := eng.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v\n%s", err, logs)
	}
	defer eng.release()
	port := rpcPort(t, in)
	if err := in.proc.Kill(); err != nil {
		t.Fatal(err)
	}
	<-in.exited
	sq := newSquatter(t, port)
	in.tr.CloseIdleConnections()
	_, err = in.rpc.tellStatuses(ctx, []string{"0000000000000001"})
	if !errors.Is(err, errPortTaken) {
		t.Fatalf("a call after the port changed hands: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, got := sq.seen(); got != "" {
		t.Fatalf("the program on aria2c's old port got %q", got)
	}
	if l := logs.String(); !strings.Contains(l, "no longer aria2c's") || strings.Contains(l, strings.TrimPrefix(in.rpc.token, "token:")) {
		t.Fatalf("log:\n%s", l)
	}
}
