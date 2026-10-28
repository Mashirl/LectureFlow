// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// minisign (https://jedisct1.github.io/minisign/) signature verification.
//
// Public key: base64 of "Ed" || key id (8 bytes) || Ed25519 public key (32
// bytes); the key file has an "untrusted comment:" line before it.
//
// Signature file (.minisig), four lines:
//
//	untrusted comment: <text>
//	base64("Ed" or "ED" || key id (8) || signature (64))
//	trusted comment: <text>
//	base64(global signature (64))
//
// "Ed" (legacy) signs the file itself, "ED" (the default since minisign
// 0.11) signs its BLAKE2b-512 hash. The global signature signs
// signature || trusted comment, so the trusted comment cannot be altered
// either. Both kinds are accepted.

const (
	untrustedPrefix = "untrusted comment: "
	trustedPrefix   = "trusted comment: "
	keyIDLen        = 8
)

var (
	algLegacy    = [2]byte{'E', 'd'}
	algPrehashed = [2]byte{'E', 'D'}
)

// Key is a minisign public key.
type Key struct {
	KeyID [keyIDLen]byte
	Key   ed25519.PublicKey
}

// ParsePublicKey reads a public key: the base64 line alone ("RWQ…") or the
// whole .pub file (an untrusted comment line, then the base64 line).
func ParsePublicKey(s string) (Key, error) {
	var pk Key
	lines := splitLines(strings.TrimSpace(s))
	line := ""
	switch {
	case len(lines) == 1:
		line = lines[0]
	case len(lines) == 2 && strings.HasPrefix(lines[0], untrustedPrefix):
		line = lines[1]
	default:
		return pk, errors.New("minisign public key: want one base64 line, optionally after an untrusted comment")
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(line))
	if err != nil || len(b) != 2+keyIDLen+ed25519.PublicKeySize {
		return pk, errors.New("minisign public key: not a base64-encoded Ed25519 key")
	}
	if [2]byte{b[0], b[1]} != algLegacy {
		return pk, fmt.Errorf("minisign public key: unsupported algorithm %q", b[:2])
	}
	copy(pk.KeyID[:], b[2:2+keyIDLen])
	pk.Key = ed25519.PublicKey(bytes.Clone(b[2+keyIDLen:]))
	return pk, nil
}

// String returns the public key's base64 line.
func (pk Key) String() string {
	b := make([]byte, 0, 2+keyIDLen+ed25519.PublicKeySize)
	b = append(b, algLegacy[:]...)
	b = append(b, pk.KeyID[:]...)
	b = append(b, pk.Key...)
	return base64.StdEncoding.EncodeToString(b)
}

// Signature is a parsed .minisig file.
type Signature struct {
	Algorithm       [2]byte
	KeyID           [keyIDLen]byte
	Sig             []byte // 64 bytes
	TrustedComment  string // without the "trusted comment: " prefix
	GlobalSignature []byte // 64 bytes
}

// ParseSignature reads a .minisig file (LF or CRLF line ends; a final line
// end is optional).
func ParseSignature(data []byte) (*Signature, error) {
	lines := splitLines(strings.TrimRight(string(data), "\r\n"))
	if len(lines) != 4 {
		return nil, fmt.Errorf("minisign signature: %d lines, want 4", len(lines))
	}
	if !strings.HasPrefix(lines[0], untrustedPrefix) {
		return nil, errors.New("minisign signature: the first line is not an untrusted comment")
	}
	b, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil || len(b) != 2+keyIDLen+ed25519.SignatureSize {
		return nil, errors.New("minisign signature: malformed signature line")
	}
	sig := &Signature{}
	copy(sig.Algorithm[:], b[:2])
	if sig.Algorithm != algLegacy && sig.Algorithm != algPrehashed {
		return nil, fmt.Errorf("minisign signature: unsupported algorithm %q", b[:2])
	}
	copy(sig.KeyID[:], b[2:2+keyIDLen])
	sig.Sig = bytes.Clone(b[2+keyIDLen:])
	if !strings.HasPrefix(lines[2], trustedPrefix) {
		return nil, errors.New("minisign signature: the third line is not a trusted comment")
	}
	sig.TrustedComment = strings.TrimPrefix(lines[2], trustedPrefix)
	g, err := base64.StdEncoding.DecodeString(lines[3])
	if err != nil || len(g) != ed25519.SignatureSize {
		return nil, errors.New("minisign signature: malformed global signature")
	}
	sig.GlobalSignature = g
	return sig, nil
}

// Verify checks that sig signs msg with pk and that the trusted comment is
// authentic.
func (pk Key) Verify(msg []byte, sig *Signature) error {
	if len(pk.Key) != ed25519.PublicKeySize {
		return errors.New("minisign: no public key")
	}
	if sig.KeyID != pk.KeyID {
		return fmt.Errorf("minisign: signed with key %X, not with the built-in key %X", keyIDDisplay(sig.KeyID), keyIDDisplay(pk.KeyID))
	}
	signed := msg
	if sig.Algorithm == algPrehashed {
		h := blake2b.Sum512(msg)
		signed = h[:]
	}
	if !ed25519.Verify(pk.Key, signed, sig.Sig) {
		return errors.New("minisign: the signature does not match the file")
	}
	global := make([]byte, 0, len(sig.Sig)+len(sig.TrustedComment))
	global = append(global, sig.Sig...)
	global = append(global, sig.TrustedComment...)
	if !ed25519.Verify(pk.Key, global, sig.GlobalSignature) {
		return errors.New("minisign: the trusted comment is not authentic")
	}
	return nil
}

// keyIDDisplay is the key id as minisign prints it (little-endian number).
func keyIDDisplay(id [keyIDLen]byte) []byte {
	out := make([]byte, keyIDLen)
	for i := range id {
		out[i] = id[keyIDLen-1-i]
	}
	return out
}

// TrustedPrefix is how the trusted comment of an update.json signature
// starts; scripts/release/sign-update.* writes
// "lectureflow update version:X.Y.Z".
const TrustedPrefix = "lectureflow update"

// TrustedComment is the trusted comment for version v.
func TrustedComment(v string) string { return TrustedPrefix + " version:" + v }

// trustedVersion returns the version a trusted comment names. The comment
// is "lectureflow update" followed by space-separated key:value fields,
// exactly one of them version:…; other fields are allowed and ignored.
func trustedVersion(tc string) (string, bool) {
	f := strings.Fields(tc)
	if len(f) < 3 || f[0] != "lectureflow" || f[1] != "update" {
		return "", false
	}
	v := ""
	for _, x := range f[2:] {
		k, val, ok := strings.Cut(x, ":")
		if !ok || k == "" {
			return "", false
		}
		if k == "version" {
			if v != "" || val == "" {
				return "", false
			}
			v = val
		}
	}
	return v, v != ""
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}
