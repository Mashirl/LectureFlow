// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
)

// Track paths are credentials (lead's ruling 3, after M0: on Panopto's
// CloudFront the GUID path alone downloads the recording, no cookie and no
// query needed). So download_task.tracks_json keeps the host and a short
// hash of the path, and a log line or a failure detail names a track URL
// by its host and that hash only (bridge.RedactURL, which keeps the path,
// is not used for track URLs here).

// pathHash is the first 16 hex digits of the SHA-256 of a URL's escaped
// path: enough to tell whether a track sent again is the same one (to keep
// its partial files), useless for downloading it.
func pathHash(escapedPath string) string {
	if escapedPath == "" {
		escapedPath = "/"
	}
	sum := sha256.Sum256([]byte(escapedPath))
	return hex.EncodeToString(sum[:8])
}

// logURL is the only form of a track URL that reaches a log or a failure
// detail: "https://cdn.example.edu (path 0123456789abcdef)".
func logURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(invalid url)"
	}
	return u.Scheme + "://" + u.Host + " (path " + pathHash(u.EscapedPath()) + ")"
}

// normalize turns a record written before ruling 3 (with the path) into
// the current form (the path's hash).
func (r *trackRecord) normalize() bool {
	if r.Path == "" {
		return false
	}
	if r.PathHash == "" {
		r.PathHash = pathHash(r.Path)
	}
	r.Path = ""
	return true
}

// parseRecords reads tracks_json; old rows with paths read as if hashed.
func parseRecords(s string) []trackRecord {
	var out []trackRecord
	_ = json.Unmarshal([]byte(s), &out)
	for i := range out {
		out[i].normalize()
	}
	return out
}

func encodeRecords(recs []trackRecord) string {
	if recs == nil {
		recs = []trackRecord{}
	}
	b, _ := json.Marshal(recs)
	return string(b)
}

// scrubTracksJSON returns tracks_json without any path (changed false when
// there was none to remove, or the value is not readable).
func scrubTracksJSON(s string) (string, bool) {
	var recs []trackRecord
	if json.Unmarshal([]byte(s), &recs) != nil {
		return s, false
	}
	changed := false
	for i := range recs {
		if recs[i].normalize() {
			changed = true
		}
	}
	if !changed {
		return s, false
	}
	return encodeRecords(recs), true
}

// sameTracks reports whether two tracks_json values name the same media
// (kind, format, role, host, path, selector; labels and size estimates may
// change between two visits of the page): then the partial files of the
// first are kept for the second.
func sameTracks(a, b string) bool {
	ra, rb := parseRecords(a), parseRecords(b)
	if len(ra) != len(rb) || len(ra) == 0 {
		return false
	}
	for i := range ra {
		x, y := ra[i], rb[i]
		if x.Kind != y.Kind || x.Format != y.Format || x.Role != y.Role || x.Host != y.Host ||
			x.PathHash != y.PathHash || x.Selector != y.Selector {
			return false
		}
	}
	return true
}
