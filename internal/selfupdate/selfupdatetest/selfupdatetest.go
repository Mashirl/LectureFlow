// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package selfupdatetest makes minisign key pairs and signatures for tests
// of the in-app update. Keys are generated on the spot; no real key is ever
// used or stored. Only test code imports this package.
package selfupdatetest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"golang.org/x/crypto/blake2b"
)

// Key is a minisign key pair.
type Key struct {
	Priv ed25519.PrivateKey
	Pub  ed25519.PublicKey
	ID   [8]byte
}

// NewKey generates a key pair with a random key id.
func NewKey() (*Key, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	k := &Key{Priv: priv, Pub: pub}
	if _, err := rand.Read(k.ID[:]); err != nil {
		return nil, err
	}
	return k, nil
}

// PublicKey is the base64 line of the .pub file.
func (k *Key) PublicKey() string {
	b := append(append([]byte("Ed"), k.ID[:]...), k.Pub...)
	return base64.StdEncoding.EncodeToString(b)
}

// PublicKeyFile is the key as minisign writes a .pub file.
func (k *Key) PublicKeyFile() string {
	var id [8]byte
	for i := range id {
		id[i] = k.ID[7-i]
	}
	return fmt.Sprintf("untrusted comment: minisign public key %X\n%s\n", id, k.PublicKey())
}

// Sign makes a .minisig file the way minisign does: prehashed ("ED",
// BLAKE2b-512, minisign's default) or legacy ("Ed").
func (k *Key) Sign(msg []byte, trustedComment string, prehashed bool) []byte {
	alg, signed := []byte("Ed"), msg
	if prehashed {
		h := blake2b.Sum512(msg)
		alg, signed = []byte("ED"), h[:]
	}
	sig := ed25519.Sign(k.Priv, signed)
	line := append(append(append([]byte{}, alg...), k.ID[:]...), sig...)
	global := ed25519.Sign(k.Priv, append(append([]byte{}, sig...), trustedComment...))
	return []byte("untrusted comment: signature from minisign secret key\n" +
		base64.StdEncoding.EncodeToString(line) + "\n" +
		"trusted comment: " + trustedComment + "\n" +
		base64.StdEncoding.EncodeToString(global) + "\n")
}
