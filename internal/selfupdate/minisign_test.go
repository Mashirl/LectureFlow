// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"lectureflow/internal/selfupdate/selfupdatetest"
)

func TestParsePublicKeyForms(t *testing.T) {
	k := newTestKey(t)
	for name, in := range map[string]string{
		"line":      k.key.String(),
		"file":      k.pubFile(),
		"file CRLF": strings.ReplaceAll(k.pubFile(), "\n", "\r\n"),
		"spaces":    "  " + k.key.String() + "\n",
	} {
		pk, err := ParsePublicKey(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if pk.KeyID != k.key.KeyID || !bytes.Equal(pk.Key, k.key.Key) {
			t.Fatalf("%s: wrong key", name)
		}
	}
	raw, _ := base64.StdEncoding.DecodeString(k.key.String())
	raw[1] = 'D' // a signature algorithm, not a key algorithm
	for name, in := range map[string]string{
		"empty":       "",
		"not base64":  "RWQ!!!",
		"short":       base64.StdEncoding.EncodeToString(raw[:20]),
		"wrong alg":   base64.StdEncoding.EncodeToString(raw),
		"three lines": "untrusted comment: x\n" + k.key.String() + "\nextra",
		"bad comment": "comment: x\n" + k.key.String(),
	} {
		if _, err := ParsePublicKey(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestVerifyLegacyAndPrehashed(t *testing.T) {
	k := newTestKey(t)
	msg := []byte(`{"hello": "world"}`)
	for _, pre := range []bool{false, true} {
		sig, err := ParseSignature(k.sign(msg, "lectureflow update version:1.2.3", pre))
		if err != nil {
			t.Fatal(err)
		}
		want := algLegacy
		if pre {
			want = algPrehashed
		}
		if sig.Algorithm != want {
			t.Fatalf("algorithm %q", sig.Algorithm[:])
		}
		if err := k.key.Verify(msg, sig); err != nil {
			t.Fatalf("prehashed=%v: %v", pre, err)
		}
		// CRLF line ends, no final newline
		crlf := bytes.TrimRight(bytes.ReplaceAll(k.sign(msg, "lectureflow update version:1.2.3", pre), []byte("\n"), []byte("\r\n")), "\r\n")
		sig2, err := ParseSignature(crlf)
		if err != nil || k.key.Verify(msg, sig2) != nil {
			t.Fatalf("CRLF: %v", err)
		}
	}
}

// Every byte of the signature line (algorithm, key id, signature), of the
// trusted comment and of the global signature is covered.
func TestSignatureTamperedAnywhere(t *testing.T) {
	k := newTestKey(t)
	msg := []byte(`{"version": "3.2.0"}`)
	for _, pre := range []bool{false, true} {
		good, err := ParseSignature(k.sign(msg, "lectureflow update version:3.2.0", pre))
		if err != nil {
			t.Fatal(err)
		}
		line := append(append(append([]byte{}, good.Algorithm[:]...), good.KeyID[:]...), good.Sig...)
		for i := range line {
			bad := bytes.Clone(line)
			bad[i] ^= 0x01
			text := strings.Join([]string{"untrusted comment: x", base64.StdEncoding.EncodeToString(bad),
				"trusted comment: " + good.TrustedComment, base64.StdEncoding.EncodeToString(good.GlobalSignature)}, "\n")
			if s, err := ParseSignature([]byte(text)); err == nil && k.key.Verify(msg, s) == nil {
				t.Fatalf("prehashed=%v: signature byte %d changed and still verifies", pre, i)
			}
		}
		for i := range good.TrustedComment {
			tc := []byte(good.TrustedComment)
			tc[i] ^= 0x01
			s := *good
			s.TrustedComment = string(tc)
			if k.key.Verify(msg, &s) == nil {
				t.Fatalf("trusted comment byte %d changed and still verifies", i)
			}
		}
		for i := range good.GlobalSignature {
			s := *good
			s.GlobalSignature = bytes.Clone(good.GlobalSignature)
			s.GlobalSignature[i] ^= 0x01
			if k.key.Verify(msg, &s) == nil {
				t.Fatalf("global signature byte %d changed and still verifies", i)
			}
		}
	}
}

// The untrusted comment is not signed (minisign's design): changing it
// changes nothing.
func TestUntrustedCommentIsNotSigned(t *testing.T) {
	k := newTestKey(t)
	msg := []byte("x")
	data := bytes.Replace(k.sign(msg, "lectureflow update version:1.0.0", true),
		[]byte("signature from minisign secret key"), []byte("anything at all"), 1)
	sig, err := ParseSignature(data)
	if err != nil || k.key.Verify(msg, sig) != nil {
		t.Fatalf("%v", err)
	}
}

func TestOtherKeyRejected(t *testing.T) {
	k, other := newTestKey(t), newTestKey(t)
	msg := []byte("update")
	sig, _ := ParseSignature(other.sign(msg, "lectureflow update version:1.0.0", true))
	if err := k.key.Verify(msg, sig); err == nil || !strings.Contains(err.Error(), "signed with key") {
		t.Fatalf("another key's signature: %v", err)
	}
	// the other key claiming our key id
	forged := testKey{Key: &selfupdatetest.Key{Priv: other.Priv, Pub: other.Pub, ID: k.key.KeyID}}
	sig, _ = ParseSignature(forged.sign(msg, "lectureflow update version:1.0.0", true))
	if err := k.key.Verify(msg, sig); err == nil {
		t.Fatal("a forged key id verified")
	}
}

func TestMalformedSignatureFiles(t *testing.T) {
	k := newTestKey(t)
	good := string(k.sign([]byte("m"), "lectureflow update version:1.0.0", true))
	lines := strings.Split(strings.TrimSpace(good), "\n")
	for name, text := range map[string]string{
		"empty":             "",
		"three lines":       strings.Join(lines[:3], "\n"),
		"five lines":        good + "extra\n",
		"no untrusted":      strings.Join(append([]string{"comment: x"}, lines[1:]...), "\n"),
		"no trusted":        strings.Join([]string{lines[0], lines[1], "comment: x", lines[3]}, "\n"),
		"bad base64":        strings.Join([]string{lines[0], "!!!", lines[2], lines[3]}, "\n"),
		"short global":      strings.Join([]string{lines[0], lines[1], lines[2], base64.StdEncoding.EncodeToString([]byte("short"))}, "\n"),
		"unknown algorithm": strings.Join([]string{lines[0], base64.StdEncoding.EncodeToString(append([]byte("XX"), make([]byte, 72)...)), lines[2], lines[3]}, "\n"),
	} {
		if _, err := ParseSignature([]byte(text)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestTrustedVersion(t *testing.T) {
	for in, want := range map[string]string{
		"lectureflow update version:3.2.0":              "3.2.0",
		"lectureflow update version:3.2.0-beta.1":       "3.2.0-beta.1",
		"lectureflow update channel:beta version:3.2.0": "3.2.0",
		TrustedComment("4.0.0"):                         "4.0.0",
	} {
		if got, ok := trustedVersion(in); !ok || got != want {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	for _, in := range []string{
		"",
		"timestamp:1700000000\tfile:update.json\thashed", // minisign's default comment
		"lectureflow update",
		"lectureflow update version:",
		"lectureflow update version:1.0.0 version:1.0.0",
		"lectureflow update 1.0.0",
		"other update version:1.0.0",
	} {
		if v, ok := trustedVersion(in); ok {
			t.Errorf("%q accepted as %q", in, v)
		}
	}
}
