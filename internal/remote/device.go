// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package remote

// The generic remote transcription device (ssh-mvp;
// .claude/research/remote-ssh-plan.md §2): what the scheduler and the asr
// stage need from a device that is not this machine. The SSH servers
// implement it (internal/sshremote); Colab keeps its own types for now
// (plan §0: its lease and batch policy stay Colab-specific) and moves to
// these later. Leaf package: no LectureFlow imports (card.go).

import (
	"context"
	"errors"
	"fmt"
)

// Device is one remote transcription device.
type Device interface {
	// ID is the device id ("ssh:<slug>").
	ID() string
	// Ready reports whether a lecture of audioSec may start on the device
	// now; when not, waitingFor is the job's waiting_for ("" = busy).
	Ready(ctx context.Context, audioSec float64) (ok bool, waitingFor string)
	// Slots is how many lectures may run on it at once.
	Slots() int
	// Transcribe runs job and returns the path of the verified result.json
	// (in job.OutDir, written atomically). Errors are *Error (Kind says
	// what the scheduler does with the lecture); a cancelled ctx returns
	// within CancelWithin.
	Transcribe(ctx context.Context, job Job, progress func(Event)) (string, error)
	// Card is the device's resource card.
	Card(ctx context.Context) Card
}

// Job is one lecture to transcribe remotely.
type Job struct {
	// ID is the asr job id; it also names the remote job directory
	// (JobIDPattern) so a reconnect continues the same job.
	ID string
	// LectureID is the lecture (the card's current job).
	LectureID string
	// AudioPath is the lecture's audio.wav; AudioSHA256 its SHA-256 (hex),
	// part of the asr input hash, checked again on the remote.
	AudioPath   string
	AudioSHA256 string
	DurationSec float64
	// Engine is the worker the remote runs (EngineMLX, EngineFasterWhisper).
	Engine string
	// Model and Precision as in the device row (precision auto resolved).
	Model, Precision string
	// Params is the worker's job.json without any path (ParamKeys): the
	// local constructor's output; the agent adds its own paths.
	Params map[string]any
	// OutDir receives result.json; WorkDir temporary files ("" = OutDir).
	OutDir  string
	WorkDir string
}

// Engines a remote may run (the backends of the device rows).
const (
	EngineFasterWhisper = "faster-whisper"
	EngineMLX           = "mlx"
)

// Steps of a remote run (the hover card's sub-steps; the same names as
// Colab's, model.RemoteSteps).
const (
	StepConnect    = "wait_accelerator" // connect, check the agent and the environment
	StepUpload     = "upload_audio"
	StepTranscribe = "remote_transcribe"
	StepDownload   = "download_result"
	StepVerify     = "verify"
)

// Event is a progress report of Transcribe.
type Event struct {
	Step string
	// Stage is the remote's own stage during StepTranscribe (StageInstall,
	// StageLoadModel, StageTranscribe, StageReconnect).
	Stage string
	// Fraction of the step (0..1, -1 unknown); Overall of the lecture.
	Fraction, Overall float64
	ETASec            float64 // -1 unknown
	WaitingFor        string
}

// Remote stages (Event.Stage).
const (
	StageInstall    = "install"
	StageLoadModel  = "load_model"
	StageTranscribe = "transcribe"
	StageReconnect  = "reconnect"
)

// ErrKind says what the scheduler does with a lecture whose remote run
// failed (plan §2.3, the same classes as Colab's).
type ErrKind int

const (
	// ErrFallback: the device cannot do this lecture; it goes back to the
	// local devices without using up a retry (stats.remote_fallback).
	ErrFallback ErrKind = iota + 1
	// ErrBusy: the device could not take the lecture yet (nothing ran);
	// back to the queue as it was.
	ErrBusy
	// ErrLocal: an error of the lecture itself (e.g. its audio is
	// unreadable): the job fails like a local run.
	ErrLocal
)

// Error is a remote run's error.
type Error struct {
	Kind   ErrKind
	Code   string
	Params map[string]any
	// WaitingFor (ErrBusy) is the waiting_for the job keeps.
	WaitingFor string
	Err        error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("remote: %s", e.Code)
}

func (e *Error) Unwrap() error { return e.Err }

func kindOf(err error) ErrKind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return 0
}

// IsFallback reports whether err hands the lecture back to the local devices.
func IsFallback(err error) bool { return err != nil && kindOf(err) == ErrFallback }

// IsBusy reports whether the device could not take the lecture yet.
func IsBusy(err error) bool { return err != nil && kindOf(err) == ErrBusy }

// IsRemote reports whether err is a remote device's classified error.
func IsRemote(err error) bool { return err != nil && kindOf(err) != 0 }

// CodeOf is err's remote code ("" when it is not a *Error).
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
