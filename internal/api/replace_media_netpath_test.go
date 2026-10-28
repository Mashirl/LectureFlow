// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// Replacing a draft's media whose stored media_path names another computer
// (state.StoredPathUnsafe; security review 2, M3): the old path is neither
// stat'ed nor removed, it is logged once (Warn), and the replacement
// completes as usual. 192.0.2.0/24 is TEST-NET-1 (RFC 5737); the paths are
// unique per run because StoredPathUnsafe logs each one once per process.
func TestReplaceMediaOldNetworkPath(t *testing.T) {
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	probe := &fakeProbe{}
	e := newEnv(t, func(s *Server) { s.runCmd = probe.run })
	withFakeFFprobe(t, e)
	c := e.course("C")
	ctx := context.Background()
	u := db.NewID()
	for i, old := range []string{
		`\\192.0.2.10\share\%s.mp4`,
		`//192.0.2.11/share/%s.mp4`,
		`\\?\UNC\192.0.2.12\share\%s.mp4`,
		`file://192.0.2.13/share/%s.mp4`,
	} {
		token := fmt.Sprintf("%s-%d", u, i) // the log quotes backslashes: look for this part
		old = fmt.Sprintf(old, token)
		l := e.draft(c.ID, fmt.Sprintf("L%d", i))
		if err := e.st.SetLectureMedia(ctx, l.ID, old); err != nil {
			t.Fatal(err)
		}
		res, b := e.tokenUpload("PUT", "/api/lectures/"+l.ID+"/media", nil, "better.mkv", []byte("better media"))
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: replace media: %d %s", old, res.StatusCode, b)
		}
		after, err := e.st.GetLecture(ctx, l.ID)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(after.MediaPath) != "media.mkv" || after.OriginalFilename != "better.mkv" {
			t.Fatalf("%s: replaced lecture %+v", old, after)
		}
		if b, err := os.ReadFile(after.MediaPath); err != nil || string(b) != "better media" {
			t.Fatalf("%s: new media %q %v", old, b, err)
		}
		var lines []string
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, token) {
				lines = append(lines, line)
			}
		}
		if len(lines) != 1 || !strings.Contains(lines[0], "level=WARN") {
			t.Errorf("%s: want one Warn, got %q", old, lines)
		}
	}
	if s := logs.String(); strings.Contains(s, "remove replaced media") {
		t.Errorf("a removal was tried:\n%s", s)
	}
}

// replacedMediaToRemove: with a Server whose data_dir is on a share but is
// not the process's data directory (state.SetDataDir is not called here)
// the old media lies inside data_dir as text, so only the StoredPathUnsafe
// check keeps it from being removed (with the data directory itself on the
// share it is removed: netpath_datadir_test.go); a URL-like path would be
// joined into the lecture directory and removed there. Local paths are
// decided as before. Nothing touches the disk (the share is TEST-NET-1).
func TestReplacedMediaToRemove(t *testing.T) {
	u := db.NewID()
	share := &Server{dataDir: "//192.0.2.20/share/" + u}
	l := model.Lecture{ID: db.NewID(), CourseID: "c1"}
	l.MediaPath = share.dataDir + "/courses/c1/" + l.ID + "/media.mp4"
	if !share.inDataDir(l.MediaPath) {
		t.Fatalf("setup: %s should lie inside data_dir %s", l.MediaPath, share.dataDir)
	}
	if got := share.replacedMediaToRemove(l, filepath.Join(model.LectureDir(share.dataDir, l.CourseID, l.ID), "media.mkv")); got != "" {
		t.Errorf("media on a share: remove %q", got)
	}

	local := &Server{dataDir: t.TempDir()}
	lecDir := model.LectureDir(local.dataDir, l.CourseID, l.ID)
	newPath := filepath.Join(lecDir, "media.mkv")
	inside := filepath.Join(lecDir, "media.mp4")
	for _, tc := range []struct{ media, want string }{
		{"", ""},
		{newPath, ""},
		{inside, inside},
		{"media.mp4", inside}, // relative to the lecture directory
		{filepath.Join(t.TempDir(), "orig.mp4"), ""},      // outside data_dir (A7)
		{`\\192.0.2.21\share\` + u + `.mp4`, ""},          // UNC
		{`\\.\UNC\192.0.2.22\share\` + u + `.mp4`, ""},    // device path
		{"file://192.0.2.23/share/" + u + ".mp4", ""},     // URL: not joined into lecDir
		{"smb:" + `\\192.0.2.24\share\` + u + ".mp4", ""}, // URL with a UNC tail
	} {
		l.MediaPath = tc.media
		if got := local.replacedMediaToRemove(l, newPath); got != tc.want {
			t.Errorf("media_path %q: remove %q, want %q", tc.media, got, tc.want)
		}
	}
}
