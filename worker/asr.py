# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
# Additional permission under AGPL-3.0 section 7: see LICENSE-EXCEPTION.md
"""LectureFlow ASR worker (SPEC §6.0, §6.2, §17.2, §17.3).

Usage:
    python worker/asr.py --in job.json --out result.json.tmp
    python worker/asr.py --selftest [--device cuda|cpu] [--compute-type float16]

Protocol (SPEC §6.0):
  * Exit code 0 = success, 2 = input error (caller must not retry),
    anything else (1) = retryable error.
  * stdout (UTF-8, one line per event, flushed immediately):
      "PROGRESS 0.42"            fraction of the audio transcribed (two decimals)
      "STATUS downloading_model" printed once before a first-time model download
      anything else              plain log line
  * stderr (UTF-8): diagnostics; on failure the last line is "ERROR: <reason>".
  * Only the --out path is written (the caller renames it). The worker never
    starts subprocesses and never touches the database.

job.json: {"audio", "model", "compute_type", "language", "initial_prompt",
           optional "device" (cuda|cpu, default cuda), optional "cpu_threads",
           optional "timing" (bool, default false),
           optional "checkpoint" ({"path", "job_id", "input_hash", "device_id", "model",
           "precision", "chunk_s" (ignored here)}, power brief §4)}
result.json: {"prompt_terms_used": int,
              "sentences": [{"start_ms", "end_ms", "text", "low_conf_words": [{"i", "w"}],
                             "words": [{"w", "s", "e"}]}],
              "timing": {...} only when job.timing is true,
              "parts": [...] only when job.checkpoint is set}
             words (SPEC-v2 P4.2): the sentence's words with integer ms times from the
             start of the lecture; see _word_times().
             timing (brief 3b §4): {"load_s", "transcribe_s", "audio_s" (the audio this
             run transcribed), "first_compile" (always false here), "peak_working_set_mb"};
             see asr_common.make_timing().
             parts (power brief §4): [{"from_ms", "to_ms", "device_id", "model",
             "precision"}], see asr_common.Checkpointer.

Checkpoints (power brief §4, docs/benchmarks/power-checkpoint.md §3): with
job.checkpoint the worker writes the checkpoint file after complete 30-second decoding
windows and, when a matching file is there, decodes the same speech-only audio from the
first window not in it (scheme B), so the resumed result is the uninterrupted one; see
WindowCheckpoints. Without job.checkpoint nothing changes.

Sentence splitting, the repeat filter, prompt truncation, result writing and the
checkpoint file live in worker/asr_common.py, shared with worker/asr_openvino.py
(SPEC-v2 I7).
"""
from __future__ import annotations

import argparse
import glob
import json
import os
import struct
import sys
import time
import traceback
from collections import namedtuple
from typing import Callable, List, Optional, Sequence, Tuple

# Shared with worker/asr_openvino.py (SPEC-v2 I7: one implementation of the sentence
# splitting, repeat filter, prompt truncation and result writing). Re-exported here
# under the old names so asr.split_sentences etc. keep working.
from asr_common import (  # noqa: F401
    ABBREVIATIONS, EXIT_INPUT, EXIT_OK, EXIT_RETRY, LOW_CONF_THRESHOLD,
    MAX_SENTENCE_SECONDS, MAX_SENTENCE_WORDS, PROMPT_SEPARATOR, PROMPT_TOKEN_LIMIT,
    REPEAT_MAX_NGRAM, REPEAT_MIN_COUNT, SAMPLE_RATE, SHORT_SENTENCE_SECONDS,
    SHORT_SENTENCE_WORDS, InputError, Word, _CLOSERS, _EDGE_PUNCT, _INITIAL_RE, _core,
    _duration, _ends_with_clause_mark, _repeat_key, _sentence_dict, _to_ms, _too_long,
    _word_times, as_word, emit_stdout, format_progress, is_sentence_end, make_timing,
    remove_repeats, split_sentences, split_word_groups, truncate_prompt, write_result,
)
from asr_common import ArgParser as _ArgParser, fail as _fail, setup_stdio as _setup_stdio
from asr_common import (
    BACKEND_FASTER_WHISPER, CHECKPOINT_INTERVAL_S, Checkpointer, Resume, audio_ms, clock,
    encode_segment, parse_checkpoint_job, samples_at,
)

# Transcription parameters (SPEC §6.2). Only initial_prompt is used, never hotwords.
TRANSCRIBE_KWARGS = {
    "vad_filter": True,
    "word_timestamps": True,
    "beam_size": 5,
    "condition_on_previous_text": False,
}
# Resuming (scheme B): transcribe() gets the speech-only audio made by speech_audio().
RESUME_KWARGS = dict(TRANSCRIBE_KWARGS, vad_filter=False)

# Hallucination filters (SPEC §6.2).
NO_SPEECH_PROB_MAX = 0.6
AVG_LOGPROB_MIN = -1.0

# Concrete compute types accepted from the caller. "auto" (and "default") must be
# resolved by Go before the job is written (SPEC §17.2).
COMPUTE_TYPES = frozenset(
    {"float16", "int8_float16", "int8", "int8_float32", "int8_bfloat16", "int16",
     "bfloat16", "float32"}
)
DEVICES = ("cuda", "cpu")


# ---------------------------------------------------------------------------
# Hallucination filters
# ---------------------------------------------------------------------------

def keep_segment(seg) -> bool:
    """Drops segments with no_speech_prob > 0.6 and avg_logprob < -1.0 (SPEC §6.2)."""
    return not (seg.no_speech_prob > NO_SPEECH_PROB_MAX and seg.avg_logprob < AVG_LOGPROB_MIN)


# ---------------------------------------------------------------------------
# initial_prompt token counting
# ---------------------------------------------------------------------------

def make_token_counter(model) -> Callable[[str], int]:
    """Counts tokens the way faster-whisper encodes initial_prompt (" " + prompt.strip())."""
    tok = model.hf_tokenizer

    def count(text: str) -> int:
        return len(tok.encode(" " + text.strip(), add_special_tokens=False).ids)

    return count


# ---------------------------------------------------------------------------
# Environment: CUDA libraries, CPU threads
# ---------------------------------------------------------------------------

def _nvidia_lib_dirs() -> List[str]:
    sub = "bin" if sys.platform == "win32" else "lib"
    bases = [os.path.join(sys.prefix, "Lib", "site-packages"),
             os.path.join(sys.prefix, "lib",
                          "python%d.%d" % sys.version_info[:2], "site-packages")]
    try:
        import site
        bases += list(site.getsitepackages())
    except Exception:
        pass
    dirs: List[str] = []
    seen = set()
    for base in bases:
        for d in sorted(glob.glob(os.path.join(base, "nvidia", "*", sub))):
            key = os.path.normcase(os.path.abspath(d))
            if os.path.isdir(d) and key not in seen:
                seen.add(key)
                dirs.append(d)
    return dirs


_cuda_lib_dirs: Optional[List[str]] = None


def setup_cuda_libs() -> List[str]:
    """Makes the cuBLAS/cuDNN libraries from the nvidia-* pip packages loadable.

    Must run before faster_whisper/ctranslate2 is imported (ctranslate2 on Windows needs
    cuBLAS 12 / cuDNN 9, which come from nvidia-cublas-cu12 / nvidia-cudnn-cu12).
    Windows: every existing site-packages/nvidia/*/bin directory is registered with
    os.add_dll_directory and prepended to this process's PATH. Linux: nvidia/*/lib
    directories are prepended to LD_LIBRARY_PATH and cuBLAS/cuDNN are preloaded (best
    effort). Only this process's environment changes. Idempotent; returns the dirs.
    """
    global _cuda_lib_dirs
    if _cuda_lib_dirs is not None:
        return _cuda_lib_dirs
    dirs = _cuda_lib_dirs = _nvidia_lib_dirs()
    if not dirs:
        return dirs
    if sys.platform == "win32":
        for d in dirs:
            try:
                os.add_dll_directory(d)
            except OSError:
                pass
        os.environ["PATH"] = os.pathsep.join(dirs + [os.environ.get("PATH", "")])
    elif sys.platform.startswith("linux"):
        os.environ["LD_LIBRARY_PATH"] = os.pathsep.join(
            dirs + [os.environ.get("LD_LIBRARY_PATH", "")])
        import ctypes
        for pattern in ("libcublasLt.so*", "libcublas.so*", "libcudnn.so*"):
            for d in dirs:
                for lib in sorted(glob.glob(os.path.join(d, pattern))):
                    try:
                        ctypes.CDLL(lib, mode=ctypes.RTLD_GLOBAL)
                    except OSError:
                        pass
    return dirs


def _physical_cores_windows() -> Optional[int]:
    import ctypes
    from ctypes import wintypes
    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    relation_processor_core = 0
    length = wintypes.DWORD(0)
    kernel32.GetLogicalProcessorInformationEx(relation_processor_core, None, ctypes.byref(length))
    if length.value == 0:
        return None
    buf = ctypes.create_string_buffer(length.value)
    if not kernel32.GetLogicalProcessorInformationEx(relation_processor_core, buf,
                                                     ctypes.byref(length)):
        return None
    count, offset = 0, 0
    while offset + 8 <= length.value:
        relationship, size = struct.unpack_from("<II", buf.raw, offset)
        if size == 0:
            break
        if relationship == relation_processor_core:
            count += 1
        offset += size
    return count or None


def _physical_cores_linux() -> Optional[int]:
    try:
        with open("/proc/cpuinfo", encoding="utf-8") as f:
            text = f.read()
    except OSError:
        return None
    cores = set()
    for block in text.split("\n\n"):
        fields = {}
        for line in block.splitlines():
            if ":" in line:
                k, v = line.split(":", 1)
                fields[k.strip()] = v.strip()
        if "core id" in fields:
            cores.add((fields.get("physical id", "0"), fields["core id"]))
    return len(cores) or None


def physical_cores() -> int:
    n = None
    try:
        if sys.platform == "win32":
            n = _physical_cores_windows()
        elif sys.platform.startswith("linux"):
            n = _physical_cores_linux()
    except Exception:
        n = None
    return n or os.cpu_count() or 1


def default_cpu_threads(device: str) -> int:
    """SPEC §17.3: 4 threads in cuda mode, physical cores - 1 in cpu mode."""
    if device == "cuda":
        return 4
    return max(1, physical_cores() - 1)


# ---------------------------------------------------------------------------
# Job input
# ---------------------------------------------------------------------------

# timing (brief 3b §4, opt-in here): write result.json's "timing" block. Optional
# with default False, so result.json and Job(...) with seven fields stay as before.
# checkpoint (power brief §4): asr_common.CheckpointJob or None (default).
Job = namedtuple("Job", ["audio", "model", "compute_type", "language", "initial_prompt",
                         "device", "cpu_threads", "timing", "checkpoint"],
                 defaults=(False, None))


def validate_compute_type(ct) -> str:
    if ct == "auto":
        raise InputError("compute_type 'auto' must be resolved by the caller (SPEC §17.2)")
    if not isinstance(ct, str) or ct not in COMPUTE_TYPES:
        raise InputError("unsupported compute_type %r (expected one of %s)"
                         % (ct, ", ".join(sorted(COMPUTE_TYPES))))
    return ct


def validate_device(dev) -> str:
    if dev not in DEVICES:
        raise InputError("unsupported device %r (expected cuda or cpu)" % (dev,))
    return dev


def parse_job(data) -> Job:
    if not isinstance(data, dict):
        raise InputError("job.json must contain a JSON object")
    audio = data.get("audio")
    if not isinstance(audio, str) or not audio:
        raise InputError("job.audio must be a non-empty string")
    model = data.get("model")
    if not isinstance(model, str) or not model:
        raise InputError("job.model must be a non-empty string")
    compute_type = validate_compute_type(data.get("compute_type"))
    language = data.get("language")
    if language is not None and not isinstance(language, str):
        raise InputError("job.language must be a string")
    prompt = data.get("initial_prompt")
    if prompt is not None and not isinstance(prompt, str):
        raise InputError("job.initial_prompt must be a string")
    device = validate_device(data.get("device", "cuda") or "cuda")
    threads = data.get("cpu_threads")
    if threads is None:
        threads = default_cpu_threads(device)
    elif isinstance(threads, bool) or not isinstance(threads, int) or threads < 1:
        raise InputError("job.cpu_threads must be a positive integer")
    timing = data.get("timing", False)
    if not isinstance(timing, bool):
        raise InputError("job.timing must be true or false")
    checkpoint = parse_checkpoint_job(data.get("checkpoint"))
    return Job(audio, model, compute_type, language or None, prompt or "", device, threads,
               timing, checkpoint)


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


def decode_audio(path: str):
    """Decodes the audio in-process (PyAV via faster-whisper) to 16 kHz mono float32."""
    from faster_whisper import decode_audio as fw_decode
    try:
        return fw_decode(path, sampling_rate=SAMPLE_RATE)
    except MemoryError:
        raise
    except Exception as e:
        raise InputError("cannot decode audio %s: %s" % (path, e))


# ---------------------------------------------------------------------------
# Model loading
# ---------------------------------------------------------------------------

def _has_model_files(path) -> bool:
    return bool(path) and os.path.isfile(os.path.join(path, "model.bin"))


def resolve_model_path(model_name: str, emit: Callable[[str], None],
                       download: Optional[Callable] = None) -> str:
    """Returns a local model directory, downloading the model on first use.

    A cached model is loaded from its local directory without any network request
    (SPEC §17.6). When it is not cached, "STATUS downloading_model" is printed before
    the download starts (SPEC §17.2).
    """
    if os.path.isdir(model_name):
        if not _has_model_files(model_name):
            raise InputError("model directory has no model.bin: %s" % model_name)
        return model_name
    if download is None:
        from faster_whisper.utils import available_models, download_model as download
        if "/" not in model_name and model_name not in available_models():
            raise InputError("unknown model %r (expected one of %s)"
                             % (model_name, ", ".join(available_models())))
    try:
        path = download(model_name, local_files_only=True)
    except Exception:
        path = None
    if _has_model_files(path):
        return path
    emit("STATUS downloading_model")
    path = download(model_name)
    if not _has_model_files(path):
        raise RuntimeError("model download finished but model.bin is missing in %s" % path)
    return path


def load_model(model_name: str, device: str, compute_type: str, cpu_threads: int,
               emit: Callable[[str], None]):
    path = resolve_model_path(model_name, emit)
    from faster_whisper import WhisperModel
    t0 = time.monotonic()
    try:
        model = WhisperModel(path, device=device, compute_type=compute_type,
                             cpu_threads=cpu_threads)
    except ValueError as e:
        # e.g. a compute type the device does not support: retrying cannot help.
        raise InputError("cannot load model %s (device=%s, compute_type=%s): %s"
                         % (model_name, device, compute_type, e))
    emit("model %s loaded in %.1fs (device=%s, compute_type=%s, cpu_threads=%d)"
         % (model_name, time.monotonic() - t0, device, compute_type, cpu_threads))
    return model


# ---------------------------------------------------------------------------
# Transcription
# ---------------------------------------------------------------------------

_NO_WINDOW = object()

# The clock of the checkpoint interval (tests replace it).
_monotonic = time.monotonic


def checkpoint_engine() -> Tuple[str, str]:
    """(engine, vad) a faster-whisper checkpoint depends on: the speech-only time line and
    the windows on it come from this faster-whisper version and these VAD options (the
    defaults transcribe(vad_filter=True) uses); another version or options start over."""
    import faster_whisper
    from faster_whisper.vad import VadOptions
    return "faster-whisper %s" % faster_whisper.__version__, repr(VadOptions())


def speech_audio(audio):
    """The VAD step of transcribe(vad_filter=True), done here (resuming, scheme B): the
    speech chunks of the whole lecture (default VadOptions) and the speech-only audio
    made of them, exactly as transcribe() makes it."""
    import numpy as np
    from faster_whisper.vad import VadOptions, collect_chunks, get_speech_timestamps
    chunks = get_speech_timestamps(audio, VadOptions())
    pieces, _ = collect_chunks(audio, chunks)
    return chunks, np.concatenate(pieces, axis=0)


def restore_times(segments, chunks):
    """Segment and word times from the speech-only time line back onto the lecture's (the
    last step of transcribe(vad_filter=True))."""
    from faster_whisper.transcribe import restore_speech_timestamps
    return restore_speech_timestamps(segments, chunks, SAMPLE_RATE)


class WindowCheckpoints:
    """faster-whisper checkpoints after complete 30-second decoding windows (scheme B,
    docs/benchmarks/power-checkpoint.md §3).

    faster-whisper 1.2 (WhisperModel.generate_segments) decodes the speech-only audio
    (the lecture without what the VAD removed) window by window and yields each window's
    segments one after another, all carrying seek = the window's first mel frame on that
    speech-only time line (later windows have larger seeks). With
    condition_on_previous_text=False nothing is carried from one window to the next
    (initial_prompt reaches the first decoded window only, G4). So a window is complete
    as soon as a segment of another window arrives, and a run that decodes the same
    speech-only audio from that window's seek (clip_timestamps), without initial_prompt,
    produces the rest exactly as the uninterrupted run (measured: identical at 25 %,
    50 %, 75 %).

    The file holds the complete windows' raw segments (seek, no_speech_prob,
    avg_logprob, words with probability; the hallucination filter, remove_repeats and
    the sentence split run over all segments at the end), resume_seek = the first window
    not in it, and resume_ms = the end of its last segment or word on the lecture's
    timeline (parts, progress). It is written after a complete window when the last
    write is at least CHECKPOINT_INTERVAL_S old, and once more by finish() when the last
    complete window was not written yet. Segments without an integer seek (never from
    faster-whisper) give no resume point: then no checkpoint is written.
    """

    def __init__(self, checkpointer: Checkpointer, resume: Optional[Resume], terms_used: int,
                 total_ms: int):
        self.ck = checkpointer
        self.resume = resume
        self.terms_used = terms_used
        self.total_ms = total_ms
        self.resume_ms = resume.resume_ms if resume is not None else 0
        self.resume_seek = resume.resume_seek if resume is not None else None
        self.segments = list(resume.segments) if resume is not None else []
        self.pending: List[str] = []       # segments of the current window, encoded
        self.pending_end_ms = 0
        self.key = _NO_WINDOW
        self.unwritten = False             # a complete window is not in the file yet
        self.last_write: Optional[float] = None
        self.off = False

    def add(self, seg, words: Sequence[Word]) -> None:
        """Adds a segment (times on the lecture's timeline); a segment of a new window first
        completes the current one."""
        if self.off:
            return
        seek = getattr(seg, "seek", None)
        if not isinstance(seek, int) or isinstance(seek, bool):
            self.off = True
            self.ck.emit("segments without seek: no checkpoints in this run")
            return
        if self.key is not _NO_WINDOW and seek != self.key:
            self.commit(seek)
        self.key = seek
        self.pending_end_ms = max([self.pending_end_ms, _to_ms(seg.end)]
                                  + [_to_ms(w.end) for w in words])
        self.pending.append(encode_segment(_to_ms(seg.start), _to_ms(seg.end), words, seek,
                                           seg.no_speech_prob, seg.avg_logprob))

    def commit(self, next_seek: int) -> None:
        """The current window is complete; next_seek is the first incomplete one's."""
        self.segments.extend(self.pending)
        self.pending = []
        self.resume_ms = min(self.total_ms, max(self.resume_ms, self.pending_end_ms))
        self.pending_end_ms = 0
        self.resume_seek = next_seek
        self.unwritten = True
        now = _monotonic()
        if self.last_write is None or now - self.last_write >= CHECKPOINT_INTERVAL_S:
            self.write(now)

    def write(self, now: float) -> None:
        if self.resume_ms <= 0:
            return                         # nothing a resumed run could skip
        self.ck.save(self.resume_ms, self.terms_used, self.ck.parts(self.resume, self.resume_ms),
                     self.segments, self.resume_seek)
        self.last_write = now
        self.unwritten = False

    def finish(self) -> None:
        """The transcription ended: write the last complete window if it is not written."""
        if not self.off and self.unwritten:
            self.write(_monotonic())


def transcribe_job(model, audio, job: Job, emit: Callable[[str], None],
                   count_tokens: Optional[Callable[[str], int]] = None,
                   checkpointer: Optional[Checkpointer] = None,
                   resume: Optional[Resume] = None) -> dict:
    """Transcribes audio (16 kHz mono) and returns {"prompt_terms_used", "sentences"}.

    checkpointer (from job.checkpoint): checkpoints after complete windows
    (WindowCheckpoints) and "parts" in the result; the transcription itself is the same
    as without it. resume (Checkpointer.load): the VAD of transcribe(vad_filter=True)
    is done here on the whole lecture, the same speech-only audio is decoded from
    resume.resume_seek on (clip_timestamps, vad_filter=False) without initial_prompt
    (first window only, G4) and its times are mapped back as transcribe() maps them;
    the checkpoint's segments come first, then the hallucination filter, remove_repeats
    and the sentence split run over all of them, as in the uninterrupted run. Progress
    starts at resume_ms / duration.
    """
    if resume is not None:
        prompt, terms_used = "", resume.prompt_terms_used
        emit("continuing at %s (seek %d) without initial_prompt (first window only)"
             % (clock(resume.resume_ms), resume.resume_seek))
        t0 = time.monotonic()
        chunks, vad_audio = speech_audio(audio)
        emit("VAD of the whole lecture: %d speech chunks, %.1fs of speech, %.1fs"
             % (len(chunks), len(vad_audio) / float(SAMPLE_RATE), time.monotonic() - t0))
        if chunks:
            segments, info = model.transcribe(
                vad_audio,
                language=job.language,
                initial_prompt=None,
                clip_timestamps=[resume.resume_seek / 100.0],
                **RESUME_KWARGS,
            )
            segments = restore_times(segments, chunks)
        else:
            segments, info = iter(()), None
        duration = len(audio) / float(SAMPLE_RATE)
    else:
        if count_tokens is None:
            count_tokens = make_token_counter(model)
        prompt, terms_used = truncate_prompt(job.initial_prompt, count_tokens)
        if job.initial_prompt:
            emit("initial_prompt: %d terms, %d tokens"
                 % (terms_used, count_tokens(prompt) if prompt else 0))
        segments, info = model.transcribe(
            audio,
            language=job.language,
            initial_prompt=prompt or None,
            **TRANSCRIBE_KWARGS,
        )
        duration = float(getattr(info, "duration", 0.0) or 0.0)
        if duration <= 0:
            duration = len(audio) / float(SAMPLE_RATE)
    emit("audio duration %.1fs, language %s" % (duration, getattr(info, "language", None)))

    words: List[Word] = []
    if resume is not None:
        words = [w for s in resume.raw if keep_segment(s) for w in s.words]
    windows = None
    if checkpointer is not None:
        windows = WindowCheckpoints(checkpointer, resume, terms_used, audio_ms(len(audio)))
    n_segments = n_dropped = 0
    progress = 0.0
    if resume is not None:
        progress = min(1.0, resume.resume_ms / 1000.0 / duration) if duration > 0 else 1.0
        emit(format_progress(progress))
    for seg in segments:
        n_segments += 1
        kept = keep_segment(seg)
        if kept or windows is not None:
            seg_words = [as_word(w) for w in (seg.words or [])]
        if windows is not None:
            windows.add(seg, seg_words)
        if kept:
            words.extend(seg_words)
        else:
            n_dropped += 1
        if duration > 0:
            progress = max(progress, min(1.0, float(seg.end) / duration))
        else:
            progress = 1.0
        emit(format_progress(progress))
    if windows is not None:
        windows.finish()

    n_before = len(words)
    words = remove_repeats(words)
    sentences = split_sentences(words)
    emit("segments %d (dropped %d no-speech), words %d (removed %d repeated), sentences %d"
         % (n_segments, n_dropped, n_before, n_before - len(words), len(sentences)))
    result = {"prompt_terms_used": terms_used, "sentences": sentences}
    if checkpointer is not None:
        result["parts"] = checkpointer.parts(resume, audio_ms(len(audio)))
        emit("checkpoints written: %d" % checkpointer.writes)
    return result


def run(in_path: str, out_path: str, emit: Callable[[str], None] = emit_stdout) -> None:
    job = load_job(in_path)
    setup_cuda_libs()
    audio = decode_audio(job.audio)
    checkpointer = resume = None
    if job.checkpoint is not None:
        engine, vad = checkpoint_engine()
        checkpointer = Checkpointer(job.checkpoint, BACKEND_FASTER_WHISPER, emit, engine, vad)
        resume = checkpointer.load(audio_ms(len(audio)))
    t0 = time.monotonic()
    model = load_model(job.model, job.device, job.compute_type, job.cpu_threads, emit)
    t1 = time.monotonic()
    result = transcribe_job(model, audio, job, emit, checkpointer=checkpointer, resume=resume)
    if job.timing:
        done = len(audio) - (samples_at(resume.resume_ms) if resume is not None else 0)
        result["timing"] = make_timing(t1 - t0, time.monotonic() - t1,
                                       done / float(SAMPLE_RATE), False)
    write_result(out_path, result)
    emit(format_progress(1.0))


def selftest(device: str, compute_type: Optional[str],
             emit: Callable[[str], None] = emit_stdout) -> None:
    """SPEC §17.1 "CUDA 推理": load the tiny model and transcribe 1 s of silence.

    vad_filter is off here: with it, silence would be removed before the model runs
    and the GPU would never be exercised.
    """
    validate_device(device)
    if compute_type is None:
        compute_type = "float16" if device == "cuda" else "int8"
    validate_compute_type(compute_type)
    setup_cuda_libs()
    import numpy as np
    t0 = time.monotonic()
    model = load_model("tiny", device, compute_type, default_cpu_threads(device), emit)
    t1 = time.monotonic()
    audio = np.zeros(SAMPLE_RATE, dtype=np.float32)
    kwargs = dict(TRANSCRIBE_KWARGS, vad_filter=False)
    segments, _ = model.transcribe(audio, language="en", **kwargs)
    n = len(list(segments))
    emit("SELFTEST ok device=%s compute_type=%s load_s=%.1f transcribe_s=%.2f segments=%d"
         % (device, compute_type, t1 - t0, time.monotonic() - t1, n))


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def build_parser() -> argparse.ArgumentParser:
    p = _ArgParser(prog="asr.py", description="LectureFlow faster-whisper worker")
    p.add_argument("--in", dest="in_path", help="job.json")
    p.add_argument("--out", dest="out_path", help="result file to write (e.g. result.json.tmp)")
    p.add_argument("--selftest", action="store_true",
                   help="load the tiny model and transcribe 1 s of silence")
    p.add_argument("--device", default="cuda", help="selftest device: cuda or cpu")
    p.add_argument("--compute-type", dest="compute_type", default=None,
                   help="selftest compute type (default float16 on cuda, int8 on cpu)")
    return p


def main(argv: Optional[Sequence[str]] = None) -> int:
    try:
        args = build_parser().parse_args(argv)
        if args.selftest:
            selftest(args.device, args.compute_type)
            return EXIT_OK
        if not args.in_path or not args.out_path:
            raise InputError("--in and --out are required")
        run(args.in_path, args.out_path)
        return EXIT_OK
    except InputError as e:
        _fail("input error: ", e)
        return EXIT_INPUT
    except SystemExit:
        raise
    except KeyboardInterrupt:
        _fail("", "interrupted")
        return EXIT_RETRY
    except Exception as e:  # every other failure is retryable (SPEC §6.0)
        traceback.print_exc(file=sys.stderr)
        _fail("", "%s: %s" % (type(e).__name__, e))
        return EXIT_RETRY


if __name__ == "__main__":
    _setup_stdio()
    sys.exit(main())
