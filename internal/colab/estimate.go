// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// Remaining-time estimate and the dispatch decision (V14.3 step 4), pure
// functions with a golden test (testdata/estimate.golden.json):
//
//	remaining window = 4 h − what this session has occupied (queue wait and
//	                   boot included)
//	estimate         = audio minutes × measured seconds per audio minute
//	                   + fixed overhead (upload, model load, download,
//	                   verification) [+ one-time setup on a fresh runtime]
//
// A lecture is dispatched only when the estimate fits the remaining window,
// the compute-unit balance covers its cost (paid tier) and the credential
// outlives it with a margin (so the runtime can still be released).

import (
	"math"

	"lectureflow/internal/model"
)

// EstimateDefaults are the values used before anything was measured.
type EstimateDefaults struct {
	// SecPerAudioMin before the first measurement on an accelerator that
	// SecPerAudioMinBy does not list (the T4's, the slowest).
	SecPerAudioMin float64 `json:"sec_per_audio_min"`
	// SecPerAudioMinBy: remote transcription seconds per audio minute per
	// accelerator, measured on Colab (colab-prof §9.1, 2026-10-27:
	// large-v3 float16, beam 5, VAD, 12-minute CSC3631 excerpts, one
	// lecture at a time): T4 4.2, L4 3.1, A100 2.3, G4 1.8. The second
	// decode of degenerate windows comes on top (none to 60 % more); the
	// first measured lecture replaces these (Rate).
	SecPerAudioMinBy map[string]float64 `json:"sec_per_audio_min_by,omitempty"`
	// OverheadSec per lecture before measurement: everything but the
	// transcription — the upload of the lecture's FLAC (a 55-minute
	// lecture is about 62 MB on the wire: 16 s at 30 Mbit/s, 70 s at
	// 7 Mbit/s), the cell, decoding, download and verification (2–3 s
	// after colab-prof R1–R3) — about twice the typical measurement.
	OverheadSec float64 `json:"overhead_sec"`
	// SetupSec once per runtime (its first lecture): the borrow and boot
	// (3.5–5 s), kernel, memory probe and scripts (about 6 s), and what of
	// the resident worker's preparation (uv install, model download and
	// load: about 15 s, in parallel with the upload) the upload does not
	// hide — about twice the measurement without Colab's own hiccups.
	SetupSec float64 `json:"setup_sec"`
	// AuthMarginSec: the credential must outlive the lecture by this much
	// so the runtime can still be released before it ends.
	AuthMarginSec float64 `json:"auth_margin_sec"`
	// CUPerHour per accelerator before the session reports its real rate
	// (ccu-info consumptionRateHourly): measured on the user's paid account
	// (colab-prof §9.1); H100 (no entitlement, never measured) stays a
	// deliberately high guess. Used for the balance check and "本次预计消耗".
	CUPerHour map[string]float64 `json:"cu_per_hour"`
}

// DefaultEstimateDefaults returns the values documented on EstimateDefaults.
func DefaultEstimateDefaults() EstimateDefaults {
	return EstimateDefaults{
		SecPerAudioMin: 4.2, OverheadSec: 90, SetupSec: 60, AuthMarginSec: 600,
		SecPerAudioMinBy: map[string]float64{"T4": 4.2, "L4": 3.1, "A100": 2.3, "G4": 1.8},
		CUPerHour:        map[string]float64{"T4": 1.07, "L4": 1.54, "A100": 5.3, "G4": 8.9, "H100": 20.0},
	}
}

// For returns d with the accelerator's own speed as SecPerAudioMin.
func (d EstimateDefaults) For(accelerator string) EstimateDefaults {
	if v := d.SecPerAudioMinBy[accelerator]; v > 0 {
		d.SecPerAudioMin = v
	}
	return d
}

// Rate is the measured speed for one accelerator, model and precision.
type Rate struct {
	// SecPerAudioMin: remote transcription seconds per minute of audio
	// (weighted by audio length over all samples).
	SecPerAudioMin float64 `json:"sec_per_audio_min"`
	// OverheadSec: mean per-lecture overhead (everything but the
	// transcription itself and the one-time setup).
	OverheadSec float64 `json:"overhead_sec"`
	// Samples measured; AudioMin is their total audio.
	Samples  int     `json:"samples"`
	AudioMin float64 `json:"audio_min"`
}

// Sample is one finished lecture.
type Sample struct {
	AudioSec      float64 `json:"audio_sec"`
	TranscribeSec float64 `json:"transcribe_sec"`
	OverheadSec   float64 `json:"overhead_sec"`
}

// Add folds a sample into the rate. Samples without audio are ignored.
func (r Rate) Add(s Sample) Rate {
	mins := s.AudioSec / 60
	if mins <= 0 || s.TranscribeSec < 0 {
		return r
	}
	total := r.SecPerAudioMin*r.AudioMin + s.TranscribeSec
	r.AudioMin += mins
	r.SecPerAudioMin = total / r.AudioMin
	r.OverheadSec = (r.OverheadSec*float64(r.Samples) + max(0, s.OverheadSec)) / float64(r.Samples+1)
	r.Samples++
	return r
}

// EstimateSec is the expected wall time of one lecture.
func EstimateSec(audioSec float64, r Rate, d EstimateDefaults, setupDone bool) float64 {
	spm, ov := d.SecPerAudioMin, d.OverheadSec
	if r.Samples > 0 {
		spm, ov = r.SecPerAudioMin, r.OverheadSec
	}
	e := max(0, audioSec)/60*spm + ov
	if !setupDone {
		e += d.SetupSec
	}
	return e
}

// DispatchInput is everything the decision depends on.
type DispatchInput struct {
	AudioSec  float64 `json:"audio_sec"`
	WindowSec float64 `json:"window_sec"`
	// UsedSec: what the current session has occupied so far (0 without a
	// session: a new one starts with the whole window).
	UsedSec float64 `json:"used_sec"`
	// SetupDone: the runtime already ran a lecture (no setup cost).
	SetupDone bool             `json:"setup_done"`
	Rate      Rate             `json:"rate"`
	Defaults  EstimateDefaults `json:"defaults"`
	// Paid: check the balance (paid tier); the free tier is best effort.
	Paid         bool    `json:"paid"`
	BalanceKnown bool    `json:"balance_known"`
	BalanceCU    float64 `json:"balance_cu"`
	CUPerHour    float64 `json:"cu_per_hour"`
	// AuthRemainingSec until the credential's lifecycle ends; < 0 = no
	// time limit.
	AuthRemainingSec float64 `json:"auth_remaining_sec"`
}

// Decision is the outcome for one lecture.
type Decision struct {
	Dispatch     bool    `json:"dispatch"`
	WaitingFor   string  `json:"waiting_for,omitempty"`
	EstimateSec  float64 `json:"estimate_sec"`
	RemainingSec float64 `json:"remaining_sec"`
	CostCU       float64 `json:"cost_cu"`
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// Decide is the dispatch decision for one lecture. Order: credential
// (remote_auth), window (remote_window), balance (remote_balance).
func Decide(in DispatchInput) Decision {
	est := EstimateSec(in.AudioSec, in.Rate, in.Defaults, in.SetupDone)
	remaining := max(0, in.WindowSec-in.UsedSec)
	d := Decision{EstimateSec: round2(est), RemainingSec: round2(remaining), CostCU: round2(est / 3600 * max(0, in.CUPerHour))}
	switch {
	case in.AuthRemainingSec >= 0 && in.AuthRemainingSec < est+in.Defaults.AuthMarginSec:
		d.WaitingFor = model.WaitRemoteAuth
	case est > remaining:
		d.WaitingFor = model.WaitRemoteWindow
	case in.Paid && (!in.BalanceKnown || in.BalanceCU < est/3600*max(0, in.CUPerHour)):
		d.WaitingFor = model.WaitRemoteBalance
	default:
		d.Dispatch = true
	}
	return d
}

// PlanResult is how a queue of lectures would go in this session.
type PlanResult struct {
	// Lectures that fit, in queue order; a lecture that does not fit is
	// skipped (it goes to the local devices) and the next one is tried.
	Lectures int    `json:"lectures"`
	Fits     []bool `json:"fits"`
	// CostCU of the lectures that fit (the "本次预计消耗").
	CostCU float64 `json:"cost_cu"`
	// UsedSec of the session after them.
	UsedSec float64 `json:"used_sec"`
}

// PlanQueue simulates the queue (lecture audio lengths in seconds) from the
// state in in (AudioSec ignored): each lecture that fits uses window,
// balance and credential time; the runtime's setup is paid once.
func PlanQueue(in DispatchInput, audioSecs []float64) PlanResult {
	res := PlanResult{Fits: make([]bool, len(audioSecs))}
	for i, a := range audioSecs {
		in.AudioSec = a
		d := Decide(in)
		if !d.Dispatch {
			continue
		}
		est := EstimateSec(a, in.Rate, in.Defaults, in.SetupDone)
		cost := est / 3600 * max(0, in.CUPerHour)
		res.Fits[i] = true
		res.Lectures++
		res.CostCU += cost
		in.UsedSec += est
		in.SetupDone = true
		if in.Paid {
			in.BalanceCU -= cost
		}
		if in.AuthRemainingSec >= 0 {
			in.AuthRemainingSec = max(0, in.AuthRemainingSec-est)
		}
	}
	res.CostCU = round2(res.CostCU)
	res.UsedSec = round2(in.UsedSec)
	return res
}

// ApproxLectures is "本次还能再处理约 N 节" without a known queue: how many
// lectures of typicalAudioSec still fit (at most 999).
func ApproxLectures(in DispatchInput, typicalAudioSec float64) int {
	if typicalAudioSec <= 0 {
		return 0
	}
	n := 0
	for n < 999 {
		in.AudioSec = typicalAudioSec
		if !Decide(in).Dispatch {
			break
		}
		est := EstimateSec(typicalAudioSec, in.Rate, in.Defaults, in.SetupDone)
		in.UsedSec += est
		in.SetupDone = true
		if in.Paid {
			in.BalanceCU -= est / 3600 * max(0, in.CUPerHour)
		}
		if in.AuthRemainingSec >= 0 {
			in.AuthRemainingSec = max(0, in.AuthRemainingSec-est)
		}
		n++
	}
	return n
}

// TypicalLectureSec is the lecture length ApproxLectures assumes when the
// queue is empty: 50 minutes.
const TypicalLectureSec = 50 * 60
