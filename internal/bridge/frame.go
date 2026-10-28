// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package bridge

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Native messaging frames a message as a 32-bit length in native byte order
// followed by that many bytes of UTF-8 JSON. The host uses the same framing
// on its local IPC connection to the app, so it can relay bytes without
// parsing them. Every supported platform (x64, arm64) is little-endian.

// Size limits. Chrome rejects host-to-browser messages over 1 MB; a request
// from the browser may be up to 64 MiB, but nothing in this protocol comes
// near that, so the app reads with the smaller MaxRequest.
const (
	MaxToBrowser = 1 << 20
	MaxRequest   = 4 << 20
)

// ErrFrameTooLarge is returned by ReadFrame for a length above max; the
// stream is then unusable and the caller should close it.
var ErrFrameTooLarge = errors.New("bridge: frame too large")

// ReadFrame reads one frame. It returns io.EOF only on a clean end before
// the length prefix.
func ReadFrame(r io.Reader, max int) ([]byte, error) {
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("bridge: truncated length: %w", err)
		}
		return nil, err
	}
	if int64(n) > int64(max) {
		return nil, ErrFrameTooLarge
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("bridge: truncated frame: %w", err)
	}
	return buf, nil
}

// WriteFrame writes one frame in a single Write call, so concurrent writers
// that serialise their calls never interleave a length with another body.
func WriteFrame(w io.Writer, msg []byte, max int) error {
	if len(msg) > max {
		return ErrFrameTooLarge
	}
	buf := make([]byte, 4+len(msg))
	binary.LittleEndian.PutUint32(buf, uint32(len(msg)))
	copy(buf[4:], msg)
	_, err := w.Write(buf)
	return err
}
