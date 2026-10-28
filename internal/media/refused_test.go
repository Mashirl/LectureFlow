// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package media

import (
	"strings"
	"testing"
)

// Refused reads the whitelist refusals of ffprobe ("[hls @ …]"), of the
// ffmpeg command line ("[in#0 @ …]", no demuxer name) and of a protocol.
func TestRefused(t *testing.T) {
	wl := "'" + FormatWhitelist + "'"
	for _, c := range []struct {
		stderr, what string
		refused      bool
	}{
		{"[hls @ 00000266368090c0] Format not on whitelist " + wl + "\nx.m3u8: Invalid argument", "hls", true},
		{"[concat @ 000002374c0a90c0] Format not on whitelist " + wl, "concat", true},
		{"[in#0 @ 0000027b863a8540] Format not on whitelist " + wl + "\n[in#0 @ 0000027b863a81c0] Error opening input: Invalid argument", "", true},
		{"[http @ 0000] Protocol 'http' not on whitelist 'file'!", "http", true},
		{"[mov,mp4,m4a,3gp,3g2,mj2 @ 0000] moov atom not found\nx.mp4: Invalid data found when processing input", "", false},
		{"", "", false},
	} {
		what, refused := Refused(c.stderr)
		if what != c.what || refused != c.refused {
			t.Errorf("Refused(%q) = %q, %v; want %q, %v", c.stderr, what, refused, c.what, c.refused)
		}
	}
}

// HLSFormatWhitelist is the hls demuxer plus Demuxers, nothing else.
func TestHLSFormatWhitelist(t *testing.T) {
	names := strings.Split(HLSFormatWhitelist, ",")
	if names[0] != "hls" || strings.Join(names[1:], ",") != FormatWhitelist {
		t.Errorf("HLSFormatWhitelist = %q", HLSFormatWhitelist)
	}
	if FormatAllowed("hls") {
		t.Error("hls is a demuxer of lecture media")
	}
}
