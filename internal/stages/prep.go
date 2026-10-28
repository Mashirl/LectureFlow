// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	lfmedia "lectureflow/internal/media"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// prepStage converts the uploaded media to 16 kHz mono WAV (SPEC §6.1,
// §17.3), then makes the playback file stream.mp4 (SPEC-v2 P8.1) and hands
// the media's creation_time to the recording-time resolution (P13.3.4).
// stream-parallel (user decision 2026-10-27): only a remux (about a second)
// is done here; a source that must be transcoded (minutes) gets its
// stream.mp4 from the stream task, queued at the end of prep, which runs in
// the background while asr and the later stages go on.
type prepStage struct {
	d     Deps
	media mediaTools // nil = execMedia (tests inject fakes)
}

func (s *prepStage) Name() string                 { return model.StagePrep }
func (s *prepStage) Resource() sched.ResourceKind { return sched.ResourceCPU }

// InputHash: prep has no input hash (SPEC §4.4 defines none).
func (s *prepStage) InputHash(context.Context, string) (string, error) { return "", nil }

// ffmpeg failure classes.
const (
	ffmpegOther   = iota // anything else: retryable (output not writable, disk full, killed, …)
	ffmpegNoAudio        // the media has no audio stream: permanent
	ffmpegCorrupt        // the media cannot be read: permanent
)

// classifyFFmpeg decides from ffmpeg's stderr whether a failure is caused by
// the input media itself. Only what ffmpeg prints when it really cannot
// read the input counts: it cannot open it, finds no audio stream, or gives
// up because too much of it fails to decode ("Decode error rate … exceeds
// maximum …", exit code 69). The same wording in the messages of a run
// that goes on (ffmpegNonFatal: a damaged packet, a read error that ends
// the input early) or about the output file does not.
func classifyFFmpeg(stderr string) int {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case ffmpegNonFatal(line):
			continue
		case strings.Contains(line, "does not contain any stream"), strings.Contains(line, "matches no streams"):
			return ffmpegNoAudio
		}
	}
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case ffmpegNonFatal(line):
			continue
		case strings.HasSuffix(line, "Invalid data found when processing input"),
			strings.Contains(line, "moov atom not found"),
			strings.Contains(line, "EBML header parsing failed"),
			strings.Contains(line, "Decode error rate") && strings.Contains(line, "exceeds maximum"):
			return ffmpegCorrupt
		}
	}
	return ffmpegOther
}

// nonFatalMarkers are ffmpeg messages after which the run goes on, so their
// "Invalid data found when processing input" does not say the file cannot
// be read (ffmpeg 9.0.1 exits 0 with any number of them):
//   - "Error opening output …": the output's problem;
//   - a damaged packet: "Error while decoding stream #0:1: …" (ffmpeg up to
//     6), "[aist#0:0/aac @ …] [dec:aac @ …] Error submitting packet to
//     decoder: …" and "… Decoding error: …" (7+);
//   - "[in#0/ogg @ …] Error during demuxing: …" (7+): a read error in the
//     middle of the input, which then ends there.
var nonFatalMarkers = []string{
	"Error opening output",
	"Error while decoding",
	"Decoding error:",
	"to decoder:",
	"Error during demuxing:",
}

// ffmpegNonFatal reports whether line is one of the nonFatalMarkers
// messages.
func ffmpegNonFatal(line string) bool {
	for _, m := range nonFatalMarkers {
		if strings.Contains(line, m) {
			return true
		}
	}
	return false
}

// audioShareRemux is the share of prep's progress taken by audio.wav when a
// remux follows (quick). Without a playback file step in prep — no video, or
// a transcode, which the stream task does in the background (stream-parallel,
// deviating from P8.1 "转码耗时较长，计入 prep 的进度") — audio.wav is all of it.
const audioShareRemux = 0.8

func (s *prepStage) Run(ctx context.Context, lectureID string, progress func(float64)) error {
	d := s.d
	l, err := d.Store.GetLecture(ctx, lectureID)
	if err != nil {
		return err
	}
	// Absolute paths everywhere: ffmpeg runs with the lecture directory as
	// its working directory, so a relative data_dir would resolve twice.
	dir, err := filepath.Abs(d.lectureDir(l))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// A media_path naming another computer (a UNC share, a URL: only a
	// database edited elsewhere has one) is a missing file and is never
	// opened (security review 2, M3; state.StoredPathUnsafe).
	media := ""
	if !state.StoredPathUnsafe(state.PathLectureMedia, lectureID, l.MediaPath) {
		media = resolvePath(l.MediaPath, dir)
	}
	if fi, err := os.Stat(media); media == "" || err != nil || fi.IsDir() {
		return sched.Permanent(model.Errorf(CodePrepMediaMissing, map[string]any{"file": filepath.Base(l.MediaPath)},
			fmt.Errorf("找不到讲座的原始媒体文件（%s）。请重新上传录音或录像", l.MediaPath)))
	}
	// A text playlist (an HLS .m3u8, an ffconcat script, …) is no recording,
	// and ffmpeg would read the files it lists (review M3): refused before
	// ffmpeg sees it. Uploads are checked the same way; this catches media
	// stored before that check.
	if err := playlistRefusal(media); err != nil {
		return err
	}
	tools, err := d.tools(s.media)
	if err != nil {
		return sched.Permanent(model.Errorf(CodePrepFFmpegMissing, map[string]any{"path": d.Cfg.FFmpeg},
			fmt.Errorf("找不到 ffmpeg（config.toml 的 ffmpeg = %q）：%v。请安装 ffmpeg 或在配置中填写 ffmpeg.exe 的完整路径", d.Cfg.FFmpeg, err)))
	}

	// The probe decides the playback file and gives the recording time. It
	// never fails prep: problems of the file itself are reported by the
	// audio conversion below, and the playback file is optional (the
	// player falls back to audio mode). The exception is a file the
	// demuxer whitelist refuses (media.format_rejected): the conversion,
	// which reads it with the same whitelist, would refuse it too, without
	// naming the format.
	info, probeErr := probeMedia(ctx, tools, media)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if isFormatRejected(probeErr) {
		return probeErr
	}
	plan := planNone
	if probeErr == nil {
		plan = planOf(info)
	}
	audioShare := 1.0
	if plan == planRemux {
		audioShare = audioShareRemux
	}

	if err := s.convertAudio(ctx, tools, l, dir, media, func(p float64) { progress(p * audioShare) }); err != nil {
		return err
	}
	progress(audioShare)

	// P8.1: stream.mp4. Its hash is not part of asr's input_hash (asr reads
	// audio.wav only), so a transcode need not hold up asr: it is left to
	// the stream task (stream-parallel), which records the result in kv and
	// deletes the original under keep_original_media = false once the file
	// exists. A remux, an audio-only source and a failed probe are settled
	// here as before.
	background := plan == planTranscode
	if !background {
		var st model.StreamState
		if probeErr == nil {
			st, err = makeStream(ctx, tools, dir, media, info, func(p float64) { progress(audioShare + p*(1-audioShare)) })
		} else {
			err = probeErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			slog.Warn("prep: playback file not generated", "lecture", lectureID, "err", err)
			st = model.StreamState{Status: StreamFailed, Error: errorInfoJSON(err)}
		}
		d.setStreamState(ctx, lectureID, st)
		progress(1)
		if !d.keepOriginalMedia() {
			removeOriginal(ctx, d, lectureID, media, dir, st.Status)
		}
	}

	// P13.3.4 / lead ruling 3: the media's creation_time is a "metadata"
	// candidate for the recording time. Auxiliary information: a failure is
	// logged, prep stays done.
	if probeErr == nil && info.CreationTime != nil {
		if err := ResolveRecordedAt(context.WithoutCancel(ctx), d, lectureID, info.CreationTime); err != nil {
			slog.Warn("prep: resolve recorded_at from creation_time", "lecture", lectureID, "err", err)
		}
	}
	if background {
		// Last, so that nothing after it can fail or cancel prep: a stream
		// job waiting for a prep that never finishes would read as
		// "generating" (api streamInfo). The job waits for prep done
		// (state auxDeps) and then takes a CPU slot, in parallel with asr.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		queueStream(ctx, d, lectureID)
	}
	return nil
}

// queueStream asks for a run of the stream task after prep
// (stream-parallel): state.EnqueueAux (a new pending job, also after a done,
// failed or cancelled run; nothing when one waits or runs already) and the
// job event that tells the UI. When it cannot be queued the playback file is
// recorded as failed (media.failed with the reason), so the lecture shows
// the retry instead of waiting for a run that never comes; prep stays done.
func queueStream(ctx context.Context, d Deps, lectureID string) {
	ctx = context.WithoutCancel(ctx)
	queued, err := state.EnqueueAux(ctx, d.Store, model.ScopeLecture, lectureID, model.StageStream)
	if err != nil {
		slog.Warn("prep: queue the playback file", "lecture", lectureID, "err", err)
		d.setStreamState(ctx, lectureID, model.StreamState{Status: StreamFailed,
			Error: errorInfoJSON(model.Errorf(MediaFailed, nil, fmt.Errorf("queueing the playback file (stream task) failed: %w", err)))})
		return
	}
	if !queued {
		return // one waits or runs already (e.g. the player asked for it)
	}
	slog.Info("prep: playback file to be transcoded in the background", "lecture", lectureID)
	if j, err := d.Store.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageStream); err == nil {
		d.events().Job(j)
	}
}

// KeptOriginalNote is the job note of a prep (or stream task) that kept the
// original media despite keep_original_media = false because the playback
// file could not be made.
const KeptOriginalNote = "播放文件未生成（%s），已保留原始媒体文件，以便之后补生成；生成成功后会按 keep_original_media = false 删除"

// removeOriginal is keep_original_media = false (SPEC §17.5 as changed by
// P8.1 and the lead's ruling): the original goes once stream.mp4 exists
// (ready) or the source has no video (audio_only); while the playback file
// could not be made (failed, no_space) it is kept, so the stream task can
// still make it — deleting it would be an unrecoverable loss of the video.
// The reason is written to the job note. Only files inside the lecture
// directory (uploads) are ever deleted: a media path pointing elsewhere is
// the user's file.
func removeOriginal(ctx context.Context, d Deps, lectureID, media, dir, streamStatus string) {
	if streamStatus != StreamReady && streamStatus != StreamAudioOnly {
		slog.Info("keep_original_media=false: original kept until the playback file exists", "lecture", lectureID, "stream", streamStatus)
		sched.UpdateStats(ctx, func(st *model.JobStats) {
			st.Note = fmt.Sprintf(KeptOriginalNote, streamStatus)
			st.NoteInfo = noteInfo(part(NotePrepKeptOriginal, map[string]any{"stream_status": streamStatus}))
		})
		return
	}
	if !within(media, dir) {
		slog.Warn("keep_original_media=false but the media is outside the lecture directory; not deleted", "lecture", lectureID, "media", media)
		return
	}
	if err := os.Remove(media); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("remove original media", "lecture", lectureID, "err", err)
	}
}

// audioArgs are the ffmpeg arguments that write the 16 kHz mono WAV of
// SPEC §6.1 from media to out. The input is read with the whitelists of
// media.InputArgs (security review 2, M3), which must come right before its
// "-i": after it they would be output options and silently do nothing.
//
// audioTimeline keeps the WAV on the media's timeline: a WAV has no
// timestamps, so without it an audio track that starts after the file
// does (audio 3 s later than the picture), a negative start, or a hole in
// the audio (a damaged stretch, a recording paused and resumed) was closed
// up, and every sentence after it came early against the playback file
// and the original media (test-media I2).
func audioArgs(media, out string) []string {
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-progress", "pipe:1", "-y", "-threads", "2"}
	args = append(args, lfmedia.InputArgs()...)
	return append(args, "-i", media, "-vn", "-af", audioTimeline, "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-f", "wav", out)
}

// audioTimeline is the audio filter of audioArgs: aresample's timestamp
// compensation with filling and trimming only (async=1: no stretching), from
// time 0 of the file (first_pts=0). A start later than the file's is padded
// with silence and an earlier one trimmed (the playback file starts at 0
// as well); a gap or overlap of more than 0.1 s (swresample's
// min_hard_comp) is filled with silence or dropped. Media whose audio is
// continuous from the file's start come out sample for sample as without
// it: checked on the bundled ffmpeg 9.0.2 with all 21 recordings of the
// acceptance courses, full length (NOTES.md, pipeline-builder test-media
// fixes).
const audioTimeline = "aresample=async=1:first_pts=0"

// convertAudio writes audio.wav (SPEC §6.1) through audio.wav.tmp (or
// another free .tmp name when that one is still held: tmpFor) and puts it
// in place with replaceFile: an audio.wav that another program has open
// fails the run with prep.file_in_use and stays as it was.
func (s *prepStage) convertAudio(ctx context.Context, tools mediaTools, l model.Lecture, dir, media string, progress func(float64)) error {
	out := filepath.Join(dir, model.AudioFile)
	tmp, err := tmpFor(out)
	if err != nil {
		return inUseCoded(CodePrepFileInUse, err)
	}

	msg, werr := tools.FFmpeg(ctx, dir, audioArgs(media, tmp), progress)
	if ctx.Err() != nil {
		os.Remove(tmp)
		return ctx.Err()
	}
	if werr != nil {
		os.Remove(tmp)
		var se *ffmpegStartError
		if errors.As(werr, &se) {
			return sched.Permanent(model.Errorf(CodePrepFFmpegStartFailed, map[string]any{"path": se.exe}, se))
		}
		if _, err := os.Stat(media); err != nil {
			return sched.Permanent(model.Errorf(CodePrepMediaVanished, map[string]any{"file": filepath.Base(l.MediaPath)},
				fmt.Errorf("讲座的原始媒体文件（%s）在预处理过程中消失了。请重新上传录音或录像", l.MediaPath)))
		}
		// Appendix D6 (lead ruling at the phase-2 gate): classified like
		// the playback file — corrupt, unsupported and no_audio are the
		// file's own problems (permanent), anything else media.failed
		// (retryable); the raw ffmpeg output is the detail.
		return mediaError(msg, werr)
	}
	fi, err := os.Stat(tmp)
	if err != nil {
		return model.Errorf(MediaFailed, nil, fmt.Errorf("预处理失败：ffmpeg 退出码为 0 但没有写出 %s：%v。ffmpeg 输出：%s", tmp, err, msg))
	}
	if fi.Size() <= 44 { // a WAV header and no samples
		os.Remove(tmp)
		return sched.Permanent(model.Errorf(MediaNoAudio, nil,
			fmt.Errorf("媒体文件中没有可用的音频（转换结果为空）。请确认录像带有声音。ffmpeg 输出：%s", msg)))
	}
	if err := replaceFile(tmp, out); err != nil {
		os.Remove(tmp)
		return inUseCoded(CodePrepFileInUse, err)
	}
	return nil
}
