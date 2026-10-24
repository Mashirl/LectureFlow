// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/model"
)

// H.264 encoder choice for the playback file (SPEC-v3 V7 "ffmpeg"). The
// distributed ffmpeg is an LGPL build without libx264, so a transcode to
// H.264 uses, in this order: a hardware encoder (NVIDIA NVENC, Intel Quick
// Sync, Apple VideoToolbox), then OpenH264, else no transcode at all (the
// player runs in audio mode with the reason MediaNoH264Encoder). libx264 is
// never used, not even when a user-supplied ffmpeg has it, so that every
// installation behaves like the distributed one. A remux (-c copy) needs
// no encoder and is not affected.

// MediaNoH264Encoder: a transcode to H.264 is needed but this ffmpeg has no
// usable H.264 encoder. Recorded as the error of a failed stream state
// (the player's audio mode); params.tried lists the encoders checked, the
// detail tells what happened to each.
const MediaNoH264Encoder = "media.no_h264_encoder"

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

// runCombined runs exe and returns stdout and stderr together.
func runCombined(ctx context.Context, exe string, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.WaitDelay = 3 * time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
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
