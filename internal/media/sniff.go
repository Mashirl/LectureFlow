// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package media

import (
	"bytes"
	"errors"
	"io"
	"os"
)

// HeadSize is how much of a file SniffFile reads.
const HeadSize = 4096

// playlistSigns are the first bytes (after a UTF-8 byte order mark and
// white space, ASCII case ignored) of text files that name other files or
// network sources: what ffmpeg's hls, concat and sdp demuxers probe for, and
// the other playlist and manifest formats (PLS, DASH/SMIL/ASX/XSPF XML)
// that are no recording either. An AMR file starts with "#!AMR": not here.
var playlistSigns = []struct {
	prefix string
	kind   string
}{
	{"#extm3u", "m3u8"},
	{"ffconcat", "ffconcat"},
	{"[playlist]", "pls"},
	{"<?xml", "xml"},
	{"<mpd", "mpd"},
	{"<smil", "smil"},
	{"<asx", "asx"},
	{"<playlist", "xspf"},
	{"v=0\n", "sdp"},
	{"v=0\r\n", "sdp"},
}

// SniffPlaylist returns the kind of text playlist head starts with ("m3u8",
// "ffconcat", "pls", "xml", "mpd", "smil", "asx", "xspf", "sdp"), or "".
func SniffPlaylist(head []byte) string {
	b := bytes.TrimPrefix(head, []byte("\xef\xbb\xbf"))
	b = bytes.TrimLeft(b, " \t\r\n\f\v")
	for _, s := range playlistSigns {
		if len(b) >= len(s.prefix) && bytes.EqualFold(b[:len(s.prefix)], []byte(s.prefix)) {
			return s.kind
		}
	}
	return ""
}

// SniffFile is SniffPlaylist of the first HeadSize bytes of the file at path.
func SniffFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := make([]byte, HeadSize)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", err
	}
	return SniffPlaylist(head[:n]), nil
}
