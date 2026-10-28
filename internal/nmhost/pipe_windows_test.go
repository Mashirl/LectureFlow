// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package nmhost

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The pipe admits the current user only: its DACL is protected and has a
// single allow entry, for our SID.
func TestPipeSecurityDescriptor(t *testing.T) {
	ep := uniquePipe(t)
	ln, err := Listen(ep)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			defer c.Close()
			c.Read(make([]byte, 1))
		}
	}()
	c, err := Dial(t.Context(), ep)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	h := windows.Handle(c.(*pipeConn).Fd())
	sd, err := windows.GetSecurityInfo(h, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	sddl := sd.String()
	sid, _ := currentUserSID()
	// SDDL names the built-in Administrator (CI runners) "LA", not by SID
	ref, err := windows.SecurityDescriptorFromString("D:(A;;FA;;;" + sid + ")")
	if err != nil {
		t.Fatal(err)
	}
	rs := ref.String()
	tok := strings.TrimSuffix(rs[strings.LastIndex(rs, ";")+1:], ")")
	if !strings.HasPrefix(sddl, "D:P") || strings.Count(sddl, "(A;") != 1 || !strings.Contains(sddl, ";;;"+tok+")") {
		t.Fatalf("pipe DACL %s, want only %s", sddl, sid)
	}
}

// A client checks that the pipe's server runs as the current user (our
// own test process passes).
func TestCheckPipeServerAcceptsOwnProcess(t *testing.T) {
	ep := uniquePipe(t)
	ln, err := Listen(ep)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
	}()
	c, err := Dial(t.Context(), ep)
	if err != nil {
		t.Fatalf("dial own pipe: %v", err)
	}
	c.Close()
}

// Security review L5 (nm-harden): the client trusts a pipe's server only
// when it runs as the current user, at medium integrity or above, outside
// any AppContainer.
func TestTrustPipeServer(t *testing.T) {
	me, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	ours := me.User.Sid
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		tok  pipeServerToken
		ok   bool
	}{
		{"medium", pipeServerToken{user: ours, integrity: mandatoryMediumRID}, true},
		{"medium plus", pipeServerToken{user: ours, integrity: mandatoryMediumRID + 0x100}, true},
		{"high (elevated)", pipeServerToken{user: ours, integrity: mandatoryHighRID}, true},
		{"low", pipeServerToken{user: ours, integrity: mandatoryLowRID}, false},
		{"untrusted", pipeServerToken{user: ours, integrity: mandatoryUntrustedRID}, false},
		{"just below medium", pipeServerToken{user: ours, integrity: mandatoryMediumRID - 1}, false},
		{"AppContainer", pipeServerToken{user: ours, integrity: mandatoryMediumRID, appContainer: true}, false},
		{"low AppContainer", pipeServerToken{user: ours, integrity: mandatoryLowRID, appContainer: true}, false},
		{"another user", pipeServerToken{user: system, integrity: mandatoryHighRID}, false},
		{"no user", pipeServerToken{integrity: mandatoryMediumRID}, false},
	}
	for _, c := range cases {
		err := trustPipeServer(c.tok, ours)
		if c.ok && err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, ErrForeign) {
			t.Errorf("%s: %v, want ErrForeign", c.name, err)
		}
	}
	if err := trustPipeServer(pipeServerToken{user: ours, integrity: mandatoryMediumRID}, nil); !errors.Is(err, ErrForeign) {
		t.Errorf("no own user: %v", err)
	}
}

// readPipeServerToken reads real tokens: our own process (medium or
// high, not an AppContainer), and a copy of it lowered to low integrity.
func TestReadPipeServerToken(t *testing.T) {
	var cur windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &cur); err != nil {
		t.Fatal(err)
	}
	defer cur.Close()
	own, err := readPipeServerToken(cur)
	if err != nil {
		t.Fatal(err)
	}
	me, _ := windows.GetCurrentProcessToken().GetTokenUser()
	if !windows.EqualSid(own.user, me.User.Sid) || own.integrity < mandatoryMediumRID || own.appContainer {
		t.Fatalf("own token: user %v integrity %#x appContainer %v", own.user, own.integrity, own.appContainer)
	}
	if err := trustPipeServer(own, me.User.Sid); err != nil {
		t.Fatalf("own token not trusted: %v", err)
	}
	lt, err := readPipeServerToken(lowIntegrityToken(t))
	if err != nil {
		t.Fatal(err)
	}
	if lt.integrity != mandatoryLowRID || lt.appContainer || !windows.EqualSid(lt.user, me.User.Sid) {
		t.Fatalf("lowered token: integrity %#x appContainer %v", lt.integrity, lt.appContainer)
	}
	if err := trustPipeServer(lt, me.User.Sid); !errors.Is(err, ErrForeign) {
		t.Fatalf("low token trusted: %v", err)
	}
}

// Dial refuses the pipe whenever the server's token is not trusted or
// cannot be read (serverTokenOf stands in for the server here).
func TestDialChecksServerToken(t *testing.T) {
	ep := uniquePipe(t)
	ln, err := Listen(ep)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	real := serverTokenOf
	defer func() { serverTokenOf = real }()
	me, _ := windows.GetCurrentProcessToken().GetTokenUser()
	fakes := map[string]func(windows.Handle) (pipeServerToken, error){
		"low": func(windows.Handle) (pipeServerToken, error) {
			return pipeServerToken{user: me.User.Sid, integrity: mandatoryLowRID}, nil
		},
		"AppContainer": func(windows.Handle) (pipeServerToken, error) {
			return pipeServerToken{user: me.User.Sid, integrity: mandatoryMediumRID, appContainer: true}, nil
		},
		"unreadable": func(windows.Handle) (pipeServerToken, error) {
			return pipeServerToken{}, windows.ERROR_ACCESS_DENIED
		},
	}
	for name, fake := range fakes {
		serverTokenOf = fake
		c, err := Dial(t.Context(), ep)
		if c != nil {
			c.Close()
		}
		if !errors.Is(err, ErrForeign) {
			t.Errorf("%s: dial %v, want ErrForeign", name, err)
		}
	}
	serverTokenOf = real
	c, err := Dial(t.Context(), ep)
	if err != nil {
		t.Fatalf("real token: %v", err)
	}
	c.Close()
}

// A real low integrity process of the current user that took the pipe's
// name first (the squatter of L5): this test binary, started with a copy
// of our token lowered to low integrity, serves the pipe; Dial refuses it.
func TestDialRefusesLowIntegrityServer(t *testing.T) {
	ep := uniquePipe(t)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), lowPipeEnv+"="+ep)
	cmd.SysProcAttr = &syscall.SysProcAttr{Token: syscall.Token(lowIntegrityToken(t))}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start a low integrity process: %v", err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	line, err := bufio.NewReader(out).ReadString('\n')
	if strings.TrimSpace(line) != "ready" {
		t.Fatalf("low integrity server: %q %v", line, err)
	}
	// What the client learns about the server: our user (the check before
	// L5 let it through), at low integrity.
	var seen pipeServerToken
	real := serverTokenOf
	defer func() { serverTokenOf = real }()
	serverTokenOf = func(h windows.Handle) (pipeServerToken, error) {
		tok, err := real(h)
		seen = tok
		return tok, err
	}
	c, err := Dial(t.Context(), ep)
	if c != nil {
		c.Close()
	}
	if !errors.Is(err, ErrForeign) {
		t.Fatalf("dial a low integrity server: %v, want ErrForeign", err)
	}
	me, _ := windows.GetCurrentProcessToken().GetTokenUser()
	if seen.user == nil || !windows.EqualSid(seen.user, me.User.Sid) || seen.integrity != mandatoryLowRID {
		t.Fatalf("the server's token was not read as ours at low integrity: %+v", seen)
	}
}

// lowPipeEnv makes this test binary a pipe server (runTestChild).
const lowPipeEnv = "LECTUREFLOW_NMHOST_TEST_LOWPIPE"

// runTestChild serves the pipe named by lowPipeEnv until its first client
// leaves (or 30 seconds), after printing "ready".
func runTestChild() (int, bool) {
	name := os.Getenv(lowPipeEnv)
	if name == "" {
		return 0, false
	}
	ln, err := Listen(name)
	if err != nil {
		fmt.Println("error:", err)
		return 3, true
	}
	defer ln.Close()
	fmt.Println("ready")
	done := make(chan struct{})
	go func() {
		defer close(done)
		if c, err := ln.Accept(); err == nil {
			c.Read(make([]byte, 1))
			c.Close()
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
	}
	return 0, true
}

// lowIntegrityToken is a primary token like ours at low integrity (lowering
// one's own token needs no privilege; CreateProcessAsUser accepts it).
func lowIntegrityToken(t *testing.T) windows.Token {
	t.Helper()
	var cur windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY|windows.TOKEN_ADJUST_DEFAULT|windows.TOKEN_ASSIGN_PRIMARY, &cur); err != nil {
		t.Fatal(err)
	}
	defer cur.Close()
	var low windows.Token
	if err := windows.DuplicateTokenEx(cur, 0, nil, windows.SecurityImpersonation, windows.TokenPrimary, &low); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { low.Close() })
	sid, err := windows.CreateWellKnownSid(windows.WinLowLabelSid)
	if err != nil {
		t.Fatal(err)
	}
	tml := windows.Tokenmandatorylabel{Label: windows.SIDAndAttributes{Sid: sid, Attributes: windows.SE_GROUP_INTEGRITY}}
	if err := windows.SetTokenInformation(low, windows.TokenIntegrityLevel, (*byte)(unsafe.Pointer(&tml)), tml.Size()); err != nil {
		t.Fatal(err)
	}
	return low
}
