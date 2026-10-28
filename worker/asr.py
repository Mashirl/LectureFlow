# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
# Additional permission under AGPL-3.0 section 7: see LICENSE-EXCEPTION.md
"""LectureFlow ASR worker (SPEC §6.0, §6.2, §17.2, §17.3).

Usage:
    python worker/asr.py --in job.json --out result.json.tmp
    python worker/asr.py --selftest [--device cuda|cpu] [--compute-type float16]
    python worker/asr.py --check-cudnn

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
           "precision", "chunk_s" (ignored here)}, power brief §4),
           optional "redecode" (bool, default true: decode degenerate windows again)}
result.json: {"prompt_terms_used": int,
              "sentences": [{"start_ms", "end_ms", "text", "low_conf_words": [{"i", "w"}],
                             "words": [{"w", "s", "e"}]}],
              "timing": {...} only when job.timing is true,
              "parts": [...] only when job.checkpoint is set}
             words (SPEC-v2 P4.2): the sentence's words with integer ms times from the
             start of the lecture; see _word_times().
             timing (brief 3b §4): {"load_s", "transcribe_s", "audio_s" (the audio this
             run transcribed), "first_compile" (always false here), "peak_working_set_mb",
             "postprocess": {"redecode": RedecodeStats, "textrepair": the stats of
             textrepair.repair_transcript}}; see asr_common.make_timing().
             parts (power brief §4): [{"from_ms", "to_ms", "device_id", "model",
             "precision"}], see asr_common.Checkpointer.

Checkpoints (power brief §4, docs/benchmarks/power-checkpoint.md §3): with
job.checkpoint the worker writes the checkpoint file after complete 30-second decoding
windows and, when a matching file is there, decodes the same speech-only audio from the
first window not in it (scheme B), so the resumed result is the uninterrupted one; see
WindowCheckpoints. The file holds windows after their redecode, with the redecode counters
("redecode"); one written before the redecode existed that has a degenerate window is not
continued (checkpoint_needs_redecode). Without job.checkpoint nothing changes.

Degenerate windows (redecode, 2026-10-27, user-approved): a window the main pass wrote all
lower case without punctuation or all upper case (asr_common.window_style) is decoded once
more with the text before it as initial_prompt and replaced when the second decode passes
accept_redecode(); see Redecoder. The VAD runs once per run, before the main pass
(speech_audio), and the redecode reuses its speech-only audio (textrepair-int).

Text repair (textrepair-int, user decision 2026-10-27 03:30): after remove_repeats the
words of an English lecture go through textrepair.repair_transcript (sentence ends and
capitals in what is still degenerate) before the sentence split; §6.2's splitting rules
are unchanged.

--selftest (SPEC §17.1 "CUDA 推理"): load the tiny model, transcribe 1 s of silence with
VAD off; stdout "SELFTEST ok device=… compute_type=… load_s=… transcribe_s=… segments=N".
--check-cudnn (ext-step1, the diagnostics' cuDNN row): load the cuDNN library of the
nvidia-cudnn-* packages (else the system's) and print "CUDNN ok version=9.1.0"; exit 2
with "ERROR: …" when it cannot be loaded. Neither imports faster_whisper for the check.

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
from types import SimpleNamespace
from typing import Callable, Dict, List, Optional, Sequence, Tuple

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
from asr_common import (
    DEGENERATE_UNPUNCTUATED, DEGENERATE_UPPERCASE, degeneration, window_style,
)
# The statistical repair of degenerate text before the sentence split (textrepair-int).
import textrepair

# Transcription parameters (SPEC §6.2). Only initial_prompt is used, never hotwords.
TRANSCRIBE_KWARGS = {
    "vad_filter": True,
    "word_timestamps": True,
    "beam_size": 5,
    "condition_on_previous_text": False,
}
# Every decode of a lecture (the main pass, a resumed one, a redecode) gets the
# speech-only audio of speech_audio(), the VAD step of transcribe(vad_filter=True) done
# here once (textrepair-int, lead ruling 2026-10-27: the redecode reuses it), and its
# times are mapped back by restore_times(), so the result is transcribe(vad_filter=True)'s.
DECODE_KWARGS = dict(TRANSCRIBE_KWARGS, vad_filter=False)
# Resuming (scheme B): the same call from a seek on (clip_timestamps).
RESUME_KWARGS = DECODE_KWARGS

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


class CheckFailed(Exception):
    """A check of the environment failed (exit 2: retrying does not help)."""


def cudnn_library_patterns() -> Tuple[str, ...]:
    """File names of the cuDNN library on this platform (cuDNN 9, then 8)."""
    if sys.platform == "win32":
        return ("cudnn64_9.dll", "cudnn64_*.dll")
    if sys.platform == "darwin":
        return ()
    return ("libcudnn.so.9", "libcudnn.so.8", "libcudnn.so*")


def cudnn_candidates(dirs: Optional[Sequence[str]] = None) -> List[str]:
    """Where cuDNN may be: the nvidia-cudnn-* packages' directories (setup_cuda_libs),
    then the name the system's loader finds."""
    if dirs is None:
        dirs = setup_cuda_libs()
    out: List[str] = []
    for pattern in cudnn_library_patterns():
        for d in dirs:
            for path in sorted(glob.glob(os.path.join(d, pattern))):
                if path not in out:
                    out.append(path)
    if sys.platform != "darwin":
        import ctypes.util
        name = ctypes.util.find_library("cudnn64_9" if sys.platform == "win32" else "cudnn")
        if name and name not in out:
            out.append(name)
    return out


def cudnn_version_text(v: int) -> str:
    """cudnnGetVersion() as "major.minor.patch" (cuDNN 9: 90100 = 9.1.0; 8: 8902 = 8.9.2)."""
    if v >= 90000:
        major, rest = divmod(v, 10000)
    else:
        major, rest = divmod(v, 1000)
    minor, patch = divmod(rest, 100)
    return "%d.%d.%d" % (major, minor, patch)


def check_cudnn(emit: Callable[[str], None] = emit_stdout,
                candidates: Optional[Sequence[str]] = None, load=None) -> None:
    """--check-cudnn: the first candidate that loads and has cudnnGetVersion gives
    "CUDNN ok version=…"; none does -> CheckFailed (exit 2)."""
    import ctypes
    if load is None:
        load = ctypes.CDLL
    if candidates is None:
        candidates = cudnn_candidates()
    tried: List[str] = []
    for path in candidates:
        try:
            lib = load(path)
        except OSError as e:
            tried.append("%s: %s" % (os.path.basename(path), e))
            continue
        fn = getattr(lib, "cudnnGetVersion", None)
        if fn is None:
            tried.append("%s: no cudnnGetVersion" % os.path.basename(path))
            continue
        fn.restype = ctypes.c_size_t
        emit("CUDNN ok version=%s" % cudnn_version_text(int(fn())))
        return
    detail = "; ".join(tried) if tried else "not installed (nvidia-cudnn-cu12)"
    raise CheckFailed("cuDNN cannot be loaded: %s" % detail)


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
# redecode (2026-10-27): the settings switch "修复退化的转写窗口" (Go: kv asr.redecode);
# true (the default) decodes degenerate windows again, false only counts them.
Job = namedtuple("Job", ["audio", "model", "compute_type", "language", "initial_prompt",
                         "device", "cpu_threads", "timing", "checkpoint", "redecode"],
                 defaults=(False, None, True))


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
    redecode = data.get("redecode", True)
    if not isinstance(redecode, bool):
        raise InputError("job.redecode must be true or false")
    return Job(audio, model, compute_type, language or None, prompt or "", device, threads,
               timing, checkpoint, redecode)


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


def onnxruntime_telemetry_off() -> None:
    """onnxruntime (the Silero VAD of faster-whisper) logs usage events (ProcessInfo,
    SessionCreation, ...) to Windows' diagnostic data unless they are disabled before its
    first InferenceSession (NOTES.md "无遥测"). Idempotent; nothing when it is missing."""
    try:
        import onnxruntime
        onnxruntime.disable_telemetry_events()
    except Exception:  # not installed, or a build without the call: nothing to turn off
        pass


def speech_audio(audio):
    """The VAD step of transcribe(vad_filter=True), done here for every run (the main
    pass, a resumed one, the redecode): the speech chunks of the whole lecture (default
    VadOptions) and the speech-only audio made of them, exactly as transcribe() makes it."""
    import numpy as np
    from faster_whisper.vad import VadOptions, collect_chunks, get_speech_timestamps
    onnxruntime_telemetry_off()
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

    transcribe_job hands over each window once it is final (add_window: after the
    degenerate-window redecode, Redecoder), with the next window's seek. The file holds
    the complete windows' final raw segments (seek, no_speech_prob, avg_logprob, words
    with probability; the hallucination filter, remove_repeats and the sentence split run
    over all segments at the end), resume_seek = the first window not in it, resume_ms =
    the end of its last segment or word on the lecture's timeline (parts, progress), and
    "redecode" = the redecode counters of the windows in it (RedecodeStats), so a resumed
    run neither decodes those windows again nor counts them twice. It is written after a
    complete window when the last write is at least CHECKPOINT_INTERVAL_S old, and once
    more by finish() when the last complete window was not written yet. The last window
    of a run (no next seek) is never in it. Segments without an integer seek (never from
    faster-whisper) give no resume point: then no checkpoint is written (disable()).
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
        self.redecode = resume.redecode if resume is not None else None
        self.unwritten = False             # a complete window is not in the file yet
        self.last_write: Optional[float] = None
        self.off = False

    def disable(self) -> None:
        if not self.off:
            self.off = True
            self.ck.emit("segments without seek: no checkpoints in this run")

    def add_window(self, win: "Window", redecode: dict) -> None:
        """A final window (times on the lecture's timeline). It is complete when the next
        window's seek is known (win.next_seek); redecode = the counters including it."""
        if self.off or win.next_seek is None:
            return
        end_ms = 0
        for seg, words in win.segs:
            end_ms = max([end_ms, _to_ms(seg.end)] + [_to_ms(w.end) for w in words])
            self.segments.append(encode_segment(_to_ms(seg.start), _to_ms(seg.end), words,
                                                win.seek, seg.no_speech_prob, seg.avg_logprob))
        self.resume_ms = min(self.total_ms, max(self.resume_ms, end_ms))
        self.resume_seek = win.next_seek
        self.redecode = dict(redecode)
        self.unwritten = True
        now = _monotonic()
        if self.last_write is None or now - self.last_write >= CHECKPOINT_INTERVAL_S:
            self.write(now)

    def write(self, now: float) -> None:
        if self.resume_ms <= 0:
            return                         # nothing a resumed run could skip
        self.ck.save(self.resume_ms, self.terms_used, self.ck.parts(self.resume, self.resume_ms),
                     self.segments, self.resume_seek, self.redecode)
        self.last_write = now
        self.unwritten = False

    def finish(self) -> None:
        """The transcription ended: write the last complete window if it is not written."""
        if not self.off and self.unwritten:
            self.write(_monotonic())


# ---------------------------------------------------------------------------
# Degenerate windows decoded again (redecode: worker-builder, 2026-10-27; the user
# approved it as a second pass of the §6.2 decoding on 2026-10-27, NOTES)
# ---------------------------------------------------------------------------
#
# large-v3 writes about half of the windows of a fast lecture all lower case without
# punctuation or all upper case (asr_common.window_style). Decoding such a window once
# more, alone (its speech-only audio, clip as below), with the text before it as
# initial_prompt brings back normal punctuation and case: the prompt shows the decoder
# how the transcript is written, which condition_on_previous_text=False otherwise never
# does after the first window (.claude/research/asr-degeneration.md, 13 of 13 windows).
# The main pass is unchanged; the second decode replaces a window only when it passes
# accept_redecode() (else the original stays: SPEC §0.1, keep rather than change for the
# worse), at most once per window. Only the text before a window is used, so the result
# does not depend on where a run was interrupted (checkpoints hold final windows).

# Off only for tests and measurements.
REDECODE = True
# faster-whisper keeps the last max_length // 2 - 1 = 223 tokens of a prompt
# (WhisperModel.get_prompt); a redecode prompt stays within that, so nothing is cut off.
REDECODE_PROMPT_TOKEN_LIMIT = 223
# The previous text's share of the prompt (its last whole words); the rest goes to the
# leading terms of the job's initial_prompt (see redecode_prompt()).
REDECODE_CONTEXT_TOKEN_LIMIT = 120
# accept_redecode(): words after remove_repeats, new / old, within these bounds ...
REDECODE_MIN_WORD_RATIO = 0.85
REDECODE_MAX_WORD_RATIO = 1.15
# ... and its first words are no run of this many words copied from the prompt that the
# original lacks (the decoder echoing its prompt starts the window with it; a phrase the
# lecturer repeats later in the window is no echo).
REDECODE_ECHO_WORDS = 8
# A window whose text can be the prompt: normal style, this many words or more, at
# least one sentence end (is_style_source()).
STYLE_MIN_WORDS = 10
# Mel frames: 10 ms = 160 samples; a decoding window is 30 s = 3000 frames.
HOP_SAMPLES = 160
WINDOW_FRAMES = 3000

# One decoding window: seek (its first mel frame on the speech-only time line), segs
# [(segment, [Word])] with times on the lecture's timeline, next_seek (the next window's
# seek; None for the last window), then what the redecode found and did: kind
# (asr_common.degeneration of its kept words, "" = normal), redecoded, accepted and
# redecode_s (seconds the second decode took, the VAD it needed first included).
Window = namedtuple("Window", ["seek", "segs", "next_seek", "kind", "redecoded", "accepted",
                               "redecode_s"], defaults=("", False, False, 0.0))


def kept_words(segs) -> List[Word]:
    """The words of the segments the hallucination filter keeps, in order."""
    return [w for seg, ws in segs if keep_segment(seg) for w in ws]


def is_style_source(words: Sequence[Word]) -> bool:
    """Whether a window's text can show the decoder the normal style: not degenerate,
    STYLE_MIN_WORDS words or more, at least one sentence end."""
    st = window_style(words)
    return not st.kind and st.words >= STYLE_MIN_WORDS and st.sentence_ends > 0


def checkpoint_windows(raw) -> List[List[Word]]:
    """The kept words of each window of a checkpoint's raw segments (grouped by seek)."""
    out: List[List[Word]] = []
    key = _NO_WINDOW
    for s in raw:
        if s.seek != key:
            out.append([])
            key = s.seek
        if keep_segment(s):
            out[-1].extend(s.words)
    return out


def checkpoint_style(resume: Optional[Resume]) -> Optional[List[Word]]:
    """The words of the last window of the checkpoint that can be a prompt: what the
    uninterrupted run would prompt the next degenerate window with (None: none)."""
    style = None
    if resume is not None:
        for words in checkpoint_windows(resume.raw):
            if is_style_source(words):
                style = words
    return style


def checkpoint_needs_redecode(resume: Resume) -> bool:
    """A checkpoint whose windows were not decoded again (written before the redecode
    existed: no "redecode" counters; or with the switch off: "skipped") that has a
    degenerate window: a run with the switch on does not continue it, since it would
    keep that window as it is."""
    done = resume.redecode is not None and not resume.redecode.get("skipped")
    return not done and any(degeneration(words) for words in checkpoint_windows(resume.raw))


def _tail_words(words: Sequence[Word], count_tokens: Callable[[str], int], limit: int) -> str:
    """The longest run of the last whole words whose text fits in limit tokens."""
    text = ""
    for i in range(len(words) - 1, -1, -1):
        cand = "".join(w.word for w in words[i:]).strip()
        if count_tokens(cand) > limit:
            break
        text = cand
    return text


def redecode_prompt(style: Sequence[Word], terms: str, count_tokens: Callable[[str], int]) -> str:
    """initial_prompt of a redecode: the leading terms of the job's (truncated)
    initial_prompt, then the end of the style window's text (its last whole words, at
    most REDECODE_CONTEXT_TOKEN_LIMIT tokens), together within REDECODE_PROMPT_TOKEN_LIMIT
    tokens as faster-whisper counts them. The text comes last because the decoder
    continues from the end of the prompt; the terms keep the window's spelling of the
    course's terms as the first window's (the main pass gives them to the first window
    only)."""
    context = _tail_words(style, count_tokens, REDECODE_CONTEXT_TOKEN_LIMIT)
    if not terms:
        return context
    room = REDECODE_PROMPT_TOKEN_LIMIT - count_tokens(context + " .") if context else \
        REDECODE_PROMPT_TOKEN_LIMIT
    head, _ = truncate_prompt(terms, count_tokens, room)
    if not head:
        return context
    return head + ". " + context if context else head


def _ngrams(words: Sequence[str], n: int) -> set:
    keys = [_repeat_key(w) for w in words]
    return {tuple(keys[i:i + n]) for i in range(len(keys) - n + 1)}


def accept_redecode(old_segs, new_segs, prompt: str) -> Tuple[bool, str]:
    """Whether a window's second decode replaces the first (ok, why not). It must read
    normally (not degenerate), keep about the same words (after remove_repeats, between
    REDECODE_MIN_WORD_RATIO and REDECODE_MAX_WORD_RATIO of the original's: no dropped
    speech, nothing invented), repeat no more than the original, need no higher
    temperature than the original (no fallback) and not begin with REDECODE_ECHO_WORDS
    words in a row from the prompt that the original does not have."""
    old, new = kept_words(old_segs), kept_words(new_segs)
    if not new:
        return False, "no words"
    kind = degeneration(new)
    if kind:
        return False, "still " + kind
    old_kept, new_kept = remove_repeats(old), remove_repeats(new)
    n_old, n_new = len(old_kept), len(new_kept)
    if not REDECODE_MIN_WORD_RATIO * n_old <= n_new <= REDECODE_MAX_WORD_RATIO * n_old:
        return False, "%d words instead of %d" % (n_new, n_old)
    if len(new) - n_new > len(old) - n_old:
        return False, "repeats a phrase"
    t_old = max([float(getattr(s, "temperature", 0.0) or 0.0) for s, _ in old_segs] + [0.0])
    t_new = max([float(getattr(s, "temperature", 0.0) or 0.0) for s, _ in new_segs] + [0.0])
    if t_new > t_old:
        return False, "temperature fallback to %.1f" % t_new
    head = [w.word for w in new[:2 * REDECODE_ECHO_WORDS]]
    copied = (_ngrams(head, REDECODE_ECHO_WORDS) & _ngrams(prompt.split(), REDECODE_ECHO_WORDS)) \
        - _ngrams([w.word for w in old], REDECODE_ECHO_WORDS)
    if copied:
        return False, "repeats the prompt"
    return True, ""


def _shifted(seg, words, offset: float, seek: int):
    """A segment of a redecode (times from the start of its piece) with those of its
    words that belong to the window, on the speech-only time line, as the window's
    segment (seek)."""
    end = words[-1].end if words else seg.end
    return SimpleNamespace(
        seek=seek, start=seg.start + offset, end=end + offset, text="".join(w.word for w in words),
        temperature=getattr(seg, "temperature", 0.0), avg_logprob=seg.avg_logprob,
        compression_ratio=getattr(seg, "compression_ratio", 0.0),
        no_speech_prob=seg.no_speech_prob,
        words=[SimpleNamespace(word=w.word, start=w.start + offset, end=w.end + offset,
                               probability=w.probability) for w in words])


class RedecodeStats:
    """The redecode counters of a run (result.json timing.postprocess.redecode, the
    checkpoint's "redecode"): windows (decoding windows), unpunctuated / uppercase (of
    them degenerate), redecoded (decoded again: a degenerate window with no normal text
    in the whole lecture to prompt it with is not), accepted (replaced by the second
    decode) and redecode_s (seconds of the second decodes, with the VAD they need);
    "skipped": true when the switch was off for this run (job.redecode false: the
    degenerate windows are counted, none is decoded again). A resumed run starts from the
    checkpoint's counters, so they cover the lecture."""

    KEYS = ("windows", DEGENERATE_UNPUNCTUATED, DEGENERATE_UPPERCASE, "redecoded", "accepted")

    def __init__(self, start: Optional[dict] = None, skipped: bool = False):
        start = start or {}
        self.counts: Dict[str, int] = {k: int(start.get(k, 0)) for k in self.KEYS}
        self.seconds = float(start.get("redecode_s", 0.0))
        self.skipped = skipped

    def add(self, win: Window) -> None:
        self.counts["windows"] += 1
        if win.kind:
            self.counts[win.kind] += 1
        self.counts["redecoded"] += int(win.redecoded)
        self.counts["accepted"] += int(win.accepted)
        self.seconds += win.redecode_s

    def as_dict(self) -> dict:
        out = {"skipped": True} if self.skipped else {}
        out.update(self.counts, redecode_s=round(self.seconds, 2))
        return out

    def line(self) -> str:
        c = self.counts
        return ("redecode%s: %d of %d windows degenerate (%d unpunctuated, %d uppercase), "
                "%d decoded again, %d accepted, %.1fs"
                % (" off (job.redecode false)" if self.skipped else "",
                   c[DEGENERATE_UNPUNCTUATED] + c[DEGENERATE_UPPERCASE], c["windows"],
                   c[DEGENERATE_UNPUNCTUATED], c[DEGENERATE_UPPERCASE], c["redecoded"],
                   c["accepted"], self.seconds))


class Redecoder:
    """The second decoding pass over degenerate windows (see above).

    feed() takes each window of the main pass once it is complete (the next window's seek
    known) and returns the windows that are final now, in order. A degenerate window is
    decoded again (redecode()) with the text of the last window before it that can be a
    prompt (is_style_source, after its own redecode); when no such window precedes it
    (the start of a lecture), it waits for the first one after it. flush() returns what
    still waits when the decoding ends (kept as decoded). style: the prompt text a
    resumed run starts with (checkpoint_style). enabled False (the switch off): every
    window is only judged (kind) and final at once.
    """

    def __init__(self, model, job: Job, emit: Callable[[str], None], audio,
                 count_tokens: Optional[Callable[[str], int]] = None, terms: str = "",
                 style: Optional[List[Word]] = None, enabled: bool = True):
        self.model = model
        self.job = job
        self.emit = emit
        self.audio = audio
        self._count = count_tokens
        self.terms = terms
        self.style = style
        self.enabled = enabled
        self.waiting: List[Window] = []
        # (chunks, speech-only audio): transcribe_job hands over its run's VAD
        # (textrepair-int); made here on first use only without it
        self._speech = None

    def count_tokens(self, text: str) -> int:
        if self._count is None:
            self._count = make_token_counter(self.model)
        return self._count(text)

    def feed(self, win: Window) -> List[Window]:
        words = kept_words(win.segs)
        win = win._replace(kind=degeneration(words))
        if not self.enabled:
            return [win]
        if self.style is None and (win.kind or self.waiting) and not is_style_source(words):
            self.waiting.append(win)
            return []
        out = []
        if self.waiting:
            # win is the first window that can be a prompt: it prompts those before it
            # that no window before them can
            for w in self.waiting:
                out.append(self._finish(w, self.style or words))
            self.waiting = []
        out.append(self._finish(win, self.style))
        return out

    def flush(self) -> List[Window]:
        out, self.waiting = self.waiting, []
        return out

    def _finish(self, win: Window, style: Optional[List[Word]]) -> Window:
        if win.kind and style is not None:
            win = self.redecode(win, style)
        words = kept_words(win.segs)
        if is_style_source(words):
            self.style = words
        return win

    def speech(self):
        if self._speech is None:
            t0 = time.monotonic()
            self._speech = speech_audio(self.audio)
            self.emit("VAD of the whole lecture for the redecode: %d speech chunks, %.1fs"
                      % (len(self._speech[0]), time.monotonic() - t0))
        return self._speech

    def redecode(self, win: Window, style: Sequence[Word]) -> Window:
        """Decodes the window once more with the style text as initial_prompt, otherwise
        as the main pass: the same 30 s of speech-only audio the main pass decoded it
        from (from its seek), of which the words before the next window's seek (the
        word's middle) are the window's; maps the times back like transcribe(); keeps
        the result when accept_redecode() does.

        Not cutting the audio at the next window's seek matters: decoding audio that
        stops in the middle of speech makes the decoder add words that are not there
        (measured: "Thank you." at the cut in 4 of 100 windows, words squeezed into the
        last 20 ms). Segments are taken until one reaches the next window, so the
        decoder does not go on with audio that belongs to the next window."""
        t0 = time.monotonic()
        chunks, speech = self.speech()
        frames = len(speech) // HOP_SAMPLES
        end = min(win.seek + WINDOW_FRAMES, frames)
        cut = end if win.next_seek is None else min(win.next_seek, end)
        if not chunks or cut <= win.seek:
            return win
        prompt = redecode_prompt(style, self.terms, self.count_tokens)
        piece = speech[win.seek * HOP_SAMPLES:end * HOP_SAMPLES]
        segments, _ = self.model.transcribe(piece, language=self.job.language,
                                            initial_prompt=prompt or None, **RESUME_KWARGS)
        offset, limit = win.seek / 100.0, (cut - win.seek) / 100.0
        segs = []
        for seg in segments:
            if seg.start >= limit:
                break
            # A segment the decoder itself rates as probably no speech is left out: after
            # the window's last word it decodes the rest of the 30 s once more, without the
            # prompt, and in a pause makes up "Thank you very much." (measured: 1 of 59
            # windows, no_speech_prob 0.96, avg_logprob -0.87, which the §6.2 filter keeps).
            if seg.no_speech_prob <= NO_SPEECH_PROB_MAX:
                words = [w for w in (seg.words or []) if (w.start + w.end) / 2.0 < limit]
                if words:
                    segs.append(_shifted(seg, words, offset, win.seek))
            if seg.end >= limit:
                break
        segs = [(s, [as_word(w) for w in s.words]) for s in restore_times(segs, chunks)]
        ok, why = accept_redecode(win.segs, segs, prompt)
        seconds = time.monotonic() - t0
        self.emit("window at %s (seek %d) %s: decoded again in %.1fs, %s"
                  % (clock(_to_ms(win.segs[0][0].start)), win.seek, win.kind, seconds,
                     "accepted" if ok else "kept the original (%s)" % why))
        return win._replace(segs=segs if ok else win.segs, redecoded=True, accepted=ok,
                            redecode_s=seconds)


def _is_seek(v) -> bool:
    return isinstance(v, int) and not isinstance(v, bool)


def transcribe_job(model, audio, job: Job, emit: Callable[[str], None],
                   count_tokens: Optional[Callable[[str], int]] = None,
                   checkpointer: Optional[Checkpointer] = None,
                   resume: Optional[Resume] = None,
                   postprocess: Optional[dict] = None) -> dict:
    """Transcribes audio (16 kHz mono) and returns {"prompt_terms_used", "sentences"}.

    The VAD of transcribe(vad_filter=True) is done here on the whole lecture
    (speech_audio; textrepair-int: once per run, the redecode reuses it), the
    speech-only audio is decoded (DECODE_KWARGS) and the times are mapped back as
    transcribe() maps them (restore_times): the result is transcribe(vad_filter=True)'s.

    checkpointer (from job.checkpoint): checkpoints after complete windows
    (WindowCheckpoints) and "parts" in the result; the transcription itself is the same
    as without it. resume (Checkpointer.load): the same speech-only audio is decoded from
    resume.resume_seek on (clip_timestamps) without initial_prompt (first window only,
    G4); the checkpoint's segments come first, then the hallucination filter,
    remove_repeats, the text repair and the sentence split run over all of them, as in
    the uninterrupted run. Progress starts at resume_ms / duration.

    Degenerate windows are decoded once more (Redecoder, REDECODE) before the windows go
    into the checkpoint and the result. The words are then repaired (textrepair.
    repair_transcript: English lectures, the job's whole term list, the windows' start
    times). postprocess (a dict, optional) receives the counters for the timing block:
    postprocess["redecode"] (RedecodeStats.as_dict) and postprocess["textrepair"].
    Segments without seek (never faster-whisper's) are taken as they come: no windows,
    no redecode, no redecode counters, the repair without window starts.
    """
    if count_tokens is None and resume is None:
        count_tokens = make_token_counter(model)
    if resume is not None:
        prompt, terms_used = "", resume.prompt_terms_used
        emit("continuing at %s (seek %d) without initial_prompt (first window only)"
             % (clock(resume.resume_ms), resume.resume_seek))
    else:
        prompt, terms_used = truncate_prompt(job.initial_prompt, count_tokens)
        if job.initial_prompt:
            emit("initial_prompt: %d terms, %d tokens"
                 % (terms_used, count_tokens(prompt) if prompt else 0))
    t0 = time.monotonic()
    chunks, vad_audio = speech_audio(audio)
    emit("VAD of the whole lecture: %d speech chunks, %.1fs of speech, %.1fs"
         % (len(chunks), len(vad_audio) / float(SAMPLE_RATE), time.monotonic() - t0))
    if chunks:
        kwargs = dict(DECODE_KWARGS)
        if resume is not None:
            kwargs["clip_timestamps"] = [resume.resume_seek / 100.0]
        segments, info = model.transcribe(
            vad_audio,
            language=job.language,
            initial_prompt=prompt or None,
            **kwargs,
        )
        segments = restore_times(segments, chunks)
    else:
        segments, info = iter(()), None
    # info.duration is the speech-only audio's: progress is measured on the lecture's
    duration = len(audio) / float(SAMPLE_RATE)
    emit("audio duration %.1fs, language %s"
         % (duration, getattr(info, "language", None) or job.language))

    words: List[Word] = []
    # the decoding windows' start times on the lecture's time line (their first kept
    # word), for the text repair; None once segments come without seek
    win_starts: Optional[List[float]] = []
    if resume is not None:
        words = [w for s in resume.raw if keep_segment(s) for w in s.words]
        win_starts = [ws[0].start for ws in checkpoint_windows(resume.raw) if ws]
    windows = None
    if checkpointer is not None:
        windows = WindowCheckpoints(checkpointer, resume, terms_used, audio_ms(len(audio)))
    on = REDECODE and job.redecode
    stats = RedecodeStats(resume.redecode if resume is not None else None, skipped=not on)
    redecoder = None
    if REDECODE:
        # The terms of a redecode prompt: the job's prompt as the first window got it
        # (a resumed run truncates it again, with the same result).
        terms = prompt
        if on and resume is not None and job.initial_prompt:
            if count_tokens is None:
                count_tokens = make_token_counter(model)
            terms, _ = truncate_prompt(job.initial_prompt, count_tokens)
        redecoder = Redecoder(model, job, emit, audio, count_tokens, terms,
                              checkpoint_style(resume) if on else None, enabled=on)
        redecoder._speech = (chunks, vad_audio)   # this run's VAD, done already
    n_segments = n_dropped = 0
    progress = 0.0
    if resume is not None:
        progress = min(1.0, resume.resume_ms / 1000.0 / duration) if duration > 0 else 1.0
        emit(format_progress(progress))

    def commit(win: Window) -> None:
        nonlocal n_dropped
        first = None
        for seg, seg_words in win.segs:
            if keep_segment(seg):
                words.extend(seg_words)
                if first is None and seg_words:
                    first = seg_words[0].start
            else:
                n_dropped += 1
        if first is not None and win_starts is not None:
            win_starts.append(first)
        stats.add(win)
        if windows is not None:
            windows.add_window(win, stats.as_dict())

    def complete(win: Window) -> None:
        for final in (redecoder.feed(win) if redecoder is not None else [win]):
            commit(final)

    cur: Optional[Window] = None
    plain = False                          # segments without seek: no windows
    for seg in segments:
        n_segments += 1
        seg_words = [as_word(w) for w in (seg.words or [])]
        seek = getattr(seg, "seek", None)
        if not plain and not _is_seek(seek):
            plain = True
            win_starts = None
            if cur is not None:
                complete(cur)
                cur = None
            if redecoder is not None:
                for final in redecoder.flush():
                    commit(final)
                redecoder = None
                emit("segments without seek: degenerate windows are not decoded again")
            if windows is not None:
                windows.disable()
        if plain:
            if keep_segment(seg):
                words.extend(seg_words)
            else:
                n_dropped += 1
        else:
            if cur is not None and seek != cur.seek:
                complete(cur._replace(next_seek=seek))
                cur = None
            if cur is None:
                cur = Window(seek, [], None)
            cur.segs.append((seg, seg_words))
        if duration > 0:
            progress = max(progress, min(1.0, float(seg.end) / duration))
        else:
            progress = 1.0
        emit(format_progress(progress))
    if cur is not None:
        complete(cur)
    if redecoder is not None:
        for final in redecoder.flush():
            commit(final)
    if windows is not None:
        windows.finish()

    n_before = len(words)
    words = remove_repeats(words)
    words, repaired = textrepair.repair_transcript(words, job.initial_prompt, job.language,
                                                   win_starts or None, emit)
    sentences = split_sentences(words)
    emit("segments %d (dropped %d no-speech), words %d (removed %d repeated), sentences %d"
         % (n_segments, n_dropped, n_before, n_before - len(words), len(sentences)))
    if redecoder is not None:
        emit(stats.line())
        if postprocess is not None:
            postprocess["redecode"] = stats.as_dict()
    if postprocess is not None:
        postprocess["textrepair"] = repaired
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
        if resume is not None and REDECODE and job.redecode and \
                checkpoint_needs_redecode(resume):
            # its degenerate windows were not decoded again (written before the redecode
            # or with the switch off): start over, so the result is the one of an
            # uninterrupted run with the switch on
            checkpointer.discard("its degenerate windows were not decoded again")
            resume = None
    t0 = time.monotonic()
    model = load_model(job.model, job.device, job.compute_type, job.cpu_threads, emit)
    t1 = time.monotonic()
    post: dict = {}
    result = transcribe_job(model, audio, job, emit, checkpointer=checkpointer, resume=resume,
                            postprocess=post)
    if job.timing:
        done = len(audio) - (samples_at(resume.resume_ms) if resume is not None else 0)
        result["timing"] = make_timing(t1 - t0, time.monotonic() - t1,
                                       done / float(SAMPLE_RATE), False, postprocess=post)
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
    p.add_argument("--check-cudnn", dest="check_cudnn", action="store_true",
                   help='print "CUDNN ok version=…" when the cuDNN library loads')
    return p


def main(argv: Optional[Sequence[str]] = None) -> int:
    try:
        args = build_parser().parse_args(argv)
        if args.check_cudnn:
            check_cudnn()
            return EXIT_OK
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
    except CheckFailed as e:
        _fail("", e)
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
