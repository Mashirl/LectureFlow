// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package wav reads the length of the PCM WAV files that prep writes
// (audio.wav, SPEC §6.1). Shared by the API (Lecture.duration_ms, audio
// clips) and the export stage (cite.json duration_ms), so both report the
// same media length.
package wav

import (
	"encoding/binary"
	"io"
	"os"
)

// DurationMS reads the length of a PCM WAV file from its header (the
// byte rate of the "fmt " chunk and the size of the "data" chunk, measured
// from the file size so a streamed header with a placeholder size still
// works). ok is false when the header cannot be read.
func DurationMS(path string) (int64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, false
	}
	head := make([]byte, 64<<10)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if len(head) < 12 || string(head[0:4]) != "RIFF" || string(head[8:12]) != "WAVE" {
		return 0, false
	}
	var byteRate int64
	for off := 12; off+8 <= len(head); {
		id := string(head[off : off+4])
		size := int64(binary.LittleEndian.Uint32(head[off+4 : off+8]))
		body := off + 8
		switch id {
		case "fmt ":
			if body+12 <= len(head) {
				byteRate = int64(binary.LittleEndian.Uint32(head[body+8 : body+12]))
			}
		case "data":
			if byteRate <= 0 {
				return 0, false
			}
			data := info.Size() - int64(body)
			if size > 0 && size != 0xFFFFFFFF && size < data {
				data = size
			}
			if data < 0 {
				data = 0
			}
			return data * 1000 / byteRate, true
		}
		next := int64(body) + size + size%2
		if next > int64(len(head)) || size < 0 {
			return 0, false
		}
		off = int(next)
	}
	return 0, false
}
