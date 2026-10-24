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

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// prepStage converts the uploaded media to 16 kHz mono WAV (SPEC §6.1,
// §17.3), then makes the playback file stream.mp4 (SPEC-v2 P8.1) and hands
// the media's creation_time to the recording-time resolution (P13.3.4).
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
// the input media itself. Only the fatal messages ffmpeg prints when it
// cannot open/demux the input or finds no audio stream count; the same
// wording inside per-packet decoder warnings ("Error while decoding …") or
// any problem with the output file does not.
func classifyFFmpeg(stderr string) int {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.Contains(line, "Error opening output"), strings.Contains(line, "Error while decoding"):
			continue
		case strings.Contains(line, "does not contain any stream"), strings.Contains(line, "matches no streams"):
			return ffmpegNoAudio
		}
	}
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.Contains(line, "Error opening output"), strings.Contains(line, "Error while decoding"):
			continue
		case strings.HasSuffix(line, "Invalid data found when processing input"),
			strings.Contains(line, "moov atom not found"),
			strings.Contains(line, "EBML header parsing failed"):
			return ffmpegCorrupt
		}
	}
	return ffmpegOther
}

// Shares of prep's progress taken by audio.wav when a playback file follows:
// a remux is quick, a transcode takes most of the time (P8.1 "转码耗时较长，
// 计入 prep 的进度").
const (
	audioShareRemux     = 0.8
	audioShareTranscode = 0.2
)

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
	media := resolvePath(l.MediaPath, dir)
	if fi, err := os.Stat(media); l.MediaPath == "" || err != nil || fi.IsDir() {
		return sched.Permanent(model.Errorf(CodePrepMediaMissing, map[string]any{"file": filepath.Base(l.MediaPath)},
			fmt.Errorf("找不到讲座的原始媒体文件（%s）。请重新上传录音或录像", l.MediaPath)))
	}
	tools, err := d.tools(s.media)
	if err != nil {
		return sched.Permanent(model.Errorf(CodePrepFFmpegMissing, map[string]any{"path": d.Cfg.FFmpeg},
			fmt.Errorf("找不到 ffmpeg（config.toml 的 ffmpeg = %q）：%v。请安装 ffmpeg 或在配置中填写 ffmpeg.exe 的完整路径", d.Cfg.FFmpeg, err)))
	}

	// The probe decides the playback file and gives the recording time. It
	// never fails prep: problems of the file itself are reported by the
	// audio conversion below, and the playback file is optional (the
	// player falls back to audio mode).
	info, probeErr := probeMedia(ctx, tools, media)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	audioShare := 1.0
	if probeErr == nil {
		switch planOf(info) {
		case planRemux:
			audioShare = audioShareRemux
		case planTranscode:
			audioShare = audioShareTranscode
		}
	}

	if err := s.convertAudio(ctx, tools, l, dir, media, func(p float64) { progress(p * audioShare) }); err != nil {
		return err
	}
	progress(audioShare)

	// P8.1: stream.mp4. Its hash is not part of asr's input_hash (asr reads
	// audio.wav only).
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

	// P13.3.4 / lead ruling 3: the media's creation_time is a "metadata"
	// candidate for the recording time. Auxiliary information: a failure is
	// logged, prep stays done.
	if probeErr == nil && info.CreationTime != nil {
		if err := ResolveRecordedAt(context.WithoutCancel(ctx), d, lectureID, info.CreationTime); err != nil {
			slog.Warn("prep: resolve recorded_at from creation_time", "lecture", lectureID, "err", err)
		}
	}
	return nil
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

// convertAudio writes audio.wav (SPEC §6.1) through audio.wav.tmp.
func (s *prepStage) convertAudio(ctx context.Context, tools mediaTools, l model.Lecture, dir, media string, progress func(float64)) error {
	out := filepath.Join(dir, model.AudioFile)
	tmp := out + ".tmp"
	os.Remove(tmp)

	args := []string{"-hide_banner", "-nostdin", "-nostats", "-progress", "pipe:1",
		"-y", "-threads", "2", "-i", media, "-vn", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-f", "wav", tmp}
	msg, werr := tools.FFmpeg(ctx, dir, args, progress)
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
	if err := os.Rename(tmp, out); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
