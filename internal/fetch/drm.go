// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

// Encrypted MP4 files (security review, the DRM follow-up; NOTES
// "sec-fetch"). DRM is recognised in HLS and DASH manifests
// (playlistFailure); a file track, or the one file or init section of an
// HLS or DASH track, can be a Common Encryption MP4 all the same, and
// ffmpeg's -c copy would carry its samples over still encrypted into a
// file that looks fine and plays as noise. So before a task's tracks are
// joined or combined (merge), every MP4 among them is read, its moov only:
// a protected sample entry (encv, enca, ... with their sinf) or a pssh box
// fails the task with CodeDRMProtected, as a protected manifest does.

// protectedEntries are the sample entry types of protected tracks: ISO
// Common Encryption (ISO/IEC 23001-7: encv, enca, enct, encs) and Apple's
// FairPlay of the iTunes Store (drms, drmi).
var protectedEntries = map[string]bool{"encv": true, "enca": true, "enct": true, "encs": true, "drms": true, "drmi": true}

// mp4Start are the box types an MP4 (ISO BMFF) file starts with; a file
// starting otherwise (MPEG-TS, WebM) is not read.
var mp4Start = map[string]bool{"ftyp": true, "styp": true, "moov": true, "moof": true, "mdat": true, "free": true,
	"skip": true, "wide": true, "pdin": true, "sidx": true, "uuid": true, "prft": true, "emsg": true}

// The containers between moov and the sample descriptions.
var sampleTablePath = map[string]bool{"trak": true, "mdia": true, "minf": true, "stbl": true}

// maxBoxes bounds the boxes read at one level (a damaged or hostile file).
const maxBoxes = 100000

// mp4Protection returns what protects the MP4 file at path ("encv", "pssh",
// ...), "" when nothing does or the file is not an MP4. Only box headers
// and the sample descriptions are read, wherever the moov is.
func mp4Protection(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	return readerProtection(f, fi.Size()), nil
}

func readerProtection(r io.ReaderAt, size int64) string {
	off := int64(0)
	for n := 0; off < size && n < maxBoxes; n++ {
		typ, body, end, ok := readBox(r, off, size)
		if !ok || (n == 0 && !mp4Start[typ]) {
			return ""
		}
		if typ == "moov" {
			return moovProtection(r, body, end, 0)
		}
		off = end
	}
	return ""
}

// readBox reads the header of the box at off (which must end by limit): its
// type, where its body starts and where it ends.
func readBox(r io.ReaderAt, off, limit int64) (typ string, body, end int64, ok bool) {
	var h [16]byte
	if limit-off < 8 {
		return "", 0, 0, false
	}
	if _, err := r.ReadAt(h[:8], off); err != nil {
		return "", 0, 0, false
	}
	for _, c := range h[4:8] {
		if c < 0x20 || c > 0x7e {
			return "", 0, 0, false
		}
	}
	typ = string(h[4:8])
	size := int64(binary.BigEndian.Uint32(h[:4]))
	body = off + 8
	switch size {
	case 0: // to the end of the enclosing space
		end = limit
	case 1: // 64-bit size
		if limit-off < 16 {
			return "", 0, 0, false
		}
		if _, err := r.ReadAt(h[8:16], off+8); err != nil {
			return "", 0, 0, false
		}
		large := binary.BigEndian.Uint64(h[8:16])
		if large > uint64(limit-off) {
			return "", 0, 0, false
		}
		size, body = int64(large), off+16
		end = off + size
	default:
		end = off + size
	}
	if end > limit || end < body {
		return "", 0, 0, false
	}
	return typ, body, end, true
}

// moovProtection walks moov (or a container below it) for a pssh box or a
// protected sample entry.
func moovProtection(r io.ReaderAt, start, end int64, depth int) string {
	if depth > len(sampleTablePath)+1 {
		return ""
	}
	off := start
	for n := 0; off < end && n < maxBoxes; n++ {
		typ, body, next, ok := readBox(r, off, end)
		if !ok {
			return ""
		}
		switch {
		case typ == "pssh":
			return "pssh"
		case sampleTablePath[typ]:
			if p := moovProtection(r, body, next, depth+1); p != "" {
				return p
			}
		case typ == "stsd":
			if p := stsdProtection(r, body, next); p != "" {
				return p
			}
		}
		off = next
	}
	return ""
}

// stsdProtection reads the sample entries of an stsd box (after its
// version, flags and entry count).
func stsdProtection(r io.ReaderAt, start, end int64) string {
	off := start + 8
	for n := 0; off < end && n < maxBoxes; n++ {
		typ, _, next, ok := readBox(r, off, end)
		if !ok {
			return ""
		}
		if protectedEntries[typ] {
			return typ
		}
		off = next
	}
	return ""
}

// checkProtected fails the task when one of its downloaded tracks is a
// protected MP4: a finished track file (finals[i]), or the parts of a
// track still to be joined (its one file, a DASH init section or the
// joined DASH file, an HLS init section).
func checkProtected(dir string, finals []string, n int) error {
	for i := 0; i < n; i++ {
		files := []string{finals[i]}
		if finals[i] == "" {
			files = trackPartFiles(dir, i)
		}
		for _, f := range files {
			if f == "" {
				continue
			}
			if p, err := mp4Protection(f); err == nil && p != "" {
				return &failure{code: CodeDRMProtected, detail: "protected file (" + p + "): " + filepath.Base(f)}
			}
		}
	}
	return nil
}

// trackPartFiles are the files of track i's ready.json that tell whether
// it is protected.
func trackPartFiles(dir string, i int) []string {
	b, err := os.ReadFile(filepath.Join(dir, trackName(i), "ready.json"))
	if err != nil {
		return nil
	}
	var plan trackPlan
	if json.Unmarshal(b, &plan) != nil {
		return nil
	}
	parts := []trackPart{plan.Main}
	if plan.Audio != nil {
		parts = append(parts, *plan.Audio)
	}
	var out []string
	for _, p := range parts {
		pdir := filepath.Join(dir, filepath.FromSlash(p.Dir))
		switch {
		case p.Source != "":
			out = append(out, filepath.Join(pdir, p.Source))
		case len(p.Parts) > 0:
			ext := p.Ext
			if ext == "" {
				ext = ".mp4"
			}
			if joined := filepath.Join(pdir, "joined"+ext); fileExists(joined) {
				out = append(out, joined)
			} else {
				out = append(out, filepath.Join(pdir, p.Parts[0]))
			}
		case p.Playlist != "":
			inits, _ := filepath.Glob(filepath.Join(pdir, "m[0-9][0-9][0-9]*"))
			out = append(out, inits...)
		}
	}
	return out
}
