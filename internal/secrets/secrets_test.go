// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package secrets

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// reset clears the process's keys (tests only).
func reset(t *testing.T) {
	t.Helper()
	mu.Lock()
	aead, fallbacks = nil, nil
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		aead, fallbacks = nil, nil
		mu.Unlock()
	})
}

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, KeySize) }

const fakeAPIKey = "sk-test-0123456789abcdef"

func TestSealOpenRoundTrip(t *testing.T) {
	reset(t)
	if _, err := Seal([]byte(fakeAPIKey)); !errors.Is(err, ErrNoKey) {
		t.Fatalf("Seal without a key: %v", err)
	}
	if HasKey() {
		t.Fatal("HasKey before SetKey")
	}
	if err := SetKey(testKey(1)[:16]); err == nil {
		t.Fatal("a 16-byte key was accepted")
	}
	if err := SetKey(testKey(1)); err != nil {
		t.Fatal(err)
	}
	a, err := Seal([]byte(fakeAPIKey))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Seal([]byte(fakeAPIKey))
	if !IsSealed(a) || a == b || strings.Contains(a, fakeAPIKey) {
		t.Fatalf("sealed %q / %q", a, b)
	}
	for _, s := range []string{a, b} {
		p, err := Open(s)
		if err != nil || string(p) != fakeAPIKey {
			t.Fatalf("Open: %q %v", p, err)
		}
		p, cur, err := OpenAny(s)
		if err != nil || !cur || string(p) != fakeAPIKey {
			t.Fatalf("OpenAny: %q %v %v", p, cur, err)
		}
	}
	// empty values seal too
	e, _ := Seal(nil)
	if p, err := Open(e); err != nil || len(p) != 0 {
		t.Fatalf("empty: %q %v", p, err)
	}
	if _, err := Open(fakeAPIKey); err == nil || errors.Is(err, ErrUndecryptable) {
		t.Fatalf("plain value: %v", err)
	}
}

// Any change of a sealed value, and another key, give ErrUndecryptable.
func TestOpenTampered(t *testing.T) {
	reset(t)
	if err := SetKey(testKey(2)); err != nil {
		t.Fatal(err)
	}
	s, _ := Seal([]byte(fakeAPIKey))
	raw, _ := base64.RawStdEncoding.DecodeString(s[len(Prefix):])
	for i := range raw {
		mod := append([]byte(nil), raw...)
		mod[i] ^= 0x01
		if _, err := Open(Prefix + base64.RawStdEncoding.EncodeToString(mod)); !errors.Is(err, ErrUndecryptable) {
			t.Fatalf("byte %d flipped: %v", i, err)
		}
	}
	for _, bad := range []string{Prefix, Prefix + "!!!", Prefix + base64.RawStdEncoding.EncodeToString(raw[:20]), s[:len(s)-2]} {
		if _, err := Open(bad); !errors.Is(err, ErrUndecryptable) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if err := SetKey(testKey(3)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(s); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("another key: %v", err)
	}
	if _, _, err := OpenAny(s); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("OpenAny, another key: %v", err)
	}
}

func TestFallbackKeys(t *testing.T) {
	reset(t)
	old := testKey(4)
	if err := SetKey(old); err != nil {
		t.Fatal(err)
	}
	s, _ := Seal([]byte(fakeAPIKey))
	if err := SetKey(testKey(5)); err != nil {
		t.Fatal(err)
	}
	if err := SetFallbackKeys(testKey(6), old); err != nil {
		t.Fatal(err)
	}
	p, cur, err := OpenAny(s)
	if err != nil || cur || string(p) != fakeAPIKey {
		t.Fatalf("fallback: %q %v %v", p, cur, err)
	}
	// Open (the frozen API) only uses the data key
	if _, err := Open(s); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("Open with a fallback: %v", err)
	}
	if p, err := OpenWith(old, s); err != nil || string(p) != fakeAPIKey {
		t.Fatalf("OpenWith: %q %v", p, err)
	}
	if _, err := OpenWith(testKey(7), s); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("OpenWith another key: %v", err)
	}
	if err := SetFallbackKeys(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenAny(s); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("fallbacks cleared: %v", err)
	}
	if err := SetFallbackKeys([]byte("short")); err == nil {
		t.Fatal("a short fallback key was accepted")
	}
	reset(t)
	if _, _, err := OpenAny(s); !errors.Is(err, ErrNoKey) {
		t.Fatalf("no key: %v", err)
	}
}
