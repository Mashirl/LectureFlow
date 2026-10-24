// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// The playback file of SPEC-v2 P8.1: stream.mp4 in the lecture directory,
// browser-playable MP4 (H.264 + AAC). prep makes it right after audio.wav;
// the auxiliary task "stream" makes it for lectures prepared before v2.
// Its state is kv stream:{lecture} (model.StreamState, lead ruling 7 of
// phase 2), which records outcomes only: while a stream job is queued or
// running the API shows "generating", and no kv entry means "pending".

// Stream states written to kv stream:{lecture} (api.StreamInfo.Status).
const (
	StreamReady     = "ready"      // stream.mp4 exists
	StreamAudioOnly = "audio_only" // the source has no video: the player runs in audio mode
	StreamNoSpace   = "no_space"   // appendix D6: under StreamMinFreeBytes free, not generated
	StreamFailed    = "failed"     // StreamState.Error is the JSON of a model.ErrorInfo
)

// StreamMinFreeBytes is the free space appendix D6 requires on the lecture
// directory's volume before a playback file is generated.
const StreamMinFreeBytes = 10 << 30

// Error codes of the playback file (appendix D6; P14.3 "errors.{code}").
// The ffmpeg output is the error's detail.
const (
	MediaCorrupt         = "media.corrupt"          // the file is incomplete or damaged
	MediaUnsupported     = "media.unsupported"      // a codec or format ffmpeg cannot handle
	MediaNoAudio         = "media.no_audio"         // the file has no audio track
	MediaFailed          = "media.failed"           // anything else ("处理这个文件时出错")
	MediaOriginalMissing = "media.original_missing" // the stream task found no original file (deleted after prep)
)

// mediaTools runs the external programs of prep and of the playback file
// and answers the free-space question of appendix D6. The stages use
// execMedia; tests inject fakes to check arguments, states and error
// classes without ffmpeg.
type mediaTools interface {
	// FFmpeg runs ffmpeg with args in dir. progress gets the fraction of
	// the input duration processed (from -progress pipe:1 and the
	// "Duration:" line), 1 at the end. It returns the tail of stderr; err
	// is the process error (*ffmpegStartError when it could not start).
	FFmpeg(ctx context.Context, dir string, args []string, progress func(float64)) (stderr string, err error)
	// Probe returns ffprobe's JSON for media (-show_format -show_streams)
	// and the tail of its stderr.
	Probe(ctx context.Context, media string) (out []byte, stderr string, err error)
	// FreeSpace returns the bytes available to this user on dir's volume.
	FreeSpace(dir string) (int64, error)
	// H264Encoders checks the H.264 encoders of SPEC-v3 V7 (h264Candidates,
	// in that order) on this ffmpeg; cached per ffmpeg for the process.
	H264Encoders(ctx context.Context) ([]encoderCheck, error)
}

// ffmpegStartError: ffmpeg (or ffprobe) could not be started at all.
type ffmpegStartError struct {
	exe string
	err error
}

func (e *ffmpegStartError) Error() string {
	// The name without its directory, whichever separator the path uses
	// (a Windows path stays one element for filepath.Base on macOS).
	name := e.exe[strings.LastIndexAny(e.exe, `/\`)+1:]
	return fmt.Sprintf("无法启动 %s（%s）：%v", strings.TrimSuffix(name, filepath.Ext(name)), e.exe, e.err)
}
func (e *ffmpegStartError) Unwrap() error { return e.err }

// execMedia runs the real programs. ffprobe is looked for next to the
// configured ffmpeg (the official builds ship both in one directory), then
// in PATH.
type execMedia struct {
	ffmpeg string // resolved path
}

// tools returns the injected tools, or execMedia for the configured ffmpeg
// (error: ffmpeg not found).
func (d Deps) tools(injected mediaTools) (mediaTools, error) {
	if injected != nil {
		return injected, nil
	}
	ffmpeg, err := executable(d.Cfg.FFmpeg)
	if err != nil {
		return nil, err
	}
	return &execMedia{ffmpeg: ffmpeg}, nil
}

var durationRE = regexp.MustCompile(`Duration: (\d+):(\d{2}):(\d{2}(?:\.\d+)?)`)

func (m *execMedia) FFmpeg(ctx context.Context, dir string, args []string, progress func(float64)) (string, error) {
	cmd := exec.CommandContext(ctx, m.ffmpeg, args...)
	cmd.Dir = dir
	cmd.WaitDelay = 3 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", &ffmpegStartError{exe: m.ffmpeg, err: err}
	}
	var (
		mu       sync.Mutex
		duration float64 // seconds, from stderr
	)
	tail := &tailBuffer{n: stderrTailBytes}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanLines(stderr, func(line string) {
			_, _ = io.WriteString(tail, line+"\n")
			if m := durationRE.FindStringSubmatch(line); m != nil {
				h, _ := strconv.Atoi(m[1])
				mi, _ := strconv.Atoi(m[2])
				sec, _ := strconv.ParseFloat(m[3], 64)
				mu.Lock()
				if duration == 0 {
					duration = float64(h*3600+mi*60) + sec
				}
				mu.Unlock()
			}
		})
	}()
	scanLines(stdout, func(line string) {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return
		}
		switch k {
		case "out_time_us":
			us, err := strconv.ParseFloat(v, 64)
			mu.Lock()
			total := duration
			mu.Unlock()
			if err == nil && total > 0 {
				progress(min(0.99, us/1e6/total))
			}
		case "progress":
			if v == "end" {
				progress(1)
			}
		}
	})
	wg.Wait()
	werr := cmd.Wait()
	return tail.String(), werr
}

// ffprobeFor returns the ffprobe next to ffmpeg ("ffmpeg" in the file name
// replaced by "ffprobe"), else the one in PATH.
func ffprobeFor(ffmpeg string) (string, error) {
	dir, base := filepath.Split(ffmpeg)
	ext := filepath.Ext(base)
	if name := strings.TrimSuffix(base, ext); strings.EqualFold(name, "ffmpeg") {
		p := filepath.Join(dir, "ffprobe"+ext)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	p, err := exec.LookPath("ffprobe")
	if err != nil {
		return "", fmt.Errorf("找不到 ffprobe（应与 ffmpeg 位于同一目录：%s）：%w", ffmpeg, err)
	}
	return p, nil
}

func (m *execMedia) Probe(ctx context.Context, media string) ([]byte, string, error) {
	ffprobe, err := ffprobeFor(m.ffmpeg)
	if err != nil {
		return nil, "", err
	}
	cmd := exec.CommandContext(ctx, ffprobe, "-v", "error", "-print_format", "json", "-show_format", "-show_streams", media)
	cmd.WaitDelay = 3 * time.Second
	tail := &tailBuffer{n: stderrTailBytes}
	cmd.Stderr = tail
	out, err := cmd.Output()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return nil, tail.String(), &ffmpegStartError{exe: ffprobe, err: err}
	}
	return out, tail.String(), err
}

func (m *execMedia) FreeSpace(dir string) (int64, error) { return diskFree(dir) }

// ---------- probing ----------

// mediaInfo is what the playback-file step needs to know about a media file.
type mediaInfo struct {
	Format       string       // ffprobe format_name, e.g. "mov,mp4,m4a,3gp,3g2,mj2"
	Video        *mediaStream // the first video stream that is not a cover picture; nil = none
	Audio        *mediaStream // the first audio stream; nil = none
	CreationTime *time.Time   // the container's (or a stream's) creation_time, UTC; nil = unknown
}

type mediaStream struct {
	Index  int    // ffprobe stream index (the -map 0:{index} of ffmpeg)
	Codec  string // codec_name, e.g. h264, hevc, aac
	PixFmt string // pix_fmt of a video stream, e.g. yuv420p
}

// probeJSON is the part of ffprobe's JSON that is read.
type probeJSON struct {
	Streams []struct {
		Index       int               `json:"index"`
		CodecType   string            `json:"codec_type"`
		CodecName   string            `json:"codec_name"`
		PixFmt      string            `json:"pix_fmt"`
		Disposition map[string]int    `json:"disposition"`
		Tags        map[string]string `json:"tags"`
	} `json:"streams"`
	Format struct {
		FormatName string            `json:"format_name"`
		Tags       map[string]string `json:"tags"`
	} `json:"format"`
}

// parseProbe reads ffprobe's JSON.
func parseProbe(raw []byte) (mediaInfo, error) {
	var p probeJSON
	if err := json.Unmarshal(raw, &p); err != nil {
		return mediaInfo{}, fmt.Errorf("ffprobe 的输出无法解析：%w", err)
	}
	info := mediaInfo{Format: p.Format.FormatName}
	info.CreationTime = creationTime(p.Format.Tags)
	for _, s := range p.Streams {
		switch {
		case s.CodecType == "video" && info.Video == nil && s.Disposition["attached_pic"] == 0:
			info.Video = &mediaStream{Index: s.Index, Codec: s.CodecName, PixFmt: s.PixFmt}
		case s.CodecType == "audio" && info.Audio == nil:
			info.Audio = &mediaStream{Index: s.Index, Codec: s.CodecName}
		}
		if info.CreationTime == nil {
			info.CreationTime = creationTime(s.Tags)
		}
	}
	return info, nil
}

// creationTime reads the creation_time tag (any letter case). Values before
// 2000 are muxer defaults (the MP4 epoch 1904, the Unix epoch), not a
// recording time, and count as unknown.
func creationTime(tags map[string]string) *time.Time {
	for k, v := range tags {
		if !strings.EqualFold(k, "creation_time") {
			continue
		}
		v = strings.TrimSpace(v)
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
			if t, err := time.Parse(layout, v); err == nil {
				if t.Year() < 2000 {
					return nil
				}
				t = t.UTC()
				return &t
			}
		}
	}
	return nil
}

// probeMedia runs ffprobe and parses it; errors are CodedErrors (D6).
func probeMedia(ctx context.Context, tools mediaTools, media string) (mediaInfo, error) {
	out, stderr, err := tools.Probe(ctx, media)
	if err != nil {
		if ctx.Err() != nil {
			return mediaInfo{}, ctx.Err()
		}
		return mediaInfo{}, mediaError(stderr, err)
	}
	info, err := parseProbe(out)
	if err != nil {
		return mediaInfo{}, model.Errorf(MediaFailed, nil, err)
	}
	return info, nil
}

// ---------- generating stream.mp4 ----------

// browserReady reports whether the media can be remuxed without re-encoding
// (P8.1 "MP4 容器、H.264 视频、AAC 音频"): an MP4-family container, H.264
// video in 8-bit 4:2:0 (what browsers decode; High 10 or 4:4:4 H.264 is not
// playable) and AAC audio.
func browserReady(info mediaInfo) bool {
	mp4 := false
	for _, f := range strings.Split(info.Format, ",") {
		if strings.TrimSpace(f) == "mp4" {
			mp4 = true
		}
	}
	v, a := info.Video, info.Audio
	return mp4 && v != nil && a != nil && v.Codec == "h264" &&
		(v.PixFmt == "" || v.PixFmt == "yuv420p" || v.PixFmt == "yuvj420p") && a.Codec == "aac"
}

// streamArgs are the ffmpeg arguments that write out from media: a remux
// (-c copy, P8.1) when enc is "", else a transcode to H.264 with encoder
// enc (SPEC-v3 V7, videoEncodeArgs: 8-bit 4:2:0, even dimensions) + AAC.
// Both map exactly the probed video and audio stream, use -threads 2
// (SPEC §17.3) and put the index first (+faststart) so seeking plays at
// once.
func streamArgs(info mediaInfo, media, out, enc string) []string {
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-progress", "pipe:1", "-y", "-threads", "2", "-i", media,
		"-map", "0:" + strconv.Itoa(info.Video.Index), "-map", "0:" + strconv.Itoa(info.Audio.Index)}
	if enc == "" {
		args = append(args, "-c", "copy")
	} else {
		args = append(args, videoEncodeArgs(enc)...)
		args = append(args, "-c:a", "aac", "-b:a", "128k", "-threads", "2")
	}
	return append(args, "-movflags", "+faststart", "-f", "mp4", out)
}

// streamPlan tells how the playback file will be made, for weighting the
// progress of prep: no file, a quick remux, or a long transcode.
type streamPlan int

const (
	planNone streamPlan = iota
	planRemux
	planTranscode
)

func planOf(info mediaInfo) streamPlan {
	switch {
	case info.Video == nil || info.Audio == nil:
		return planNone
	case browserReady(info):
		return planRemux
	}
	return planTranscode
}

// makeStream makes dir/stream.mp4 from media (P8.1, appendix D6) and
// returns the state to record: audio_only (no video; a stale stream.mp4 is
// removed), no_space, failed with MediaNoH264Encoder (a transcode is needed
// and no encoder of SPEC-v3 V7 is usable; the player runs in audio mode,
// as with no_space), or ready. Failures are CodedErrors (media.*); a
// cancelled ctx returns ctx.Err(). The file is written as stream.mp4.tmp and
// renamed, so a partial file is never served.
func makeStream(ctx context.Context, tools mediaTools, dir, media string, info mediaInfo, progress func(float64)) (model.StreamState, error) {
	out := filepath.Join(dir, model.StreamFile)
	tmp := out + ".tmp"
	if info.Video == nil {
		if err := os.Remove(out); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("stream: remove stale stream.mp4", "dir", dir, "err", err)
		}
		return model.StreamState{Status: StreamAudioOnly}, nil
	}
	if info.Audio == nil {
		return model.StreamState{}, sched.Permanent(model.Errorf(MediaNoAudio, nil, errors.New("媒体文件中没有音轨")))
	}
	free, err := tools.FreeSpace(dir)
	switch {
	case err != nil:
		slog.Warn("stream: free space unknown, generating anyway", "dir", dir, "err", err)
	case free < StreamMinFreeBytes:
		return model.StreamState{Status: StreamNoSpace}, nil
	}
	remux := browserReady(info)
	encoders := []string{""} // "" = remux
	if !remux {
		checks, err := tools.H264Encoders(ctx)
		if ctx.Err() != nil {
			return model.StreamState{}, ctx.Err()
		}
		if err != nil {
			return model.StreamState{}, model.Errorf(MediaFailed, nil, fmt.Errorf("checking the H.264 encoders: %w", err))
		}
		if encoders = usableEncoders(checks); len(encoders) == 0 {
			slog.Warn("stream: no usable H.264 encoder, playback file not generated (audio mode)", "dir", dir, "checks", checks)
			return noEncoderState(checks), nil
		}
	}
	// A transcode falls back to the next usable encoder when one fails for
	// a reason that is not the file's (a hardware encoder can pass the test
	// and still refuse a size or run out of sessions). The progress never
	// goes back.
	var done float64
	mono := func(p float64) {
		if p > done {
			done = p
			progress(p)
		}
	}
	var stderr string
	for i, enc := range encoders {
		os.Remove(tmp)
		stderr, err = tools.FFmpeg(ctx, dir, streamArgs(info, media, tmp, enc), mono)
		if ctx.Err() != nil {
			os.Remove(tmp)
			return model.StreamState{}, ctx.Err()
		}
		if err == nil {
			if enc != "" {
				slog.Info("stream: transcoded", "dir", dir, "encoder", enc)
			}
			break
		}
		os.Remove(tmp)
		merr := mediaError(stderr, err)
		if enc == "" || i+1 == len(encoders) || sched.IsPermanent(merr) {
			return model.StreamState{}, merr
		}
		slog.Warn("stream: encoder failed, trying the next one", "dir", dir, "encoder", enc, "next", encoders[i+1], "err", merr)
	}
	fi, err := os.Stat(tmp)
	if err != nil || fi.Size() == 0 {
		os.Remove(tmp)
		return model.StreamState{}, model.Errorf(MediaFailed, nil, fmt.Errorf("ffmpeg 退出码为 0 但没有写出 %s：%v。ffmpeg 输出：%s", tmp, err, stderr))
	}
	if err := os.Rename(tmp, out); err != nil {
		os.Remove(tmp)
		return model.StreamState{}, model.Errorf(MediaFailed, nil, err)
	}
	progress(1)
	return model.StreamState{Status: StreamReady, Transcoded: !remux, SizeBytes: fi.Size()}, nil
}

// unsupportedMarkers are fatal ffmpeg/ffprobe messages about a codec or
// format that cannot be handled.
var unsupportedMarkers = []string{
	"Unknown decoder", "Decoder (codec", "Unsupported codec", "unsupported codec",
	"codec not currently supported in container", "Could not find codec parameters",
}

// outputFailureMarkers are ffmpeg messages that a write failed; with one of
// them the failure is not blamed on the input file, whatever warnings about
// the input's streams precede it (a full disk is not "格式不支持").
var outputFailureMarkers = []string{"No space left on device"}

// mediaError classifies a failed ffmpeg/ffprobe run (appendix D6) into a
// CodedError whose wrapped error carries the raw output ("查看详情").
// Problems of the file itself are permanent. prep's audio conversion and
// the playback file share it (lead ruling at the phase-2 gate).
func mediaError(stderr string, err error) error {
	detail := stderr
	if detail == "" {
		detail = err.Error()
	} else {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			detail = fmt.Sprintf("退出码 %d：%s", ee.ExitCode(), stderr)
		} else {
			detail = fmt.Sprintf("%v：%s", err, stderr)
		}
	}
	var se *ffmpegStartError
	if errors.As(err, &se) {
		return model.Errorf(MediaFailed, nil, err)
	}
	cause := errors.New(detail)
	for _, m := range outputFailureMarkers {
		if strings.Contains(stderr, m) {
			return model.Errorf(MediaFailed, nil, cause)
		}
	}
	switch classifyFFmpeg(stderr) {
	case ffmpegNoAudio:
		return sched.Permanent(model.Errorf(MediaNoAudio, nil, cause))
	case ffmpegCorrupt:
		return sched.Permanent(model.Errorf(MediaCorrupt, nil, cause))
	}
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, "Error opening output") {
			continue
		}
		for _, m := range unsupportedMarkers {
			if strings.Contains(line, m) {
				return sched.Permanent(model.Errorf(MediaUnsupported, nil, cause))
			}
		}
	}
	return model.Errorf(MediaFailed, nil, cause)
}

// errorInfoJSON is the language-neutral form of err for StreamState.Error:
// the JSON of a model.ErrorInfo, Detail = the wrapped error's text (the
// same convention as job.error of a CodedError).
func errorInfoJSON(err error) string {
	info := model.ErrorInfo{Code: MediaFailed, Detail: err.Error()}
	var ce *model.CodedError
	if errors.As(err, &ce) {
		info.Code, info.Params = ce.Info.Code, ce.Info.Params
		if ce.Err != nil {
			info.Detail = ce.Err.Error()
		} else {
			info.Detail = ""
		}
	}
	b, _ := json.Marshal(info)
	return string(b)
}

// setStreamState records the playback file's state (kv stream:{lecture})
// and tells the UI. A failure to record is logged: the file itself is what
// GET /media serves.
func (d Deps) setStreamState(ctx context.Context, lectureID string, st model.StreamState) {
	st.UpdatedAt = d.now().UTC().Format(time.RFC3339)
	b, _ := json.Marshal(st)
	if err := d.Store.SetKV(context.WithoutCancel(ctx), model.KVStreamPrefix+lectureID, string(b)); err != nil {
		slog.Warn("stream: record state", "lecture", lectureID, "err", err)
		return
	}
	d.events().Changed("lecture", lectureID)
}

// StreamStateOf reads kv stream:{lecture}; ok is false when nothing was
// recorded (pending, or a lecture prepared before v2).
func StreamStateOf(ctx context.Context, st db.Store, lectureID string) (model.StreamState, bool, error) {
	raw, ok, err := st.GetKV(ctx, model.KVStreamPrefix+lectureID)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return model.StreamState{}, false, err
	}
	var s model.StreamState
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return model.StreamState{}, false, fmt.Errorf("kv %s%s: %w", model.KVStreamPrefix, lectureID, err)
	}
	return s, true, nil
}

// ---------- the auxiliary task ----------

// streamStage is the auxiliary task "stream" (P8.1, lead ruling 1 of phase
// 2): it makes stream.mp4 for a lecture prepared before v2, from its
// original media, by the rules of prep. It is queued by GET /media; a
// failure is not retried automatically.
type streamStage struct {
	d     Deps
	media mediaTools // nil = execMedia
}

func (s *streamStage) Name() string                 { return model.StageStream }
func (s *streamStage) Resource() sched.ResourceKind { return sched.ResourceCPU }

// InputHash: an auxiliary task has none.
func (s *streamStage) InputHash(context.Context, string) (string, error) { return "", nil }

func (s *streamStage) Run(ctx context.Context, lectureID string, progress func(float64)) error {
	d := s.d
	l, err := d.Store.GetLecture(ctx, lectureID)
	if err != nil {
		return err
	}
	dir, err := filepath.Abs(d.lectureDir(l))
	if err != nil {
		return err
	}
	fail := func(err error) error {
		d.setStreamState(ctx, lectureID, model.StreamState{Status: StreamFailed, Error: errorInfoJSON(err)})
		return err
	}
	media := resolvePath(l.MediaPath, dir)
	if fi, err := os.Stat(media); l.MediaPath == "" || err != nil || fi.IsDir() {
		return fail(sched.Permanent(model.Errorf(MediaOriginalMissing, nil,
			fmt.Errorf("讲座的原始媒体文件不存在（%s），无法生成播放文件；可能已按 keep_original_media = false 在预处理后删除", l.MediaPath))))
	}
	tools, err := d.tools(s.media)
	if err != nil {
		return fail(sched.Permanent(model.Errorf(MediaFailed, nil, fmt.Errorf("找不到 ffmpeg（%s）：%w", d.Cfg.FFmpeg, err))))
	}
	info, err := probeMedia(ctx, tools, media)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fail(err)
	}
	st, err := makeStream(ctx, tools, dir, media, info, progress)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fail(err)
	}
	d.setStreamState(ctx, lectureID, st)
	// A prep that kept the original for this (lead ruling on
	// keep_original_media) deletes it now that the playback file exists.
	if !d.keepOriginalMedia() {
		removeOriginal(ctx, d, lectureID, media, dir, st.Status)
	}
	progress(1)
	return nil
}
