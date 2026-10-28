# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
# Additional permission under AGPL-3.0 section 7: see LICENSE-EXCEPTION.md
"""Shared parts of the LectureFlow ASR workers (SPEC §6.0, §6.2; SPEC-v2 I7).

worker/asr.py (faster-whisper) and worker/asr_openvino.py (OpenVINO GenAI) import
everything here, so both workers split sentences, filter repeats, truncate the
initial prompt and write result.json with the same code (I7: one implementation,
not two copies). Standard library only: the two workers run in different virtual
environments (.venv and .venv-ov).

Contents: exit codes and InputError; Word / as_word; sentence splitting
(split_word_groups, split_sentences, _word_times); degenerate windows (window_style,
degeneration: all lower case without punctuation, all upper case); remove_repeats;
truncate_prompt; format_progress / emit_stdout; write_result; the timing block of
result.json; the
checkpoints of both workers (power management: job.json "checkpoint", asr.ckpt.json,
result.json "parts"); the command-line helpers both workers use.

No usage reporting: importing this module runs telemetry_off() (below), before any worker
imports a third-party library.
"""
from __future__ import annotations

import argparse
import json
import math
import os
import re
import string
import sys
from collections import namedtuple
from typing import Callable, Iterable, List, Optional, Sequence, Tuple

# ---------------------------------------------------------------------------
# No usage reporting (privacy audit before 3.2.0, NOTES.md "无遥测")
# ---------------------------------------------------------------------------

# The same list as internal/pyenv TelemetryOff (the Go side sets it for every Python
# child; this covers a worker started any other way) and worker/colab_remote.py.
TELEMETRY_OFF_ENV = {
    "HF_HUB_DISABLE_TELEMETRY": "1",
    "HF_HUB_DISABLE_IMPLICIT_TOKEN": "1",
    "HF_XET_TELEMETRY_ENABLED": "0",
    "DO_NOT_TRACK": "1",
}

# Packages whose only job is usage reporting. openvino-telemetry has no environment switch
# (only a consent file in the user's profile): `import openvino` sent a Google Analytics
# event (openvino.tools.ovc) and nncf reports its compression calls through it. Both fall
# back to built-in no-op stubs when the import fails, which a None entry in sys.modules makes
# it do.
TELEMETRY_MODULES = ("openvino_telemetry",)


def telemetry_off(environ=None, modules=None) -> None:
    """Turns off the usage reporting of the libraries the workers load: TELEMETRY_OFF_ENV
    (huggingface_hub reads it when it is imported) and TELEMETRY_MODULES made unimportable.
    Must run before openvino / nncf / huggingface_hub are imported; idempotent."""
    environ = os.environ if environ is None else environ
    modules = sys.modules if modules is None else modules
    environ.update(TELEMETRY_OFF_ENV)
    for name in TELEMETRY_MODULES:
        modules[name] = None


telemetry_off()

EXIT_OK = 0
EXIT_RETRY = 1
EXIT_INPUT = 2

SAMPLE_RATE = 16000

PROMPT_TOKEN_LIMIT = 200
PROMPT_SEPARATOR = ", "

REPEAT_MIN_COUNT = 3      # a phrase repeated this many times in a row (or more) ...
REPEAT_MAX_NGRAM = 12     # ... with a phrase length of 1..12 words is collapsed to one copy

LOW_CONF_THRESHOLD = 0.5

# Sentence splitting (SPEC §6.2).
ABBREVIATIONS = frozenset(
    {"e.g", "i.e", "etc", "vs", "dr", "mr", "mrs", "ms", "prof", "fig", "eq", "no", "approx"}
)
MAX_SENTENCE_WORDS = 60
MAX_SENTENCE_SECONDS = 30.0
SHORT_SENTENCE_WORDS = 2       # fewer words than this ...
SHORT_SENTENCE_SECONDS = 1.0   # ... and shorter than this -> merged into the previous sentence

# Closing quotes/brackets that may follow sentence punctuation, e.g.  done."
_CLOSERS = "\"')]}”’»"
_EDGE_PUNCT = string.punctuation + "“”‘’«»"
_INITIAL_RE = re.compile(r"[A-Z]\.")


class InputError(Exception):
    """Invalid job input; mapped to exit code 2 (not retried)."""


# ---------------------------------------------------------------------------
# Words and sentence splitting
# ---------------------------------------------------------------------------

Word = namedtuple("Word", ["word", "start", "end", "probability"])


def as_word(w) -> Word:
    """Accepts a dict, a faster-whisper Word dataclass, or a Word tuple."""
    if isinstance(w, Word):
        return w
    if isinstance(w, dict):
        prob = w.get("probability", 1.0)
        return Word(str(w["word"]), float(w["start"]), float(w["end"]),
                    1.0 if prob is None else float(prob))
    prob = getattr(w, "probability", 1.0)
    return Word(str(w.word), float(w.start), float(w.end), 1.0 if prob is None else float(prob))


def _core(text: str) -> str:
    """Word text without surrounding whitespace and trailing closing quotes/brackets."""
    return text.strip().rstrip(_CLOSERS)


def is_sentence_end(text: str) -> bool:
    """True when a word ends a sentence (SPEC §6.2).

    A word ends a sentence when it ends with '.', '?' or '!' (optionally followed by
    closing quotes/brackets). A trailing '.' does not end the sentence when the word is
    an abbreviation from the table or a single capital letter with a dot (an initial).
    Decimal numbers such as "3.14" do not end with '.', so they need no special case.
    """
    core = _core(text)
    if not core or core[-1] not in ".?!":
        return False
    if core[-1] in "?!":
        return True
    if core.strip(_EDGE_PUNCT).lower() in ABBREVIATIONS:
        return False
    if _INITIAL_RE.fullmatch(core.lstrip(_EDGE_PUNCT)):
        return False
    return True


def _ends_with_clause_mark(text: str) -> bool:
    return _core(text).endswith((",", ";"))


def _duration(ws: Sequence[Word]) -> float:
    return ws[-1].end - ws[0].start


def _too_long(ws: Sequence[Word]) -> bool:
    return len(ws) > MAX_SENTENCE_WORDS or _duration(ws) > MAX_SENTENCE_SECONDS


def _to_ms(seconds: float) -> int:
    """Seconds -> integer milliseconds, rounding half up."""
    return int(math.floor(seconds * 1000.0 + 0.5))


def _word_times(ws: Sequence[Word], start_ms: int, end_ms: int) -> List[dict]:
    """The "words" of one sentence: [{"w", "s", "e"}] (SPEC-v2 P4.2).

    One entry per word of the sentence, in order, skipping words that are only
    whitespace (they add nothing to the text). "w" is the word without surrounding
    whitespace, punctuation attached as faster-whisper gives it (" two" + "-way" stay
    two entries); "s"/"e" are integer ms from the start of the lecture, rounded like
    start_ms/end_ms. After rounding the times are made consistent: s <= e, s never
    decreases from one word to the next, and every word lies inside the sentence's
    [start_ms, end_ms]. Ordered input (faster-whisper's normal output) is unchanged;
    only degenerate times (a word ending before it starts, words out of order) move.
    """
    out: List[dict] = []
    prev_s = start_ms
    for w in ws:
        text = w.word.strip()
        if not text:
            continue
        s = min(max(_to_ms(w.start), prev_s), end_ms)
        e = min(max(_to_ms(w.end), s), end_ms)
        out.append({"w": text, "s": s, "e": e})
        prev_s = s
    return out


def _sentence_dict(ws: Sequence[Word]) -> dict:
    start_ms = _to_ms(ws[0].start)
    end_ms = max(start_ms, _to_ms(ws[-1].end))
    return {
        "start_ms": start_ms,
        "end_ms": end_ms,
        "text": "".join(w.word for w in ws).strip(),
        "low_conf_words": [
            {"i": i, "w": w.word.strip()}
            for i, w in enumerate(ws)
            if w.probability < LOW_CONF_THRESHOLD
        ],
        "words": _word_times(ws, start_ms, end_ms),
    }


def split_word_groups(words: Iterable) -> List[List[Word]]:
    """Splits a flat, time-ordered word list into sentences (lists of Word). SPEC §6.2.

    1. Break after a word for which is_sentence_end() is true.
    2. When the current sentence exceeds 60 words or 30 seconds (checked after each
       word is appended), break after the last word ending with ',' or ';' among the
       words before the one that caused the overflow; the rest carries over to the next
       sentence. Without such a word, a sentence of more than 60 words is broken after
       its 60th word; a sentence that is only too long in time keeps growing until a
       ',' / ';' word, a sentence end or the 60-word limit appears.
    3. A sentence with fewer than 2 words lasting less than 1 second is merged into the
       previous sentence (the first sentence has no previous one and is kept).
    """
    groups: List[List[Word]] = []
    cur: List[Word] = []
    for raw in words:
        w = as_word(raw)
        cur.append(w)
        while _too_long(cur):
            cut = None
            for i in range(len(cur) - 2, -1, -1):
                if _ends_with_clause_mark(cur[i].word):
                    cut = i + 1
                    break
            if cut is None:
                if len(cur) <= MAX_SENTENCE_WORDS:
                    break
                cut = MAX_SENTENCE_WORDS
            groups.append(cur[:cut])
            cur = cur[cut:]
        if cur and is_sentence_end(w.word):
            groups.append(cur)
            cur = []
    if cur:
        groups.append(cur)

    merged: List[List[Word]] = []
    for g in groups:
        if merged and len(g) < SHORT_SENTENCE_WORDS and _duration(g) < SHORT_SENTENCE_SECONDS:
            merged[-1] = merged[-1] + g
        else:
            merged.append(g)
    return merged


def split_sentences(words: Iterable) -> List[dict]:
    """Returns result.json sentence dicts: start_ms, end_ms, text, low_conf_words, words."""
    return [_sentence_dict(g) for g in split_word_groups(words)]


# ---------------------------------------------------------------------------
# Degenerate decoding windows (redecode: worker-builder, 2026-10-27, user-approved)
# ---------------------------------------------------------------------------
#
# large-v3 with condition_on_previous_text=False (SPEC §6.2) writes about half of the
# 30-second windows of a fast lecture in one of two styles a human transcript never
# has: all lower case without any punctuation, or all upper case. Both are confident
# T=0 decodes, so neither the temperature fallback nor the hallucination filter sees
# them, and the sentence split can only cut such text at the 60th word
# (.claude/research/asr-degeneration.md). window_style() recognises them from the
# words alone (no audio, no model), so worker/asr.py (which decodes such a window again)
# and the statistical repair layer judge text the same way.

DEGENERATE_UNPUNCTUATED = "unpunctuated"
DEGENERATE_UPPERCASE = "uppercase"

# Punctuation that counts, at the end of a word (closing quotes/brackets stripped):
# "3.14" or "e.g" inside a word does not make a window punctuated.
WINDOW_PUNCT = ".,?!;:"
# "unpunctuated": at least this many words, no sentence end among them and at most one
# other mark (comma, ...) per UNPUNCTUATED_WORDS_PER_MARK words. Normal Whisper text has
# a comma or full stop every ~10 words and a sentence end in every window (CSC3631,
# 2 lectures, 209 windows); the degenerate style has none at all, or a stray comma or
# two in 60 words. Shorter windows (the end of a talk, a pause) are left alone.
UNPUNCTUATED_MIN_WORDS = 30
UNPUNCTUATED_WORDS_PER_MARK = 10
# "uppercase": at least this many words and UPPERCASE_MIN_RATIO of the letters upper
# case. Acronym-dense lecture text (AES, XOR, CBC, LFSR) stays far below: the words
# around the acronyms are lower case.
UPPERCASE_MIN_WORDS = 8
UPPERCASE_MIN_RATIO = 0.8

# The numbers window_style() bases its verdict on; kind is "" (not degenerate),
# DEGENERATE_UPPERCASE or DEGENERATE_UNPUNCTUATED.
WindowStyle = namedtuple("WindowStyle", ["words", "punct", "sentence_ends", "letters",
                                         "upper", "kind"])


def _word_text(w) -> str:
    """The text of a word: a str, a Word / faster-whisper Word (.word), or a dict with
    "word" (checkpoint, as_word) or "w" (result.json sentence words)."""
    if isinstance(w, str):
        return w
    if isinstance(w, dict):
        return str(w.get("word", w.get("w", "")))
    return str(w.word)


def window_style(words: Iterable) -> WindowStyle:
    """Counts of a decoded window's words and whether its writing style is degenerate.

    Words without letters or digits (stray punctuation) do not count as words. An
    upper-case window is DEGENERATE_UPPERCASE even when it has punctuation; otherwise a
    window of UNPUNCTUATED_MIN_WORDS words or more without a sentence end and with at
    most one word ending in WINDOW_PUNCT per UNPUNCTUATED_WORDS_PER_MARK words is
    DEGENERATE_UNPUNCTUATED.
    """
    n = punct = ends = letters = upper = 0
    for raw in words:
        text = _word_text(raw).strip()
        if not any(c.isalnum() for c in text):
            continue
        n += 1
        core = _core(text)
        if core and core[-1] in WINDOW_PUNCT:
            punct += 1
        if is_sentence_end(text):
            ends += 1
        for c in text:
            if c.isalpha():
                letters += 1
                if c.isupper():
                    upper += 1
    kind = ""
    if n >= UPPERCASE_MIN_WORDS and letters and upper >= UPPERCASE_MIN_RATIO * letters:
        kind = DEGENERATE_UPPERCASE
    elif n >= UNPUNCTUATED_MIN_WORDS and ends == 0 and punct * UNPUNCTUATED_WORDS_PER_MARK <= n:
        kind = DEGENERATE_UNPUNCTUATED
    return WindowStyle(n, punct, ends, letters, upper, kind)


def degeneration(words: Iterable) -> str:
    """"" when the words read like normal text, else the degenerate style
    (DEGENERATE_UPPERCASE or DEGENERATE_UNPUNCTUATED); see window_style()."""
    return window_style(words).kind


# ---------------------------------------------------------------------------
# Repetition filter
# ---------------------------------------------------------------------------

def _repeat_key(text: str) -> str:
    t = text.strip().lower()
    k = t.strip(_EDGE_PUNCT)
    return k if k else t


def remove_repeats(words: Sequence, min_count: int = REPEAT_MIN_COUNT,
                   max_ngram: int = REPEAT_MAX_NGRAM) -> List[Word]:
    """Collapses a phrase repeated min_count or more times in a row to one copy.

    Definition (SPEC §6.2 "同一短语连续重复 3 次以上"): words are compared by a key
    (stripped, lower-cased, leading/trailing punctuation removed). Scanning left to
    right, at position i the phrase lengths n = 1..max_ngram are tried in increasing
    order; if words[i:i+n] occurs k >= min_count times back to back, the first copy is
    kept (original words and timestamps) and the other k-1 copies are dropped, and the
    scan continues after the kept copy. Passes repeat until nothing changes. The scan
    runs over the flattened word sequence, so repetitions across segment boundaries
    are found too.
    """
    ws = [as_word(w) for w in words]
    changed = True
    while changed:
        changed = False
        keys = [_repeat_key(w.word) for w in ws]
        out: List[Word] = []
        i, n_words = 0, len(ws)
        while i < n_words:
            matched = False
            for n in range(1, max_ngram + 1):
                if i + n * min_count > n_words:
                    break
                pat = keys[i:i + n]
                k = 1
                while keys[i + k * n:i + (k + 1) * n] == pat:
                    k += 1
                if k >= min_count:
                    out.extend(ws[i:i + n])
                    i += k * n
                    matched = changed = True
                    break
            if not matched:
                out.append(ws[i])
                i += 1
        ws = out
    return ws


# ---------------------------------------------------------------------------
# initial_prompt truncation
# ---------------------------------------------------------------------------

def truncate_prompt(prompt: Optional[str], count_tokens: Callable[[str], int],
                    limit: int = PROMPT_TOKEN_LIMIT) -> Tuple[str, int]:
    """Keeps the longest prefix of whole terms whose prompt fits in `limit` tokens.

    `prompt` is the caller's term list joined with ", " (already in priority order).
    Returns (prompt_text, number_of_terms_used); ("", 0) when nothing fits.
    """
    if not prompt:
        return "", 0
    terms = [t.strip() for t in prompt.split(PROMPT_SEPARATOR)]
    terms = [t for t in terms if t]
    used: List[str] = []
    for term in terms:
        candidate = PROMPT_SEPARATOR.join(used + [term])
        if count_tokens(candidate) > limit:
            break
        used.append(term)
    return PROMPT_SEPARATOR.join(used), len(used)


# ---------------------------------------------------------------------------
# Output
# ---------------------------------------------------------------------------

def emit_stdout(line: str) -> None:
    sys.stdout.write(line + "\n")
    sys.stdout.flush()


def format_progress(fraction: float) -> str:
    return "PROGRESS %.2f" % min(1.0, max(0.0, fraction))


def write_result(path: str, result: dict) -> None:
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        json.dump(result, f, ensure_ascii=False)
        f.flush()
        os.fsync(f.fileno())


def peak_working_set_mb() -> int:
    """This process's peak memory in MB (Windows: PeakWorkingSetSize; Linux / macOS:
    ru_maxrss); 0 when it cannot be read."""
    try:
        if sys.platform == "win32":
            import ctypes
            from ctypes import wintypes

            class _PMC(ctypes.Structure):
                _fields_ = [("cb", wintypes.DWORD), ("PageFaultCount", wintypes.DWORD),
                            ("PeakWorkingSetSize", ctypes.c_size_t),
                            ("WorkingSetSize", ctypes.c_size_t),
                            ("QuotaPeakPagedPoolUsage", ctypes.c_size_t),
                            ("QuotaPagedPoolUsage", ctypes.c_size_t),
                            ("QuotaPeakNonPagedPoolUsage", ctypes.c_size_t),
                            ("QuotaNonPagedPoolUsage", ctypes.c_size_t),
                            ("PagefileUsage", ctypes.c_size_t),
                            ("PeakPagefileUsage", ctypes.c_size_t)]

            pmc = _PMC()
            pmc.cb = ctypes.sizeof(_PMC)
            k32 = ctypes.WinDLL("kernel32")
            psapi = ctypes.WinDLL("psapi")
            k32.GetCurrentProcess.restype = wintypes.HANDLE
            psapi.GetProcessMemoryInfo.argtypes = [wintypes.HANDLE, ctypes.POINTER(_PMC),
                                                   wintypes.DWORD]
            if not psapi.GetProcessMemoryInfo(k32.GetCurrentProcess(), ctypes.byref(pmc),
                                              pmc.cb):
                return 0
            return int(round(pmc.PeakWorkingSetSize / (1024.0 * 1024.0)))
        import resource
        peak = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
        # ru_maxrss is KiB on Linux, bytes on macOS
        return int(round(peak / (1024.0 * 1024.0 if sys.platform == "darwin" else 1024.0)))
    except Exception:
        return 0


def make_timing(load_s: float, transcribe_s: float, audio_s: float, first_compile: bool,
                peak_mb: Optional[int] = None, postprocess: Optional[dict] = None) -> dict:
    """The "timing" block of result.json (brief 3b §4): seconds, the first-compile flag
    and the process's peak working set in MB (0 = unknown). postprocess (redecode,
    2026-10-27): the counters of the steps between decoding and the sentence split
    that ran, one object per step ({"redecode": {...}}, see worker/asr.py
    RedecodeStats); left out when no step ran (None or empty). They describe the run,
    never the transcript (Go: model.ASRTiming.Postprocess)."""
    timing = {
        "load_s": round(max(0.0, load_s), 2),
        "transcribe_s": round(max(0.0, transcribe_s), 2),
        "audio_s": round(max(0.0, audio_s), 2),
        "first_compile": bool(first_compile),
        "peak_working_set_mb": int(peak_working_set_mb() if peak_mb is None else peak_mb),
    }
    if postprocess:
        timing["postprocess"] = {k: dict(v) for k, v in postprocess.items()}
    return timing


# ---------------------------------------------------------------------------
# Checkpoints (power management, v2-step20; power brief §4)
# ---------------------------------------------------------------------------
#
# job.json "checkpoint" (Go: devices.CheckpointJob) asks the worker to keep the file
# {lecture dir}/asr.ckpt.json (Go: devices.Checkpoint) up to date while it transcribes,
# so that a run the scheduler kills (power migration or suspension, a cancel, a restart)
# continues where it stopped instead of starting over. Only the worker writes the file;
# Go deletes it after a successful run (and before a run that must not continue it).
#
#   * Written after complete units (faster-whisper: decoding windows, at most about
#     every CHECKPOINT_INTERVAL_S; OpenVINO: every chunk) as "asr.ckpt.json.tmp" + fsync
#     + os.replace: a killed worker leaves the previous complete file and at most a stray
#     .tmp (removed by the next run, and by the §4.7 recovery).
#   * segments: what the worker has transcribed so far, before the post-processing that
#     runs over the whole lecture when the run ends (faster-whisper: the raw segments
#     with seek, no_speech_prob and avg_logprob, so the hallucination filter is part of
#     that post-processing; OpenVINO: one segment per chunk), on the lecture's timeline.
#   * A run continues from the file only when it is well formed and its version,
#     job_id, input_hash, backend, engine and vad match; otherwise the file is deleted
#     and the run starts over.
#   * result.json "parts": a run that got a checkpoint object always returns the audio
#     ranges with the device, model and precision that transcribed them (at least one
#     part; the last one is this run's). A part that continues the previous one on the
#     same device, model and precision extends it.

# Go: devices.CheckpointVersion. 2: faster-whisper continues on the speech-only time line
# (resume_seek, raw segments, engine and vad; docs/benchmarks/power-checkpoint.md §3.4).
CHECKPOINT_VERSION = 2
BACKEND_FASTER_WHISPER = "faster-whisper"   # Go: model.BackendFasterWhisper
BACKEND_OPENVINO = "openvino"               # Go: model.BackendOpenVINO

# faster-whisper writes a checkpoint after a complete window only when the last write is
# at least this many seconds old (a window takes about 2 s on a GPU, and the file grows
# with the lecture); the last complete window is written when the transcription ends.
CHECKPOINT_INTERVAL_S = 10.0

CheckpointJob = namedtuple("CheckpointJob", ["path", "job_id", "input_hash", "device_id",
                                             "model", "precision", "chunk_s"])

# One segment of a checkpoint as read back: seek, no_speech_prob and avg_logprob are the
# faster-whisper segment's (None for OpenVINO), words are Word. The faster-whisper
# worker's keep_segment() takes it as it takes a Segment.
CheckpointSegment = namedtuple("CheckpointSegment", ["start_ms", "end_ms", "seek",
                                                     "no_speech_prob", "avg_logprob",
                                                     "words"])

# A checkpoint to continue from: resume_ms (where the run continues on the lecture's
# timeline: progress, parts), resume_seek (faster-whisper: the first window not in the
# checkpoint, in mel frames of the speech-only audio; None for OpenVINO), the first
# part's prompt_terms_used, the parts so far (dicts), the segments (encoded JSON, written
# again as they are; raw = read back as CheckpointSegment) and all their words in order.
# redecode (faster-whisper, 2026-10-27): the checkpoint's "redecode" counters (dict) of
# the windows in it, None when the file has none (written before degenerate windows
# were decoded again, or by OpenVINO).
Resume = namedtuple("Resume", ["resume_ms", "resume_seek", "prompt_terms_used", "parts",
                               "segments", "raw", "words", "redecode"], defaults=(None,))

_PART_KEYS = ("from_ms", "to_ms", "device_id", "model", "precision")
_SAME_PART_KEYS = ("device_id", "model", "precision")
_FW_SEGMENT_KEYS = ("no_speech_prob", "avg_logprob")


def _is_int(v) -> bool:
    return isinstance(v, int) and not isinstance(v, bool)


def _is_num(v) -> bool:
    return isinstance(v, (int, float)) and not isinstance(v, bool) and math.isfinite(v)


def parse_checkpoint_job(data) -> Optional[CheckpointJob]:
    """job.json "checkpoint" -> CheckpointJob, None when it is absent or null. A malformed
    object is an input error (exit 2: a caller bug that retrying cannot fix)."""
    if data is None:
        return None
    if not isinstance(data, dict):
        raise InputError("job.checkpoint must be an object")
    vals = {}
    for key in ("path", "job_id", "input_hash"):
        v = data.get(key)
        if not isinstance(v, str) or not v:
            raise InputError("job.checkpoint.%s must be a non-empty string" % key)
        vals[key] = v
    for key in ("device_id", "model", "precision"):
        v = data.get(key)
        if v is None:
            v = ""
        if not isinstance(v, str):
            raise InputError("job.checkpoint.%s must be a string" % key)
        vals[key] = v
    chunk_s = data.get("chunk_s")
    if chunk_s is None:
        chunk_s = 0
    if not _is_int(chunk_s) or chunk_s < 0:
        raise InputError("job.checkpoint.chunk_s must be a non-negative integer")
    return CheckpointJob(chunk_s=chunk_s, **vals)


def audio_ms(n_samples: int) -> int:
    """Whole milliseconds of n_samples at SAMPLE_RATE (rounded down)."""
    return int(n_samples) * 1000 // SAMPLE_RATE


def samples_at(ms: int) -> int:
    """Sample index of a time in whole milliseconds."""
    return int(ms) * SAMPLE_RATE // 1000


def clock(ms: int) -> str:
    """"m:ss" of a time in milliseconds (log lines)."""
    m, s = divmod(int(ms) // 1000, 60)
    return "%d:%02d" % (m, s)


def encode_segment(start_ms: int, end_ms: int, words: Sequence[Word], seek: Optional[int] = None,
                   no_speech_prob: Optional[float] = None,
                   avg_logprob: Optional[float] = None) -> str:
    """One checkpoint segment as compact JSON; every checkpoint repeats the encoded
    segments as they are, so each segment is encoded once. seek, no_speech_prob and
    avg_logprob (faster-whisper) are left out when None. Word times are seconds (the
    floats themselves: json round-trips them exactly, so as_word reads them back
    unchanged)."""
    seg = {"start_ms": int(start_ms), "end_ms": int(end_ms)}
    if seek is not None:
        seg["seek"] = int(seek)
    if no_speech_prob is not None:
        seg["no_speech_prob"] = float(no_speech_prob)
    if avg_logprob is not None:
        seg["avg_logprob"] = float(avg_logprob)
    seg["words"] = [{"word": w.word, "start": w.start, "end": w.end,
                     "probability": w.probability} for w in words]
    return json.dumps(seg, ensure_ascii=False, separators=(",", ":"))


def merge_part(parts: Sequence[dict], part: dict) -> List[dict]:
    """parts followed by part (copies). A part that starts where the last one ends, on
    the same device, model and precision, extends it instead (a cancel and retry on the
    same device is still one part)."""
    out = [dict(p) for p in parts]
    if out and out[-1]["to_ms"] == part["from_ms"] and \
            all(out[-1][k] == part[k] for k in _SAME_PART_KEYS):
        out[-1]["to_ms"] = part["to_ms"]
    else:
        out.append(dict(part))
    return out


def _valid_part(p) -> bool:
    return isinstance(p, dict) and _is_int(p.get("from_ms")) and _is_int(p.get("to_ms")) \
        and all(isinstance(p.get(k), str) for k in _SAME_PART_KEYS)


def _valid_word(w) -> bool:
    return isinstance(w, dict) and isinstance(w.get("word"), str) and \
        all(_is_num(w.get(k)) for k in ("start", "end", "probability"))


def _valid_counters(d) -> bool:
    """A checkpoint's "redecode" object: non-negative numbers (counts and seconds) and
    the flag "skipped"."""
    return isinstance(d, dict) and all(
        isinstance(k, str) and (isinstance(v, bool) if k == "skipped" else _is_num(v) and v >= 0)
        for k, v in d.items())


def _valid_segment(s, faster_whisper: bool) -> bool:
    if not (isinstance(s, dict) and _is_int(s.get("start_ms")) and _is_int(s.get("end_ms"))
            and isinstance(s.get("words"), list) and all(_valid_word(w) for w in s["words"])):
        return False
    if faster_whisper:
        return _is_int(s.get("seek")) and s["seek"] >= 0 and \
            all(_is_num(s.get(k)) for k in _FW_SEGMENT_KEYS)
    return True


class Checkpointer:
    """Reads, checks and writes the checkpoint file of one run (job.json "checkpoint").

    engine / vad: what the content depends on besides the job (faster-whisper: its
    version and the VAD options, which decide the speech-only time line and so the
    windows; "" for OpenVINO). load() returns the Resume to continue from, or None (no
    file, or a file that does not belong to this job, backend or engine: deleted).
    save() writes the file; a failed write is logged and the run goes on (the result
    does not depend on it). parts() gives the "parts" of result.json and of the file.
    """

    def __init__(self, job: CheckpointJob, backend: str, emit: Callable[[str], None],
                 engine: str = "", vad: str = ""):
        self.job = job
        self.backend = backend
        self.emit = emit
        self.engine = engine
        self.vad = vad
        self.path = job.path
        self.tmp = job.path + ".tmp"
        self.writes = 0

    @property
    def chunk_s(self) -> int:
        return self.job.chunk_s

    @property
    def faster_whisper(self) -> bool:
        return self.backend == BACKEND_FASTER_WHISPER

    def part(self, from_ms: int, to_ms: int) -> dict:
        return {"from_ms": int(from_ms), "to_ms": int(to_ms), "device_id": self.job.device_id,
                "model": self.job.model, "precision": self.job.precision}

    def parts(self, resume: Optional[Resume], to_ms: int) -> List[dict]:
        """The parts up to to_ms: the checkpoint's, then this run's from where it began."""
        if resume is None:
            return [self.part(0, to_ms)]
        return merge_part(resume.parts, self.part(resume.resume_ms, to_ms))

    def _remove(self, path: str) -> None:
        try:
            os.remove(path)
        except FileNotFoundError:
            pass
        except OSError as e:
            self.emit("cannot delete %s: %s" % (os.path.basename(path), e))

    def _mismatch(self, data, total_ms: int) -> str:
        """Why data cannot be continued from ("" = it can)."""
        if not isinstance(data, dict):
            return "not a JSON object"
        version = data.get("version")
        if not _is_int(version) or version != CHECKPOINT_VERSION:
            return "version %r, expected %d" % (version, CHECKPOINT_VERSION)
        if data.get("job_id") != self.job.job_id:
            return "job_id %r, this job is %r" % (data.get("job_id"), self.job.job_id)
        if data.get("input_hash") != self.job.input_hash:
            return "input_hash differs (the audio or the language changed)"
        if data.get("backend") != self.backend:
            return "backend %r, this worker is %r" % (data.get("backend"), self.backend)
        if data.get("engine", "") != self.engine:
            return "engine %r, this worker is %r" % (data.get("engine", ""), self.engine)
        if data.get("vad", "") != self.vad:
            return "vad %r, this worker uses %r" % (data.get("vad", ""), self.vad)
        resume_ms = data.get("resume_ms")
        if not _is_int(resume_ms) or not 0 < resume_ms <= total_ms:
            return "resume_ms %r outside the audio (%d ms)" % (resume_ms, total_ms)
        if self.faster_whisper:
            seek = data.get("resume_seek")
            if not _is_int(seek) or seek <= 0:
                return "resume_seek %r" % (seek,)
        terms = data.get("prompt_terms_used")
        if not _is_int(terms) or terms < 0:
            return "prompt_terms_used %r" % (terms,)
        parts = data.get("parts")
        if not isinstance(parts, list) or not parts or not all(_valid_part(p) for p in parts):
            return "malformed parts"
        segments = data.get("segments")
        if not isinstance(segments, list) or \
                not all(_valid_segment(s, self.faster_whisper) for s in segments):
            return "malformed segments"
        if not segments:
            # a checkpoint holds at least one complete window / chunk; without one the
            # run starts over, initial_prompt included
            return "no segments"
        if "redecode" in data and not _valid_counters(data["redecode"]):
            return "malformed redecode"
        return ""

    def load(self, total_ms: int) -> Optional[Resume]:
        """The checkpoint to continue from, or None. total_ms = the audio's length. A stray
        .tmp of a killed run is deleted; so is a file that cannot be continued from (the
        run then starts over and writes its own)."""
        self._remove(self.tmp)
        name = os.path.basename(self.path)
        try:
            with open(self.path, encoding="utf-8") as f:
                data = json.load(f)
        except FileNotFoundError:
            return None
        except (OSError, ValueError) as e:
            why = "unreadable: %s" % e
        else:
            why = self._mismatch(data, total_ms)
        if why:
            self.emit("checkpoint %s not used (%s); starting over" % (name, why))
            self._remove(self.path)
            return None
        segments, raw, words = [], [], []
        for s in data["segments"]:
            ws = [as_word(w) for w in s["words"]]
            seg = CheckpointSegment(s["start_ms"], s["end_ms"], s.get("seek"),
                                    s.get("no_speech_prob"), s.get("avg_logprob"), ws)
            raw.append(seg)
            words.extend(ws)
            segments.append(encode_segment(seg.start_ms, seg.end_ms, ws, seg.seek,
                                           seg.no_speech_prob, seg.avg_logprob))
        parts = [{k: p[k] for k in _PART_KEYS} for p in data["parts"]]
        seek = data["resume_seek"] if self.faster_whisper else None
        redecode = dict(data["redecode"]) if "redecode" in data else None
        resume = Resume(data["resume_ms"], seek, data["prompt_terms_used"], parts, segments,
                        raw, words, redecode)
        self.emit("resuming from checkpoint %s at %s%s: %d segments, %d words; parts %s"
                  % (name, clock(resume.resume_ms),
                     " (seek %d)" % seek if seek is not None else "", len(segments),
                     len(words),
                     ", ".join("%s-%s %s" % (clock(p["from_ms"]), clock(p["to_ms"]),
                                             p["device_id"] or "?") for p in parts)))
        return resume

    def discard(self, why: str) -> None:
        """Deletes a checkpoint load() accepted but the worker cannot continue (it says
        why, as load() does); the run starts over."""
        self.emit("checkpoint %s not used (%s); starting over" % (os.path.basename(self.path), why))
        self._remove(self.path)

    def save(self, resume_ms: int, prompt_terms_used: int, parts: Sequence[dict],
             segments: Sequence[str], resume_seek: Optional[int] = None,
             redecode: Optional[dict] = None) -> bool:
        """Writes the checkpoint (.tmp, fsync, os.replace); False when it failed (logged).
        redecode: the faster-whisper worker's counters of the windows in the file (left
        out when None)."""
        head = {"version": CHECKPOINT_VERSION, "job_id": self.job.job_id,
                "input_hash": self.job.input_hash, "backend": self.backend}
        if self.engine:
            head["engine"] = self.engine
        if self.vad:
            head["vad"] = self.vad
        head["resume_ms"] = int(resume_ms)
        if resume_seek is not None:
            head["resume_seek"] = int(resume_seek)
        head["prompt_terms_used"] = int(prompt_terms_used)
        if redecode is not None:
            head["redecode"] = dict(redecode)
        head["parts"] = list(parts)
        text = json.dumps(head, ensure_ascii=False, separators=(",", ":"))
        text = text[:-1] + ',"segments":[' + ",".join(segments) + "]}"
        try:
            with open(self.tmp, "w", encoding="utf-8", newline="\n") as f:
                f.write(text)
                f.flush()
                os.fsync(f.fileno())
            os.replace(self.tmp, self.path)
        except OSError as e:
            self.emit("checkpoint not written (%s: %s); the run goes on"
                      % (type(e).__name__, e))
            self._remove(self.tmp)
            return False
        if self.writes == 0:
            self.emit("checkpoint %s written at %s (then %s)"
                      % (os.path.basename(self.path), clock(resume_ms),
                         "after complete windows, at most every %g s" % CHECKPOINT_INTERVAL_S
                         if self.faster_whisper else "after every chunk"))
        self.writes += 1
        return True


# ---------------------------------------------------------------------------
# Command line
# ---------------------------------------------------------------------------

class ArgParser(argparse.ArgumentParser):
    """argparse that raises InputError (exit code 2) instead of exiting."""

    def error(self, message):
        self.print_usage(sys.stderr)
        raise InputError(message)


def fail(prefix: str, err) -> None:
    """Writes the last stderr line "ERROR: <reason>" (SPEC §6.0)."""
    sys.stderr.write("ERROR: %s%s\n" % (prefix, err))
    sys.stderr.flush()


def setup_stdio() -> None:
    for stream in (sys.stdout, sys.stderr):
        try:
            stream.reconfigure(encoding="utf-8", errors="replace", line_buffering=True)
        except (AttributeError, ValueError):
            pass
