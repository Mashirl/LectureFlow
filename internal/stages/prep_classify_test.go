// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// ---------- qsv-retry follow-up: only a file that cannot be read is media.corrupt ----------

// Outputs of the real ffmpeg 9.0.1 (prep's audio conversion of damaged
// files; addresses as printed). The runs with the per-packet and demuxing
// messages exited 0: ffmpeg skips what it cannot decode and goes on.
const (
	// AAC (ADTS) with damaged bytes in the middle
	ff9PacketToDecoder = "[aac @ 0000023bd382bd80] Number of bands (14) exceeds limit (12).\n" +
		"[aist#0:0/aac @ 0000023bd382b140] [dec:aac @ 0000023bd382bd80] Error submitting packet to decoder: Invalid data found when processing input\n" +
		"[aist#0:0/aac @ 0000023bd382b140] [dec:aac @ 0000023bd382bd80] Error submitting packet to decoder: Invalid data found when processing input"
	// FLAC with damaged bytes
	ff9DecodingError = "[aist#0:0/flac @ 0000018d26cba040] [dec:flac @ 0000018d26cba200] Decoding error: Invalid data found when processing input"
	// Ogg Opus with damaged pages: the input ends there
	ff9DemuxingError = "[in#0/ogg @ 0000020be4553d40] Error during demuxing: Invalid data found when processing input"
	// MP3 mostly overwritten: more than 2/3 of the packets fail, exit code 69
	ff9ErrorRate = "[mp3float @ 0000020309175140] Header missing\n" +
		"[aist#0:0/mp3 @ 000002030916b500] [dec:mp3float @ 0000020309174bc0] Error submitting packet to decoder: Invalid data found when processing input\n" +
		"[aist#0:0/mp3 @ 000002030916b500] [dec:mp3float @ 0000020309174bc0] Decode error rate 0.783898 exceeds maximum 0.666667\n" +
		"[aist#0:0/mp3 @ 000002030916b500] [dec:mp3float @ 0000020309174bc0] Task finished with error code: -1145393733 (Error number -1145393733 occurred)\n" +
		"[aist#0:0/mp3 @ 000002030916b500] [dec:mp3float @ 0000020309174bc0] Terminating thread with return code -1145393733 (Error number -1145393733 occurred)\n" +
		"[out#0/wav @ 00000203090e1a80] video:0KiB audio:41KiB subtitle:0KiB other streams:0KiB global headers:0KiB muxing overhead: 0.186469%\n" +
		"Conversion failed!"
	// MP4 cut in half (moov at the end): cannot be opened, exit code 183
	ff9CannotOpen = "[in#0 @ 0000015cc1634b80] moov atom not found\n" +
		"[in#0 @ 0000015cc1634a00] Error opening input: Invalid data found when processing input\n" +
		"Error opening input file trunc_end.mp4.\n" +
		"Error opening input files: Invalid data found when processing input"
	// the video decoder of a transcode (MPEG-4 Part 2 in MP4, damaged)
	ff9VideoPackets = "[vist#0:0/mpeg4 @ 00000112d537aa80] [dec:mpeg4 @ 00000112d53bee00] Error submitting packet to decoder: Invalid data found when processing input\n" +
		"[vist#0:0/mpeg4 @ 00000112d537aa80] [dec:mpeg4 @ 00000112d53bee00] corrupt decoded frame"
)

// TestMediaErrorReadableFile: the messages of a run that went on do not
// make a failure media.corrupt (ffmpeg 7+ wording as well as up to 6); a
// file that cannot be opened, or of which too much fails to decode, is
// media.corrupt (permanent). A failure with only the non-fatal messages
// had another cause: media.failed, retryable.
func TestMediaErrorReadableFile(t *testing.T) {
	crash := errors.New("exit status 0xc0000005") // the run ended for a reason of its own
	cases := []struct {
		name      string
		stderr    string
		code      string
		permanent bool
	}{
		{"packets to the decoder (7+)", ff9PacketToDecoder, MediaFailed, false},
		{"decoding error (7+)", ff9DecodingError, MediaFailed, false},
		{"demuxing error (7+)", ff9DemuxingError, MediaFailed, false},
		{"video packets (7+)", ff9VideoPackets, MediaFailed, false},
		{"while decoding (up to 6)", "[aac @ 0000] Error while decoding stream #0:1: Invalid data found when processing input", MediaFailed, false},
		{"decode error rate", ff9ErrorRate, MediaCorrupt, true},
		{"decode error rate (up to 6)", "Decode error rate 0.9 exceeds maximum 0.666667", MediaCorrupt, true},
		{"cannot open (7+)", ff9CannotOpen, MediaCorrupt, true},
		{"cannot open after damaged packets", ff9PacketToDecoder + "\n" + ff9CannotOpen, MediaCorrupt, true},
		{"cannot open (up to 6)", "[mov,mp4 @ 0000] moov atom not found\nm.mp4: Invalid data found when processing input", MediaCorrupt, true},
		{"no audio among damaged packets", ff9VideoPackets + "\n[out#0/wav @ 0000] Output file does not contain any stream", MediaNoAudio, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := mediaError(c.stderr, crash)
			var ce *model.CodedError
			if !errors.As(err, &ce) || ce.Info.Code != c.code || sched.IsPermanent(err) != c.permanent {
				t.Fatalf("%v (permanent=%v), want %s permanent=%v", err, sched.IsPermanent(err), c.code, c.permanent)
			}
			if !strings.Contains(errDetail(err), strings.Split(c.stderr, "\n")[0]) {
				t.Errorf("detail %q lacks the ffmpeg output", errDetail(err))
			}
		})
	}
}

// TestPrepAudioReadableFile: prep's audio conversion follows the same
// rule. A damaged but readable file whose conversion failed for another
// reason is a retryable media.failed (the scheduler tries prep again); one
// that cannot be read is a permanent media.corrupt. Either way no
// audio.wav (nor its .tmp) is left.
func TestPrepAudioReadableFile(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name      string
		stderr    string
		code      string
		permanent bool
	}{
		{"damaged packets, then a crash", ff9PacketToDecoder, MediaFailed, false},
		{"input ends early, then a crash", ff9DemuxingError, MediaFailed, false},
		{"too much undecodable", ff9ErrorRate, MediaCorrupt, true},
		{"cannot be opened", ff9CannotOpen, MediaCorrupt, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, dir := e.mediaLecture(c.name)
			fm := &fakeMedia{probe: probeM4A, fail: func(args []string) (string, error) {
				if slices.Contains(args, "pcm_s16le") {
					return c.stderr, errors.New("exit status 69")
				}
				return "", nil
			}}
			r := e.run(&prepStage{d: e.deps(nil), media: fm}, l.ID)
			var ce *model.CodedError
			if r.err == nil || !errors.As(r.err, &ce) || ce.Info.Code != c.code || sched.IsPermanent(r.err) != c.permanent || r.job.Status != model.StatusFailed {
				t.Fatalf("prep %v (permanent=%v) %s, want %s permanent=%v", r.err, sched.IsPermanent(r.err), r.job.Status, c.code, c.permanent)
			}
			for _, n := range []string{model.AudioFile, model.AudioFile + ".tmp"} {
				if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
					t.Errorf("%s left behind", n)
				}
			}
		})
	}
}

// TestStreamDamagedReadableSource: a transcode of a damaged but readable
// source that failed with the decoder's per-packet messages only is not
// media.corrupt: the next encoder is tried (V7 ruling 4), and its success
// makes the playback file.
func TestStreamDamagedReadableSource(t *testing.T) {
	e := newEnv(t)
	l, _ := e.mediaLecture("damaged")
	e.enqueueStream(l.ID)
	fm := &fakeMedia{probe: probeHEVC,
		encoders: []encoderCheck{{Name: encNVENC, Listed: true}, {Name: encQSV}, {Name: encVideoToolbox}, {Name: encOpenH264, Listed: true}},
		fail: func(args []string) (string, error) {
			if isStreamCall(args) && slices.Contains(args, encNVENC) {
				return ff9VideoPackets + "\nConversion failed!", errors.New("exit status 0xc0000005")
			}
			return "", nil
		}}
	r := e.run(&streamStage{d: e.deps(nil), media: fm}, l.ID)
	if r.err != nil || r.job.Status != model.StatusDone {
		t.Fatalf("stream %v %s", r.err, r.job.Status)
	}
	if got := encodersOf(fm.streamCalls()); !slices.Equal(got, []string{encNVENC, encOpenH264}) {
		t.Errorf("stream calls %v", got)
	}
	if st := e.streamState(l.ID); st.Status != StreamReady {
		t.Errorf("state %+v", st)
	}
}
