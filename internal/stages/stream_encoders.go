// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/procutil"
	"lectureflow/internal/sched"
)

// H.264 encoder choice for the playback file (SPEC-v3 V7 "ffmpeg"). The
// distributed ffmpeg is an LGPL build without libx264, so a transcode to
// H.264 uses, in this order: a hardware encoder (NVIDIA NVENC, Intel Quick
// Sync, Apple VideoToolbox), then OpenH264, else no transcode at all (the
// player runs in audio mode with the reason MediaNoH264Encoder). libx264 is
// never used, not even when a user-supplied ffmpeg has it, so that every
// installation behaves like the distributed one. A remux (-c copy) needs
// no encoder and is not affected. An encoder that passes the test encode
// can still fail during a transcode (qsv-retry): a hardware encoder is then
// tried once more, then the next usable one (makeStream).

// MediaNoH264Encoder: a transcode to H.264 is needed but this ffmpeg has no
// usable H.264 encoder. Recorded as the error of a failed stream state
// (the player's audio mode); params.tried lists the encoders checked, the
// detail tells what happened to each.
const MediaNoH264Encoder = "media.no_h264_encoder"

// MediaEncoderFailed: the transcode failed because the H.264 encoder itself
// failed (it could not be opened or rejected a frame: a busy GPU, a driver
// glitch such as QSV's "Invalid FrameType"), with every usable encoder and
// the hardware ones tried twice. Not a problem of the file, and a retry may
// succeed; recorded like the other media.* errors (prep stays done, the
// stream task fails and can be retried). params.tried lists the encoders
// tried, the detail has each attempt's ffmpeg output.
const MediaEncoderFailed = "media.encoder_failed"

// H.264 encoders in order of preference (V7). libx264 is deliberately not
// among them.
const (
	encNVENC        = "h264_nvenc"
	encQSV          = "h264_qsv"
	encVideoToolbox = "h264_videotoolbox"
	encOpenH264     = "libopenh264"
)

var h264Candidates = []string{encNVENC, encQSV, encVideoToolbox, encOpenH264}

// encoderCheck is the result of checking one candidate encoder.
type encoderCheck struct {
	Name   string
	Listed bool   // ffmpeg -encoders lists it
	Err    string // the test encode failed (ffmpeg's output); "" = usable when listed
}

func (c encoderCheck) usable() bool { return c.Listed && c.Err == "" }

// usableEncoders are the names of the usable encoders, in the order of checks.
func usableEncoders(checks []encoderCheck) []string {
	var out []string
	for _, c := range checks {
		if c.usable() {
			out = append(out, c.Name)
		}
	}
	return out
}

// videoEncodeArgs are the ffmpeg output arguments of the video stream for
// encoder enc: quality roughly that of the former libx264 -preset veryfast
// -crf 23, 8-bit 4:2:0 output (what browsers decode) and even dimensions
// (encoders reject odd ones, common in window captures). The test encode
// uses the same arguments, so an ffmpeg that does not know one of the
// options is found out before a lecture is transcoded.
func videoEncodeArgs(enc string) []string {
	var args []string
	pixFmt := "yuv420p"
	switch enc {
	case encNVENC:
		// constant quality (VBR with -cq, no bitrate cap), the middle preset
		args = []string{"-c:v", encNVENC, "-preset", "p4", "-tune", "hq", "-rc", "vbr", "-cq", "23", "-b:v", "0", "-profile:v", "high"}
	case encQSV:
		// ICQ (-global_quality) is QSV's constant-quality mode; its encoder
		// takes NV12, which is 8-bit 4:2:0 as well
		args = []string{"-c:v", encQSV, "-preset", "veryfast", "-global_quality", "23", "-profile:v", "high"}
		pixFmt = "nv12"
	case encVideoToolbox:
		// constant quality (-q:v) is not available on every Mac: an average
		// bitrate ample for lecture slides and camera video at 1080p
		args = []string{"-c:v", encVideoToolbox, "-b:v", "3000k", "-profile:v", "high"}
	case encOpenH264:
		args = []string{"-c:v", encOpenH264, "-b:v", "3000k"}
	default:
		args = []string{"-c:v", enc}
	}
	return append(args, "-pix_fmt", pixFmt, "-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2")
}

// Timeouts of the encoder checks: listing is instant; a hardware encoder's
// test encode opens a session on the device, a second or two at most when
// it works. Variables for the tests.
var (
	encoderListTimeout  = 15 * time.Second
	encoderTrialTimeout = 15 * time.Second
)

// trialArgs encode 0.2 s of a small black picture with enc to the null
// muxer. NVENC and QSV are listed by ffmpeg builds whatever the machine has;
// only opening a session tells whether the driver and device are there.
func trialArgs(enc string) []string {
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=black:s=320x240:r=25:d=0.2"}
	args = append(args, videoEncodeArgs(enc)...)
	return append(args, "-an", "-f", "null", "-")
}

// parseEncoderList reads the names of `ffmpeg -encoders`: lines of the form
// " V....D h264_nvenc   NVIDIA NVENC H.264 encoder (codec h264)" after the
// legend, which ends with a " ------" line.
func parseEncoderList(out string) map[string]bool {
	names := map[string]bool{}
	started := false
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if !started {
			if strings.HasPrefix(f[0], "---") {
				started = true
			}
			continue
		}
		if len(f) >= 2 && len(f[0]) == 6 {
			names[f[1]] = true
		}
	}
	if !started {
		// an ffmpeg without the legend: take every "flags name" line
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) >= 2 && len(f[0]) == 6 && strings.Trim(f[0], "VASFXBD.") == "" {
				names[f[1]] = true
			}
		}
	}
	return names
}

// encoderProber checks the H.264 encoders of an ffmpeg and caches the
// result per ffmpeg (path, size and modification time) for the life of the
// process, so that a lecture does not pay for the checks.
type encoderProber struct {
	// run runs exe with args and returns its combined output.
	run func(ctx context.Context, exe string, args []string) (string, error)
	// stat identifies the ffmpeg binary for the cache key (nil = os.Stat).
	stat func(string) (os.FileInfo, error)

	mu    sync.Mutex // held during a check: concurrent lectures wait for one check
	cache map[string][]encoderCheck
}

// defaultEncoderProber serves execMedia.
var defaultEncoderProber = &encoderProber{run: runCombined}

// runCombined runs exe and returns stdout and stderr together. Like every
// ffmpeg it ends with LectureFlow (procutil.Tied; test-chaos 1).
func runCombined(ctx context.Context, exe string, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.WaitDelay = 3 * time.Second
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := procutil.Run(cmd, procutil.Tied)
	return out.String(), err
}

func (p *encoderProber) key(exe string) string {
	stat := p.stat
	if stat == nil {
		stat = os.Stat
	}
	if fi, err := stat(exe); err == nil {
		return fmt.Sprintf("%s|%d|%d", exe, fi.Size(), fi.ModTime().UnixNano())
	}
	return exe
}

// check returns the checks of h264Candidates for ffmpeg exe, from the cache
// when it has them. An ffmpeg that cannot list its encoders is an error
// (not cached); a cancelled ctx returns ctx.Err() (not cached).
func (p *encoderProber) check(ctx context.Context, exe string) ([]encoderCheck, error) {
	key := p.key(exe)
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.cache[key]; ok {
		return c, nil
	}
	lctx, cancel := context.WithTimeout(ctx, encoderListTimeout)
	out, err := p.run(lctx, exe, []string{"-hide_banner", "-nostdin", "-encoders"})
	cancel()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("ffmpeg -encoders: %v: %s", err, tailText(out))
	}
	listed := parseEncoderList(out)
	checks := make([]encoderCheck, 0, len(h264Candidates))
	for _, name := range h264Candidates {
		c := encoderCheck{Name: name, Listed: listed[name]}
		if c.Listed {
			tctx, cancel := context.WithTimeout(ctx, encoderTrialTimeout)
			out, err := p.run(tctx, exe, trialArgs(name))
			timedOut := errors.Is(tctx.Err(), context.DeadlineExceeded)
			cancel()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			switch {
			case timedOut:
				c.Err = fmt.Sprintf("test encode timed out after %s", encoderTrialTimeout)
			case err != nil:
				c.Err = strings.TrimSpace(fmt.Sprintf("%v: %s", err, tailText(out)))
			}
		}
		checks = append(checks, c)
	}
	if p.cache == nil {
		p.cache = map[string][]encoderCheck{}
	}
	p.cache[key] = checks
	return checks, nil
}

// tailText is the last 600 bytes of s (whole UTF-8 characters), trimmed.
func tailText(s string) string {
	s = strings.TrimSpace(s)
	const n = 600
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && s[i]&0xC0 == 0x80 {
		i++
	}
	return "…" + s[i:]
}

// H264Encoders of the real ffmpeg (mediaTools).
func (m *execMedia) H264Encoders(ctx context.Context) ([]encoderCheck, error) {
	return defaultEncoderProber.check(ctx, m.ffmpeg)
}

// ---------- a failing encoder during the transcode ----------

// hardwareEncoder: an encoder on a GPU or media engine. Its failures can be
// transient (a busy device, sessions used up by other programs, a driver
// glitch), so a transcode tries it a second time before the next encoder.
func hardwareEncoder(enc string) bool {
	return enc == encNVENC || enc == encQSV || enc == encVideoToolbox
}

// encoderAttempts is how often a transcode runs enc when the encoder itself
// fails.
func encoderAttempts(enc string) int {
	if hardwareEncoder(enc) {
		return 2
	}
	return 1
}

// videoEncoderFailures are fftools' messages that the video encoder could
// not be opened or failed on a frame: "Error submitting video frame to the
// encoder" (ffmpeg 6+), "Video encoding failed" (up to 5) / "video encoding
// failed" (7+), "Error while opening encoder - maybe incorrect parameters
// …" (7+; up to 6 "… for output stream #0:0 - …", also inside "Error
// initializing output stream 0:0 -- …").
var videoEncoderFailures = []string{
	"Error submitting video frame to the encoder",
	"Video encoding failed", "video encoding failed",
	"Error while opening encoder",
}

// encoderErrorWords mark a line of the encoder's own that reports a failure
// (its warnings, e.g. QSV's "partial acceleration", do not count).
var encoderErrorWords = regexp.MustCompile(`(?i)error|fail|invalid|cannot|unable|not supported|unsupported`)

// outputStreamRE finds "output stream #0:N" in the messages of ffmpeg up to
// 6, which name the stream instead of tagging the line.
var outputStreamRE = regexp.MustCompile(`output stream #\d+:(\d+)`)

// loggedBy reports whether an ffmpeg output line is logged by encoder enc:
// its codec context "[h264_qsv @ 0x…]", ffmpeg 7+'s encoder "[enc:h264_qsv
// @ …]" or video output stream "[vost#0:0/h264_qsv @ …]". tagged reports
// whether the line has a "[… @ …]" tag at all.
func loggedBy(line, enc string) (by, tagged bool) {
	if !strings.HasPrefix(line, "[") {
		return false, false
	}
	tag, _, ok := strings.Cut(line[1:], " @ ")
	if !ok || strings.Contains(tag, "]") {
		return false, false
	}
	return tag == enc || tag == "enc:"+enc || (strings.HasPrefix(tag, "vost#") && strings.HasSuffix(tag, "/"+enc)), true
}

// encoderFailed reports whether the output of a failed transcode with
// encoder enc shows that the encoder itself failed: one of fftools'
// messages about the video encoder (untagged, or tagged by enc), or an
// error line of enc's own — e.g. QSV's "Invalid FrameType:0.", NVENC's
// "OpenEncodeSessionEx failed", and ffmpeg 7+'s "[vost#0:0/h264_qsv @ …]
// Error encoding a frame: Invalid data found when processing input". The
// encoder is opened only after the input was opened and its first frame
// decoded, so such a failure is not the input file's: its error code
// (AVERROR_INVALIDDATA above) says nothing about the file.
func encoderFailed(stderr, enc string) bool {
	if enc == "" {
		return false
	}
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		by, tagged := loggedBy(line, enc)
		if by && encoderErrorWords.MatchString(line) {
			return true
		}
		if tagged && !by {
			continue // another component's line, e.g. the audio encoder's "[aost#0:1/aac @ …]"
		}
		for _, m := range videoEncoderFailures {
			if !strings.Contains(line, m) {
				continue
			}
			// up to ffmpeg 6: only the video stream (#0:0, streamArgs maps it first)
			if s := outputStreamRE.FindStringSubmatch(line); s != nil && s[1] != "0" {
				continue
			}
			return true
		}
	}
	return false
}

// transcodeError classifies a failed transcode with encoder enc (remux: "").
// A failure of the encoder itself is MediaEncoderFailed (fault true; the
// caller tries again or moves to the next encoder) unless the output could
// not be written (a full disk is no encoder's fault); everything else is
// classified by mediaError (appendix D6) as before.
func transcodeError(stderr string, err error, enc string) (merr error, fault bool) {
	if encoderFailed(stderr, enc) && !outputFailed(stderr) {
		return model.Errorf(MediaEncoderFailed, map[string]any{"tried": enc}, errors.New(ffmpegDetail(stderr, err))), true
	}
	return mediaError(stderr, err), false
}

// encoderTry is a failed transcode attempt.
type encoderTry struct {
	enc     string
	attempt int   // 1, 2 (the second run of a hardware encoder)
	err     error // transcodeError's
	fault   bool  // the encoder itself failed
}

// tryNames are the encoders of tries, each once, in order.
func tryNames(tries []encoderTry) []string {
	var names []string
	for _, t := range tries {
		if !slices.Contains(names, t.enc) {
			names = append(names, t.enc)
		}
	}
	return names
}

// transcodeFailure is the error of a transcode after every usable encoder
// failed (tries in order, at least one). As before (V7 ruling 4) it is the
// last attempt's error; when that is an encoder failure it becomes one
// MediaEncoderFailed for all of them: params.tried = the encoders tried,
// the detail every attempt's ffmpeg output (each the tail of stderr, at
// most stderrTailBytes; the failing line can come well before the end).
func transcodeFailure(tries []encoderTry) error {
	last := tries[len(tries)-1]
	if !last.fault {
		return last.err
	}
	parts := make([]string, 0, len(tries))
	for _, t := range tries {
		parts = append(parts, fmt.Sprintf("%s (attempt %d): %s", t.enc, t.attempt, errDetail(t.err)))
	}
	return model.Errorf(MediaEncoderFailed, map[string]any{"tried": strings.Join(tryNames(tries), ", ")},
		errors.New("the H.264 encoder failed during the transcode (not a problem of the file)\n"+strings.Join(parts, "\n\n")))
}

// errDetail is the wrapped error's text of a CodedError (its "查看详情"), or
// err's text.
func errDetail(err error) string {
	var ce *model.CodedError
	if errors.As(err, &ce) && ce.Err != nil {
		return ce.Err.Error()
	}
	return err.Error()
}

// noteEncoderRetried records on the job (prep or the stream task) that the
// playback file was made only after the encoder failed: failed lists the
// encoders that failed (tryNames), enc made the file.
func noteEncoderRetried(ctx context.Context, failed []string, enc string) {
	names := strings.Join(failed, ", ")
	sched.UpdateStats(ctx, func(st *model.JobStats) {
		st.Note = fmt.Sprintf(EncoderRetriedNote, names, enc)
		st.NoteInfo = noteInfo(part(NotePrepEncoderRetried, map[string]any{"failed": names, "encoder": enc}))
	})
}

// EncoderRetriedNote is the Chinese text of NotePrepEncoderRetried.
const EncoderRetriedNote = "转码时视频编码器 %s 出错，已用 %s 重新生成播放文件"

// noEncoderState is the stream state when no encoder can transcode: failed
// with MediaNoH264Encoder (the player's audio mode; GET /media answers the
// code and the user may retry from the lecture page).
func noEncoderState(checks []encoderCheck) model.StreamState {
	names := make([]string, 0, len(checks))
	lines := make([]string, 0, len(checks))
	for _, c := range checks {
		names = append(names, c.Name)
		switch {
		case !c.Listed:
			lines = append(lines, c.Name+": not available in this ffmpeg")
		case c.Err != "":
			lines = append(lines, c.Name+": test encode failed: "+c.Err)
		default:
			lines = append(lines, c.Name+": usable") // not reached: then it would have been used
		}
	}
	info := model.ErrorInfo{
		Code:   MediaNoH264Encoder,
		Params: map[string]any{"tried": strings.Join(names, ", ")},
		Detail: "no usable H.264 encoder (libx264 is never used)\n" + strings.Join(lines, "\n"),
	}
	b, _ := json.Marshal(info)
	return model.StreamState{Status: StreamFailed, Error: string(b)}
}
