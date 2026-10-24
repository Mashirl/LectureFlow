// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package wav

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// file builds a 16 kHz mono 16-bit WAV of ms milliseconds; dataSize
// overrides the data chunk's size field (0xFFFFFFFF: a streamed header),
// extra adds a chunk before "data".
func file(ms int, dataSize uint32, extra bool) []byte {
	n := 16000 * 2 * ms / 1000
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	w(uint32(36 + n))
	b.WriteString("WAVEfmt ")
	w(uint32(16))
	w(uint16(1))
	w(uint16(1))
	w(uint32(16000))
	w(uint32(32000))
	w(uint16(2))
	w(uint16(16))
	if extra {
		b.WriteString("LIST")
		w(uint32(5))
		b.WriteString("abcde\x00") // odd size: one pad byte
	}
	b.WriteString("data")
	if dataSize == 0 {
		dataSize = uint32(n)
	}
	w(dataSize)
	b.Write(make([]byte, n))
	return b.Bytes()
}

func TestDurationMS(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		name string
		data []byte
		ms   int64
		ok   bool
	}{
		{"plain", file(6000, 0, false), 6000, true},
		{"streamed header", file(2500, 0xFFFFFFFF, false), 2500, true},
		{"chunk before data", file(1000, 0, true), 1000, true},
		{"size larger than the file", file(1000, 1<<30, false), 1000, true},
		{"not a wav", []byte("RIFF fake wav"), 0, false},
		{"empty", nil, 0, false},
	} {
		p := filepath.Join(dir, c.name+".wav")
		if err := os.WriteFile(p, c.data, 0o644); err != nil {
			t.Fatal(err)
		}
		if ms, ok := DurationMS(p); ms != c.ms || ok != c.ok {
			t.Errorf("%s: %d %v, want %d %v", c.name, ms, ok, c.ms, c.ok)
		}
	}
	if _, ok := DurationMS(filepath.Join(dir, "missing.wav")); ok {
		t.Error("missing file")
	}
}
