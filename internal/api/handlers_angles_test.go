// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/fetch"
	"lectureflow/internal/media"
	"lectureflow/internal/model"
)

// fakeRemux stands in for ffmpeg's angle remux: it copies the input to the
// output (the last argument); an input starting with "NOVIDEO" has no
// video stream, one starting with "BROKEN" cannot be read.
func fakeRemux(calls *[][]string) func(ctx context.Context, name string, args ...string) (string, string, error) {
	return func(ctx context.Context, name string, args ...string) (string, string, error) {
		if !slices.Contains(args, "-an") {
			return fakeCmd(ctx, name, args...)
		}
		*calls = append(*calls, append([]string{name}, args...))
		in := args[slices.Index(args, "-i")+1]
		b, err := os.ReadFile(in)
		if err != nil {
			return "", err.Error(), err
		}
		switch {
		case bytes.HasPrefix(b, []byte("NOVIDEO")):
			return "", "Stream map '0:v:0' matches no streams.\nTo ignore this, add a trailing '?' to the map.\n", errors.New("exit status 1")
		case bytes.HasPrefix(b, []byte("BROKEN")):
			return "", in + ": Invalid data found when processing input\n", errors.New("exit status 1")
		}
		return "", "", os.WriteFile(args[len(args)-1], b, 0o644)
	}
}

func (e *env) getLecture(id string) Lecture {
	e.t.Helper()
	res, b := e.doWith("GET", "/api/lectures/"+id, nil, testToken, "")
	if res.StatusCode != http.StatusOK {
		e.t.Fatalf("GET lecture: %d %s", res.StatusCode, b)
	}
	var l Lecture
	decodeInto(e.t, b, &l)
	return l
}

func (e *env) getRange(path, rng string) (*http.Response, []byte) {
	e.t.Helper()
	req, err := http.NewRequest("GET", e.ts.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set(TokenHeader, testToken)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

func wantAngles(t *testing.T, l Lecture, names ...string) {
	t.Helper()
	var got []string
	for _, a := range l.Angles {
		got = append(got, a.Name)
	}
	if !slices.Equal(got, names) {
		t.Fatalf("angles %v, want %v", got, names)
	}
}

// A download with a second angle: the angle moves into the lecture's
// directory next to the media; the lecture lists it; GET serves it like
// the media (Range, video/mp4, the desktop token); deleting the lecture
// removes it.
func TestImportDownloadWithAngle(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("CSC1003")
	src := writeDownload(t, e, "media bytes")
	angleSrc := filepath.Join(filepath.Dir(src), fetch.AngleFile)
	if err := os.WriteFile(angleSrc, []byte("angle two bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := e.srv.ImportDownload(ctx, fetch.ImportRequest{TaskID: 7, CourseID: c.ID, Path: src,
		Filename: "Lecture 3.mp4", Title: "Lecture 3", Angles: []fetch.ImportAngle{{Name: fetch.AngleName, Path: angleSrc}}})
	if err != nil {
		t.Fatal(err)
	}
	lrow, err := e.st.GetLecture(ctx, res.LectureID)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(lrow.MediaPath)
	if b, err := os.ReadFile(filepath.Join(dir, "angle2.mp4")); err != nil || string(b) != "angle two bytes" {
		t.Fatalf("angle in the lecture directory: %q %v", b, err)
	}
	if _, err := os.Stat(angleSrc); !os.IsNotExist(err) {
		t.Fatalf("the angle was copied, not moved: %v", err)
	}
	e.desktopOn()
	l := e.getLecture(res.LectureID)
	wantAngles(t, l, "angle2")
	// the course's list carries it too
	r, b := e.doWith("GET", "/api/courses/"+c.ID+"/lectures", nil, testToken, "")
	var list []Lecture
	decodeInto(t, b, &list)
	if r.StatusCode != http.StatusOK || len(list) != 1 || len(list[0].Angles) != 1 {
		t.Fatalf("list %d %s", r.StatusCode, b)
	}
	path := "/api/lectures/" + res.LectureID + "/angles/angle2"
	r, b = e.getRange(path, "")
	if r.StatusCode != http.StatusOK || string(b) != "angle two bytes" || r.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("GET angle: %d %q %s", r.StatusCode, b, r.Header.Get("Content-Type"))
	}
	r, b = e.getRange(path, "bytes=6-8")
	if r.StatusCode != http.StatusPartialContent || string(b) != "two" {
		t.Fatalf("Range: %d %q", r.StatusCode, b)
	}
	if r, _ := e.doWith("GET", path, nil, "", ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without the token: %d", r.StatusCode)
	}
	for _, p := range []string{"/angles/angle3", "/angles/media", "/angles/angle2.mp4"} {
		r, b := e.getRange("/api/lectures/"+res.LectureID+p, "")
		if r.StatusCode != http.StatusNotFound || errCode(t, b) != codeAngleNotFound {
			t.Fatalf("%s: %d %s", p, r.StatusCode, b)
		}
	}
	r, b = e.doWith("DELETE", "/api/lectures/"+res.LectureID+"?force=1", nil, testToken, "")
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("delete lecture: %d %s", r.StatusCode, b)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the lecture directory (with the angle) is still there: %v", err)
	}
}

// A refused angle gives every file back to the task's directory (a retry
// needs no download).
func TestImportDownloadAngleGivesBack(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("CSC1003")
	src := writeDownload(t, e, "media bytes")
	angleSrc := filepath.Join(filepath.Dir(src), fetch.AngleFile)
	if err := os.WriteFile(angleSrc, []byte("angle"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := e.srv.ImportDownload(ctx, fetch.ImportRequest{CourseID: c.ID, Path: src, Filename: "x.mp4", Title: "x",
		Angles: []fetch.ImportAngle{{Name: fetch.AngleName, Path: angleSrc}, {Name: "../evil", Path: angleSrc}}})
	if err == nil {
		t.Fatal("an invalid angle name was accepted")
	}
	for _, p := range []string{src, angleSrc} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s not given back: %v", p, err)
		}
	}
	if ls, _ := e.st.ListLectures(ctx, c.ID); len(ls) != 0 {
		t.Fatalf("a lecture was created: %+v", ls)
	}
}

// Adding a second angle to an existing lecture (desktop: by path, or a
// multipart file), replacing it, refusals, and removing it.
func TestAngleEndpoints(t *testing.T) {
	var calls [][]string
	e := newEnv(t, func(s *Server) { s.runCmd = fakeRemux(&calls) })
	c := e.course("C")
	l := e.draft(c.ID, "Week 1")
	e.desktopOn()
	srcDir := t.TempDir()
	base := "/api/lectures/" + l.ID + "/angles"
	angle := filepath.Join(e.srv.lectureDir(e.lectureRow(l.ID)), "angle2.mp4")

	if got := e.getLecture(l.ID); got.Angles != nil {
		t.Fatalf("a lecture without an angle lists %v", got.Angles)
	}
	// by path
	src := writeSrc(t, srcDir, "Camera 2.MOV", []byte("camera two"))
	res, b := e.pathUpload(base, map[string]any{"path": src})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST angle: %d %s", res.StatusCode, b)
	}
	var got Lecture
	decodeInto(t, b, &got)
	wantAngles(t, got, "angle2")
	if b, _ := os.ReadFile(angle); string(b) != "camera two" {
		t.Fatalf("angle content %q", b)
	}
	if len(calls) != 1 {
		t.Fatalf("ffmpeg calls %v", calls)
	}
	joined := strings.Join(calls[0], " ")
	// checkSourcePath follows links: on macOS the temporary directory
	// (/var/folders/…, /tmp) is itself reached through /private.
	srcResolved := src
	if r, err := filepath.EvalSymlinks(src); err == nil {
		srcResolved = r
	}
	for _, want := range []string{"-i " + srcResolved, "-map 0:v:0 -c copy -an -f mp4", "angle2.mp4.tmp"} {
		if !strings.Contains(joined, want) {
			t.Errorf("remux lacks %q: %s", want, joined)
		}
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatal("the user's file must stay")
	}
	// replaced by a multipart upload; the upload's copy is removed
	res, b = e.tokenUpload("POST", base, nil, "cam2.mp4", []byte("camera two, again"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST angle (multipart): %d %s", res.StatusCode, b)
	}
	if b, _ := os.ReadFile(angle); string(b) != "camera two, again" {
		t.Fatalf("replaced angle %q", b)
	}
	entries, _ := os.ReadDir(filepath.Dir(angle))
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), angleUploadBase) {
			t.Fatalf("upload copy left: %s", en.Name())
		}
	}
	noTmpFiles(t, e.cfg.DataDir)

	// refusals keep the angle there is
	novideo := writeSrc(t, srcDir, "audio.m4a", []byte("NOVIDEO"))
	broken := writeSrc(t, srcDir, "broken.mp4", []byte("BROKEN"))
	// security review 2 M3: a media extension, and no playlist
	notes := writeSrc(t, srcDir, "notes.txt", []byte("BROKEN"))
	m3u8 := writeSrc(t, srcDir, "cam.m3u8", []byte("#EXTM3U\n#EXTINF:1,\n\\\\server\\share\\x.ts\n"))
	disguised := writeSrc(t, srcDir, "cam.mp4", []byte("ffconcat version 1.0\nfile 'x.mp4'\n"))
	inside := filepath.Join(filepath.Dir(angle), "angle2.mp4")
	for _, c := range []struct {
		path   string
		status int
		code   string
	}{
		{novideo, http.StatusBadRequest, codeAngleNoVideo},
		{broken, http.StatusBadRequest, codeAngleRemuxFailed},
		{notes, http.StatusBadRequest, codeUploadNotMedia},
		{m3u8, http.StatusBadRequest, codeUploadNotMedia},
		{disguised, http.StatusBadRequest, codeUploadPlaylist},
		{inside, http.StatusBadRequest, "upload.path_in_data_dir"},
		{"relative.mp4", http.StatusBadRequest, "upload.path_invalid"},
	} {
		res, b := e.pathUpload(base, map[string]any{"path": c.path})
		if res.StatusCode != c.status || errCode(t, b) != c.code {
			t.Errorf("%s: %d %s", c.path, res.StatusCode, b)
		}
	}
	// the same over multipart; the upload's copy is removed
	for _, c := range []struct{ name, body, code string }{
		{"cam.m3u8", "#EXTM3U\n", codeUploadNotMedia},
		{"cam.mp4", "\xef\xbb\xbf#EXTM3U\n#EXT-X-TARGETDURATION:1\n", codeUploadPlaylist},
	} {
		res, b := e.tokenUpload("POST", base, nil, c.name, []byte(c.body))
		if res.StatusCode != http.StatusBadRequest || errCode(t, b) != c.code {
			t.Errorf("multipart %s: %d %s", c.name, res.StatusCode, b)
		}
	}
	entries, _ = os.ReadDir(filepath.Dir(angle))
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), angleUploadBase) {
			t.Fatalf("refused upload copy left: %s", en.Name())
		}
	}
	// the remux reads its input with the media whitelists, before -i
	for _, call := range calls {
		joined := strings.Join(call, " ")
		if !strings.Contains(joined, strings.Join(media.InputArgs(), " ")+" -i ") {
			t.Errorf("remux without the input whitelists: %s", joined)
		}
	}
	if b, _ := os.ReadFile(angle); string(b) != "camera two, again" {
		t.Fatalf("a refused angle changed the one there: %q", b)
	}
	noTmpFiles(t, e.cfg.DataDir)
	res, b = e.pathUpload("/api/lectures/01NOSUCHLECTURE0000000000/angles", map[string]any{"path": src})
	if res.StatusCode != http.StatusNotFound || errCode(t, b) != "lecture.not_found" {
		t.Fatalf("unknown lecture: %d %s", res.StatusCode, b)
	}

	// removed
	res, b = e.doWith("DELETE", base+"/angle2", nil, testToken, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("DELETE angle: %d %s", res.StatusCode, b)
	}
	var after Lecture
	decodeInto(t, b, &after)
	wantAngles(t, after)
	if strings.Contains(string(b), `"angles"`) {
		t.Fatalf("an empty angles list is not left out: %s", b)
	}
	if _, err := os.Stat(angle); !os.IsNotExist(err) {
		t.Fatalf("angle file: %v", err)
	}
	res, b = e.doWith("DELETE", base+"/angle2", nil, testToken, "")
	if res.StatusCode != http.StatusNotFound || errCode(t, b) != codeAngleNotFound {
		t.Fatalf("DELETE again: %d %s", res.StatusCode, b)
	}
}

// In web mode a path is not accepted (as for every upload); a multipart
// file is.
func TestAngleWebMode(t *testing.T) {
	var calls [][]string
	e := newEnv(t, func(s *Server) { s.runCmd = fakeRemux(&calls) })
	c := e.course("C")
	l := e.draft(c.ID, "Week 1")
	b, _ := json.Marshal(map[string]any{"path": writeSrc(t, t.TempDir(), "cam.mp4", []byte("x"))})
	res, body := e.doWith("POST", "/api/lectures/"+l.ID+"/angles", b, "", "")
	if res.StatusCode != http.StatusBadRequest || errCode(t, body) != "upload.not_multipart" {
		t.Fatalf("path in web mode: %d %s", res.StatusCode, body)
	}
	res, body = e.upload("/api/lectures/"+l.ID+"/angles", nil, "cam.mp4", []byte("web camera"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("multipart in web mode: %d %s", res.StatusCode, body)
	}
	var got Lecture
	decodeInto(t, body, &got)
	wantAngles(t, got, "angle2")
}

func (e *env) lectureRow(id string) model.Lecture {
	e.t.Helper()
	l, err := e.st.GetLecture(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}
