// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

// Host keys (plan §3): the first connection shows the server's key (its
// algorithm and SHA256 fingerprint, and whether the user's own known_hosts
// agrees) and stops; only after the user confirmed it is the key pinned
// (remote_server.host_key). From then on any other key is refused, before
// authentication: nothing is sent to a server that is not the pinned one.

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// HostKeyInfo is a presented host key.
type HostKeyInfo struct {
	Algorithm   string `json:"algorithm"`
	Fingerprint string `json:"fingerprint"` // "SHA256:…"
	// Key is the key in authorized_keys format (what Trust pins).
	Key string `json:"key"`
	// KnownHosts: "match" (the user's ~/.ssh/known_hosts lists this key for
	// the host), "mismatch" (it lists another), "" (no entry or no file).
	KnownHosts string `json:"known_hosts,omitempty"`
	// Check is the command that shows the fingerprint on the server.
	Check string `json:"check"`
}

var (
	// errHostKeyUnknown: no key is pinned yet (the connection stopped
	// after reading it).
	errHostKeyUnknown = errors.New("the server's host key is not confirmed yet")
	// errHostKeyMismatch: the server presented another key than the pinned one.
	errHostKeyMismatch = errors.New("the server's host key differs from the confirmed one")
)

// hostKeyCallback checks the presented key against pinned (authorized_keys
// format; "" = none yet). seen receives the presented key either way.
func hostKeyCallback(pinned string, seen func(ssh.PublicKey)) ssh.HostKeyCallback {
	var want ssh.PublicKey
	if pinned != "" {
		if k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pinned)); err == nil {
			want = k
		}
	}
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if seen != nil {
			seen(key)
		}
		if pinned == "" {
			return errHostKeyUnknown
		}
		if want == nil || !bytes.Equal(want.Marshal(), key.Marshal()) {
			return fmt.Errorf("%w (presented %s)", errHostKeyMismatch, ssh.FingerprintSHA256(key))
		}
		return nil
	}
}

// describeKey is the HostKeyInfo of a presented key for host:port.
func describeKey(key ssh.PublicKey, host string, port int) HostKeyInfo {
	info := HostKeyInfo{Algorithm: key.Type(), Fingerprint: ssh.FingerprintSHA256(key),
		Key: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))}
	alg := strings.TrimPrefix(key.Type(), "ssh-")
	if strings.HasPrefix(key.Type(), "ecdsa-") {
		alg = "ecdsa"
	}
	info.Check = fmt.Sprintf("ssh-keygen -lf /etc/ssh/ssh_host_%s_key.pub", alg)
	info.KnownHosts = knownHostsVerdict(key, host, port)
	return info
}

// knownHostsVerdict compares with the user's own ~/.ssh/known_hosts (read
// only; LectureFlow never writes it).
func knownHostsVerdict(key ssh.PublicKey, host string, port int) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	file := filepath.Join(home, ".ssh", "known_hosts")
	if _, err := os.Stat(file); err != nil {
		return ""
	}
	cb, err := knownhosts.New(file)
	if err != nil {
		return ""
	}
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	err = cb(knownhosts.Normalize(addr), &net.TCPAddr{}, key)
	var ke *knownhosts.KeyError
	switch {
	case err == nil:
		return "match"
	case errors.As(err, &ke) && len(ke.Want) > 0:
		return "mismatch"
	}
	return ""
}
