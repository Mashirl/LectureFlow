// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package nmhost

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/bridge"
)

func TestResolveEndpoint(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	def, err := DefaultEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := ResolveEndpoint("", getenv); got != def {
		t.Fatalf("default: %q, want %q", got, def)
	}
	env[EnvEndpoint] = `\\.\pipe\from-env`
	if got, _ := ResolveEndpoint("", getenv); got != `\\.\pipe\from-env` {
		t.Fatalf("env: %q", got)
	}
	if got, _ := ResolveEndpoint(`\\.\pipe\from-flag`, getenv); got != `\\.\pipe\from-flag` {
		t.Fatalf("flag over env: %q", got)
	}
	if got, err := ResolveEndpoint("off", getenv); got != "" || err != nil {
		t.Fatalf("off: %q %v", got, err)
	}
	env[EnvEndpoint] = "OFF"
	if got, _ := ResolveEndpoint("", getenv); got != "" {
		t.Fatalf("env off: %q", got)
	}
	if got, _ := ResolveEndpoint("rel/nm.sock", getenv); !filepath.IsAbs(got) {
		t.Fatalf("a socket path is made absolute: %q", got)
	}
}

func TestDefaultEndpointIsPerUser(t *testing.T) {
	def, err := DefaultEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		sid, _ := currentUserSID()
		if !IsPipe(def) || !strings.HasSuffix(def, "org.lectureflow.nm."+sid) || !strings.HasPrefix(sid, "S-1-5-") {
			t.Fatalf("default endpoint %q", def)
		}
		return
	}
	if !strings.HasSuffix(def, filepath.Join("LectureFlow", "run", "nm.sock")) {
		t.Fatalf("default endpoint %q", def)
	}
}

// roundTrip checks both directions at once on a connection pair: a read
// pending on one side must not hold up a write on the same side.
func roundTrip(t *testing.T, server, cl net.Conn) {
	t.Helper()
	var wg sync.WaitGroup
	wg.Add(1)
	got := make(chan []byte, 1)
	go func() {
		defer wg.Done()
		b, err := bridge.ReadFrame(server, 1<<20) // pending while the server also writes
		if err != nil {
			t.Error(err)
		}
		got <- b
	}()
	time.Sleep(50 * time.Millisecond)
	if err := bridge.WriteFrame(server, []byte(`{"type":"status"}`), 1<<20); err != nil {
		t.Fatal(err)
	}
	b, err := bridge.ReadFrame(cl, 1<<20)
	if err != nil || string(b) != `{"type":"status"}` {
		t.Fatalf("client read %q %v", b, err)
	}
	if err := bridge.WriteFrame(cl, []byte(`{"type":"hello"}`), 1<<20); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if b := <-got; string(b) != `{"type":"hello"}` {
		t.Fatalf("server read %q", b)
	}
}

func testEndpoint(t *testing.T, ep string) {
	ln, err := Listen(ep)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Several clients at once.
	accepted := make(chan net.Conn, 3)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	var clients []net.Conn
	for range 3 {
		c, err := Dial(t.Context(), ep)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
	}
	var servers []net.Conn
	for range 3 {
		select {
		case c := <-accepted:
			servers = append(servers, c)
		case <-time.After(5 * time.Second):
			t.Fatal("accept timed out")
		}
	}
	// Each accepted connection belongs to one client; find its partner by
	// a marker.
	for i, cl := range clients {
		if err := bridge.WriteFrame(cl, []byte{byte('0' + i)}, 10); err != nil {
			t.Fatal(err)
		}
	}
	byMarker := map[byte]net.Conn{}
	for _, s := range servers {
		b, err := bridge.ReadFrame(s, 10)
		if err != nil {
			t.Fatal(err)
		}
		byMarker[b[0]] = s
	}
	for i, cl := range clients {
		roundTrip(t, byMarker[byte('0'+i)], cl)
	}
	// Closing the client ends the server's read with EOF.
	clients[0].Close()
	if _, err := bridge.ReadFrame(byMarker['0'], 10); !errors.Is(err, io.EOF) {
		t.Fatalf("after the client closed: %v, want EOF", err)
	}
	// Deadlines work (the browser host relies on them).
	byMarker['1'].SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := byMarker['1'].Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read deadline: %v", err)
	}
	for _, c := range append(clients, servers...) {
		c.Close()
	}

	// A second listener on the same endpoint is refused (for a socket the
	// check connects once, which the accept loop above takes and drops).
	if ln2, err := Listen(ep); !errors.Is(err, ErrInUse) {
		if ln2 != nil {
			ln2.Close()
		}
		t.Fatalf("second Listen: %v, want ErrInUse", err)
	}

	// Close ends a pending Accept and frees the name.
	ln.Close()
	if _, err := Dial(t.Context(), ep); !errors.Is(err, ErrNotListening) {
		t.Fatalf("dial after close: %v, want ErrNotListening", err)
	}
	ln3, err := Listen(ep)
	if err != nil {
		t.Fatalf("listen again: %v", err)
	}
	ln3.Close()
}

func TestNamedPipeEndpoint(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("named pipes are Windows only")
	}
	testEndpoint(t, uniquePipe(t))
}

func TestUnixSocketEndpoint(t *testing.T) {
	testEndpoint(t, uniqueSocket(t))
}

func TestDialNotListening(t *testing.T) {
	if runtime.GOOS == "windows" {
		if _, err := Dial(t.Context(), uniquePipe(t)); !errors.Is(err, ErrNotListening) {
			t.Fatalf("pipe: %v", err)
		}
	}
	if _, err := Dial(t.Context(), uniqueSocket(t)); !errors.Is(err, ErrNotListening) {
		t.Fatalf("socket: %v", err)
	}
}

func TestUnixSocketLeftoverIsReplaced(t *testing.T) {
	ep := uniqueSocket(t)
	ln, err := Listen(ep)
	if err != nil {
		t.Fatal(err)
	}
	// A run that ended without removing its socket file.
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	ln.Close()
	if _, err := os.Lstat(ep); err != nil {
		t.Skipf("the platform removed the socket file anyway: %v", err)
	}
	if _, err := Dial(t.Context(), ep); !errors.Is(err, ErrNotListening) {
		t.Fatalf("dial a leftover: %v", err)
	}
	ln2, err := Listen(ep)
	if err != nil {
		t.Fatalf("listen over a leftover: %v", err)
	}
	ln2.Close()
	// Something that is not a socket is never removed.
	if err := os.WriteFile(ep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(ep); err == nil {
		t.Fatal("listened over a regular file")
	}
	if b, _ := os.ReadFile(ep); string(b) != "x" {
		t.Fatal("the regular file was touched")
	}
}

func TestUnixSocketPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	ep := uniqueSocket(t)
	ln, err := Listen(ep)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fi, err := os.Lstat(ep)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v", fi.Mode().Perm())
	}
}

func TestListenerCloseUnblocksAccept(t *testing.T) {
	eps := []string{uniqueSocket(t)}
	if runtime.GOOS == "windows" {
		eps = append(eps, uniquePipe(t))
	}
	for _, ep := range eps {
		ln, err := Listen(ep)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := ln.Accept()
			done <- err
		}()
		time.Sleep(50 * time.Millisecond)
		ln.Close()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("%s: Accept returned a connection", ep)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: Close did not end Accept", ep)
		}
	}
}

func TestDialHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Dial(ctx, uniqueSocket(t)); err == nil {
		t.Fatal("dial with a cancelled context succeeded")
	}
}
