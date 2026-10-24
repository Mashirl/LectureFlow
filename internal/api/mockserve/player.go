// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// The player endpoints of SPEC-v2 P8.2 for the mock server (v2 player work):
//
//	GET  /api/lectures/{id}/media     -media <file> served with http.ServeContent (Range → 206)
//	GET  /api/lectures/{id}/media?audio=1  a synthetic audio.wav as long as the lecture (Range → 206)
//	GET  /api/lectures/{id}/cues      subs.SegmentLecture of the mock sentences
//	GET  /api/lectures/{id}/chapters  200 {chapters: [], stale: false, running: false} until generated
//	POST /api/lectures/{id}/chapters  simulated generation, SSE changed {chapters}
//	GET  /api/lectures/{id}/resume
//	PUT  /api/lectures/{id}/resume    {position_ms}
//
// Without -media every lecture answers 404 for /media, so the player runs in
// audio mode (/media?audio=1, like the real server's audio.wav). A lecture whose stream is
// "pending" starts a simulated background generation on its first /media
// request (P8.1 "首次打开播放器时，在后台生成"); it becomes "ready" after
// playerTiming.stream.

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
	"lectureflow/internal/subs"
)

// playerTiming: the simulated durations (tests shorten them).
var playerTiming = struct {
	chapters time.Duration // POST chapters → done
	stream   time.Duration // pending → ready
}{chapters: 3 * time.Second, stream: 20 * time.Second}

// playerData is the player state of one store (resume positions and
// chapters live only in memory, like everything else in the mock).
type playerData struct {
	resume   map[string]api.ResumeView
	chapters map[string]*mockChapters
}

type mockChapters struct {
	// fracs are the chapter starts as fractions of the sentence list, so the
	// same chapters fit the default and the "many" sentence lists.
	fracs       []float64
	titles      [][2]string // en, zh
	generatedAt string
	model       string
	hash        string // sentence texts at generation (P7 input hash)
	forceStale  bool   // example data: generated before an edit
	running     bool
	err         string
	gen         int
}

func (m *mock) registerPlayer(mux *http.ServeMux) {
	m.player = map[*store]*playerData{}
	for _, s := range []*store{m.main, m.empty} {
		if s == nil {
			continue
		}
		pd := &playerData{resume: map[string]api.ResumeView{}, chapters: map[string]*mockChapters{}}
		m.player[s] = pd
		// Example data: CSC1003 L01 has chapters; L04 has chapters that the
		// sentences have changed since (the "可能已过时" state); L01 has a
		// saved position (the "从 01:10 继续" notice).
		now := time.Now().UTC()
		pd.chapters[mockLectureChapters] = exampleChapters(now.Add(-26*time.Hour), false)
		pd.chapters[mockLectureStaleChapters] = exampleChapters(now.Add(-50*time.Hour), true)
		pd.resume[mockLectureChapters] = api.ResumeView{PositionMS: 70500, SavedAt: now.Add(-3 * time.Hour).Format(time.RFC3339)}
	}
	a := m.api
	mux.HandleFunc("GET /api/lectures/{id}/media", m.serveMedia)
	mux.HandleFunc("GET /api/lectures/{id}/cues", a(m.cues))
	mux.HandleFunc("GET /api/lectures/{id}/chapters", a(m.getChapters))
	mux.HandleFunc("POST /api/lectures/{id}/chapters", a(m.postChapters))
	mux.HandleFunc("GET /api/lectures/{id}/resume", a(m.getResume))
	mux.HandleFunc("PUT /api/lectures/{id}/resume", a(m.putResume))
}

// Example lectures of the player data (fixtures_test.go: CSC1003 L01, L04).
const (
	mockLectureChapters      = "01JD3631000000000000000001"
	mockLectureStaleChapters = "01JD3631000000000000000004"
)

var chapterTitles = [][2]string{
	{"Getting started", "开场"},
	{"The cache hierarchy", "缓存层次结构"},
	{"Write policies", "写策略"},
	{"Replacement and LRU", "替换策略与 LRU"},
	{"Mapping and conflict misses", "映射方式与冲突缺失"},
	{"Write-through versus write-back", "写直达与写回"},
	{"TLBs and virtual memory", "TLB 与虚拟内存"},
	{"The memory wall", "内存墙"},
}

func exampleChapters(at time.Time, stale bool) *mockChapters {
	c := &mockChapters{generatedAt: at.Format(time.RFC3339), model: "DeepSeek / deepseek-flash", forceStale: stale}
	c.fill(5)
	return c
}

func (c *mockChapters) fill(k int) {
	c.fracs, c.titles = nil, nil
	for i := 0; i < k; i++ {
		c.fracs = append(c.fracs, float64(i)/float64(k))
		c.titles = append(c.titles, chapterTitles[i%len(chapterTitles)])
	}
}

// sentenceHash is the P7 input hash of the mock (sentence texts only; the
// mock has no summary).
func sentenceHash(ss []api.Sentence) string {
	h := sha256.New()
	for _, s := range ss {
		h.Write([]byte(s.Text))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (m *mock) playerOf(s *store) *playerData {
	if pd := m.player[s]; pd != nil {
		return pd
	}
	pd := &playerData{resume: map[string]api.ResumeView{}, chapters: map[string]*mockChapters{}}
	m.player[s] = pd
	return pd
}

// ---------- media (P8.2: http.ServeContent, Range requests) ----------

func (m *mock) serveMedia(w http.ResponseWriter, r *http.Request) {
	if m.loadErr != nil {
		writeError(w, http.StatusInternalServerError, "mock：无法读取示例数据："+m.loadErr.Error())
		return
	}
	s := m.storeFor(r)
	s.mu.Lock()
	l, err := s.findLecture(r.PathValue("id"))
	if err != nil {
		s.mu.Unlock()
		writeErr(w, err)
		return
	}
	status := ""
	if l.Stream != nil {
		status = l.Stream.Status
	}
	if status == "pending" {
		s.startStream(l.ID)
		status = "generating"
	}
	audio := r.URL.Query().Get("audio") == "1"
	var durMS int64
	if l.DurationMS != nil {
		durMS = *l.DurationMS
	}
	s.mu.Unlock()
	if audio && l.Playable {
		// lead ruling 7 (phase 2): ?audio=1 is audio.wav, also with Range
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, "audio.wav", time.Time{}, newToneWAV(max(durMS, 60000)))
		return
	}
	switch {
	case !l.Playable:
		writeErr(w, codedErr(http.StatusConflict, "media.not_ready", nil, ""))
		return
	case status == "generating":
		writeErr(w, codedErr(http.StatusNotFound, "media.generating", nil, ""))
		return
	case status != "ready":
		code := "media.failed"
		if status == "audio_only" || status == "no_space" {
			code = "media." + status
		}
		writeErr(w, codedErr(http.StatusNotFound, code, nil, ""))
		return
	case m.mediaFile == "":
		writeError(w, http.StatusNotFound, "mock：没有用 -media 指定视频文件，播放器以音频模式运行")
		return
	}
	f, err := os.Open(m.mediaFile)
	if err != nil {
		writeError(w, http.StatusNotFound, "mock：无法打开 -media 指定的文件："+err.Error())
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, filepath.Base(m.mediaFile), info.ModTime(), f)
}

// startStream simulates the background generation of stream.mp4 for an old
// lecture (P8.1): generating with progress, then ready. Must hold s.mu.
func (s *store) startStream(id string) {
	l, err := s.findLecture(id)
	if err != nil || l.Stream == nil || l.Stream.Status != "pending" {
		return
	}
	l.Stream = &api.StreamInfo{Status: "generating"}
	s.changed(l)
	total := playerTiming.stream
	const steps = 10
	for k := 1; k <= steps; k++ {
		k := k
		time.AfterFunc(total*time.Duration(k)/steps, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			l, err := s.findLecture(id)
			if err != nil || l.Stream == nil || l.Stream.Status != "generating" {
				return
			}
			if k == steps {
				l.Stream = &api.StreamInfo{Status: "ready", SizeBytes: 412 << 20}
			} else {
				l.Stream = &api.StreamInfo{Status: "generating", Progress: math.Round(float64(k)/steps*100) / 100}
			}
			s.changed(l)
		})
	}
}

// ---------- cues (P8.2: subs.SegmentLecture of the sentences) ----------

// subsSentences converts the API sentences for internal/subs: English is
// always the effective text (P4.3); a translation counts once it exists.
func subsSentences(ss []api.Sentence) []subs.Sentence {
	out := make([]subs.Sentence, 0, len(ss))
	for _, s := range ss {
		zh := ""
		if s.Zh != nil && s.ZhStatus != model.ZhNone {
			zh = *s.Zh
		}
		out = append(out, subs.Sentence{Idx: s.Idx, StartMS: s.StartMS, EndMS: s.EndMS, Text: s.Text, ASRText: s.ASRText, Zh: zh})
	}
	return out
}

func (m *mock) cues(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.findLecture(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if !l.Playable {
		writeError(w, http.StatusConflict, "转写完成后才有字幕")
		return
	}
	out := subs.SegmentLecture(subsSentences(s.sentenceList(r)))
	if out == nil {
		out = []subs.Cue{}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------- resume (P6.4) ----------

func (m *mock) getResume(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.findLecture(r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m.playerOf(s).resume[r.PathValue("id")])
}

func (m *mock) putResume(w http.ResponseWriter, r *http.Request, s *store) {
	var in struct {
		PositionMS *int64 `json:"position_ms"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if in.PositionMS == nil || *in.PositionMS < 0 {
		writeError(w, http.StatusBadRequest, "position_ms 无效")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.findLecture(r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	v := api.ResumeView{PositionMS: *in.PositionMS, SavedAt: time.Now().UTC().Format(time.RFC3339)}
	m.playerOf(s).resume[r.PathValue("id")] = v
	writeJSON(w, http.StatusOK, v)
}

// ---------- chapters (P7) ----------

// chaptersView renders the stored chapters for the current sentence list.
// Must hold s.mu.
func chaptersView(c *mockChapters, ss []api.Sentence) api.ChaptersView {
	v := api.ChaptersView{Chapters: []api.Chapter{}, GeneratedAt: c.generatedAt, Model: c.model, Running: c.running, Error: c.err}
	last := -1
	for i, f := range c.fracs {
		idx := int(f * float64(len(ss)))
		if idx >= len(ss) || idx <= last {
			continue
		}
		last = idx
		v.Chapters = append(v.Chapters, api.Chapter{StartIdx: idx, StartMS: ss[idx].StartMS, TitleEn: c.titles[i][0], TitleZh: c.titles[i][1]})
	}
	v.Stale = c.generatedAt != "" && (c.forceStale || (c.hash != "" && c.hash != sentenceHash(ss)))
	return v
}

func (m *mock) getChapters(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")
	if _, err := s.findLecture(id); err != nil {
		writeErr(w, err)
		return
	}
	c := m.playerOf(s).chapters[id]
	if c == nil {
		// never generated (lead ruling 21 of power management): 200 with an
		// empty result, as the real API answers since then
		writeJSON(w, http.StatusOK, api.ChaptersView{Chapters: []api.Chapter{}})
		return
	}
	writeJSON(w, http.StatusOK, chaptersView(c, s.sentenceList(r)))
}

// postChapters starts a simulated generation: running → done after
// playerTiming.chapters with 3–8 chapters (the "error" scenario fails as if
// the output was invalid twice, P7). Completion: SSE changed {chapters}.
func (m *mock) postChapters(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")
	l, err := s.findLecture(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !l.Playable {
		writeError(w, http.StatusConflict, "转写完成后才能生成章节")
		return
	}
	pd := m.playerOf(s)
	c := pd.chapters[id]
	if c == nil {
		c = &mockChapters{}
		pd.chapters[id] = c
	}
	ss := s.sentenceList(r)
	if !c.running {
		c.running, c.err = true, ""
		c.gen++
		gen, fail, n := c.gen, scenario(r) == "error", len(ss)
		hash := sentenceHash(ss)
		s.notify(api.ChangedEvent{Kind: "chapters", ID: id})
		time.AfterFunc(playerTiming.chapters, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if c.gen != gen {
				return
			}
			c.running = false
			if fail {
				c.err = "章节结果两次未通过校验：start_idx 不是单调递增"
			} else {
				c.fill(max(3, min(8, n/40)))
				c.generatedAt = time.Now().UTC().Format(time.RFC3339)
				c.model = "DeepSeek / deepseek-flash"
				c.hash, c.forceStale = hash, false
			}
			s.notify(api.ChangedEvent{Kind: "chapters", ID: id})
		})
	}
	writeJSON(w, http.StatusAccepted, chaptersView(c, ss))
}

// ---------- audio.wav (?audio=1) ----------

// toneWAV is a 16 kHz mono WAV of a quiet tone whose pitch changes every
// 4 seconds, computed on the fly (an hour is 115 MB; nothing is held in
// memory), so http.ServeContent can answer Range requests on it.
type toneWAV struct {
	header []byte
	size   int64
	pos    int64
}

func newToneWAV(ms int64) *toneWAV {
	n := ms * sampleRate / 1000
	return &toneWAV{header: wavHeader(int(n)), size: 44 + 2*n}
}

func toneSample(i int64) int16 {
	t := float64(i) / sampleRate
	seg := math.Floor(t / 4)
	in := t - seg*4 // seconds into the segment
	env := math.Min(1, math.Min(in/0.05, (4-in)/0.05))
	freq := 330 + float64(int64(seg)%12)*30
	return int16(2500 * env * math.Sin(2*math.Pi*freq*in))
}

func (w *toneWAV) Read(p []byte) (int, error) {
	if w.pos >= w.size {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && w.pos < w.size {
		if w.pos < 44 {
			p[n] = w.header[w.pos]
		} else {
			var b [2]byte
			binary.LittleEndian.PutUint16(b[:], uint16(toneSample((w.pos-44)/2)))
			p[n] = b[(w.pos-44)%2]
		}
		n++
		w.pos++
	}
	return n, nil
}

func (w *toneWAV) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		off += w.pos
	case io.SeekEnd:
		off += w.size
	default:
		return 0, errors.New("toneWAV: bad whence")
	}
	if off < 0 {
		return 0, errors.New("toneWAV: negative position")
	}
	w.pos = off
	return off, nil
}
