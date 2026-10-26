// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// GET /api/lectures/{id}/english/{format} (3.1.0 "只转写", SPEC-v3 V4.2
// step 4; pipeline-builder, NOTES.md): the English subtitles (format
// "vtt") or transcript ("txt") of a lecture, generated from its sentences
// when requested — available once asr is done, without the export stage.
// Downloaded as "{number} {English title}.en.vtt" / ".en.txt" (SPEC-v2
// P13.6; without a title the uploaded file's name). 404 export.not_found
// while the lecture has no sentences, 400 export.bad_name for another format.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/stages"
)

func init() {
	v3Routes = append(v3Routes, func(s *Server, h func(string, http.HandlerFunc)) {
		h("GET /api/lectures/{id}/english/{format}", s.englishFile)
	})
}

func (s *Server) englishFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var suffix, ct string
	switch r.PathValue("format") {
	case "vtt":
		suffix, ct = stages.EnglishVTTSuffix, "text/vtt; charset=utf-8"
	case "txt":
		suffix, ct = stages.EnglishTxtSuffix, "text/plain; charset=utf-8"
	default:
		writeError(w, http.StatusBadRequest, "export.bad_name", nil)
		return
	}
	l, err := s.getLectureRow(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ss, err := s.st.ListSentences(ctx, l.ID, 0, 0)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(ss) == 0 {
		writeError(w, http.StatusNotFound, "export.not_found", nil)
		return
	}
	var buf bytes.Buffer
	if suffix == stages.EnglishVTTSuffix {
		err = stages.WriteEnglishVTT(&buf, ss)
	} else {
		err = stages.WriteEnglishText(&buf, ss)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	name := stages.EnglishFileName(s.lectureFileLabel(ctx, l), l, suffix)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store") // made from the current sentences each time
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, asciiFileName(name), url.PathEscape(name)))
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(buf.Bytes()))
}

// lectureFileLabel is the number in a lecture's file names: its citation
// label (P13.6, as exportDownloadName and the export stage use), "" when
// the course numbering cannot be computed.
func (s *Server) lectureFileLabel(ctx context.Context, l model.Lecture) string {
	c, err := s.st.GetCourse(ctx, l.CourseID)
	if err != nil {
		slog.Warn("english file name", "lecture", l.ID, "err", err)
		return ""
	}
	nums, _, err := s.courseNumbering(ctx, c)
	if err != nil {
		slog.Warn("english file name", "lecture", l.ID, "err", err)
		return ""
	}
	n, ok := nums[l.ID]
	if !ok {
		return ""
	}
	if label := strings.TrimSpace(n.CiteLabel); label != "" {
		return label
	}
	return n.Label
}
