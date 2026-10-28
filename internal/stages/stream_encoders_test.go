// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// ---------- SPEC-v3 V7: choosing the H.264 encoder ----------

// encoderListing is a typical `ffmpeg -hide_banner -encoders` with the
// given H.264 encoders (libx264 always listed: a GPL build of a user).
func encoderListing(names ...string) string {
	var b strings.Builder
	b.WriteString("Encoders:\n V..... = Video\n A..... = Audio\n S..... = Subtitle\n .F.... = Frame-level multithreading\n" +
		" ..S... = Slice-level multithreading\n ...X.. = Codec is experimental\n ....B. = Supports draw_horiz_band\n" +
		" .....D = Supports direct rendering method 1\n ------\n" +
		" V....D a64multi             Multicolor charset for Commodore 64 (codec a64_multi)\n" +
		" V....D libx264              libx264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10 (codec h264)\n")
	for _, n := range names {
		b.WriteString(" V....D " + n + "           some H.264 encoder (codec h264)\n")
	}
	b.WriteString(" A....D aac                  AAC (Advanced Audio Coding)\n")
	return b.String()
}

// fakeFFmpegRun stands in for ffmpeg in encoderProber: it lists listed and
// fails the test encodes of failing (a "hang" value blocks until the
// timeout). It records every call.
type fakeFFmpegRun struct {
	mu      sync.Mutex
	listed  []string
	failing map[string]string // encoder → ffmpeg output of a failed test encode, or "hang"
	listErr error
	calls   [][]string
	exes    []string
	onTrial func(enc string) // optional hook
}

func (f *fakeFFmpegRun) run(ctx context.Context, exe string, args []string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, slices.Clone(args))
	f.exes = append(f.exes, exe)
	f.mu.Unlock()
	if slices.Contains(args, "-encoders") {
		if f.listErr != nil {
			return "cannot run", f.listErr
		}
		return encoderListing(f.listed...), nil
	}
	enc := args[slices.Index(args, "-c:v")+1]
	if f.onTrial != nil {
		f.onTrial(enc)
	}
	switch out := f.failing[enc]; out {
	case "":
		return "", nil
	case "hang":
		<-ctx.Done()
		return "", ctx.Err()
	default:
		return out, errors.New("exit status 1")
	}
}

func (f *fakeFFmpegRun) trials() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if i := slices.Index(c, "-c:v"); i >= 0 {
			out = append(out, c[i+1])
		}
	}
	return out
}

func TestParseEncoderList(t *testing.T) {
	got := parseEncoderList(encoderListing(encNVENC, encOpenH264))
	for _, n := range []string{"a64multi", "libx264", encNVENC, encOpenH264, "aac"} {
		if !got[n] {
			t.Errorf("%s not parsed", n)
		}
	}
	for _, n := range []string{"=", encQSV, "Encoders:"} {
		if got[n] {
			t.Errorf("%q must not be an encoder", n)
		}
	}
}

// TestH264CandidatesNeverLibx264: libx264 is not a candidate, and no
// encoder's arguments mention it (V7: even when the user's ffmpeg has it).
func TestH264CandidatesNeverLibx264(t *testing.T) {
	if !slices.Equal(h264Candidates, []string{"h264_nvenc", "h264_qsv", "h264_videotoolbox", "libopenh264"}) {
		t.Errorf("candidates %v: hardware (NVENC, QSV, VideoToolbox) > OpenH264", h264Candidates)
	}
	for _, enc := range h264Candidates {
		args := videoEncodeArgs(enc)
		if slices.Contains(args, "libx264") || !argsContain(args, "-c:v", enc) || !argsContain(args, "-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2") {
			t.Errorf("%s: %v", enc, args)
		}
		pix := "yuv420p"
		if enc == encQSV {
			pix = "nv12" // 8-bit 4:2:0 too
		}
		if !argsContain(args, "-pix_fmt", pix) {
			t.Errorf("%s: %v lacks -pix_fmt %s", enc, args, pix)
		}
		ta := trialArgs(enc)
		if !argsContain(ta, "-f", "null", "-") || !argsContain(ta, "-c:v", enc) || !slices.Contains(ta, "-an") {
			t.Errorf("trial args %v", ta)
		}
	}
}

// TestEncoderChoice: the listed / test-encode combinations choose the
// encoder of V7's order; only listed encoders are test-encoded; libx264
// (listed in every case) is never chosen.
func TestEncoderChoice(t *testing.T) {
	old := encoderTrialTimeout
	encoderTrialTimeout = 50 * time.Millisecond
	defer func() { encoderTrialTimeout = old }()
	cases := []struct {
		name    string
		listed  []string
		failing map[string]string
		usable  []string // in order; [0] is the choice
		trials  []string
	}{
		{"all usable", []string{encOpenH264, encVideoToolbox, encQSV, encNVENC}, nil,
			[]string{encNVENC, encQSV, encVideoToolbox, encOpenH264}, []string{encNVENC, encQSV, encVideoToolbox, encOpenH264}},
		{"nvenc listed but no device", []string{encNVENC, encQSV}, map[string]string{encNVENC: "[h264_nvenc @ 0] No capable devices found"},
			[]string{encQSV}, []string{encNVENC, encQSV}},
		{"hardware fails, openh264", []string{encNVENC, encQSV, encOpenH264},
			map[string]string{encNVENC: "Cannot load nvcuda.dll", encQSV: "Error initializing an internal MFX session: unsupported (-3)"},
			[]string{encOpenH264}, []string{encNVENC, encQSV, encOpenH264}},
		{"videotoolbox only", []string{encVideoToolbox}, nil, []string{encVideoToolbox}, []string{encVideoToolbox}},
		{"openh264 only", []string{encOpenH264}, nil, []string{encOpenH264}, []string{encOpenH264}},
		{"trial hangs", []string{encNVENC, encOpenH264}, map[string]string{encNVENC: "hang"}, []string{encOpenH264}, []string{encNVENC, encOpenH264}},
		{"only libx264", nil, nil, nil, nil},
		{"all fail", []string{encNVENC, encOpenH264}, map[string]string{encNVENC: "x", encOpenH264: "Failed to load libopenh264"}, nil, []string{encNVENC, encOpenH264}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeFFmpegRun{listed: c.listed, failing: c.failing}
			p := &encoderProber{run: f.run}
			checks, err := p.check(context.Background(), `C:\ff\ffmpeg.exe`)
			if err != nil {
				t.Fatal(err)
			}
			if got := usableEncoders(checks); !slices.Equal(got, c.usable) {
				t.Errorf("usable %v, want %v (checks %+v)", got, c.usable, checks)
			}
			if got := f.trials(); !slices.Equal(got, c.trials) {
				t.Errorf("test encodes %v, want %v", got, c.trials)
			}
			for _, ch := range checks {
				if ch.Name == "libx264" {
					t.Error("libx264 checked")
				}
				if out := c.failing[ch.Name]; out != "" && ch.Listed && (ch.Err == "" || (out != "hang" && !strings.Contains(ch.Err, out))) {
					t.Errorf("%s: Err %q, want the ffmpeg output %q", ch.Name, ch.Err, out)
				}
			}
			if c.name == "trial hangs" && !strings.Contains(checks[0].Err, "timed out") {
				t.Errorf("hang: %+v", checks[0])
			}
		})
	}
}

// TestEncoderCache: the checks run once per ffmpeg; another ffmpeg is
// checked by itself; a failed listing and a cancelled check are not
// cached.
func TestEncoderCache(t *testing.T) {
	f := &fakeFFmpegRun{listed: []string{encNVENC, encOpenH264}}
	p := &encoderProber{run: f.run}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if c, err := p.check(ctx, `C:\a\ffmpeg.exe`); err != nil || usableEncoders(c)[0] != encNVENC {
			t.Fatalf("check %d: %v %v", i, c, err)
		}
	}
	if n := len(f.calls); n != 3 { // -encoders + two test encodes
		t.Errorf("%d ffmpeg runs for three checks of one ffmpeg, want 3", n)
	}
	if _, err := p.check(ctx, `C:\b\ffmpeg.exe`); err != nil || len(f.calls) != 6 || f.exes[5] != `C:\b\ffmpeg.exe` {
		t.Errorf("another ffmpeg: %v, runs %d", err, len(f.calls))
	}

	// a listing failure is an error and is tried again next time
	f2 := &fakeFFmpegRun{listErr: errors.New("exit status 0xc0000135")}
	p2 := &encoderProber{run: f2.run}
	if _, err := p2.check(ctx, `C:\c\ffmpeg.exe`); err == nil || !strings.Contains(err.Error(), "cannot run") {
		t.Errorf("listing failure: %v", err)
	}
	f2.listErr = nil
	f2.listed = []string{encOpenH264}
	if c, err := p2.check(ctx, `C:\c\ffmpeg.exe`); err != nil || !slices.Equal(usableEncoders(c), []string{encOpenH264}) {
		t.Errorf("after a listing failure: %v %v", c, err)
	}

	// cancelled during a test encode: ctx.Err(), nothing cached
	cctx, cancel := context.WithCancel(ctx)
	f3 := &fakeFFmpegRun{listed: []string{encNVENC}, onTrial: func(string) { cancel() }}
	p3 := &encoderProber{run: f3.run}
	if _, err := p3.check(cctx, `C:\d\ffmpeg.exe`); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	f3.onTrial = nil
	if c, err := p3.check(ctx, `C:\d\ffmpeg.exe`); err != nil || len(usableEncoders(c)) != 1 || len(f3.calls) != 4 {
		t.Errorf("after cancel: %v %v, runs %d", c, err, len(f3.calls))
	}
}

// TestStreamEncoderSelection: makeStream (the stream task, also when prep
// queued it; stream-parallel) transcodes with the first usable encoder, falls back to the next when it
// fails for a reason that is not the file's, records audio mode with
// media.no_h264_encoder when none is usable, and never uses libx264. A
// remux checks no encoder.
func TestStreamEncoderSelection(t *testing.T) {
	e := newEnv(t)
	noLibx264 := func(t *testing.T, fm *fakeMedia) {
		t.Helper()
		for _, c := range fm.ffmpegCalls() {
			if slices.Contains(c, "libx264") {
				t.Errorf("libx264 in %v", c)
			}
		}
	}
	nvencFails := func(args []string) (string, error) {
		if slices.Contains(args, encNVENC) {
			return "[h264_nvenc @ 0] OpenEncodeSessionEx failed: out of memory (10)\nConversion failed!", errors.New("exit status 1")
		}
		return "", nil
	}

	t.Run("openh264 after a failed hardware check", func(t *testing.T) {
		l, _ := e.mediaLecture("oh")
		fm := &fakeMedia{probe: probeHEVC, encoders: []encoderCheck{{Name: encNVENC, Listed: true, Err: "no device"}, {Name: encQSV}, {Name: encVideoToolbox}, {Name: encOpenH264, Listed: true}}}
		if _, r := e.prepThenStream(fm, l.ID); r.err != nil {
			t.Fatal(r.err)
		}
		sc := fm.streamCalls()
		if len(sc) != 1 || !argsContain(sc[0], "-c:v", encOpenH264) || !argsContain(sc[0], "-c:a", "aac") || !argsContain(sc[0], "-movflags", "+faststart") {
			t.Errorf("stream calls %v", sc)
		}
		if st := e.streamState(l.ID); st.Status != StreamReady || !st.Transcoded {
			t.Errorf("state %+v", st)
		}
		noLibx264(t, fm)
	})

	t.Run("fallback to the next encoder", func(t *testing.T) {
		l, _ := e.mediaLecture("fb")
		e.enqueueStream(l.ID)
		fm := &fakeMedia{probe: probeHEVC, fail: nvencFails,
			encoders: []encoderCheck{{Name: encNVENC, Listed: true}, {Name: encQSV}, {Name: encVideoToolbox}, {Name: encOpenH264, Listed: true}}}
		r := e.run(&streamStage{d: e.deps(nil), media: fm}, l.ID)
		if r.err != nil || r.job.Status != model.StatusDone {
			t.Fatalf("stream %v %s", r.err, r.job.Status)
		}
		// NVENC's own failure: run once more (qsv-retry), then OpenH264
		sc := fm.streamCalls()
		if len(sc) != 3 || !argsContain(sc[0], "-c:v", encNVENC) || !argsContain(sc[1], "-c:v", encNVENC) || !argsContain(sc[2], "-c:v", encOpenH264) {
			t.Errorf("stream calls %v", sc)
		}
		if st := e.streamState(l.ID); st.Status != StreamReady || !st.Transcoded {
			t.Errorf("state %+v", st)
		}
		if !monotone(r.progress.vals) || r.progress.last() != 1 {
			t.Errorf("progress %v", r.progress.vals)
		}
		noLibx264(t, fm)
	})

	t.Run("the last encoder's failure is the error", func(t *testing.T) {
		l, _ := e.mediaLecture("last")
		e.enqueueStream(l.ID)
		fm := &fakeMedia{probe: probeHEVC, fail: nvencFails}
		r := e.run(&streamStage{d: e.deps(nil), media: fm}, l.ID)
		var ce *model.CodedError
		// the only encoder failed itself, twice: media.encoder_failed (qsv-retry)
		if r.err == nil || !errors.As(r.err, &ce) || ce.Info.Code != MediaEncoderFailed || len(fm.streamCalls()) != 2 ||
			!paramsMatch(&ce.Info, "tried", encNVENC) || sched.IsPermanent(r.err) {
			t.Errorf("err %v, calls %v", r.err, fm.streamCalls())
		}
	})

	t.Run("another failure falls back at once", func(t *testing.T) {
		// not the encoder's own failure (V7 ruling 4): the next encoder, no
		// second run; when it fails too, its error is the result
		l, _ := e.mediaLecture("other")
		e.enqueueStream(l.ID)
		fm := &fakeMedia{probe: probeHEVC,
			fail: func(args []string) (string, error) {
				if isStreamCall(args) {
					return "Conversion failed!", errors.New("exit status 1")
				}
				return "", nil
			},
			encoders: []encoderCheck{{Name: encNVENC, Listed: true}, {Name: encOpenH264, Listed: true}}}
		r := e.run(&streamStage{d: e.deps(nil), media: fm}, l.ID)
		var ce *model.CodedError
		sc := fm.streamCalls()
		if r.err == nil || !errors.As(r.err, &ce) || ce.Info.Code != MediaFailed || len(sc) != 2 ||
			!argsContain(sc[0], "-c:v", encNVENC) || !argsContain(sc[1], "-c:v", encOpenH264) {
			t.Errorf("err %v, calls %v", r.err, sc)
		}
	})

	t.Run("a file problem does not fall back", func(t *testing.T) {
		l, _ := e.mediaLecture("corrupt")
		e.enqueueStream(l.ID)
		fm := &fakeMedia{probe: probeHEVC,
			fail: func(args []string) (string, error) {
				return "[mov,mp4 @ 0] moov atom not found\nm.mp4: Invalid data found when processing input", errors.New("exit status 1")
			},
			encoders: []encoderCheck{{Name: encNVENC, Listed: true}, {Name: encOpenH264, Listed: true}}}
		r := e.run(&streamStage{d: e.deps(nil), media: fm}, l.ID)
		var ce *model.CodedError
		if r.err == nil || !errors.As(r.err, &ce) || ce.Info.Code != MediaCorrupt || len(fm.streamCalls()) != 1 {
			t.Errorf("err %v, calls %v", r.err, fm.streamCalls())
		}
	})

	t.Run("no usable encoder: audio mode with the reason", func(t *testing.T) {
		for _, stage := range []string{"prep", "stream"} {
			l, dir := e.mediaLecture("none-" + stage)
			fm := &fakeMedia{probe: probeHEVC, encoders: []encoderCheck{
				{Name: encNVENC, Listed: true, Err: "exit status 1: No capable devices found"}, {Name: encQSV}, {Name: encVideoToolbox}, {Name: encOpenH264}}}
			var r runResult
			if stage == "prep" {
				_, r = e.prepThenStream(fm, l.ID) // stream-parallel: the task prep queued
			} else {
				e.enqueueStream(l.ID)
				r = e.run(&streamStage{d: e.deps(nil), media: fm}, l.ID)
			}
			if r.err != nil || r.job.Status != model.StatusDone {
				t.Fatalf("%s: %v %s", stage, r.err, r.job.Status)
			}
			if sc := fm.streamCalls(); len(sc) != 0 {
				t.Errorf("%s: stream calls %v", stage, sc)
			}
			if _, err := os.Stat(filepath.Join(dir, model.StreamFile)); !os.IsNotExist(err) {
				t.Errorf("%s: stream.mp4 written", stage)
			}
			st := e.streamState(l.ID)
			info := streamError(t, st)
			if st.Status != StreamFailed || info.Code != MediaNoH264Encoder ||
				info.Params["tried"] != "h264_nvenc, h264_qsv, h264_videotoolbox, libopenh264" ||
				!strings.Contains(info.Detail, "No capable devices found") || !strings.Contains(info.Detail, "libopenh264: not available") {
				t.Errorf("%s: state %+v, error %+v", stage, st, info)
			}
			noLibx264(t, fm)
		}
	})

	t.Run("the encoder check fails", func(t *testing.T) {
		l, _ := e.mediaLecture("encerr")
		fm := &fakeMedia{probe: probeHEVC, encErr: errors.New("ffmpeg -encoders: exit status 1")}
		// prep is done (prepThenStream checks it); the stream task it queued fails
		if _, r := e.prepThenStream(fm, l.ID); r.job.Status != model.StatusFailed {
			t.Fatalf("stream task %v %s", r.err, r.job.Status)
		}
		if info := streamError(t, e.streamState(l.ID)); info.Code != MediaFailed || !strings.Contains(info.Detail, "-encoders") {
			t.Errorf("error %+v", info)
		}
	})

	t.Run("remux checks no encoder", func(t *testing.T) {
		l, _ := e.mediaLecture("remux")
		fm := &fakeMedia{probe: probeMP4, encErr: errors.New("must not be called")}
		if r := e.run(&prepStage{d: e.deps(nil), media: fm}, l.ID); r.err != nil {
			t.Fatal(r.err)
		}
		sc := fm.streamCalls()
		if fm.encCalls != 0 || len(sc) != 1 || !argsContain(sc[0], "-c", "copy") || slices.Contains(sc[0], "-c:v") {
			t.Errorf("encoder checks %d, calls %v", fm.encCalls, sc)
		}
		if st := e.streamState(l.ID); st.Status != StreamReady || st.Transcoded {
			t.Errorf("state %+v", st)
		}
	})
}
