// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package nmhost

import (
	"errors"
	"strings"
	"testing"
)

func TestServiceStatusAndRegister(t *testing.T) {
	f := newRegFixture(t)
	reg := NewRegistrar(f.opts())
	ep := testEndpointFor(t)
	log, logs := newTestLogger()
	svc := StartService(ServiceOptions{Endpoint: ep, Handler: newFakeHandler(), Registrar: reg, RegisterOnStart: true, Log: log})
	defer svc.Close()

	waitFor(t, func() bool { return svc.Status().LastRegister != nil })
	st := svc.Status()
	if !st.Managed || !st.Listening || st.ListenError != "" || st.LastConnection != nil {
		t.Fatalf("status: %+v", st)
	}
	if s := states(st); s[BrowserChrome] != StateRegistered {
		t.Fatalf("states: %v", s)
	}

	// An extension connects: the status shows it.
	nc, err := Dial(t.Context(), ep)
	if err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, nc: nc}
	c.send(hello("h"))
	c.recv()
	nc.Close()
	st = svc.Status()
	if st.LastConnection == nil || st.LastConnection.Browser != "chrome" || st.LastConnection.ExtensionVersion != "0.1.0" {
		t.Fatalf("last connection: %+v", st.LastConnection)
	}

	// A second instance on the same endpoint runs without it.
	svc2 := StartService(ServiceOptions{Endpoint: ep, Handler: newFakeHandler(), Log: log})
	defer svc2.Close()
	st2 := svc2.Status()
	if st2.Listening || st2.ListenError != ListenInUse || st2.Managed || len(st2.Browsers) != 0 {
		t.Fatalf("second instance: %+v", st2)
	}
	if _, err := svc2.Register(); !errors.Is(err, ErrNotManaged) {
		t.Fatalf("register on an unmanaged instance: %v", err)
	}
	if !strings.Contains(logs.String(), "served by another process") {
		t.Errorf("no warning for the endpoint in use:\n%s", logs.String())
	}

	// Registering again ("重新登记") repairs a removed key.
	delete(f.reg.vals, RegistryKey(BrowserFirefox))
	if s := states(svc.Status()); s[BrowserFirefox] != StateNotRegistered {
		t.Fatalf("after removing the key: %v", s)
	}
	st, err = svc.Register()
	if err != nil || states(st)[BrowserFirefox] != StateRegistered {
		t.Fatalf("register: %v %+v", err, st)
	}

	svc.Close()
	if svc.Status().Listening {
		t.Fatal("still listening after Close")
	}
	if _, err := Dial(t.Context(), ep); !errors.Is(err, ErrNotListening) {
		t.Fatalf("dial after Close: %v", err)
	}
}

func TestServiceOff(t *testing.T) {
	svc := StartService(ServiceOptions{Endpoint: "", Handler: newFakeHandler()})
	defer svc.Close()
	if st := svc.Status(); st.Listening || st.ListenError != ListenOff {
		t.Fatalf("status: %+v", st)
	}
}
