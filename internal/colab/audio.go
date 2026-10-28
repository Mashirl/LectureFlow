// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"lectureflow/internal/procutil"
)

// The upload is the lecture's audio.wav (prep's 16 kHz mono PCM) re-encoded
// as FLAC: lossless, so the remote run decodes exactly the samples a local
// run would (identical transcription), at roughly half the bytes. When
// ffmpeg is missing or fails, the WAV itself is uploaded.

// FFmpegRunner runs ffmpeg with args (tests replace it).
type FFmpegRunner func(ctx context.Context, ffmpeg string, args []string) error

func runFFmpeg(ctx context.Context, ffmpeg string, args []string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	hideWindow(cmd)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := procutil.Run(cmd, procutil.Tied) // ends with LectureFlow, also on a crash (test-chaos 1)
	out := buf.Bytes()
	if err != nil {
		if len(out) > 400 {
			out = out[len(out)-400:]
		}
		return fmt.Errorf("ffmpeg: %v: %s", err, out)
	}
	return nil
}

// flacArgs encodes in (WAV) to out (FLAC, highest compression).
func flacArgs(in, out string) []string {
	return []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-i", in,
		"-map", "0:a:0", "-c:a", "flac", "-compression_level", "8", "-f", "flac", out}
}

// preparedAudio is the file to upload.
type preparedAudio struct {
	Path   string // local file (the FLAC in workDir, or the WAV itself)
	Name   string // remote file name: audio.flac or audio.wav
	Data   []byte
	SHA256 string
	Temp   bool // Path is a temporary file to delete
}

// prepareAudio makes the upload: FLAC through ffmpeg when possible, the
// WAV otherwise; reads it and hashes it.
func prepareAudio(ctx context.Context, run FFmpegRunner, ffmpeg, wav, workDir string, logf func(msg string, args ...any)) (preparedAudio, error) {
	if _, err := os.Stat(wav); err != nil {
		return preparedAudio{}, newErr(CodeAudioMissing, map[string]any{"file": filepath.Base(wav)}, kindLocal, "",
			errors.New("the lecture's audio file is missing"))
	}
	p := preparedAudio{Path: wav, Name: "audio.wav"}
	if ffmpeg != "" && run != nil {
		out := filepath.Join(workDir, "colab-upload-"+randomHex(6)+".flac")
		tmp := out + ".tmp"
		err := run(ctx, ffmpeg, flacArgs(wav, tmp))
		if err == nil {
			err = os.Rename(tmp, out)
		}
		if err == nil {
			p = preparedAudio{Path: out, Name: "audio.flac", Temp: true}
		} else {
			_ = os.Remove(tmp)
			if ctx.Err() != nil {
				return preparedAudio{}, ctx.Err()
			}
			logf("colab: FLAC encoding failed; uploading the WAV", "err", err.Error())
		}
	}
	// A FLAC that cannot be read back goes at once (the caller only
	// removes the temporary file of an upload it got).
	drop := func() {
		if p.Temp {
			_ = os.Remove(p.Path)
		}
	}
	f, err := os.Open(p.Path)
	if err != nil {
		drop()
		return preparedAudio{}, newErr(CodeAudioMissing, map[string]any{"file": filepath.Base(p.Path)}, kindLocal, "", err)
	}
	h := sha256.New()
	data, err := io.ReadAll(io.TeeReader(f, h))
	f.Close()
	if err != nil {
		drop()
		return preparedAudio{}, err
	}
	p.Data = data
	p.SHA256 = hex.EncodeToString(h.Sum(nil))
	return p, nil
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
