// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/media"
)

// Security review 2, M3: the media extensions the pickers offer and the
// front end accepts are the ones the server accepts (internal/media.Exts):
// MEDIA_EXT_LIST in util.js, its copy in api.js (the system dialog's
// filter), and the pickers and checks are built from them, by extension
// only (a playlist can have an audio/* MIME type).

var extListRe = regexp.MustCompile(`(?s)const MEDIA_EXT_LIST = \[(.*?)\];`)

func jsExtList(t *testing.T, file string) []string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	m := extListRe.FindSubmatch(b)
	if m == nil {
		t.Fatalf("%s: no MEDIA_EXT_LIST", file)
	}
	var out []string
	for _, q := range regexp.MustCompile(`'([a-z0-9]+)'`).FindAllSubmatch(m[1], -1) {
		out = append(out, string(q[1]))
	}
	return out
}

func TestMediaExtListsMatchServer(t *testing.T) {
	for _, f := range []string{"util.js", "api.js"} {
		if got := jsExtList(t, f); !slices.Equal(got, media.Exts) {
			t.Errorf("%s MEDIA_EXT_LIST = %v, want the server's %v", f, got, media.Exts)
		}
	}
	util, err := os.ReadFile("util.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(util)
	for _, want := range []string{
		"export const MEDIA_ACCEPT = MEDIA_EXT_LIST.map((e) => '.' + e).join(',');",
		"return MEDIA_EXTS.has(extOf((file && file.name) || ''));",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("util.js lacks %q", want)
		}
	}
	if strings.Contains(s, "type.startsWith('audio/')") {
		t.Error("util.js isMediaFile still accepts a file by its MIME type")
	}
	// the lecture pickers and the second view's dialog use the extension list
	for file, want := range map[string]string{
		"view_course.js":    "const MEDIA_ACCEPT = U.MEDIA_ACCEPT;",
		"wizard.js":         "const MEDIA_ACCEPT = U.MEDIA_ACCEPT;",
		"lecture_angles.js": "const ACCEPT = MEDIA_ACCEPT;",
	} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), want) || strings.Contains(string(b), "'audio/*") || strings.Contains(string(b), "'video/*'") {
			t.Errorf("%s: want %q and no audio/* or video/* accept", file, want)
		}
	}
	// the second view refuses other files before sending (the dialog also offers "all files")
	b, _ := os.ReadFile("lecture_angles.js")
	if !strings.Contains(string(b), "if (!isMediaFile({ name }))") || !strings.Contains(string(b), "t('lecture.angle.not_media'") {
		t.Error("lecture_angles.js does not refuse non-media files")
	}
	// every lecture upload goes through the extension filter
	b, _ = os.ReadFile("uploads.js")
	if !strings.Contains(string(b), "const media = files.filter(U.isMediaFile);") {
		t.Error("uploads.js addLectureFiles no longer filters by isMediaFile")
	}
}
