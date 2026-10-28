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

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// ---------- qsv-retry: an encoder failing during the transcode ----------

// qsvMidwayFailure is the output of a real failure (ffmpeg 9.0.1, Intel
// Quick Sync under load, exit code 183): the encoder returned a frame
// without a type, which ffmpeg reports as AVERROR_INVALIDDATA, worded like
// a damaged input.
const qsvMidwayFailure = `Input #0, avi, from 'in.avi':
  Stream #0:0: Video: mpeg4 (Simple Profile) (FMP4 / 0x34504D46), yuv420p, 320x240 [SAR 1:1 DAR 4:3], 10 fps, 10 tbr, 10 tbn
  Stream #0:1: Audio: mp2 (P[0][0][0] / 0x0050), 44100 Hz, mono, s16p, 64 kb/s
Stream mapping:
  Stream #0:0 -> #0:0 (mpeg4 (native) -> h264 (h264_qsv))
  Stream #0:1 -> #0:1 (mp2 (native) -> aac (native))
Output #0, mp4, to 'stream.mp4.tmp':
[h264_qsv @ 000001a3c55e2a40] Invalid FrameType:0.
[vost#0:0/h264_qsv @ 000001a3c560ddc0] [enc:h264_qsv @ 000001a3c559ac00] Error submitting video frame to the encoder
[vost#0:0/h264_qsv @ 000001a3c560ddc0] [enc:h264_qsv @ 000001a3c559ac00] Error encoding a frame: Invalid data found when processing input
[vost#0:0/h264_qsv @ 000001a3c560ddc0] Task finished with error code: -1094995529 (Invalid data found when processing input)
[vost#0:0/h264_qsv @ 000001a3c560ddc0] Terminating thread with return code -1094995529 (Invalid data found when processing input)
[mp4 @ 000001a3c55ddc00] Starting second pass: moving the moov atom to the beginning of the file
[out#0/mp4 @ 000001a3c5561b00] video:10KiB audio:30KiB subtitle:0KiB other streams:0KiB global headers:0KiB muxing overhead: 5.056002%
[aac @ 000001a3c5609d80] Qavg: 997.267
Conversion failed!`

// corruptInput is ffmpeg 7+'s output for an input it cannot open.
const corruptInput = "[mov,mp4,m4a,3gp,3g2,mj2 @ 0000] moov atom not found\n" +
	"[in#0 @ 0000] Error opening input: Invalid data found when processing input\n" +
	"Error opening input file m.mp4.\nError opening input files: Invalid data found when processing input"

// openh264Fails is a failure of the software encoder (ffmpeg 7+).
const openh264Fails = "[libopenh264 @ 0] EncodeFrame failed\n[vost#0:0/libopenh264 @ 0] [enc:libopenh264 @ 0] video encoding failed\nConversion failed!"

// TestEncoderFailureOutput: which outputs of a failed transcode are the
// encoder's own failure (run again, then the next encoder) and which keep
// their appendix D6 class; the detail is the raw output.
func TestEncoderFailureOutput(t *testing.T) {
	exit := errors.New("exit status 183")
	cases := []struct {
		name   string
		stderr string
		enc    string
		fault  bool   // transcodeError: the encoder's own failure
		code   string // transcodeError's code
	}{
		{"qsv mid-way (ffmpeg 9)", qsvMidwayFailure, encQSV, true, MediaEncoderFailed},
		{"nvenc cannot open (ffmpeg 7+)", "[h264_nvenc @ 0] OpenEncodeSessionEx failed: out of memory (10)\n[h264_nvenc @ 0] No capable devices found\n" +
			"[vost#0:0/h264_nvenc @ 0] Error while opening encoder - maybe incorrect parameters such as bit_rate, rate, width or height.\n" +
			"[vf#0:0 @ 0] Error sending frames to consumers: Generic error in an external library\nConversion failed!", encNVENC, true, MediaEncoderFailed},
		{"nvenc cannot open (ffmpeg 4-6)", "[h264_nvenc @ 0x1] No capable devices found\n" +
			"Error initializing output stream 0:0 -- Error while opening encoder for output stream #0:0 - maybe incorrect parameters such as bit_rate, rate, width or height\nConversion failed!", encNVENC, true, MediaEncoderFailed},
		{"video encoding failed (ffmpeg 4-5)", "[h264_qsv @ 0x1] Error during encoding: device failed (-17)\nVideo encoding failed\nConversion failed!", encQSV, true, MediaEncoderFailed},
		{"videotoolbox", "[h264_videotoolbox @ 0x1] Error encoding frame: -12902\n[vost#0:0/h264_videotoolbox @ 0x2] Error encoding a frame: Unknown error occurred\nConversion failed!", encVideoToolbox, true, MediaEncoderFailed},
		{"openh264", openh264Fails, encOpenH264, true, MediaEncoderFailed},
		// not the video encoder
		{"aac encoder (ffmpeg 7+)", "[aost#0:1/aac @ 0] Error while opening encoder - maybe incorrect parameters such as bit_rate, rate, width or height.\nConversion failed!", encQSV, false, MediaFailed},
		{"aac encoder (ffmpeg 4-6)", "Error initializing output stream 0:1 -- Error while opening encoder for output stream #0:1 - maybe incorrect parameters such as bit_rate, rate, width or height\nConversion failed!", encQSV, false, MediaFailed},
		{"another encoder's line", "[h264_nvenc @ 0] OpenEncodeSessionEx failed\nConversion failed!", encQSV, false, MediaFailed},
		{"encoder warning only", "[h264_qsv @ 0] Warning in encoder initialization: partial acceleration (4)\nConversion failed!", encQSV, false, MediaFailed},
		// the input file: its D6 class, as before
		{"corrupt input", corruptInput, encQSV, false, MediaCorrupt},
		{"corrupt input, qsv warning", "[h264_qsv @ 0] Warning in encoder initialization: partial acceleration (4)\n" + corruptInput, encQSV, false, MediaCorrupt},
		{"unsupported input", "[vist#0:1] Decoder (codec hevc) not found for input stream #0:1", encNVENC, false, MediaUnsupported},
		// a full disk is no encoder's fault
		{"disk full", "[h264_qsv @ 0] Error during encoding: device failed (-17)\nav_interleaved_write_frame(): No space left on device", encQSV, false, MediaFailed},
		// a remux has no encoder: the output is classified as before
		{"remux", qsvMidwayFailure, "", false, MediaCorrupt},
	}
	// the encoder's failure shows, but a full disk wins
	encoderLine := map[string]bool{"disk full": true}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, want := encoderFailed(c.stderr, c.enc), c.fault || encoderLine[c.name]; got != want {
				t.Errorf("encoderFailed = %v, want %v", got, want)
			}
			err, fault := transcodeError(c.stderr, exit, c.enc)
			var ce *model.CodedError
			if fault != c.fault || !errors.As(err, &ce) || ce.Info.Code != c.code {
				t.Fatalf("transcodeError = %v (fault %v), want %s (fault %v)", err, fault, c.code, c.fault)
			}
			if c.fault && sched.IsPermanent(err) {
				t.Error("an encoder failure must not be permanent")
			}
			if c.code == MediaCorrupt && !sched.IsPermanent(err) {
				t.Error("a damaged file stays permanent")
			}
			if d := errDetail(err); !strings.Contains(d, strings.Split(c.stderr, "\n")[0]) {
				t.Errorf("detail %q lacks the ffmpeg output", d)
			}
		})
	}
	nested := "[vost#0:0/h264_qsv @ 0] [enc:h264_qsv @ 0] Error submitting video frame to the encoder"
	if !encoderFailed(nested, encQSV) || encoderFailed(nested, encNVENC) {
		t.Error("nested tags: the first one names the encoder")
	}
}

// qsvEncoders are the encoder checks of a machine with Quick Sync and
// OpenH264.
func qsvEncoders() []encoderCheck {
	return []encoderCheck{{Name: encNVENC}, {Name: encQSV, Listed: true}, {Name: encVideoToolbox}, {Name: encOpenH264, Listed: true}}
}

// failStreamWith is a fakeMedia.fail: stream.mp4 calls whose encoder is in
// outputs fail with its output — only the first n of them when n > 0.
func failStreamWith(outputs map[string]string, n int) func([]string) (string, error) {
	var mu sync.Mutex
	failed := 0
	return func(args []string) (string, error) {
		i := slices.Index(args, "-c:v")
		if !isStreamCall(args) || i < 0 {
			return "", nil
		}
		out, ok := outputs[args[i+1]]
		mu.Lock()
		defer mu.Unlock()
		if !ok || (n > 0 && failed >= n) {
			return "", nil
		}
		failed++
		return out, errors.New("exit status 183")
	}
}

// encodersOf are the -c:v of calls ("copy" for a remux).
func encodersOf(calls [][]string) []string {
	var out []string
	for _, c := range calls {
		if i := slices.Index(c, "-c:v"); i >= 0 {
			out = append(out, c[i+1])
		} else {
			out = append(out, "copy")
		}
	}
	return out
}

// TestStreamEncoderRetry (qsv-retry): a hardware encoder that fails itself
// during the transcode runs once more, then the next usable encoder (V7
// order) takes over, and the job notes it. Only when every encoder failed
// is the result media.encoder_failed — never media.corrupt; a damaged file
// is still media.corrupt at once.
func TestStreamEncoderRetry(t *testing.T) {
	e := newEnv(t)

	t.Run("qsv fails once: its second run makes the file", func(t *testing.T) {
		l, dir := e.mediaLecture("qsv-once")
		fm := &fakeMedia{probe: probeHEVC, encoders: qsvEncoders(), fail: failStreamWith(map[string]string{encQSV: qsvMidwayFailure}, 1)}
		p, r := e.prepThenStream(fm, l.ID) // stream-parallel: the transcode is the task's
		if r.err != nil || r.job.Status != model.StatusDone {
			t.Fatalf("stream task %v %s", r.err, r.job.Status)
		}
		if p.job.Stats.NoteInfo != nil {
			t.Errorf("prep note %+v: the note belongs to the task that transcoded", p.job.Stats.NoteInfo)
		}
		if got := encodersOf(fm.streamCalls()); !slices.Equal(got, []string{encQSV, encQSV}) {
			t.Errorf("stream calls %v", got)
		}
		if st := e.streamState(l.ID); st.Status != StreamReady || !st.Transcoded || st.Error != "" {
			t.Errorf("state %+v", st)
		}
		if _, err := os.Stat(filepath.Join(dir, model.StreamFile)); err != nil {
			t.Error("stream.mp4 missing")
		}
		if n := r.job.Stats.NoteInfo; infoCode(n) != NotePrepEncoderRetried || !paramsMatch(n, "failed", encQSV, "encoder", encQSV) ||
			!strings.Contains(r.job.Stats.Note, encQSV) {
			t.Errorf("job note %q %+v", r.job.Stats.Note, n)
		}
		if !monotone(r.progress.vals) || r.progress.last() != 1 {
			t.Errorf("progress %v", r.progress.vals)
		}
	})

	t.Run("qsv fails twice: openh264 makes the file", func(t *testing.T) {
		l, _ := e.mediaLecture("qsv-twice")
		e.enqueueStream(l.ID)
		fm := &fakeMedia{probe: probeHEVC, encoders: qsvEncoders(), fail: failStreamWith(map[string]string{encQSV: qsvMidwayFailure}, 0)}
		r := e.run(&streamStage{d: e.deps(nil), media: fm}, l.ID)
		if r.err != nil || r.job.Status != model.StatusDone {
			t.Fatalf("stream %v %s", r.err, r.job.Status)
		}
		if got := encodersOf(fm.streamCalls()); !slices.Equal(got, []string{encQSV, encQSV, encOpenH264}) {
			t.Errorf("stream calls %v", got)
		}
		if st := e.streamState(l.ID); st.Status != StreamReady || !st.Transcoded {
			t.Errorf("state %+v", st)
		}
		if n := r.job.Stats.NoteInfo; infoCode(n) != NotePrepEncoderRetried || !paramsMatch(n, "failed", encQSV, "encoder", encOpenH264) {
			t.Errorf("job note %+v", n)
		}
		if !monotone(r.progress.vals) || r.progress.last() != 1 {
			t.Errorf("progress %v", r.progress.vals)
		}
	})

	t.Run("no note without a failure", func(t *testing.T) {
		l, _ := e.mediaLecture("qsv-fine")
		fm := &fakeMedia{probe: probeHEVC, encoders: qsvEncoders()}
		p, r := e.prepThenStream(fm, l.ID)
		for _, r := range []runResult{p, r} {
			if r.err != nil || r.job.Stats.NoteInfo != nil || r.job.Stats.Note != "" {
				t.Errorf("%s %v, note %q %+v", r.job.Stage, r.err, r.job.Stats.Note, r.job.Stats.NoteInfo)
			}
		}
		if got := encodersOf(fm.streamCalls()); !slices.Equal(got, []string{encQSV}) {
			t.Errorf("stream calls %v", got)
		}
	})

	t.Run("every encoder fails: media.encoder_failed", func(t *testing.T) {
		outputs := map[string]string{encQSV: qsvMidwayFailure, encOpenH264: openh264Fails}
		for _, stage := range []string{"stream", "prep"} {
			l, dir := e.mediaLecture("all-" + stage)
			fm := &fakeMedia{probe: probeHEVC, encoders: qsvEncoders(), fail: failStreamWith(outputs, 0)}
			var r runResult
			if stage == "prep" {
				// the playback file never fails prep (prepThenStream checks prep
				// is done); the stream task prep queued fails (stream-parallel)
				_, r = e.prepThenStream(fm, l.ID)
			} else {
				e.enqueueStream(l.ID)
				r = e.run(&streamStage{d: e.deps(nil), media: fm}, l.ID)
			}
			var ce *model.CodedError
			if r.err == nil || !errors.As(r.err, &ce) || ce.Info.Code != MediaEncoderFailed || r.job.Status != model.StatusFailed || sched.IsPermanent(r.err) {
				t.Errorf("%s: stream job %v %s", stage, r.err, r.job.Status)
			}
			// QSV twice (hardware), OpenH264 once (software: no second run)
			if got := encodersOf(fm.streamCalls()); !slices.Equal(got, []string{encQSV, encQSV, encOpenH264}) {
				t.Errorf("%s: stream calls %v", stage, got)
			}
			st := e.streamState(l.ID)
			info := streamError(t, st)
			if st.Status != StreamFailed || info.Code != MediaEncoderFailed || info.Params["tried"] != "h264_qsv, libopenh264" {
				t.Errorf("%s: state %+v, error %+v", stage, st, info)
			}
			for _, want := range []string{"h264_qsv (attempt 1)", "h264_qsv (attempt 2)", "libopenh264 (attempt 1)", "Invalid FrameType", "EncodeFrame failed"} {
				if !strings.Contains(info.Detail, want) {
					t.Errorf("%s: detail lacks %q:\n%s", stage, want, info.Detail)
				}
			}
			for _, n := range []string{model.StreamFile, model.StreamFile + ".tmp"} {
				if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
					t.Errorf("%s: %s exists after the failure", stage, n)
				}
			}
			if infoCode(r.job.Stats.NoteInfo) == NotePrepEncoderRetried {
				t.Errorf("%s: retried note without a playback file", stage)
			}
		}
	})

	t.Run("openh264 is not run twice", func(t *testing.T) {
		l, _ := e.mediaLecture("oh-fails")
		e.enqueueStream(l.ID)
		fm := &fakeMedia{probe: probeHEVC, encoders: []encoderCheck{{Name: encNVENC}, {Name: encQSV}, {Name: encVideoToolbox}, {Name: encOpenH264, Listed: true}},
			fail: failStreamWith(map[string]string{encOpenH264: openh264Fails}, 0)}
		r := e.run(&streamStage{d: e.deps(nil), media: fm}, l.ID)
		var ce *model.CodedError
		if r.err == nil || !errors.As(r.err, &ce) || ce.Info.Code != MediaEncoderFailed || !paramsMatch(&ce.Info, "tried", encOpenH264) ||
			len(fm.streamCalls()) != 1 {
			t.Errorf("err %v, calls %v", r.err, encodersOf(fm.streamCalls()))
		}
	})

	t.Run("a damaged source is still media.corrupt", func(t *testing.T) {
		for i, out := range []string{corruptInput, "[mov,mp4 @ 0] moov atom not found\nm.mp4: Invalid data found when processing input"} {
			l, _ := e.mediaLecture("corrupt-src-" + string(rune('a'+i)))
			e.enqueueStream(l.ID)
			fm := &fakeMedia{probe: probeHEVC, encoders: qsvEncoders(),
				fail: failStreamWith(map[string]string{encQSV: out, encOpenH264: out}, 0)}
			r := e.run(&streamStage{d: e.deps(nil), media: fm}, l.ID)
			var ce *model.CodedError
			if r.err == nil || !errors.As(r.err, &ce) || ce.Info.Code != MediaCorrupt || !sched.IsPermanent(r.err) {
				t.Errorf("case %d: err %v", i, r.err)
			}
			if got := encodersOf(fm.streamCalls()); !slices.Equal(got, []string{encQSV}) {
				t.Errorf("case %d: a damaged file is not tried again: calls %v", i, got)
			}
			if info := streamError(t, e.streamState(l.ID)); info.Code != MediaCorrupt {
				t.Errorf("case %d: kv error %+v", i, info)
			}
		}
	})

	t.Run("encoder failure, then a full disk", func(t *testing.T) {
		// the last attempt's error is the result when it is not the encoder's
		l, _ := e.mediaLecture("disk")
		e.enqueueStream(l.ID)
		fm := &fakeMedia{probe: probeHEVC, encoders: qsvEncoders(),
			fail: failStreamWith(map[string]string{encQSV: qsvMidwayFailure, encOpenH264: "av_interleaved_write_frame(): No space left on device\nConversion failed!"}, 0)}
		r := e.run(&streamStage{d: e.deps(nil), media: fm}, l.ID)
		var ce *model.CodedError
		if r.err == nil || !errors.As(r.err, &ce) || ce.Info.Code != MediaFailed || len(fm.streamCalls()) != 3 {
			t.Errorf("err %v, calls %v", r.err, encodersOf(fm.streamCalls()))
		}
	})

	t.Run("cancelled while an attempt fails", func(t *testing.T) {
		l, _ := e.mediaLecture("cancel")
		e.enqueueStream(l.ID)
		ctx, cancel := context.WithCancel(e.ctx)
		defer cancel()
		qsv := failStreamWith(map[string]string{encQSV: qsvMidwayFailure}, 0)
		fm := &fakeMedia{probe: probeHEVC, encoders: qsvEncoders(), fail: func(args []string) (string, error) {
			out, err := qsv(args)
			if err != nil {
				cancel() // the user cancels while the first run fails
			}
			return out, err
		}}
		r := e.runCtx(ctx, &streamStage{d: e.deps(nil), media: fm}, l.ID)
		if !errors.Is(r.err, context.Canceled) || len(fm.streamCalls()) != 1 {
			t.Errorf("err %v, calls %v", r.err, encodersOf(fm.streamCalls()))
		}
	})
}
