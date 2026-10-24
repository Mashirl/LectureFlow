// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// pcmFile builds a WAV file: fmt chunk with tag, an optional LIST chunk
// before data (odd size, padded), and data = frames × blockAlign bytes where
// byte k is k mod 251 (so a copied range can be identified).
func pcmFile(tag uint16, rate uint32, channels, bits uint16, frames int, list bool) ([]byte, []byte) {
	align := channels * bits / 8
	data := make([]byte, frames*int(align))
	for k := range data {
		data[k] = byte(k % 251)
	}
	var b bytes.Buffer
	body := new(bytes.Buffer)
	body.WriteString("WAVE")
	fmtBody := make([]byte, 16)
	binary.LittleEndian.PutUint16(fmtBody[0:], tag)
	binary.LittleEndian.PutUint16(fmtBody[2:], channels)
	binary.LittleEndian.PutUint32(fmtBody[4:], rate)
	binary.LittleEndian.PutUint32(fmtBody[8:], rate*uint32(align))
	binary.LittleEndian.PutUint16(fmtBody[12:], align)
	binary.LittleEndian.PutUint16(fmtBody[14:], bits)
	body.WriteString("fmt ")
	binary.Write(body, binary.LittleEndian, uint32(len(fmtBody)))
	body.Write(fmtBody)
	if list {
		body.WriteString("LIST")
		binary.Write(body, binary.LittleEndian, uint32(5))
		body.WriteString("INFOx\x00") // 5 bytes + pad
	}
	body.WriteString("data")
	binary.Write(body, binary.LittleEndian, uint32(len(data)))
	body.Write(data)
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(body.Len()))
	b.Write(body.Bytes())
	return b.Bytes(), data
}

func writeTemp(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCut: the clip holds exactly the source bytes of the range, aligned to
// whole frames, is a valid WAV of the right length, and replaces dst via
// dst.tmp.
func TestCut(t *testing.T) {
	// 16 kHz stereo 16-bit: block_align 4, 64000 bytes/s; 3 s of audio.
	src, data := pcmFile(formatPCM, 16000, 2, 16, 48000, true)
	in := writeTemp(t, "in.wav", src)
	dst := filepath.Join(t.TempDir(), "clip.wav")
	if err := os.WriteFile(dst, []byte("old clip"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 1000–2000 ms = bytes 64000–128000 of the data (behind an odd LIST chunk)
	ms, err := Cut(in, dst, 1000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if ms != 1000 {
		t.Errorf("duration %d ms", ms)
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Error("dst.tmp left behind")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := DurationMS(dst); !ok || d != 1000 {
		t.Errorf("DurationMS of the clip %d %v", d, ok)
	}
	samples := got[len(got)-64000:]
	if !bytes.Equal(samples, data[64000:128000]) {
		t.Error("clip samples are not the source range")
	}
	if string(got[0:4]) != "RIFF" || int(binary.LittleEndian.Uint32(got[4:8])) != len(got)-8 || string(got[36:40]) != "data" {
		t.Errorf("clip header % x", got[:44])
	}

	// Unaligned byte positions (block_align 6: 3 channels × 16 bit at
	// 1001 Hz, 6006 bytes/s; 1333 ms = byte 8005): both ends are rounded
	// down to a whole frame.
	src3, data3 := pcmFile(formatPCM, 1001, 3, 16, 3000, false)
	in3 := writeTemp(t, "in3.wav", src3)
	dst3 := filepath.Join(t.TempDir(), "c3.wav")
	if _, err := Cut(in3, dst3, 7, 1333); err != nil {
		t.Fatal(err)
	}
	b3, _ := os.ReadFile(dst3)
	n := int(binary.LittleEndian.Uint32(b3[40:44]))
	from := 7 * 6006 / 1000 // 42 bytes, a multiple of 6
	from -= from % 6
	to := 1333 * 6006 / 1000
	to -= to % 6
	if n != to-from || n%6 != 0 || !bytes.Equal(b3[44:44+n], data3[from:to]) {
		t.Errorf("unaligned cut: %d bytes, want %d (from %d)", n, to-from, from)
	}
}

// TestCutClamps: a range beyond the audio is clamped to it; a range entirely
// outside, or empty, is an error and writes nothing.
func TestCutClamps(t *testing.T) {
	src, data := pcmFile(formatPCM, 16000, 1, 16, 32000, false) // 2 s mono
	in := writeTemp(t, "in.wav", src)
	dir := t.TempDir()
	dst := filepath.Join(dir, "clip.wav")
	ms, err := Cut(in, dst, -500, 9000)
	if err != nil || ms != 2000 {
		t.Fatalf("clamped cut: %d ms, %v", ms, err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got[44:], data) {
		t.Error("clamped clip is not the whole audio")
	}
	for _, r := range [][2]int64{{2500, 4000}, {1000, 1000}, {1500, 1200}} {
		d := filepath.Join(dir, "none.wav")
		if _, err := Cut(in, d, r[0], r[1]); err == nil {
			t.Errorf("range %v: no error", r)
		}
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("range %v: dst written", r)
		}
		if _, err := os.Stat(d + ".tmp"); !os.IsNotExist(err) {
			t.Errorf("range %v: dst.tmp written", r)
		}
	}
}

// TestCutRejectsNonPCM: float or compressed WAV, other files and a header
// without data are ErrNotPCM; WAVE_FORMAT_EXTENSIBLE with a PCM sub-format
// is accepted.
func TestCutRejectsNonPCM(t *testing.T) {
	dir := t.TempDir()
	float, _ := pcmFile(3, 16000, 1, 32, 1000, false)
	mp3, _ := pcmFile(0x55, 16000, 1, 16, 1000, false)
	noData, _ := pcmFile(formatPCM, 16000, 1, 16, 1000, false)
	noData = noData[:36] // RIFF + fmt only
	for name, b := range map[string][]byte{
		"float": float, "mp3": mp3, "nodata": noData, "text": []byte("ID3 not a wav file at all"), "empty": nil,
	} {
		in := writeTemp(t, name+".wav", b)
		d := filepath.Join(dir, name+"-clip.wav")
		if _, err := Cut(in, d, 0, 100); !errors.Is(err, ErrNotPCM) {
			t.Errorf("%s: err %v, want ErrNotPCM", name, err)
		}
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("%s: clip written", name)
		}
	}
	if _, err := Cut(filepath.Join(dir, "missing.wav"), filepath.Join(dir, "x.wav"), 0, 100); !os.IsNotExist(err) {
		t.Errorf("missing source: %v", err)
	}

	// WAVE_FORMAT_EXTENSIBLE (cbSize 22) with KSDATAFORMAT_SUBTYPE_PCM
	ext, data := pcmFile(formatPCM, 8000, 1, 16, 8000, false)
	fmtBody := make([]byte, 40)
	copy(fmtBody, ext[20:36])
	binary.LittleEndian.PutUint16(fmtBody[0:], formatExtensible)
	binary.LittleEndian.PutUint16(fmtBody[16:], 22)
	binary.LittleEndian.PutUint16(fmtBody[18:], 16)
	binary.LittleEndian.PutUint16(fmtBody[24:], formatPCM)
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(4+8+40+8+len(data)))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, uint32(40))
	b.Write(fmtBody)
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	in := writeTemp(t, "ext.wav", b.Bytes())
	d := filepath.Join(dir, "ext-clip.wav")
	if ms, err := Cut(in, d, 250, 750); err != nil || ms != 500 {
		t.Fatalf("extensible PCM: %d ms, %v", ms, err)
	}
	got, _ := os.ReadFile(d)
	if !bytes.Equal(got[20:60], fmtBody) || !bytes.Equal(got[68:], data[4000:12000]) {
		t.Error("extensible clip: fmt chunk not copied verbatim or wrong samples")
	}
}
