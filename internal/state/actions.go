// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"

	"lectureflow/internal/media"
	"lectureflow/internal/model"
)

// StagesOf returns the stages of a scope in dependency order.
func StagesOf(scope string) []string {
	switch scope {
	case model.ScopeLecture:
		return model.LectureStages
	case model.ScopeMaterial:
		return model.MaterialStages
	case model.ScopeCourse:
		return []string{model.StageGlossarySeed}
	}
	return nil
}

func indexOf(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return -1
}

// Downstream returns the stages after stage in its scope, in order.
func Downstream(stage string) []string {
	order := StagesOf(model.ScopeOfStage(stage))
	i := indexOf(order, stage)
	if i < 0 {
		return nil
	}
	return append([]string(nil), order[i+1:]...)
}

// Upstream returns the stage immediately before stage in its scope, or "".
func Upstream(stage string) string {
	order := StagesOf(model.ScopeOfStage(stage))
	i := indexOf(order, stage)
	if i <= 0 {
		return ""
	}
	return order[i-1]
}

// ActionOpts carries the context-dependent rules on top of the SPEC §4.5 table.
type ActionOpts struct {
	// MediaDeleted: storage.keep_original_media = false and the original
	// media is gone, so prep offers no rerun (SPEC §17.5).
	MediaDeleted bool
	// DownstreamRunning: a later stage of the same scope is running. Rerun
	// would have to replace it, so rerun is not offered (it would be 409).
	DownstreamRunning bool
}

// AllowedActions implements the SPEC §4.5 table. The result is never nil.
// An auxiliary task (v2) has AuxActions instead: retry and cancel, no rerun.
func AllowedActions(j model.Job, o ActionOpts) []string {
	if model.IsAuxStage(j.Stage) {
		return AuxActions(j)
	}
	switch j.Status {
	case model.StatusQueued, model.StatusRunning:
		return []string{model.ActionCancel}
	case model.StatusDone, model.StatusStale:
		if o.DownstreamRunning || (j.Stage == model.StagePrep && o.MediaDeleted) {
			return []string{}
		}
		return []string{model.ActionRerun}
	case model.StatusFailed, model.StatusCancelled:
		return []string{model.ActionRetry}
	}
	return []string{}
}

// WithTextRepair adds model.ActionRerunTextOnly ("修复断句", textrepair-int)
// to the actions of a lecture's asr job when it is done or stale, its rerun
// is allowed now (acts holds ActionRerun: no downstream stage runs) and
// offer holds: the caller found the stored transcript worth repairing (word
// timestamps, an English lecture, degenerate text; see api textRepairOffer).
// Any other job or state: acts unchanged.
func WithTextRepair(j model.Job, acts []string, offer bool) []string {
	if !offer || j.Scope != model.ScopeLecture || j.Stage != model.StageASR ||
		(j.Status != model.StatusDone && j.Status != model.StatusStale) ||
		!slices.Contains(acts, model.ActionRerun) || slices.Contains(acts, model.ActionRerunTextOnly) {
		return acts
	}
	return append(slices.Clone(acts), model.ActionRerunTextOnly)
}

// StageActions computes AllowedActions for every current job of one scope
// (a lecture's six jobs, a material's two, a course's glossary_seed), and
// AuxActions for its auxiliary tasks (v2). A running auxiliary task never
// blocks the rerun of a stage. mediaDeleted only affects prep. Keyed by stage.
func StageActions(jobs []model.Job, mediaDeleted bool) map[string][]string {
	running := map[string]bool{}
	for _, j := range jobs {
		if j.Status == model.StatusRunning {
			running[j.Stage] = true
		}
	}
	out := make(map[string][]string, len(jobs))
	for _, j := range jobs {
		o := ActionOpts{MediaDeleted: mediaDeleted}
		for _, d := range Downstream(j.Stage) {
			if running[d] {
				o.DownstreamRunning = true
			}
		}
		out[j.Stage] = AllowedActions(j, o)
	}
	return out
}

// CanDelete: a lecture (or its media) may be deleted/replaced only while no
// stage is running (SPEC §4.5). Auxiliary tasks (v2) do not count: deleting
// the lecture cancels them first (sched.DeleteLecture).
func CanDelete(jobs []model.Job) bool {
	for _, j := range jobs {
		if j.Status == model.StatusRunning && !model.IsAuxStage(j.Stage) {
			return false
		}
	}
	return true
}

// Reviewable: review operations are refused (409) while repair or translate
// is running (SPEC §4.5, §9.7).
func Reviewable(jobs []model.Job) bool {
	for _, j := range jobs {
		if j.Status == model.StatusRunning && (j.Stage == model.StageRepair || j.Stage == model.StageTranslate) {
			return false
		}
	}
	return true
}

// MediaExists reports whether the lecture's original media file exists.
// media_path may be absolute, relative to the working directory, or
// relative to the lecture directory. A media_path naming another computer
// outside the data directory (StoredPathUnsafe) is missing and never
// stat'ed.
func MediaExists(dataDir string, l model.Lecture) bool {
	if l.MediaPath == "" || StoredPathUnsafe(PathLectureMedia, l.ID, l.MediaPath) {
		return false
	}
	if _, err := os.Stat(l.MediaPath); err == nil {
		return true
	}
	if filepath.IsAbs(l.MediaPath) {
		return false
	}
	_, err := os.Stat(filepath.Join(model.LectureDir(dataDir, l.CourseID, l.ID), l.MediaPath))
	return err == nil
}

// What a stored path is (StoredPathUnsafe's log line and the startup scan).
const (
	PathLectureMedia = "lecture.media_path"
	PathMaterial     = "material.path"
	PathSyllabus     = "syllabus.path"
)

// unsafeWarned holds the stored paths StoredPathUnsafe has logged.
var unsafeWarned sync.Map

// trustedDataDir is the data directory of this process (SetDataDir),
// absolute; nil or "" until set, and then nothing is trusted.
var trustedDataDir atomic.Pointer[string]

// SetDataDir records the data directory of this process; cmd sets it once
// at startup, before anything reads a stored path (a data directory move
// or import takes effect through a restart, so the next start sets the new
// one). A stored path inside it is the user's own data even when the data
// directory is on a network share — the app opens its database there
// anyway — so StoredPathUnsafe lets it through (netpath-datadir). dir is
// made absolute (lexically; nothing is touched on disk); "" trusts
// nothing.
func SetDataDir(dir string) {
	if dir != "" {
		if a, err := filepath.Abs(dir); err == nil {
			dir = a
		}
	}
	trustedDataDir.Store(&dir)
}

// TrustedDataDir returns the data directory SetDataDir recorded ("" before).
func TrustedDataDir() string {
	if p := trustedDataDir.Load(); p != nil {
		return *p
	}
	return ""
}

// StoredPathUnsafe reports whether p, a path stored in the database (what:
// one of the Path* kinds; id: its row, for the log), names another computer
// rather than a plain local file — a UNC share, a device path or a URL
// (media.UnsafeStoredPath; security review 2, M3) — and does not lie inside
// the data directory (SetDataDir; media.ForeignStoredPath, the rule the
// data import uses too). Such a path counts as a missing file and is never
// stat'ed or opened: on Windows that would connect to the server with the
// user's NTLM credentials, and ffmpeg would read from it. LectureFlow
// itself stores plain paths (inside the data directory, which may be on a
// share) and the data import refuses other ones, so only a database edited
// elsewhere (or imported by an older version) has them. Each such path is
// logged once per process (Warn).
func StoredPathUnsafe(what, id, p string) bool {
	if !media.ForeignStoredPath(p, TrustedDataDir()) {
		return false
	}
	if _, seen := unsafeWarned.LoadOrStore(p, true); !seen {
		slog.Warn("stored path points to another computer outside the data directory: treated as a missing file, not opened",
			"what", what, "id", id, "path", p)
	}
	return true
}
