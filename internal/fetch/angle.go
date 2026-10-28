// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"lectureflow/internal/bridge"
)

// Two camera angles (the popup's "双机位", lead 2026-10-27): an enqueue
// carries at most one audio track and two others (video, or unknown). The
// first other track is the main picture: combined with the audio into the
// lecture's media as before. The second is the second camera angle: it is
// downloaded, joined and resumed like any track, then remuxed alone (-c
// copy, no audio) into AngleFile, which the lecture import moves into the
// lecture's directory next to its media.

// AngleName names the second camera angle of a lecture: the lecture JSON
// lists it as {"name": AngleName} and GET /api/lectures/{id}/angles/{name}
// serves its file, AngleName + ".mp4" in the lecture's directory.
const AngleName = "angle2"

// AngleFile is the second angle's file, in the task directory once merged
// and in the lecture directory once imported.
const AngleFile = AngleName + ".mp4"

// maxOtherTracks is how many video (or unknown) tracks one enqueue may
// carry: the main picture and the second angle.
const maxOtherTracks = 2

// trackLayout says what each track of a task becomes. Indexes are into the
// task's records; -1 = none.
type trackLayout struct {
	audio int // the audio track
	main  int // the main picture: the first track that is not audio
	angle int // the second angle: the second track that is not audio
}

// layoutOf reads the layout from the records' order (tracks_json keeps the
// order the extension sent).
func layoutOf(records []trackRecord) trackLayout {
	l := trackLayout{audio: -1, main: -1, angle: -1}
	for i, r := range records {
		switch {
		case r.Kind == bridge.KindAudio:
			if l.audio < 0 {
				l.audio = i
			}
		case l.main < 0:
			l.main = i
		case l.angle < 0:
			l.angle = i
		}
	}
	return l
}

// angleDone: the second angle of the task directory dir is remuxed.
func angleDone(dir string) bool { return fileExists(filepath.Join(dir, AngleFile)) }

// remuxAngle makes AngleFile in dir from the angle track's file: the first
// video stream only, copied (no re-encoding), no audio. It goes through a
// .tmp file renamed when complete.
func (q *Queue) remuxAngle(ctx context.Context, dir, in string) error {
	out := filepath.Join(dir, AngleFile)
	tmp := out + ".tmp"
	args := appendInput([]string{"-hide_banner", "-nostdin", "-y"}, in) // whitelists of media.InputArgs (merge.go)
	args = append(args, "-map", "0:v:0", "-c", "copy", "-an", "-f", "mp4", tmp)
	if err := q.ffmpeg(ctx, args); err != nil {
		os.Remove(tmp)
		var f *failure
		if errors.As(err, &f) && strings.Contains(f.detail, "matches no streams") {
			f.code = CodeAngleNoVideo
		}
		return err
	}
	if err := renameRetry(tmp, out); err != nil {
		return writeFailure(err)
	}
	return nil
}
