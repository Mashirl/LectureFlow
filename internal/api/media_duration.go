// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"lectureflow/internal/model"
)

// Lecture.duration_ms (real acceptance issue 8): the length of the media,
// i.e. of audio.wav, which prep converts from the whole original file. The
// last sentence's end was used before, but it is the end of the last
// speech — minutes short of the recording when it ends in silence, music
// or questions the model did not transcribe. The last sentence's end stays
// the fallback while there is no audio.wav (before prep, or a lecture
// whose files are gone).

// wavDurations caches audio.wav lengths by path, checked against the
// file's size and modification time (a view must not reread a header that
// has not changed; views are built for every list and SSE refresh).
var wavDurations sync.Map // path → wavDuration

type wavDuration struct {
	size int64
	mod  time.Time
	ms   int64
	ok   bool
}

// cachedWAVDurationMS is wavDurationMS through the cache.
func cachedWAVDurationMS(path string) (int64, bool) {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return 0, false
	}
	if v, ok := wavDurations.Load(path); ok {
		if e := v.(wavDuration); e.size == fi.Size() && e.mod.Equal(fi.ModTime()) {
			return e.ms, e.ok
		}
	}
	ms, ok := wavDurationMS(path)
	wavDurations.Store(path, wavDuration{size: fi.Size(), mod: fi.ModTime(), ms: ms, ok: ok})
	return ms, ok
}

// mediaDurationMS is the length of the lecture's audio.wav once prep has
// produced it (prep done or stale: while prep runs or after it failed the
// file may be an earlier conversion). ok is false otherwise.
func (s *Server) mediaDurationMS(l model.Lecture, jobs []model.Job) (int64, bool) {
	j, found := findJob(jobs, model.StagePrep)
	if !found || (j.Status != model.StatusDone && j.Status != model.StatusStale) {
		return 0, false
	}
	ms, ok := cachedWAVDurationMS(filepath.Join(s.lectureDir(l), model.AudioFile))
	return ms, ok && ms > 0
}
