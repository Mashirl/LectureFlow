// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"lectureflow/internal/bridge"
)

// aria2c reads none of the user's files (~/.netrc) and follows no Metalink
// or torrent, with no BitTorrent discovery (security review).
func TestAria2cOptionsHardened(t *testing.T) {
	e := newQEnv(t, nil)
	e.media.put("/files/a.m4a", pattern(1, 1000))
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindAudio, "/files/a.m4a")))
	e.wait(id, bridge.StateDone)
	_, args, _, _, _ := e.aria.snapshot()
	for _, want := range []string{"--no-netrc=true", "--follow-metalink=false", "--follow-torrent=false", "--enable-dht=false",
		"--enable-dht6=false", "--bt-enable-lpd=false", "--enable-peer-exchange=false", "--rpc-listen-all=false"} {
		if !slices.Contains(args, want) {
			t.Errorf("aria2c started without %s: %v", want, args)
		}
	}
}

// A download that aria2c followed (a Metalink answer) fails the task and
// stops what was started from it.
func TestFollowedDownloadFails(t *testing.T) {
	e := newQEnv(t, nil)
	e.media.put("/files/a.meta4", []byte(`<?xml version="1.0"?><metalink xmlns="urn:ietf:params:xml:ns:metalink"/>`))
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindAudio, "/files/a.meta4")))
	info := e.wait(id, bridge.StateFailed)
	if info.Error == nil || info.Error.Code != CodeEngineFailed {
		t.Fatalf("error %+v", info.Error)
	}
	e.aria.mu.Lock()
	removed := slices.Contains(e.aria.removed, "00000000000000ff")
	e.aria.mu.Unlock()
	if !removed {
		t.Fatal("the followed download was not stopped")
	}
	if len(e.importsSnapshot()) != 0 {
		t.Fatal("the Metalink was imported")
	}
}

// A refused server certificate has its own code, from aria2c and from Go's
// own requests.
func TestTLSFailureCode(t *testing.T) {
	for _, msg := range []string{"SSL/TLS handshake failure: Invalid certificate chain",
		"SSL/TLS handshake failure: `certificate verify failed'"} {
		var f *failure
		if !errors.As(ariaFailure(ariaStatus{ErrorCode: "1", ErrorMessage: msg}), &f) || f.code != CodeTLSFailed {
			t.Errorf("%q: %+v", msg, f)
		}
	}
	if f := netFailure(fmt.Errorf("get: %w", x509.UnknownAuthorityError{})); f.code != CodeTLSFailed {
		t.Fatalf("x509: %+v", f)
	}
	if f := netFailure(errors.New("connection refused")); f.code != CodeNetwork {
		t.Fatalf("network: %+v", f)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("#EXTM3U\n")) }))
	defer srv.Close()
	e := newQEnv(t, nil)
	_, _, err := e.q.getPlaylist(context.Background(), srv.URL+"/x.m3u8", runSnapshot{})
	var f *failure
	if !errors.As(err, &f) || f.code != CodeTLSFailed {
		t.Fatalf("playlist over an untrusted certificate: %v", err)
	}
}
