// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package media

import (
	"regexp"
	"strings"
)

// HLSFormatWhitelist is the -format_whitelist of a local HLS playlist that
// LectureFlow wrote itself (internal/fetch: a download's index.m3u8, whose
// entries are the segment, key and init files it fetched): the hls demuxer
// for the playlist, and Demuxers for its segments, which inherit the list
// (MPEG-TS, fragmented MP4, ADTS AAC, MP3, AC-3). Used with
// -protocol_whitelist file,crypto. Never for a lecture's stored media, nor
// for a downloaded file: those are InputArgs.
var HLSFormatWhitelist = "hls," + FormatWhitelist

// protocolNotOnWhitelistRe finds the protocol of libavformat's "Protocol
// 'http' not on whitelist 'file'!" (ffurl_connect).
var protocolNotOnWhitelistRe = regexp.MustCompile(`Protocol '([^']*)' not on whitelist`)

// Refused reports whether the output (stderr) of ffmpeg or ffprobe says an
// input was refused by the whitelists of InputArgs: its demuxer is not in
// -format_whitelist ("Format not on whitelist", what ProbeFormat looks for)
// or a protocol is not in -protocol_whitelist. what is the demuxer ("hls",
// "concat") or the protocol ("http") when the output names it: ffprobe
// logs the refusal under the demuxer ("[hls @ …]"), the ffmpeg command line
// under its input ("[in#0 @ …]"), so what is "" there.
func Refused(stderr string) (what string, refused bool) {
	if !strings.Contains(stderr, "not on whitelist") {
		return "", false
	}
	if m := notOnWhitelistRe.FindStringSubmatch(stderr); m != nil {
		return m[1], true
	}
	if m := protocolNotOnWhitelistRe.FindStringSubmatch(stderr); m != nil {
		return m[1], true
	}
	return "", true
}
