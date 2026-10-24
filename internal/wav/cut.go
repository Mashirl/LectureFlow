// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package wav

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrNotPCM: the source of Cut is not a PCM WAV file.
var ErrNotPCM = errors.New("not a PCM WAV file")

// Format tags of the fmt chunk that hold integer PCM samples.
const (
	formatPCM        = 1
	formatExtensible = 0xFFFE // WAVE_FORMAT_EXTENSIBLE; its sub-format must be PCM
)

// Cut writes the samples of the PCM WAV file src between startMS and endMS
// into a new WAV file dst (phase 3b task D: the self-test clip,
// SPEC-v2 I9). The range is clamped to the source's data; both ends are
// rounded down to a whole sample frame (the fmt chunk's block_align). The
// fmt chunk is copied verbatim. dst is written to dst.tmp first and renamed
// (SPEC §4.7); nothing is left behind on failure. durMS is the length of
// the written clip. An empty range, or a source that is not PCM WAV
// (ErrNotPCM), is an error.
func Cut(src, dst string, startMS, endMS int64) (durMS int64, err error) {
	f, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	h, err := readPCMHeader(f, fi.Size())
	if err != nil {
		return 0, fmt.Errorf("%s: %w", src, err)
	}
	align := int64(h.blockAlign)
	byteAt := func(ms int64) int64 {
		b := max(ms, 0) * int64(h.byteRate) / 1000
		b -= b % align
		return min(b, h.dataLen-h.dataLen%align)
	}
	from, to := byteAt(startMS), byteAt(endMS)
	if to <= from {
		return 0, fmt.Errorf("%s: empty clip %d–%d ms", src, startMS, endMS)
	}
	n := to - from

	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	fail := func(err error) (int64, error) {
		out.Close()
		os.Remove(tmp)
		return 0, err
	}
	fmtPad := int64(len(h.fmt) % 2)
	riff := 4 + (8 + int64(len(h.fmt)) + fmtPad) + (8 + n + n%2)
	if riff > 0xFFFFFFFF {
		return fail(fmt.Errorf("%s: clip too large for WAV", src))
	}
	hdr := make([]byte, 0, 44+len(h.fmt))
	hdr = append(hdr, "RIFF"...)
	hdr = binary.LittleEndian.AppendUint32(hdr, uint32(riff))
	hdr = append(hdr, "WAVEfmt "...)
	hdr = binary.LittleEndian.AppendUint32(hdr, uint32(len(h.fmt)))
	hdr = append(hdr, h.fmt...)
	if fmtPad == 1 {
		hdr = append(hdr, 0)
	}
	hdr = append(hdr, "data"...)
	hdr = binary.LittleEndian.AppendUint32(hdr, uint32(n))
	if _, err := out.Write(hdr); err != nil {
		return fail(err)
	}
	if _, err := f.Seek(h.dataOff+from, io.SeekStart); err != nil {
		return fail(err)
	}
	if _, err := io.CopyN(out, f, n); err != nil {
		return fail(err)
	}
	if n%2 == 1 {
		if _, err := out.Write([]byte{0}); err != nil {
			return fail(err)
		}
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return n * 1000 / int64(h.byteRate), nil
}

type pcmHeader struct {
	fmt        []byte // the fmt chunk's body
	byteRate   uint32
	blockAlign uint16
	dataOff    int64 // file offset of the first sample
	dataLen    int64 // bytes of samples (measured from the file size, like DurationMS)
}

// readPCMHeader walks the chunks of a RIFF/WAVE file up to "data".
func readPCMHeader(f io.ReadSeeker, size int64) (pcmHeader, error) {
	var h pcmHeader
	head := make([]byte, 12)
	if _, err := io.ReadFull(f, head); err != nil || string(head[0:4]) != "RIFF" || string(head[8:12]) != "WAVE" {
		return h, ErrNotPCM
	}
	off := int64(12)
	ch := make([]byte, 8)
	for {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return h, err
		}
		if _, err := io.ReadFull(f, ch); err != nil {
			return h, fmt.Errorf("%w: no data chunk", ErrNotPCM)
		}
		id, n := string(ch[0:4]), int64(binary.LittleEndian.Uint32(ch[4:8]))
		body := off + 8
		switch id {
		case "fmt ":
			if n < 16 || n > 1<<16 {
				return h, fmt.Errorf("%w: fmt chunk of %d bytes", ErrNotPCM, n)
			}
			h.fmt = make([]byte, n)
			if _, err := io.ReadFull(f, h.fmt); err != nil {
				return h, fmt.Errorf("%w: short fmt chunk", ErrNotPCM)
			}
			tag := binary.LittleEndian.Uint16(h.fmt[0:2])
			if tag == formatExtensible && n >= 26 {
				tag = binary.LittleEndian.Uint16(h.fmt[24:26]) // first bytes of the sub-format GUID
			}
			if tag != formatPCM {
				return h, fmt.Errorf("%w: format tag 0x%04X", ErrNotPCM, tag)
			}
			h.byteRate = binary.LittleEndian.Uint32(h.fmt[8:12])
			h.blockAlign = binary.LittleEndian.Uint16(h.fmt[12:14])
			if h.byteRate == 0 || h.blockAlign == 0 {
				return h, fmt.Errorf("%w: byte rate %d, block align %d", ErrNotPCM, h.byteRate, h.blockAlign)
			}
		case "data":
			if h.fmt == nil {
				return h, fmt.Errorf("%w: data before fmt", ErrNotPCM)
			}
			h.dataOff = body
			h.dataLen = max(size-body, 0)
			if n > 0 && n != 0xFFFFFFFF && n < h.dataLen {
				h.dataLen = n
			}
			return h, nil
		}
		off = body + n + n%2
		if off > size {
			return h, fmt.Errorf("%w: no data chunk", ErrNotPCM)
		}
	}
}
