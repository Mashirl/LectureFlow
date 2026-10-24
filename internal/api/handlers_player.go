// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/stages"
	"lectureflow/internal/state"
	"lectureflow/internal/subs"
)

// The player's endpoints (SPEC-v2 P6.4, P7, P8.2; phase-2 lead rulings 7–9)
// and the title regeneration of P13.5.

// ---- P8.1 stream state ----

// streamState reads kv stream:{lecture}: the result of the last attempt
// (stages.StreamReady, StreamAudioOnly, StreamNoSpace, StreamFailed); ok is
// false when there is none.
func (s *Server) streamState(ctx context.Context, lectureID string) (model.StreamState, bool, error) {
	st, ok, err := stages.StreamStateOf(ctx, s.st, lectureID)
	if err != nil || !ok || st.Status == "" {
		return model.StreamState{}, false, err
	}
	return st, true, nil
}

// errorCode is the code of an error stored as ErrorInfo JSON (kv stream
// state, job.error); legacy text is returned as it is.
func errorCode(raw string) string {
	if info, _ := state.ParseJobError(raw); info != nil {
		return info.Code
	}
	return raw
}

// streamGenerating: the stream task waits or runs.
func streamGenerating(aux []model.Job) (model.Job, bool) {
	j, ok := findJob(aux, model.StageStream)
	if !ok {
		return j, false
	}
	switch j.Status {
	case model.StatusPending, model.StatusQueued, model.StatusRunning:
		return j, true
	}
	return j, false
}

// streamInfo is Lecture.Stream (P8.1): the stream task waiting or running
// → "generating" (with its progress); stream.mp4 on disk → "ready";
// otherwise the last result (audio_only, no_space, failed with the code of
// its ErrorInfo), or "pending" when none was recorded (GET /media starts
// it). nil before prep is done (and for drafts).
func (s *Server) streamInfo(ctx context.Context, l model.Lecture, jobs, aux []model.Job) (*StreamInfo, error) {
	if j, ok := streamGenerating(aux); ok {
		return &StreamInfo{Status: "generating", Progress: j.Progress}, nil
	}
	st, has, err := s.streamState(ctx, l.ID)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(filepath.Join(s.lectureDir(l), model.StreamFile)); err == nil && info.Mode().IsRegular() {
		return &StreamInfo{Status: stages.StreamReady, Transcoded: st.Transcoded, SizeBytes: info.Size()}, nil
	}
	if has && st.Status != stages.StreamReady {
		v := &StreamInfo{Status: st.Status}
		if st.Error != "" {
			v.Error = errorCode(st.Error)
		}
		return v, nil
	}
	prep, ok := findJob(jobs, model.StagePrep)
	if !ok || (prep.Status != model.StatusDone && prep.Status != model.StatusStale) {
		return nil, nil
	}
	return &StreamInfo{Status: "pending"}, nil
}

// ---- GET /api/lectures/{id}/media[?audio=1] (P8.2, ruling 7) ----

func (s *Server) media(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	l, err := s.getLectureRow(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	dir := s.lectureDir(l)
	switch strings.ToLower(r.URL.Query().Get("audio")) {
	case "", "0", "false":
	case "1", "true":
		// The player's audio mode: audio.wav, also with Range.
		if !serveMediaFile(w, r, filepath.Join(dir, model.AudioFile), "audio/wav") {
			writeError(w, http.StatusNotFound, "audio.not_ready", nil)
		}
		return
	default:
		writeError(w, http.StatusBadRequest, "request.bad_param", map[string]any{"name": "audio"})
		return
	}
	if serveMediaFile(w, r, filepath.Join(dir, model.StreamFile), "video/mp4") {
		return
	}
	// No stream.mp4. A recorded result that is final until the user acts
	// (media-p2: an audio-only source, too little disk space, a failure)
	// is reported and never queued again here; the lecture page offers
	// retry (POST .../stages/stream/retry).
	st, has, err := s.streamState(ctx, l.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if has {
		switch st.Status {
		case stages.StreamAudioOnly:
			writeError(w, http.StatusNotFound, "media.audio_only", nil)
			return
		case stages.StreamNoSpace:
			writeError(w, http.StatusNotFound, "media.no_space", nil)
			return
		case stages.StreamFailed:
			info := model.ErrorInfo{Code: stages.MediaFailed}
			if ji, text := state.ParseJobError(st.Error); ji != nil {
				info = *ji
			} else if text != "" {
				info.Detail = text
			}
			writeErrorInfo(w, http.StatusNotFound, info)
			return
		}
	}
	// Otherwise start generating it when it can be (an old lecture
	// prepared before v2, P8.1).
	generating := false
	if s.sc != nil {
		if generating, err = s.sc.EnsureStream(ctx, l.ID); err != nil {
			s.fail(w, r, notFoundAs(err, "lecture.not_found"))
			return
		}
	}
	if generating {
		writeError(w, http.StatusNotFound, "media.generating", nil)
		return
	}
	// The last stream task failed or was cancelled (retry from the lecture
	// page), or prep has not finished.
	if j, err := s.st.GetCurrentJob(ctx, model.ScopeLecture, l.ID, model.StageStream); err == nil && j.Status == model.StatusFailed {
		info := model.ErrorInfo{Code: stages.MediaFailed}
		if j.Error != nil {
			if ji, text := state.ParseJobError(*j.Error); ji != nil {
				info = *ji
			} else {
				info.Detail = text
			}
		}
		writeErrorInfo(w, http.StatusNotFound, info)
		return
	} else if err != nil && !errors.Is(err, db.ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	writeError(w, http.StatusNotFound, "media.not_ready", nil)
}

// serveMediaFile serves a file with http.ServeContent (Range → 206); false
// when it does not exist.
func serveMediaFile(w http.ResponseWriter, r *http.Request, path, contentType string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
	return true
}

// ---- GET /api/lectures/{id}/cues (P4, P8.2, ruling 8) ----

// cues are the subtitle cues of the lecture: subs.SegmentLecture over every
// sentence, converted exactly like the VTT export (stages.CueSentences).
func (s *Server) cues(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
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
	out := subs.SegmentLecture(stages.CueSentences(ss))
	if out == nil {
		out = []subs.Cue{}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- chapters (P7, ruling 9) ----

// roleConfigured reports whether an LLM role has a usable provider and
// model now (auxiliary tasks use the role's current setting).
func (s *Server) roleConfigured(role string) bool {
	if s.set == nil {
		return false
	}
	_, err := s.set.RoleTarget(role)
	return err == nil
}

// summaryReady: title and chapters need the lecture's summary (done or
// stale; ruling 1).
func (s *Server) summaryReady(ctx context.Context, lectureID string) (bool, error) {
	j, err := s.st.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageSummary)
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	return err == nil && (j.Status == model.StatusDone || j.Status == model.StatusStale), err
}

// chaptersView builds ChaptersView; ok=false when nothing was ever
// generated and no generation runs or failed (v is then the empty view).
func (s *Server) chaptersView(ctx context.Context, l model.Lecture) (ChaptersView, bool, error) {
	v := ChaptersView{Chapters: []Chapter{}}
	raw, has, err := s.kvValue(ctx, model.KVChaptersPrefix+l.ID)
	if err != nil {
		return v, false, err
	}
	j, err := s.st.GetCurrentJob(ctx, model.ScopeLecture, l.ID, model.StageChapters)
	hasJob := err == nil
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return v, false, err
	}
	if hasJob {
		switch j.Status {
		case model.StatusPending, model.StatusQueued, model.StatusRunning:
			v.Running = true
		case model.StatusFailed:
			v.Error = "chapters.invalid_output"
			if j.Error != nil {
				v.Error = errorCode(*j.Error)
			}
		}
	}
	var ch model.Chapters
	if has {
		has = json.Unmarshal([]byte(raw), &ch) == nil
	}
	if !has {
		return v, v.Running || v.Error != "", nil
	}
	ss, err := s.st.ListSentences(ctx, l.ID, 0, 0)
	if err != nil {
		return v, false, err
	}
	start := make(map[int]int64, len(ss))
	for _, x := range ss {
		start[x.Idx] = x.StartMS
	}
	for _, c := range ch.Chapters {
		v.Chapters = append(v.Chapters, Chapter{StartIdx: c.StartIdx, StartMS: start[c.StartIdx], TitleEn: c.TitleEn, TitleZh: c.TitleZh})
	}
	v.GeneratedAt = ch.GeneratedAt
	if v.Model = ch.Model; ch.Model != "" && ch.Provider != "" {
		// Provider is an id or a name: the current name when it is a
		// provider id, else the recorded value.
		v.Model = s.providerName(ch.Provider, ch.Provider) + " / " + ch.Model
	}
	// P7: the summary or a sentence text changed since generation.
	if h, err := stages.ChaptersInputHash(ctx, s.stageDeps(), l.ID); err == nil {
		v.Stale = h != ch.InputHash
	} else if ctx.Err() == nil {
		s.logWarn("chapters input hash", l.ID, err)
	}
	return v, true, nil
}

// GET /api/lectures/{id}/chapters → ChaptersView. Never generated, and no
// generation running or failed: 200 with the empty view {"chapters": [],
// "stale": false, "running": false} (power-brief ruling 21, no longer 404
// chapters.not_found; the code stays registered, lead ruling 9.4).
func (s *Server) getChapters(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	l, err := s.getLectureRow(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, _, err := s.chaptersView(ctx, l)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// POST /api/lectures/{id}/chapters: queue the chapters task (202, running)
// when the chapters role is configured (409 chapters.not_configured) and
// the summary exists (409 chapters.summary_not_ready). Completion: SSE
// changed {kind: "chapters"}.
func (s *Server) postChapters(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	l, err := s.getLectureRow(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !s.roleConfigured(model.RoleChapters) {
		writeError(w, http.StatusConflict, "chapters.not_configured", nil)
		return
	}
	if ok, err := s.summaryReady(ctx, l.ID); err != nil {
		s.fail(w, r, err)
		return
	} else if !ok {
		writeError(w, http.StatusConflict, "chapters.summary_not_ready", nil)
		return
	}
	if _, err := s.sc.EnqueueAux(ctx, model.ScopeLecture, l.ID, model.StageChapters); err != nil {
		s.fail(w, r, notFoundAs(err, "lecture.not_found"))
		return
	}
	s.hub.Changed("chapters", l.ID)
	v, _, err := s.chaptersView(ctx, l)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v.Running, v.Error = true, ""
	writeJSON(w, http.StatusAccepted, v)
}

// ---- resume (P6.4) ----

// resumeView reads kv resume:{lecture}; {0, ""} when nothing was saved.
func (s *Server) resumeView(ctx context.Context, lectureID string) (ResumeView, error) {
	var v ResumeView
	raw, ok, err := s.kvValue(ctx, model.KVResumePrefix+lectureID)
	if err != nil || !ok {
		return v, err
	}
	if json.Unmarshal([]byte(raw), &v) != nil || v.PositionMS < 0 {
		return ResumeView{}, nil
	}
	return v, nil
}

func (s *Server) getResume(w http.ResponseWriter, r *http.Request) {
	l, err := s.getLectureRow(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.resumeView(r.Context(), l.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// PUT /api/lectures/{id}/resume {position_ms}: saved with the time it was
// saved at (UTC RFC 3339), on the server so it survives another browser.
func (s *Server) putResume(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in struct {
		PositionMS *int64 `json:"position_ms"`
	}
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if in.PositionMS == nil || *in.PositionMS < 0 {
		writeError(w, http.StatusBadRequest, "resume.bad_position", nil)
		return
	}
	l, err := s.getLectureRow(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v := ResumeView{PositionMS: *in.PositionMS, SavedAt: s.now().UTC().Format(time.RFC3339)}
	b, _ := json.Marshal(v)
	if err := s.st.SetKV(ctx, model.KVResumePrefix+l.ID, string(b)); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ---- POST /api/lectures/{id}/title (P13.5 重新生成标题与简介) ----

// regenerateTitle queues the title task (202 with the lecture). A title the
// user set is overwritten only with force (409 title.user_set); the task
// then writes it with source llm.
func (s *Server) regenerateTitle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in TitleRegenerateInput
	if err := decodeOptionalJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	l, err := s.getLectureRow(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !s.roleConfigured(model.RoleOrganizer) {
		writeError(w, http.StatusConflict, "title.not_configured", nil)
		return
	}
	if ok, err := s.summaryReady(ctx, l.ID); err != nil {
		s.fail(w, r, err)
		return
	} else if !ok {
		writeError(w, http.StatusConflict, "title.summary_not_ready", nil)
		return
	}
	if l.TitleSource == model.TitleSourceUser && !in.Force {
		writeError(w, http.StatusConflict, "title.user_set", nil)
		return
	}
	if in.Force {
		if err := stages.RequestTitleForce(ctx, s.st, l.ID); err != nil {
			s.fail(w, r, notFoundAs(err, "lecture.not_found"))
			return
		}
	}
	if _, err := s.sc.EnqueueAux(ctx, model.ScopeLecture, l.ID, model.StageTitle); err != nil {
		s.fail(w, r, notFoundAs(err, "lecture.not_found"))
		return
	}
	s.hub.Changed("lecture", l.ID)
	s.respondLecture(w, r, l.ID, http.StatusAccepted)
}
