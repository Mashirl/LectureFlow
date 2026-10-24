// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import "lectureflow/internal/model"

// ASR checkpoints (power management, v2-step20 freeze; power-plan §3,
// power-brief §4). FROZEN by the lead: the job.json object, the checkpoint
// file and the result's parts. Writers: the workers (task P2); the asr
// stage (task P3) passes CheckpointJob and deletes the file after a
// successful run. The scheduler never reads the file: the policy works on
// job.stats.asr_resume (model.ASRResume).
//
// Rework after the measurement (docs/benchmarks/power-checkpoint.md, lead
// 2026-10-24): faster-whisper resumes on the speech-only time line (scheme
// B: ResumeSeek, raw segments, Engine and VAD; format version 2), and the
// OpenVINO chunk length is 300 s.

// OVCheckpointChunkS is checkpoint.chunk_s of the OpenVINO worker: the
// target chunk length in seconds; the worker cuts at the middle of the
// quietest 0.4 s within ±10 s of each target (the last chunk at least
// 10 s), runs generate() chunk by chunk (initial_prompt on the first chunk
// only) and writes a checkpoint after every chunk but the last. 0 = no
// chunking: the OpenVINO worker writes no checkpoint and a switch between
// OpenVINO devices starts the lecture over.
//
// Q2 (user, 2026-10-24): the value is decided by measurement. Lead ruling
// after docs/benchmarks/power-checkpoint.md §2.6: 300 s (no measurable WER
// cost, sentence counts within ±5 %, a migration redoes at most 5 minutes
// of audio). faster-whisper ignores it (it checkpoints per decoding window
// at no accuracy cost).
const OVCheckpointChunkS = 300

// CheckpointVersion is Checkpoint.Version. 2: scheme B (faster-whisper
// ResumeSeek, raw segments, Engine, VAD); a version 1 file is void.
const CheckpointVersion = 2

// CheckpointJob is the "checkpoint" object of job.json, both workers. The
// worker continues from the file at Path only when its version, JobID,
// InputHash, backend (the worker's own), Engine and VAD all match;
// otherwise it deletes the file and starts over. Every run that receives
// this object returns result.json "parts" (at least one part, the last one
// being this run's DeviceID / Model / Precision).
type CheckpointJob struct {
	Path      string `json:"path"`       // absolute {lecture dir}/model.ASRCheckpointFile
	JobID     string `json:"job_id"`     // the asr job (a rerun is a new job: its checkpoint is void)
	InputHash string `json:"input_hash"` // ctxbuild.ASRInputHashV3(wav, language) (a prep rerun changes the wav)
	DeviceID  string `json:"device_id"`  // this run's device (model.Device*), recorded in the part
	Model     string `json:"model"`
	Precision string `json:"precision"` // the snapshot's precision (faster-whisper: as configured, e.g. auto)
	// ChunkS is OVCheckpointChunkS for openvino, 0 for faster-whisper
	// (ignored there).
	ChunkS int `json:"chunk_s"`
}

// Checkpoint is the file model.ASRCheckpointFile (written as
// "asr.ckpt.json.tmp" + os.replace after complete windows / chunks;
// faster-whisper at most about every 10 s, and once more after its last
// complete window). Segments hold what the worker has transcribed so far,
// before the post-processing that runs once over all segments when the run
// ends (so a resumed result is assembled exactly like an uninterrupted
// one): absolute times from the start of the lecture.
type Checkpoint struct {
	Version   int    `json:"version"` // CheckpointVersion
	JobID     string `json:"job_id"`
	InputHash string `json:"input_hash"`
	Backend   string `json:"backend"` // model.BackendFasterWhisper | model.BackendOpenVINO
	// Engine and VAD (faster-whisper; empty and omitted for OpenVINO): the
	// faster-whisper version ("faster-whisper 1.2.1") and the VAD options
	// (Python repr of VadOptions()). The speech-only time line and the
	// windows on it depend on both, so a checkpoint of another version or
	// options is void.
	Engine string `json:"engine,omitempty"`
	VAD    string `json:"vad,omitempty"`
	// ResumeMS is where the next run continues on the lecture's timeline:
	// the end of the last complete window / chunk (faster-whisper: the end
	// of the last segment or word of the last complete window). Parts and
	// progress use it.
	ResumeMS int64 `json:"resume_ms"`
	// ResumeSeek (faster-whisper; omitted for OpenVINO) is where the next
	// run continues decoding: the seek of the first window not in the
	// checkpoint, in mel frames (10 ms) of the speech-only audio (the
	// lecture without what the VAD removed), not a time of the lecture. The
	// next run decodes the same speech-only audio from there.
	ResumeSeek int64 `json:"resume_seek,omitempty"`
	// PromptTermsUsed is the first part's value (initial_prompt applies to
	// the first window only, SPEC-v2 G4; a resumed part runs without it).
	PromptTermsUsed int `json:"prompt_terms_used"`
	// Parts so far; the last one ends at ResumeMS.
	Parts    []model.ASRPart     `json:"parts"`
	Segments []CheckpointSegment `json:"segments"`
}

// CheckpointSegment is one segment with its words. faster-whisper: every
// segment of the complete windows as the model returned it, before the
// hallucination filter (SPEC §6.2, which needs NoSpeechProb and
// AvgLogprob), with its window's Seek. OpenVINO: one segment per chunk
// (StartMS / EndMS = the chunk), the words after the per-chunk fixes; the
// three faster-whisper fields are omitted.
type CheckpointSegment struct {
	StartMS      int64            `json:"start_ms"`
	EndMS        int64            `json:"end_ms"`
	Seek         *int64           `json:"seek,omitempty"`
	NoSpeechProb *float64         `json:"no_speech_prob,omitempty"`
	AvgLogprob   *float64         `json:"avg_logprob,omitempty"`
	Words        []CheckpointWord `json:"words"`
}

// CheckpointWord is asr_common.Word as JSON (seconds; as_word reads it).
// Probability is 1.0 for OpenVINO (no per-word confidence, lead ruling 6).
type CheckpointWord struct {
	Word        string  `json:"word"`
	Start       float64 `json:"start"`
	End         float64 `json:"end"`
	Probability float64 `json:"probability"`
}
