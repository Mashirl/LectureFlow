// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

// Keys (plan §3): every server gets its own ed25519 key. The private key
// exists in the clear only in this process's memory: it is stored sealed
// with internal/secrets in the kv row KeyRef (a seal fails without a data
// key, so a plain key is never written). The API hands out only the public
// key, as an authorized_keys line with "restrict" (no forwarding, no PTY,
// no ~/.ssh/rc) and its fingerprint.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"

	"lectureflow/internal/secrets"
)

// KVKeyPrefix starts the kv key of a server's sealed private key.
const KVKeyPrefix = "remote.ssh.key."

// ErrNoDataKey: the data key of internal/secrets is not available, so no
// private key can be stored or read.
var ErrNoDataKey = errors.New("sshremote: the credential store is not available")

// newKey generates a key pair and returns the sealed private key and the
// public key's authorized_keys line ("restrict ssh-ed25519 AAAA… comment").
func newKey(comment string) (sealed, authorized string, err error) {
	if !secrets.HasKey() {
		return "", "", ErrNoDataKey
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return "", "", err
	}
	sealed, err = secrets.Seal(pem.EncodeToMemory(block))
	if err != nil {
		return "", "", err
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", err
	}
	return sealed, authorizedLine(sp, comment), nil
}

// authorizedLine is the line the user appends to ~/.ssh/authorized_keys.
func authorizedLine(pub ssh.PublicKey, comment string) string {
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	return "restrict " + line + " " + comment
}

// openKey opens a sealed private key as a signer.
func openKey(sealed string) (ssh.Signer, error) {
	if !secrets.HasKey() {
		return nil, ErrNoDataKey
	}
	// OpenAny: a value sealed with an earlier key (not re-sealed yet) opens too.
	b, _, err := secrets.OpenAny(sealed)
	if err != nil {
		return nil, err
	}
	s, err := ssh.ParsePrivateKey(b)
	for i := range b {
		b[i] = 0
	}
	return s, err
}

// keyStore is where the sealed keys live (the kv table).
type keyStore interface {
	GetKV(ctx context.Context, key string) (string, bool, error)
	SetKV(ctx context.Context, key, value string) error
}

// loadSigner reads and opens a server's key.
func loadSigner(ctx context.Context, st keyStore, ref string) (ssh.Signer, error) {
	v, ok, err := st.GetKV(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !ok || v == "" {
		return nil, fmt.Errorf("sshremote: the key %s is missing", ref)
	}
	if !secrets.IsSealed(v) {
		return nil, fmt.Errorf("sshremote: the key %s is not sealed", ref)
	}
	return openKey(v)
}

// PublicFingerprint is the SHA256 fingerprint of an authorized_keys line.
func PublicFingerprint(line string) string {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "restrict "))
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(pub)
}
