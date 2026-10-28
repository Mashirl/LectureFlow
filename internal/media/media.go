// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package media holds the rules for the lecture media LectureFlow accepts
// and hands to ffmpeg (security review 2, M3): the file extensions of an
// upload, text playlists recognised by their first bytes, the ffmpeg
// demuxers a lecture's media may be read with (-format_whitelist), and
// stored paths that would make the app reach another computer.
//
// Why: ffmpeg and ffprobe follow playlists. An HLS playlist or an ffconcat
// script saved as a lecture's media makes them read files outside the
// lecture directory, and on Windows a UNC entry (\\server\share\x.ts) makes
// the system connect to that server with the user's NTLM credentials.
// HTTP is already refused by ffmpeg's default protocol whitelist.
package media

import (
	"path/filepath"
	"strings"
)

// Exts are the file extensions (lower case, without the dot) a lecture's
// media may have: the same list as MEDIA_EXT_LIST in web/util.js (and the
// copy in web/api.js), checked by web/media_exts_test.go.
var Exts = []string{
	"mp4", "m4v", "mkv", "mov", "webm", "avi", "wmv", "flv", "ts", "mts", "m2ts", "mpg", "mpeg", "3gp", "ogv",
	"mp3", "m4a", "aac", "wav", "flac", "ogg", "oga", "opus", "mka", "wma", "amr", "aiff", "aif",
}

var extSet = func() map[string]bool {
	m := make(map[string]bool, len(Exts))
	for _, e := range Exts {
		m["."+e] = true
	}
	return m
}()

// AllowedName reports whether a file name has one of Exts (any case).
func AllowedName(name string) bool {
	return extSet[strings.ToLower(filepath.Ext(name))]
}

// ExtList is Exts for a message: "mp4, m4v, …".
func ExtList() string { return strings.Join(Exts, ", ") }

// Demuxers are the ffmpeg input formats a lecture's media may be read with:
// the containers of Exts (the mov group, matroska/webm, avi, asf for
// wmv/wma, flv, MPEG-TS and -PS, ogg, the raw audio formats) and the
// elementary streams a mislabelled file of those types probes as (raw
// H.264/HEVC/MPEG-4/MPEG video, AC-3, LOAS, Wave64). None of them opens
// another file. Left out on purpose: hls, concat, dash, imf, sdp, rtsp,
// image2, webm_dash_manifest and every other demuxer that reads further
// files or the network.
var Demuxers = []string{
	"mov", "mp4", "m4a", "3gp", "3g2", "mj2",
	"matroska", "webm",
	"avi",
	"asf", "asf_o",
	"flv", "live_flv",
	"mpegts", "mpegtsraw", "mpeg", "mpegvideo",
	"mp3", "aac", "loas", "ac3", "eac3",
	"wav", "w64", "flac", "ogg", "aiff",
	"amr", "amrnb", "amrwb",
	"h264", "hevc", "m4v",
}

// FormatWhitelist is the value of ffmpeg's -format_whitelist for lecture
// media (an AVFormatContext option: ffmpeg refuses an input whose probed
// demuxer is not in it before the demuxer reads anything, and a nested
// demuxer inherits it).
var FormatWhitelist = strings.Join(Demuxers, ",")

var demuxerSet = func() map[string]bool {
	m := make(map[string]bool, len(Demuxers))
	for _, d := range Demuxers {
		m[d] = true
	}
	return m
}()

// FormatAllowed reports whether ffprobe's format_name (a demuxer name, or
// a comma-separated list of names for one demuxer: "mov,mp4,m4a,3gp,3g2,mj2")
// names a demuxer of Demuxers.
func FormatAllowed(formatName string) bool {
	for _, n := range strings.Split(formatName, ",") {
		if demuxerSet[strings.TrimSpace(n)] {
			return true
		}
	}
	return false
}

// InputArgs are the ffmpeg / ffprobe options for reading a lecture's media:
// put them right before "-i <media>" (ffmpeg) or before the media argument
// (ffprobe). -format_whitelist keeps playlists and other demuxers that open
// further files out; -protocol_whitelist file keeps a stored path from
// being read as a URL or a protocol ("concat:", "subfile:", "data:"). Both
// verified on the bundled ffmpeg 9.0.2 (NOTES.md, web-builder M3). An
// absolute path, a drive-letter path and a UNC path are all the file
// protocol. Not for an input LectureFlow wrote as a playlist itself (the
// downloads' local HLS playlist, internal/fetch).
func InputArgs() []string {
	return []string{"-format_whitelist", FormatWhitelist, "-protocol_whitelist", "file"}
}
