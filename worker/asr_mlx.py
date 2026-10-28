# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
# Additional permission under AGPL-3.0 section 7: see LICENSE-EXCEPTION.md
"""LectureFlow MLX Whisper worker for Apple Silicon (SPEC-v3 V3.1, V5; SPEC-v2 I7).

Usage (interpreter = the venv of the asr-mlx component, macOS arm64):
    python worker/asr_mlx.py --in job.json --out result.json.tmp
    python worker/asr_mlx.py --selftest --model MODEL_DIR [--precision fp16]

--selftest (ext-step1, the diagnostics' "MLX 推理测试"; the contract of asr.py --selftest):
load the model of MODEL_DIR (the model component's or {data_dir}/models/mlx/{model}; no
download), transcribe 1 s of silence (mlx_whisper has no VAD), print "SELFTEST ok
device=mlx:gpu load_s=... transcribe_s=... segments=N". Without --model, or with an
incomplete model directory: exit 2; MLX / Metal unusable: ERROR_CODE device_unavailable,
exit 2; anything else exit 1. stderr's last line is "ERROR: <reason>" as always.

Same protocol as worker/asr.py and worker/asr_openvino.py (SPEC §6.0):
  * Exit code 0 = success; 2 = input error, or MLX / the Metal GPU is not usable (the
    caller must not retry); anything else (1) = retryable.
  * stdout (UTF-8, one line per event, flushed immediately):
      "PROGRESS 0.42"            fraction transcribed; the first line comes once the model
                                 is loaded, never more than 0.95 before the end,
                                 "PROGRESS 1.00" after result.json is written
      "ERROR_CODE device_unavailable"  just before exit 2 when mlx / mlx_whisper cannot be
                                 imported or Metal is not available
      anything else              plain log line
  * stderr: diagnostics; on failure the last line is "ERROR: <reason>".
  * Only the --out path is written (the caller renames it). No subprocesses, no
    database, no network (the model is the component manager's local model_dir;
    HF_HUB_OFFLINE is set so mlx_whisper never falls back to a download). Cancelling =
    the caller kills the process; nothing is written to --out before the end.

job.json: {"audio", "model", "precision" (fp16|fp32|int8|int4, default fp16: the
           model_dir's own config decides quantization; fp32 = compute in float32),
           "model_dir" (the MLX conversion: config.json + weights.safetensors or
           weights.npz), "device" ("mlx:gpu"), "language", "initial_prompt",
           "word_timestamps" (default true), optional "checkpoint" (as the OpenVINO
           worker: {"path", "job_id", "input_hash", "device_id", "model", "precision",
           "chunk_s"})}. Unknown fields are ignored.
result.json: as worker/asr_openvino.py: "prompt_terms_used", "sentences" (start_ms,
             end_ms, text, low_conf_words, words), "timing" (always; first_compile is
             always false; "postprocess": {"textrepair": ...}, textrepair-int) and
             "parts" when job.checkpoint is set. mlx_whisper gives word
             probabilities, so low_conf_words is filled as with faster-whisper. Without
             word timestamps the sentences have no "words" (Go interpolates, SPEC-v2 P4.4).

What mlx_whisper 0.4.3 cannot do and how it is made up for (SPEC-v2 I7, NOTES.md):
  * No beam search (its decoder raises NotImplementedError): greedy decoding with the
    library's temperature fallback, as the OpenVINO worker.
  * No VAD: the OpenVINO worker's no-VAD word repair is reused (words of segments that
    are digital silence dropped, words > 2 s trimmed to their voiced frames).
  * No progress callback: its tqdm bar (disabled, verbose=None) is replaced during the
    call by a counter of decoded mel frames.
  * transcribe() returns only at the end and gives no partial results: checkpoints use
    chunks exactly like the OpenVINO worker (checkpoint.chunk_s > 0: cut at the quietest
    point within +-10 s of every chunk_s seconds; one transcribe() per chunk;
    initial_prompt on the first chunk only; checkpoint after every chunk but the last).

Sentence splitting, the repeat filter, prompt truncation, result writing and the
checkpoint file are asr_common's (one implementation, I7); the hallucination filter is
asr.keep_segment; audio reading, chunk cuts and the no-VAD word repair are
asr_openvino's. mlx and mlx_whisper are imported lazily, so the module imports anywhere.
"""
from __future__ import annotations

import argparse
import importlib
import json
import os
import sys
import time
import traceback
from collections import namedtuple
from typing import Callable, List, Optional, Sequence, Tuple

from asr_common import (  # noqa: F401  (re-exported: one implementation, I7 / I11)
    EXIT_INPUT, EXIT_OK, EXIT_RETRY, SAMPLE_RATE, ArgParser, InputError, Word, _sentence_dict,
    _word_times, as_word, emit_stdout, fail, format_progress, is_sentence_end, make_timing,
    remove_repeats, setup_stdio, split_sentences, split_word_groups, truncate_prompt,
    write_result,
)
from asr_common import (
    Checkpointer, Resume, audio_ms, clock, encode_segment, parse_checkpoint_job, samples_at,
)
# The hallucination filter of SPEC §6.2 (no_speech_prob > 0.6 and avg_logprob < -1.0),
# the same function as the faster-whisper worker's (asr.py imports only the standard
# library at module level).
from asr import keep_segment  # noqa: F401
# Shared with the OpenVINO worker, the other backend without VAD: wav reading, chunk
# cuts, the no-VAD word repair and the error type with an ERROR_CODE.
from asr_openvino import (
    PROGRESS_CAP, WorkerError, clamp_long_words, drop_silent, frame_rms, make_monotonic,
    next_cut, read_wav, silent_spans, voiced_threshold, words_from_chunks,
)
# The statistical repair of degenerate text before the sentence split (textrepair-int).
import textrepair

# Go: model.BackendMLX, model.DeviceMLXGPU (SPEC-v3 V3.1).
BACKEND_MLX = "mlx"
MLX_DEVICES = ("mlx:gpu",)
PRECISIONS = ("fp16", "fp32", "int8", "int4")
DEFAULT_PRECISION = "fp16"

# The files of an MLX Whisper conversion (mlx_whisper.load_models.load_model).
MODEL_CONFIG = "config.json"
MODEL_WEIGHTS = ("weights.safetensors", "weights.npz")

# Decoding (SPEC §6.2 as far as mlx_whisper supports it): word timestamps, no
# conditioning on the previous window (initial_prompt reaches the first window only, G4),
# initial_prompt only (no hotwords), greedy (no beam search in mlx_whisper), the
# library's temperature fallback and thresholds (the same defaults as faster-whisper's).
TRANSCRIBE_KWARGS = {
    "condition_on_previous_text": False,
    "verbose": None,
}

MEL_FRAMES_PER_SECOND = 100      # mlx_whisper's tqdm counts mel frames (10 ms)


# ---------------------------------------------------------------------------
# Job input
# ---------------------------------------------------------------------------

# checkpoint: asr_common.CheckpointJob or None.
Job = namedtuple("Job", ["audio", "model", "precision", "model_dir", "device", "language",
                         "initial_prompt", "word_timestamps", "checkpoint"],
                 defaults=(None,))


def _opt_str(data: dict, key: str) -> str:
    v = data.get(key)
    if v is None:
        return ""
    if not isinstance(v, str):
        raise InputError("job.%s must be a string" % key)
    return v


def _req_str(data: dict, key: str) -> str:
    v = data.get(key)
    if not isinstance(v, str) or not v:
        raise InputError("job.%s must be a non-empty string" % key)
    return v


def parse_job(data) -> Job:
    if not isinstance(data, dict):
        raise InputError("job.json must contain a JSON object")
    audio = _req_str(data, "audio")
    model = _req_str(data, "model")
    precision = data.get("precision", DEFAULT_PRECISION)
    if precision is None:
        precision = DEFAULT_PRECISION
    if precision not in PRECISIONS:
        raise InputError("unsupported precision %r (expected %s)"
                         % (precision, ", ".join(PRECISIONS)))
    model_dir = _req_str(data, "model_dir")
    device = data.get("device")
    if device not in MLX_DEVICES:
        raise InputError("unsupported device %r (expected %s)"
                         % (device, " or ".join(MLX_DEVICES)))
    language = _opt_str(data, "language")
    prompt = _opt_str(data, "initial_prompt")
    word_ts = data.get("word_timestamps", True)
    if not isinstance(word_ts, bool):
        raise InputError("job.word_timestamps must be true or false")
    checkpoint = parse_checkpoint_job(data.get("checkpoint"))
    return Job(audio, model, precision, model_dir, device, language or None, prompt, word_ts,
               checkpoint)


def load_job(path: str) -> Job:
    try:
        with open(path, encoding="utf-8-sig") as f:
            data = json.load(f)
    except FileNotFoundError:
        raise InputError("job file not found: %s" % path)
    except (OSError, ValueError) as e:
        raise InputError("cannot read job file %s: %s" % (path, e))
    job = parse_job(data)
    if not os.path.isfile(job.audio):
        raise InputError("audio file not found: %s" % job.audio)
    return job


def _nonempty(path: str) -> bool:
    try:
        return os.path.isfile(path) and os.path.getsize(path) > 0
    except OSError:
        return False


def model_ready(model_dir: str) -> bool:
    """config.json and the weights (safetensors or npz) are non-empty files."""
    return _nonempty(os.path.join(model_dir, MODEL_CONFIG)) and \
        any(_nonempty(os.path.join(model_dir, w)) for w in MODEL_WEIGHTS)


# ---------------------------------------------------------------------------
# MLX
# ---------------------------------------------------------------------------

MLX = namedtuple("MLX", ["mx", "whisper", "transcribe_module", "tokenizer", "version"])


def import_mlx() -> MLX:
    """Imports mlx.core and mlx_whisper (lazily) and checks the Metal GPU; any failure is
    device_unavailable, exit 2 (not an Apple Silicon Mac, or a broken component)."""
    os.environ["HF_HUB_OFFLINE"] = "1"        # model_dir is local; never download
    os.environ.setdefault("HF_HUB_DISABLE_TELEMETRY", "1")
    try:
        import mlx.core as mx
        import mlx_whisper
        transcribe_module = importlib.import_module("mlx_whisper.transcribe")
        tokenizer = importlib.import_module("mlx_whisper.tokenizer")
    except Exception as e:  # ImportError, or OSError when the Metal library fails to load
        raise WorkerError("device_unavailable", "cannot import mlx / mlx_whisper: %s: %s"
                          % (type(e).__name__, e), EXIT_INPUT) from e
    metal = getattr(mx, "metal", None)
    available = getattr(metal, "is_available", None)
    try:
        ok = bool(available()) if available is not None else True
    except Exception:
        ok = False
    if not ok:
        raise WorkerError("device_unavailable", "MLX reports no Metal GPU", EXIT_INPUT)
    return MLX(mx, mlx_whisper, transcribe_module, tokenizer,
               str(getattr(mlx_whisper, "__version__", "")))


def model_dtype(mlx: MLX, precision: str):
    return mlx.mx.float32 if precision == "fp32" else mlx.mx.float16


def load_model(mlx: MLX, job: Job):
    """Loads the model through mlx_whisper's own cache (ModelHolder), so the
    transcribe() calls reuse it instead of loading it again."""
    holder = getattr(mlx.transcribe_module, "ModelHolder", None)
    dtype = model_dtype(mlx, job.precision)
    if holder is not None:
        return holder.get_model(job.model_dir, dtype)
    return importlib.import_module("mlx_whisper.load_models").load_model(job.model_dir,
                                                                         dtype=dtype)


def make_token_counter(mlx: MLX, model, language: Optional[str]) -> Callable[[str], int]:
    """Counts tokens like mlx_whisper encodes initial_prompt (" " + prompt.strip())."""
    tok = mlx.tokenizer.get_tokenizer(bool(getattr(model, "is_multilingual", True)),
                                      num_languages=int(getattr(model, "num_languages", 99)),
                                      language=language, task="transcribe")

    def count(text: str) -> int:
        return len(tok.encode(" " + text.strip()))

    return count


def check_language(mlx: MLX, job: Job) -> None:
    languages = getattr(mlx.tokenizer, "LANGUAGES", None) or {}
    if job.language and languages and job.language.lower() not in languages:
        raise InputError("unsupported language %r for model %s" % (job.language, job.model))


# ---------------------------------------------------------------------------
# Progress (mlx_whisper's tqdm bar, replaced during the call)
# ---------------------------------------------------------------------------

class ChunkProgress:
    """Progress over the lecture: chunk(start, end) sets the audio range of the next
    transcribe() call, frames(done, total) reports its decoded mel frames (from the
    replaced tqdm bar), reach(pos) a finished chunk. Capped at PROGRESS_CAP; printed when
    the two-decimal value grows."""

    def __init__(self, duration: float, emit: Callable[[str], None], start: float = 0.0):
        self.duration = duration
        self.emit = emit
        self.base = 0.0
        self.length = 0.0
        self.updates = 0
        self.last = format_progress(self._fraction(start) if start > 0 else 0.0)

    def _fraction(self, pos: float) -> float:
        if self.duration > 0:
            return min(PROGRESS_CAP, pos / self.duration)
        return PROGRESS_CAP

    def _show(self, pos: float) -> None:
        line = format_progress(self._fraction(pos))
        if line > self.last:             # same width ("PROGRESS 0.xx"): string order works
            self.last = line
            self.emit(line)

    def chunk(self, start: float, end: float) -> None:
        self.base, self.length = start, max(0.0, end - start)

    def frames(self, done: float, total: float) -> None:
        self.updates += 1
        if total and total > 0:
            self._show(self.base + self.length * min(1.0, max(0.0, done / float(total))))

    def reach(self, pos: float) -> None:
        self._show(pos)


class _FrameBar:
    """Stands in for tqdm.tqdm inside mlx_whisper.transcribe (only the parts it uses:
    the context manager, update, close)."""

    def __init__(self, progress: ChunkProgress, total=None, **_kw):
        self.progress = progress
        self.total = total or 0
        self.n = 0

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False

    def update(self, n=1):
        self.n += n or 0
        self.progress.frames(self.n, self.total)

    def close(self):
        pass


class _HookedTqdm:
    def __init__(self, progress: ChunkProgress):
        self.progress = progress

    def tqdm(self, *args, **kw):
        if args and "total" not in kw:
            kw["total"] = args[1] if len(args) > 1 else None
        return _FrameBar(self.progress, **kw)


def call_transcribe(mlx: MLX, audio, progress: ChunkProgress, **kwargs) -> dict:
    """mlx_whisper.transcribe(audio, **kwargs) with its progress bar counting into
    progress (the module attribute tqdm is swapped for the call and put back)."""
    mod = mlx.transcribe_module
    had = hasattr(mod, "tqdm")
    old = getattr(mod, "tqdm", None)
    if had:
        mod.tqdm = _HookedTqdm(progress)
    try:
        return mod.transcribe(audio, **kwargs)
    finally:
        if had:
            mod.tqdm = old


# ---------------------------------------------------------------------------
# Words of one transcribe() result
# ---------------------------------------------------------------------------

_SegmentView = namedtuple("_SegmentView", ["no_speech_prob", "avg_logprob"])


def _num(v, default: float) -> float:
    try:
        return float(v)
    except (TypeError, ValueError):
        return default


def chunk_words(result: dict, start: float, end: float, frames, threshold: float,
                word_timestamps: bool, emit: Callable[[str], None]):
    """The words of one mlx_whisper result for the audio [start, end) s of the lecture,
    on the lecture's timeline: segments failing the hallucination filter (keep_segment)
    dropped, words of digitally silent segments dropped, long words trimmed (frames and
    threshold of the whole lecture); not yet ordered nor de-duplicated. A chunk's first
    word gets a leading space when it starts with a letter or digit, so it never sticks
    to the previous chunk's last word. Returns (words, with_words, counts): with_words
    False = pseudo-words from the segment texts (no word timestamps); counts = [segments,
    hallucination-filtered, silent segments, silent words dropped]."""
    segs = [s for s in (result or {}).get("segments") or [] if isinstance(s, dict)]
    kept = [s for s in segs
            if keep_segment(_SegmentView(_num(s.get("no_speech_prob"), 0.0),
                                         _num(s.get("avg_logprob"), 0.0)))]
    chunks = [(start + _num(s.get("start"), 0.0), start + _num(s.get("end"), 0.0),
               str(s.get("text") or "")) for s in kept]
    raw: List[Word] = []
    if word_timestamps:
        for s in kept:
            for w in s.get("words") or []:
                text = str(w.get("word") or "")
                if not text.strip():
                    continue
                prob = w.get("probability")
                raw.append(Word(text, start + _num(w.get("start"), 0.0),
                                start + _num(w.get("end"), 0.0),
                                1.0 if prob is None else _num(prob, 1.0)))
    spans = silent_spans(chunks, frames)
    n_filtered = len(segs) - len(kept)
    if raw:
        first = raw[0].word
        if first[:1].isalnum():
            raw[0] = raw[0]._replace(word=" " + first)
        words = drop_silent(raw, spans)
        n_silent = len(raw) - len(words)
        words = clamp_long_words(words, frames, threshold)
        return words, True, [len(segs), n_filtered, len(spans), n_silent]
    if word_timestamps and any(c[2].strip() for c in chunks):
        emit("no word timestamps returned; sentences from segments")
    silent = set(spans)
    texts = [c for c in chunks if (c[0], c[1]) not in silent]
    return words_from_chunks(texts), False, [len(segs), n_filtered, len(spans),
                                             len(chunks) - len(texts)]


def finish_sentences(words: Sequence[Word], duration: float, with_words: bool, counts,
                     emit: Callable[[str], None], job=None,
                     postprocess: Optional[dict] = None) -> List[dict]:
    """Orders the words (make_monotonic), removes repeats, repairs the text of an English
    lecture (textrepair.repair_transcript with job's whole term list, without window
    starts: lead's brief for textrepair-int), splits sentences; without word timestamps
    the sentences carry no "words". postprocess (optional) receives the repair's
    counters as postprocess["textrepair"]."""
    words = make_monotonic(words, duration)
    n_before = len(words)
    words = remove_repeats(words)
    words, repaired = textrepair.repair_transcript(
        words, job.initial_prompt if job is not None else "",
        job.language if job is not None else None, None, emit)
    if postprocess is not None:
        postprocess["textrepair"] = repaired
    sentences = split_sentences(words)
    if not with_words:
        for s in sentences:
            s.pop("words", None)
    emit("segments %d (%d hallucination-filtered, %d silent), words %d (%d silent dropped, "
         "%d repeated removed), sentences %d"
         % (counts[0], counts[1], counts[2], n_before, counts[3], n_before - len(words),
            len(sentences)))
    return sentences


# ---------------------------------------------------------------------------
# Transcription
# ---------------------------------------------------------------------------

def _seed(mlx: MLX, ms: int) -> None:
    """Seeds MLX's sampler per chunk (temperature fallback samples), so a chunk decodes
    the same in a resumed run as in an uninterrupted one."""
    seed = getattr(getattr(mlx.mx, "random", None), "seed", None)
    if seed is not None:
        seed(int(ms))


def transcribe(mlx: MLX, audio, job: Job, emit: Callable[[str], None],
               count_tokens: Optional[Callable[[str], int]] = None,
               checkpointer: Optional[Checkpointer] = None,
               resume: Optional[Resume] = None, model=None,
               postprocess: Optional[dict] = None) -> Tuple[dict, float]:
    """Returns ({"prompt_terms_used", "sentences"[, "parts"]}, transcribe seconds).

    One transcribe() per chunk from the resume point (or 0) on; one chunk to the end
    when there is no checkpointer or checkpoint.chunk_s is 0 (then no checkpoint file is
    written; a matching checkpoint is still continued from). initial_prompt only reaches
    the first chunk of a run that starts at 0. After every chunk but the last the
    checkpoint is written: resume_ms = the chunk's end, one segment per chunk. Sentences
    keep their "words" exactly when job.word_timestamps is set, except in a single
    uninterrupted chunk whose result has no word timestamps (as the OpenVINO worker)."""
    if resume is not None:
        prompt, terms_used = "", resume.prompt_terms_used
        emit("continuing at %s without initial_prompt (first chunk only)"
             % clock(resume.resume_ms))
    else:
        if count_tokens is None:
            count_tokens = make_token_counter(mlx, model, job.language)
        prompt, terms_used = truncate_prompt(job.initial_prompt, count_tokens)
        if job.initial_prompt:
            emit("initial_prompt: %d terms, %d tokens"
                 % (terms_used, count_tokens(prompt) if prompt else 0))
    chunk_s = checkpointer.chunk_s if checkpointer is not None else 0
    n = len(audio)
    duration = n / float(SAMPLE_RATE)
    frames = frame_rms(audio)
    threshold = voiced_threshold(frames)
    pos = samples_at(resume.resume_ms) if resume is not None else 0
    progress = ChunkProgress(duration, emit, pos / float(SAMPLE_RATE))
    words: List[Word] = list(resume.words) if resume is not None else []
    segments: List[str] = list(resume.segments) if resume is not None else []
    counts = [0, 0, 0, 0]
    n_chunks = 0
    single_with_words = True
    kwargs = dict(TRANSCRIBE_KWARGS, path_or_hf_repo=job.model_dir, language=job.language,
                  word_timestamps=bool(job.word_timestamps), fp16=job.precision != "fp32")
    t0 = time.monotonic()
    while pos < n:
        end = next_cut(frames, pos, n, chunk_s) if chunk_s > 0 else n
        a, b = pos / float(SAMPLE_RATE), end / float(SAMPLE_RATE)
        progress.chunk(a, b)
        _seed(mlx, audio_ms(pos))
        res = call_transcribe(mlx, audio[pos:end], progress,
                              initial_prompt=(prompt or None) if n_chunks == 0 else None,
                              **kwargs)
        n_chunks += 1
        ws, with_words, c = chunk_words(res, a, b, frames, threshold, job.word_timestamps,
                                        emit)
        single_with_words = with_words
        words.extend(ws)
        counts = [x + y for x, y in zip(counts, c)]
        if checkpointer is not None and chunk_s > 0 and end < n:
            segments.append(encode_segment(audio_ms(pos), audio_ms(end), ws))
            checkpointer.save(audio_ms(end), terms_used,
                              checkpointer.parts(resume, audio_ms(end)), segments)
        progress.reach(b)
        pos = end
    transcribe_s = time.monotonic() - t0
    emit("audio duration %.1fs, transcribed in %.1fs (%d chunks, %d progress updates%s)"
         % (duration, transcribe_s, n_chunks, progress.updates,
            ", %d checkpoints written" % checkpointer.writes if checkpointer is not None
            else ""))
    with_words = job.word_timestamps
    if resume is None and n_chunks == 1:
        with_words = with_words and single_with_words
    sentences = finish_sentences(words, duration, with_words, counts, emit, job, postprocess)
    result = {"prompt_terms_used": terms_used, "sentences": sentences}
    if checkpointer is not None:
        result["parts"] = checkpointer.parts(resume, audio_ms(n))
    return result, transcribe_s


def checkpoint_engine(mlx: MLX) -> str:
    """What a checkpoint depends on besides the job: the mlx_whisper version (another
    version may cut words and windows differently, so its checkpoint starts over)."""
    return "mlx-whisper %s" % mlx.version


def run(in_path: str, out_path: str, emit: Callable[[str], None] = emit_stdout) -> None:
    job = load_job(in_path)
    audio = read_wav(job.audio)
    mlx = import_mlx()
    check_language(mlx, job)
    if not model_ready(job.model_dir):
        raise InputError("model %s (%s) is not installed: %s needs %s and %s"
                         % (job.model, job.precision, job.model_dir, MODEL_CONFIG,
                            " or ".join(MODEL_WEIGHTS)))
    checkpointer = resume = None
    if job.checkpoint is not None:
        checkpointer = Checkpointer(job.checkpoint, BACKEND_MLX, emit, checkpoint_engine(mlx))
        resume = checkpointer.load(audio_ms(len(audio)))
    start = samples_at(resume.resume_ms) if resume is not None else 0
    t0 = time.monotonic()
    model = load_model(mlx, job)
    load_s = time.monotonic() - t0
    emit("model %s (%s) loaded on %s in %.1fs (mlx-whisper %s)"
         % (job.model, job.precision, job.device, load_s, mlx.version))
    # the first PROGRESS line: 0.00, or where a resumed run begins
    emit(format_progress(min(PROGRESS_CAP, start / float(len(audio)))))
    post: dict = {}
    result, transcribe_s = transcribe(mlx, audio, job, emit, checkpointer=checkpointer,
                                      resume=resume, model=model, postprocess=post)
    result["timing"] = make_timing(load_s, transcribe_s,
                                   (len(audio) - start) / float(SAMPLE_RATE), False,
                                   postprocess=post)
    write_result(out_path, result)
    emit(format_progress(1.0))


def selftest(model_dir: Optional[str], precision: str = DEFAULT_PRECISION,
             emit: Callable[[str], None] = emit_stdout) -> None:
    """--selftest: the model loads on the Apple GPU and transcribes 1 s of silence."""
    import numpy as np
    if not model_dir:
        raise InputError("--model is required: the MLX model directory")
    if precision not in PRECISIONS:
        raise InputError("unsupported precision %r (expected %s)" % (precision, ", ".join(PRECISIONS)))
    if not model_ready(model_dir):
        raise InputError("model is not installed: %s needs %s and %s"
                         % (model_dir, MODEL_CONFIG, " or ".join(MODEL_WEIGHTS)))
    mlx = import_mlx()
    job = Job("", os.path.basename(os.path.normpath(model_dir)) or "model", precision, model_dir,
              MLX_DEVICES[0], "en", "", False)
    t0 = time.monotonic()
    load_model(mlx, job)
    t1 = time.monotonic()
    audio = np.zeros(SAMPLE_RATE, dtype=np.float32)
    res = mlx.transcribe_module.transcribe(
        audio, **dict(TRANSCRIBE_KWARGS, path_or_hf_repo=model_dir, language="en",
                      word_timestamps=False, fp16=precision != "fp32"))
    n = len([seg for seg in (res or {}).get("segments") or [] if isinstance(seg, dict)])
    emit("SELFTEST ok device=%s load_s=%.1f transcribe_s=%.2f segments=%d"
         % (MLX_DEVICES[0], t1 - t0, time.monotonic() - t1, n))


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def build_parser() -> argparse.ArgumentParser:
    p = ArgParser(prog="asr_mlx.py", description="LectureFlow MLX Whisper worker")
    p.add_argument("--in", dest="in_path", help="job.json")
    p.add_argument("--out", dest="out_path", help="result file to write (e.g. result.json.tmp)")
    p.add_argument("--selftest", action="store_true",
                   help="load the model of --model and transcribe 1 s of silence")
    p.add_argument("--model", dest="model_dir", default=None, help="selftest: the MLX model directory")
    p.add_argument("--precision", default=DEFAULT_PRECISION, help="selftest precision (default fp16)")
    return p


def main(argv: Optional[Sequence[str]] = None) -> int:
    try:
        args = build_parser().parse_args(argv)
        if args.selftest:
            selftest(args.model_dir, args.precision)
            return EXIT_OK
        if not args.in_path or not args.out_path:
            raise InputError("--in and --out are required")
        run(args.in_path, args.out_path)
        return EXIT_OK
    except InputError as e:
        fail("input error: ", e)
        return EXIT_INPUT
    except WorkerError as e:
        if e.__cause__ is not None:
            traceback.print_exception(type(e.__cause__), e.__cause__, e.__cause__.__traceback__,
                                      file=sys.stderr)
        if e.code:
            emit_stdout("ERROR_CODE " + e.code
                        + (" " + json.dumps(e.params, sort_keys=True) if e.params else ""))
        fail("", e)
        return e.exit_code
    except SystemExit:
        raise
    except KeyboardInterrupt:
        fail("", "interrupted")
        return EXIT_RETRY
    except Exception as e:  # every other failure is retryable (SPEC §6.0)
        traceback.print_exc(file=sys.stderr)
        fail("", "%s: %s" % (type(e).__name__, e))
        return EXIT_RETRY


if __name__ == "__main__":
    setup_stdio()
    sys.exit(main())
