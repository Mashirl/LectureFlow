# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
# Additional permission under AGPL-3.0 section 7: see LICENSE-EXCEPTION.md
"""Statistical repair of degenerate ASR text before the sentence split (textrepair).

Whisper large-v3 decodes a good part of its 30-second windows (condition_on_previous_text
is off, SPEC §6.2) in a degenerate style: no punctuation at all (usually without
sentence-initial capitals either), or every letter upper case. §6.2's splitter can then
only cut such a stretch at a comma or at its 60th word. repair_words() runs on the
lecture's flat word list between remove_repeats() and split_sentences(); the splitting
rules themselves are unchanged, they only see the repaired words. It

  * finds the degenerate spans: unpunctuated stretches of RUN_MIN_WORDS words or more,
    and all-caps spans (a run of words without a lower-case letter holding at least
    CAPS_MIN_ORDINARY ordinary words, or, with window_starts, a decoding window whose
    letters are CAPS_WINDOW_RATIO upper case);
  * learns from the same lecture's normal text (everything else) the usual written form
    of each word, and a naive-Bayes score for "a sentence ends between these two words":
    the time from the end of the first word to the end of the next one, against the
    next word's usual length (in degenerate windows the pause after a sentence is part
    of the next word, in normal windows it is the gap at the full stop: this one measure
    is the same in both), the word before and the word after;
  * in each unpunctuated stretch appends '.' where a sentence most likely ends (score at
    least DOT_THRESHOLD, sentences at least MIN_SENTENCE_WORDS words), and ',' where
    §6.2 would otherwise cut at the 60th word, so that cut falls on the likeliest place;
  * gives the words of all-caps spans their usual form, gives lower-case words in
    unpunctuated stretches their usual form when the evidence is strong (I, I'm,
    glossary acronyms, names the lecture always capitalises), and capitalises the first
    word of each sentence in degenerate spans.

When in doubt it changes nothing (§0.1: rather keep the original): normal text is never
touched, a stretch gets a '.' only above the threshold, and a lecture with too little
normal text to learn from gets no '.' at all. Words are never added, dropped or
re-timed; only their text changes (case, an appended '.' or ','). The result is a pure
function of the input (deterministic). Standard library only, like asr_common (the
workers run in different virtual environments). The repair runs for English lectures
only (enabled_for; lead ruling 2026-10-27): it is validated on English only.

Repairing a stored lecture without transcribing it again (the asr rerun "text_only",
mode textrepair; NOTES "决定（textrepair：worker-builder，2026-10-27）"):

    python worker/textrepair.py --in job.json --out result.json.tmp

job.json: {"sentences": [{"start_ms", "end_ms", "asr_text", "low_conf_words": [{"i", "w"}],
                          "words": [{"w", "s", "e"}], optional "keep": bool}],
           optional "glossary": [term, ...], optional "language" (default "en"),
           optional "prompt_terms_used" (int, passed through, default 0)}
           = the lecture's sentence rows in idx order; "keep" marks a sentence a person
           reviewed (human_text): it comes back exactly as it is (repair_sentences).
result.json: {"prompt_terms_used", "sentences": [{"start_ms", "end_ms", "text",
              "low_conf_words", "words"}] (asr.py's format), "textrepair": {stats of
              repair_sentences, "ms"; "skipped": "language" when the lecture is not
              English, the sentences then unchanged}}
Protocol (SPEC §6.0, like asr.py): exit code 0 = success, 2 = input error (bad job.json,
a sentence without word times), anything else = retryable; stdout "PROGRESS 0.50" and
log lines; stderr ends with "ERROR: <reason>" on failure. The result is written to
--out + ".part", fsynced and renamed to --out, so --out is complete or absent; the
caller renames --out (result.json.tmp) to result.json. No database, no subprocesses.
"""
from __future__ import annotations

import bisect
import json
import math
import os
import sys
import time
import traceback
from collections import Counter, defaultdict, namedtuple
from typing import Callable, Dict, Iterable, List, Optional, Sequence, Tuple

from asr_common import (EXIT_INPUT, EXIT_OK, EXIT_RETRY, MAX_SENTENCE_WORDS,
                        PROMPT_SEPARATOR, ArgParser, InputError, Word, _CLOSERS, as_word,
                        emit_stdout, fail, format_progress, is_sentence_end, setup_stdio,
                        split_sentences)

VERSION = 1

# --- detection ---------------------------------------------------------------------
RUN_MIN_WORDS = 25            # an unpunctuated stretch this long (words) is repaired
CAPS_MIN_ORDINARY = 3         # ordinary words in a row without a lower-case letter ...
CAPS_ORDINARY_SHARE = 0.5     # ... making up at least this share of its all-caps words
CAPS_WINDOW_RATIO = 0.8       # window_starts: a window with this share of upper-case letters
CAPS_WINDOW_MIN_WORDS = 4     # ... and at least this many words is an all-caps window

# --- casing ------------------------------------------------------------------------
STATS_MIN_COUNT = 1           # a lower-case word gets the lecture's usual capitalised form
STATS_MIN_SHARE = 0.6         # only when it is written so this often (non-initial positions)

# --- sentence ends -----------------------------------------------------------------
MIN_SENTENCE_WORDS = 4        # no added '.' makes a sentence shorter than this
DOT_THRESHOLD = 0.0           # posterior log-odds needed for a '.'
MIN_TRAIN_SENTENCES = 20      # fewer sentence ends in the normal text: no '.' is added
PRIOR_STRENGTH = 5.0          # pseudo-count pulling each feature value to the base rate
WEIGHT_TIME = 1.0
WEIGHT_NEXT = 1.0
WEIGHT_PREV = 1.0
CAP_CUE_LLR = 3.0             # next word capitalised although it is usually lower case
COMMA_COST = 100.0            # a ',' is only added to keep a sentence within 60 words
TIME_BINS = (-0.1, 0.0, 0.1, 0.2, 0.3, 0.45, 0.65, 0.9, 1.3, 2.0)   # seconds
LENGTH_BUCKETS = 12           # usual length of unknown words: by character count, capped

_MARKS = ".,?!;:"
_EN_PRONOUNS = {"i": "I", "i'm": "I'm", "i'll": "I'll", "i've": "I've", "i'd": "I'd",
                "i’m": "I’m", "i’ll": "I’ll", "i’ve": "I’ve", "i’d": "I’d"}

Params = namedtuple("Params", [
    "run_min_words", "min_sentence_words", "dot_threshold", "min_train_sentences",
    "prior_strength", "weight_time", "weight_next", "weight_prev", "cap_cue_llr",
    "caps_min_ordinary", "stats_min_count", "stats_min_share"])

DEFAULT_PARAMS = Params(RUN_MIN_WORDS, MIN_SENTENCE_WORDS, DOT_THRESHOLD, MIN_TRAIN_SENTENCES,
                        PRIOR_STRENGTH, WEIGHT_TIME, WEIGHT_NEXT, WEIGHT_PREV, CAP_CUE_LLR,
                        CAPS_MIN_ORDINARY, STATS_MIN_COUNT, STATS_MIN_SHARE)

# words: the repaired words; stats: see repair_words; degenerate: per word, True when
# it lies in a span the repair worked on (an unpunctuated stretch or an all-caps span).
Repaired = namedtuple("Repaired", ["words", "stats", "degenerate"])


# ---------------------------------------------------------------------------
# Word text
# ---------------------------------------------------------------------------

class _Tok:
    """One word's text split as lead (whitespace) + pre (leading punctuation) + core
    (first to last letter / digit) + post (trailing punctuation)."""

    __slots__ = ("lead", "pre", "core", "post", "key")

    def __init__(self, text: str):
        stripped = text.lstrip()
        self.lead = text[:len(text) - len(stripped)]
        first = last = -1
        for i, ch in enumerate(stripped):
            if ch.isalnum():
                if first < 0:
                    first = i
                last = i
        if first < 0:
            self.pre, self.core, self.post = stripped, "", ""
        else:
            self.pre = stripped[:first]
            self.core = stripped[first:last + 1]
            self.post = stripped[last + 1:]
        self.key = self.core.lower()


def _has_lower(s: str) -> bool:
    return any(ch.islower() for ch in s)


def _has_upper(s: str) -> bool:
    return any(ch.isupper() for ch in s)


def _has_letter(s: str) -> bool:
    return any(ch.isalpha() for ch in s)


def has_mark(text: str) -> bool:
    """True when a word ends with . , ? ! ; or : (closing quotes / brackets allowed after)."""
    core = text.strip().rstrip(_CLOSERS)
    return bool(core) and core[-1] in _MARKS


def _cap_first(core: str) -> str:
    """Upper-cases the first letter, unless the word has capitals inside (iPhone)."""
    if core and core[0].islower() and not _has_upper(core[1:]):
        return core[0].upper() + core[1:]
    return core


def _logit(p: float) -> float:
    return math.log(p / (1.0 - p))


def _median(xs: List[float]) -> float:
    xs = sorted(xs)
    n = len(xs)
    return xs[n // 2] if n % 2 else (xs[n // 2 - 1] + xs[n // 2]) / 2.0


# ---------------------------------------------------------------------------
# Spans
# ---------------------------------------------------------------------------

def unpunctuated_stretches(texts: Sequence[str], min_words: int) -> List[Tuple[int, int]]:
    """(first, last) word index of each stretch between punctuation marks with at least
    min_words words: words[first..last-1] end without . , ? ! ; : and words[last] is the
    next word that has one (or the lecture's last word)."""
    out = []
    i, n = 0, len(texts)
    while i < n:
        j = i
        while j < n - 1 and not has_mark(texts[j]):
            j += 1
        if j - i + 1 >= min_words:
            out.append((i, j))
        i = j + 1
    return out


def _caps_runs(toks: Sequence[_Tok]) -> List[Tuple[int, int]]:
    """Maximal runs of words without a lower-case letter that hold an upper-case one."""
    runs = []
    i, n = 0, len(toks)
    while i < n:
        if _has_lower(toks[i].core) or not _has_upper(toks[i].core):
            i += 1
            continue
        j = i
        while j + 1 < n and not _has_lower(toks[j + 1].core):
            j += 1
        while not _has_upper(toks[j].core):     # trailing words without letters
            j -= 1
        runs.append((i, j))
        i = j + 1
    return runs


def _window_index(words: Sequence[Word],
                  window_starts: Optional[Sequence[float]]) -> Optional[List[int]]:
    """The decoding window of each word (index into the sorted window_starts), or None."""
    if not window_starts:
        return None
    starts = sorted(float(s) for s in window_starts)
    return [max(0, bisect.bisect_right(starts, w.start + 1e-6) - 1) for w in words]


# ---------------------------------------------------------------------------
# Casing
# ---------------------------------------------------------------------------

class _Casing:
    """The usual written form of each word: English pronoun rules, glossary acronyms,
    the lecture's normal text (non-initial positions), glossary names."""

    def __init__(self, toks: Sequence[_Tok], texts: Sequence[str], normal: Sequence[bool],
                 glossary: Iterable[str], language: str, p: Params):
        self.p = p
        self.rules = dict(_EN_PRONOUNS) if (language or "en").lower().startswith("en") else {}
        self.acronyms: Dict[str, str] = {}
        self.names: Dict[str, str] = {}
        for term in glossary or ():
            for part in str(term).split():
                core = _Tok(part).core
                if len(core) < 2 or not _has_upper(core):
                    continue
                n_up = sum(ch.isupper() for ch in core)
                if n_up >= 2 or not core[0].isupper() or any(ch.isdigit() for ch in core):
                    self.acronyms.setdefault(core.lower(), core)
                else:
                    self.names.setdefault(core.lower(), core)
        self.forms: Dict[str, Counter] = defaultdict(Counter)
        for i, t in enumerate(toks):
            if not normal[i] or not _has_letter(t.core) or i == 0:
                continue
            if not normal[i - 1] or is_sentence_end(texts[i - 1]):
                continue
            self.forms[t.key][t.core] += 1

    def usual(self, key: str) -> Tuple[Optional[str], bool]:
        """(form, strong): the form to use for this word, or None when unknown; strong =
        rule, glossary acronym, glossary name the lecture does not contradict, or the
        lecture's form used often enough."""
        if key in self.rules:
            return self.rules[key], True
        if key in self.acronyms:
            return self.acronyms[key], True
        c = self.forms.get(key)
        if c:
            total = sum(c.values())
            form, n = max(c.items(), key=lambda kv: (kv[1], sum(ch.islower() for ch in kv[0]),
                                                     kv[0]))
            if key in self.names and not _has_lower(form[:1]):
                return self.names[key], True
            return form, (n >= self.p.stats_min_count and n >= self.p.stats_min_share * total)
        if key in self.names:
            return self.names[key], True
        return None, False

    def from_caps(self, core: str) -> str:
        """The form of an all-caps word: its usual form, else lower case (digits: as is)."""
        form, _ = self.usual(core.lower())
        if form is not None:
            return form
        if any(ch.isdigit() for ch in core):
            return core
        return core.lower()

    def from_lower(self, core: str) -> str:
        """The form of an all-lower-case word: its usual capitalised form when the evidence
        is strong, else unchanged."""
        form, strong = self.usual(core)
        if strong and form is not None and _has_upper(form) and form.lower() == core:
            return form
        return core

    def usually_lower_start(self, key: str) -> bool:
        form, _ = self.usual(key)
        return form is not None and form[:1].islower()


# ---------------------------------------------------------------------------
# Sentence ends
# ---------------------------------------------------------------------------

class _Ends:
    """Naive-Bayes log-odds that a sentence ends between words k and k+1, learned from the
    lecture's normal text (sentence end = the word ends a sentence per §6.2)."""

    def __init__(self, words: Sequence[Word], toks: Sequence[_Tok], texts: Sequence[str],
                 normal: Sequence[bool], p: Params):
        self.p = p
        self.words, self.toks = words, toks
        by_key: Dict[str, List[float]] = defaultdict(list)
        by_len: Dict[int, List[float]] = defaultdict(list)
        every: List[float] = []
        for w, t in zip(words, toks):
            if not t.core:
                continue
            d = max(0.0, w.end - w.start)
            by_key[t.key].append(d)
            by_len[min(len(t.core), LENGTH_BUCKETS)].append(d)
            every.append(d)
        self.dur_key = {k: _median(v) for k, v in by_key.items() if len(v) >= 3}
        self.dur_len = {k: _median(v) for k, v in by_len.items()}
        self.dur_all = _median(every) if every else 0.3

        n_end = n_all = 0
        c_time: Counter = Counter()
        e_time: Counter = Counter()
        c_next: Counter = Counter()
        e_next: Counter = Counter()
        c_prev: Counter = Counter()
        e_prev: Counter = Counter()
        for k in range(len(words) - 1):
            if not (normal[k] and normal[k + 1]) or not self.cuttable(k):
                continue
            y = is_sentence_end(texts[k])
            tb = self.time_bin(k)
            n_all += 1
            c_time[tb] += 1
            c_next[toks[k + 1].key] += 1
            c_prev[toks[k].key] += 1
            if y:
                n_end += 1
                e_time[tb] += 1
                e_next[toks[k + 1].key] += 1
                e_prev[toks[k].key] += 1
        self.trained = n_end
        self.base = (n_end + 1.0) / (n_all + 2.0)
        self.base_logit = _logit(self.base)
        self.llr_time = self._llr(c_time, e_time)
        self.llr_next = self._llr(c_next, e_next)
        self.llr_prev = self._llr(c_prev, e_prev)

    def _rate(self, c: int, e: int) -> float:
        k = self.p.prior_strength
        return (e + k * self.base) / (c + k)

    def _llr(self, counts: Counter, ends: Counter) -> Dict:
        return {v: _logit(self._rate(c, ends[v])) - self.base_logit for v, c in counts.items()}

    def cuttable(self, k: int) -> bool:
        """A sentence can end between words k and k+1: both have letters or digits and the
        next word starts a new word (leading space; " two" + "-way" is one word)."""
        return bool(self.toks[k].core) and bool(self.toks[k + 1].core) and \
            self.toks[k + 1].lead != ""

    def expected(self, i: int) -> float:
        t = self.toks[i]
        d = self.dur_key.get(t.key)
        if d is None:
            d = self.dur_len.get(min(len(t.core), LENGTH_BUCKETS), self.dur_all)
        return d

    def time_bin(self, k: int) -> int:
        x = (self.words[k + 1].end - self.words[k].end) - self.expected(k + 1)
        return bisect.bisect_right(TIME_BINS, x)

    def score(self, k: int, cap_cue: bool) -> float:
        p = self.p
        s = self.base_logit
        s += p.weight_time * self.llr_time.get(self.time_bin(k), 0.0)
        s += p.weight_next * self.llr_next.get(self.toks[k + 1].key, 0.0)
        s += p.weight_prev * self.llr_prev.get(self.toks[k].key, 0.0)
        if cap_cue:
            s += p.cap_cue_llr
        return s


def _segment(gains: Sequence[float], n: int, min_words: int,
             max_words: int) -> Optional[List[int]]:
    """Cuts (local gap indices; gap g is after local word g) splitting n words into pieces
    of min_words..max_words words that maximise the sum of the cuts' gains; None when no
    such split exists. gains[g] = -inf: no cut after word g."""
    neg = float("-inf")
    best = [neg] * (n + 1)
    back = [-1] * (n + 1)
    best[0] = 0.0
    for e in range(min_words, n + 1):
        for s in range(max(0, e - max_words), e - min_words + 1):
            if best[s] == neg:
                continue
            g = 0.0 if s == 0 else gains[s - 1]
            if g == neg:
                continue
            v = best[s] + g
            if v > best[e]:
                best[e], back[e] = v, s
    if best[n] == neg:
        return None
    cuts = []
    e = n
    while e > 0:
        s = back[e]
        if s > 0:
            cuts.append(s - 1)
        e = s
    return sorted(cuts)


# ---------------------------------------------------------------------------
# Entry points
# ---------------------------------------------------------------------------

def repair_words(words: Iterable, glossary: Optional[Iterable[str]] = None,
                 window_starts: Optional[Sequence[float]] = None, language: str = "en",
                 params: Optional[Params] = None,
                 frozen: Optional[Sequence[bool]] = None) -> Repaired:
    """Repairs the degenerate spans of a lecture's flat, time-ordered word list.

    words: Word / dict / faster-whisper words (asr_common.as_word). glossary: the
    course's terms as written (acronyms such as XOR keep their capitals). window_starts:
    optional start times (seconds, lecture time line) of the decoding windows; a window
    whose letters are mostly upper case is then re-cased as a whole. language: the
    English pronoun rules (I, I'm ...) apply to "en*" only. frozen: optional per-word
    flags; a frozen word is never changed and no sentence end is added after it (the
    words of sentences a person reviewed, see repair_sentences); it still counts for
    the statistics like any other word.

    Returns Repaired(words, stats, degenerate): the same words with the same times and
    probabilities, only their text repaired; stats = {"version", "words", "segments"
    (degenerate spans), "unpunctuated_segments", "caps_segments", "sentence_ends_added",
    "commas_added", "words_recased", "train_sentences"}; degenerate = per word, in a
    span the repair worked on (never a frozen word).
    """
    p = params or DEFAULT_PARAMS
    ws = [as_word(w) for w in words]
    n = len(ws)
    texts = [w.word for w in ws]
    toks = [_Tok(t) for t in texts]
    stats = {"version": VERSION, "words": n, "segments": 0, "unpunctuated_segments": 0,
             "caps_segments": 0, "sentence_ends_added": 0, "commas_added": 0,
             "words_recased": 0, "train_sentences": 0}
    if n == 0:
        return Repaired(ws, stats, [])
    fixed = [bool(x) for x in frozen] if frozen is not None else [False] * n
    if len(fixed) != n:
        raise ValueError("frozen has %d flags for %d words" % (len(fixed), n))

    stretches = unpunctuated_stretches(texts, p.run_min_words)
    in_stretch = [False] * n
    for i, j in stretches:
        for k in range(i, j):          # words[j] ends with a mark: it is normal text
            in_stretch[k] = True

    runs = _caps_runs(toks)
    win = _window_index(ws, window_starts)
    caps_windows = set()
    if win is not None:
        letters: Counter = Counter()
        upper: Counter = Counter()
        count: Counter = Counter()
        for i, t in enumerate(toks):
            count[win[i]] += 1
            for ch in t.core:
                if ch.isalpha():
                    letters[win[i]] += 1
                    upper[win[i]] += ch.isupper()
        caps_windows = {x for x in letters if count[x] >= CAPS_WINDOW_MIN_WORDS
                        and upper[x] >= CAPS_WINDOW_RATIO * letters[x]}

    # Normal text (what the statistics come from): outside unpunctuated stretches,
    # outside runs of caps_min_ordinary or more all-caps words, outside all-caps windows.
    excluded = list(in_stretch)
    for i, j in runs:
        if sum(1 for k in range(i, j + 1) if sum(ch.isalpha() for ch in toks[k].core) >= 2
               and not _has_lower(toks[k].core)) >= p.caps_min_ordinary:
            for k in range(i, j + 1):
                excluded[k] = True
    if caps_windows:
        for k in range(n):
            if win[k] in caps_windows:
                excluded[k] = True
    normal = [not x for x in excluded]

    casing = _Casing(toks, texts, normal, glossary or (), language, p)
    for k in range(n):
        if fixed[k]:
            in_stretch[k] = False

    # All-caps spans: runs, and all-caps windows, holding enough ordinary words (words
    # usually written with lower-case letters; acronyms alone are not a span).
    def ordinary_enough(idx: Sequence[int]) -> bool:
        caps = [k for k in idx if _has_upper(toks[k].core) and not _has_lower(toks[k].core)]
        ordinary = 0
        for k in caps:
            form, _ = casing.usual(toks[k].key)
            if form is not None and _has_lower(form):
                ordinary += 1
        return ordinary >= p.caps_min_ordinary and ordinary >= CAPS_ORDINARY_SHARE * len(caps)

    in_caps = [False] * n
    n_caps = 0
    for i, j in runs:
        if ordinary_enough(range(i, j + 1)) and not all(fixed[i:j + 1]):
            for k in range(i, j + 1):
                in_caps[k] = not fixed[k]
            n_caps += 1
    if caps_windows:
        members: Dict[int, List[int]] = defaultdict(list)
        for k in range(n):
            if win[k] in caps_windows:
                members[win[k]].append(k)
        for x in sorted(members):
            idx = [k for k in members[x] if _has_upper(toks[k].core)
                   and not _has_lower(toks[k].core) and not fixed[k]]
            if not idx or not ordinary_enough(idx):
                continue
            if not all(in_caps[k] for k in idx):
                n_caps += 1
            for k in idx:
                in_caps[k] = True

    # 1. Case: all-caps words get their usual form; lower-case words in unpunctuated
    #    stretches get their usual capitalised form when the evidence is strong.
    cores = [t.core for t in toks]
    for k in range(n):
        core = cores[k]
        if not _has_letter(core):
            continue
        if in_caps[k] and not _has_lower(core):
            cores[k] = casing.from_caps(core)
        elif in_stretch[k] and not _has_upper(core):
            cores[k] = casing.from_lower(core)

    # 2. Sentence ends in unpunctuated stretches (each run of words that are not frozen
    #    separately: a frozen sentence keeps its own boundaries).
    ends = _Ends(ws, toks, texts, normal, p)
    stats["train_sentences"] = ends.trained
    marks: Dict[int, str] = {}
    neg = float("-inf")
    n_stretches = 0
    for i, j in stretches:
        parts, a = [], None
        for k in range(i, j + 1):
            if fixed[k]:
                if a is not None:
                    parts.append((a, k - 1))
                a = None
            elif a is None:
                a = k
        if a is not None:
            parts.append((a, j))
        if parts:
            n_stretches += 1
        for a, b in parts:
            m = b - a + 1
            if m < 2:
                continue
            scores, gains, dots = [], [], []
            for k in range(a, b):
                if not ends.cuttable(k):
                    scores.append(neg)
                    gains.append(neg)
                    dots.append(False)
                    continue
                nxt = toks[k + 1]
                # "So" inside an unpunctuated stretch (a window switching style
                # mid-sentence keeps its capitals): a sentence most likely starts there
                cap_cue = (not in_caps[k + 1] and nxt.core[:1].isupper()
                           and _has_lower(nxt.core) and casing.usually_lower_start(nxt.key))
                score = ends.score(k, cap_cue)
                dot_ok = (ends.trained >= p.min_train_sentences
                          and is_sentence_end(cores[k] + ".")
                          and score >= p.dot_threshold)
                scores.append(score)
                gains.append(score - p.dot_threshold if dot_ok else score - COMMA_COST)
                dots.append(dot_ok)
            cuts = _segment(gains, m, p.min_sentence_words, MAX_SENTENCE_WORDS)
            if cuts is None and m > MAX_SENTENCE_WORDS:
                # no split into sentences of min_sentence_words .. 60 words: only commas,
                # so that §6.2 still cuts at the likeliest places
                dots = [False] * len(dots)
                cuts = _segment([x - COMMA_COST for x in scores], m, 1, MAX_SENTENCE_WORDS)
            for g in cuts or ():
                marks[a + g] = "." if dots[g] else ","

    # 3. Capitalise the first word of each sentence inside degenerate spans.
    degenerate = [in_stretch[k] or in_caps[k] for k in range(n)]
    out_texts = []
    for k in range(n):
        t = toks[k]
        core = cores[k]
        if degenerate[k] and core:
            prev_end = k == 0 or is_sentence_end(out_texts[k - 1])
            if prev_end:
                core = _cap_first(core)
        text = t.lead + t.pre + core + t.post + marks.get(k, "")
        out_texts.append(text)
        if core != t.core:
            stats["words_recased"] += 1

    stats["unpunctuated_segments"] = n_stretches
    stats["caps_segments"] = n_caps
    stats["sentence_ends_added"] = sum(1 for v in marks.values() if v == ".")
    stats["commas_added"] = sum(1 for v in marks.values() if v == ",")
    stats["segments"] = sum(1 for k in range(n)
                            if degenerate[k] and (k == 0 or not degenerate[k - 1]))
    out = [w if text == w.word else Word(text, w.start, w.end, w.probability)
           for w, text in zip(ws, out_texts)]
    return Repaired(out, stats, degenerate)


def _sentence_text(s: dict) -> str:
    """The ASR text of a stored sentence: "asr_text" (database rows) or "text" (result.json)."""
    return str(s["asr_text"] if "asr_text" in s else s.get("text", ""))


def _sentence_words(s: dict) -> List[Word]:
    ws = s.get("words")
    if not ws:
        raise ValueError("sentence at %s ms has no words" % s.get("start_ms"))
    text = _sentence_text(s)
    low = {int(x["i"]) for x in s.get("low_conf_words") or []}
    out: List[Word] = []
    pos = 0
    for i, w in enumerate(ws):
        t = str(w["w"])
        j = text.find(t, pos)
        lead = " " if (i == 0 or j < 0 or j == 0 or text[j - 1].isspace()) else ""
        if j >= 0:
            pos = j + len(t)
        out.append(Word(lead + t, int(w["s"]) / 1000.0, int(w["e"]) / 1000.0,
                        0.0 if i in low else 1.0))
    return out


def _sentence_out(s: dict) -> dict:
    """A stored sentence as result.json writes it, unchanged."""
    return {"start_ms": int(s["start_ms"]), "end_ms": int(s["end_ms"]), "text": _sentence_text(s),
            "low_conf_words": [dict(x) for x in s.get("low_conf_words") or []],
            "words": [dict(w) for w in s["words"]]}


def words_from_sentences(sentences: Sequence[dict]) -> List[Word]:
    """The flat word list of stored sentences (result.json, or the database's sentence rows
    with "asr_text"; SPEC-v2 P4.2 "words"), for repairing a lecture without transcribing
    it again: times in seconds from the integer ms, the leading space of each word
    restored from the sentence text (" two" + "-way" stay joined), probability 0 for the
    sentence's low_conf_words and 1 otherwise, so that split_sentences() of the result
    gives back the same sentences. A sentence without "words" (stored before P4.2)
    raises ValueError."""
    out: List[Word] = []
    for s in sentences:
        out.extend(_sentence_words(s))
    return out


def repair_sentences(sentences: Sequence[dict], glossary: Optional[Iterable[str]] = None,
                     language: str = "en", params: Optional[Params] = None
                     ) -> Tuple[List[dict], dict]:
    """Stored sentences, repaired and split again (§6.2). Returns (sentences, stats).

    A sentence with "keep": true (one a person reviewed) comes back exactly as it was
    (boundaries, text, words) and its words are frozen (repair_words), so nothing next
    to it moves into or out of it; the statistics still learn from the whole lecture.
    Every other sentence that has a word in a span the repair worked on is split again
    together with its neighbours of the same kind (split_sentences over their repaired
    words); the remaining sentences come back unchanged. stats = repair_words' stats
    plus "kept_sentences", "sentences_in", "sentences_out"."""
    keep = [bool(s.get("keep")) for s in sentences]
    words: List[Word] = []
    bounds: List[int] = [0]
    for s in sentences:
        words.extend(_sentence_words(s))
        bounds.append(len(words))
    frozen = [False] * len(words)
    for si, kept in enumerate(keep):
        if kept:
            for k in range(bounds[si], bounds[si + 1]):
                frozen[k] = True
    rep = repair_words(words, glossary, None, language, params, frozen)
    touched = [False] * len(sentences)
    for si in range(len(sentences)):
        if keep[si]:
            continue
        for k in range(bounds[si], bounds[si + 1]):
            if rep.degenerate[k] or rep.words[k].word != words[k].word:
                touched[si] = True
                break
    out: List[dict] = []
    si = 0
    while si < len(sentences):
        if not touched[si]:
            out.append(_sentence_out(sentences[si]))
            si += 1
            continue
        sj = si
        while sj + 1 < len(sentences) and touched[sj + 1]:
            sj += 1
        out.extend(split_sentences(rep.words[bounds[si]:bounds[sj + 1]]))
        si = sj + 1
    stats = dict(rep.stats)
    stats["kept_sentences"] = sum(keep)
    stats["sentences_in"] = len(sentences)
    stats["sentences_out"] = len(out)
    return out, stats


# ---------------------------------------------------------------------------
# Command line: repair a stored lecture (SPEC §6.0)
# ---------------------------------------------------------------------------

Job = namedtuple("Job", ["sentences", "glossary", "language", "prompt_terms_used"])


def enabled_for(language) -> bool:
    """The repair runs for English lectures only (lead ruling 2026-10-27)."""
    return str(language or "").strip().lower().startswith("en")


def glossary_terms(initial_prompt: Optional[str]) -> List[str]:
    """The course's terms of a transcription job's initial_prompt (Go joins them with ", "
    in priority order): the whole list, before the worker cuts the prompt down to
    PROMPT_TOKEN_LIMIT tokens (asr_common.truncate_prompt), so every term's spelling helps
    the casing (textrepair-int, lead's brief)."""
    if not initial_prompt:
        return []
    return [t.strip() for t in str(initial_prompt).split(PROMPT_SEPARATOR) if t.strip()]


def repair_transcript(words: Sequence, initial_prompt: Optional[str] = "",
                      language: Optional[str] = None,
                      window_starts: Optional[Sequence[float]] = None,
                      emit: Optional[Callable[[str], None]] = None) -> Tuple[List[Word], dict]:
    """The repair step of the transcription workers (asr.py, asr_openvino.py, asr_mlx.py;
    textrepair-int): the lecture's flat word list after remove_repeats(), before
    split_sentences(). English lectures (enabled_for(language)) get repair_words() with
    the job's whole term list as the glossary (glossary_terms) and the decoding windows'
    start times when the backend knows them; any other language keeps its words.

    Returns (words, stats): stats = repair_words' stats plus "ms" (the repair's time), or
    {"version", "skipped": "language", "words", "ms"} when the language is not English.
    The stats go into result.json timing.postprocess.textrepair (Go:
    model.ASRTextRepair). emit (optional) gets one log line."""
    t0 = time.monotonic()
    ws = [as_word(w) for w in words]
    if not enabled_for(language):
        stats = {"version": VERSION, "skipped": "language", "words": len(ws)}
        out = ws
    else:
        rep = repair_words(ws, glossary_terms(initial_prompt), window_starts, language)
        out, stats = list(rep.words), dict(rep.stats)
    stats["ms"] = int(round((time.monotonic() - t0) * 1000.0))
    if emit is not None:
        if stats.get("skipped"):
            emit("textrepair: skipped (%s %r)" % (stats["skipped"], language))
        else:
            emit("textrepair: %d words, %d degenerate segments (%d unpunctuated, %d all caps), "
                 "+%d '.', +%d ',', %d words recased%s; %d ms"
                 % (stats["words"], stats["segments"], stats["unpunctuated_segments"],
                    stats["caps_segments"], stats["sentence_ends_added"], stats["commas_added"],
                    stats["words_recased"],
                    "" if window_starts else " (no decoding windows)", stats["ms"]))
    return out, stats


def _int(v) -> bool:
    return isinstance(v, int) and not isinstance(v, bool)


def parse_job(data) -> Job:
    """job.json -> Job; InputError (exit 2) when it is malformed."""
    if not isinstance(data, dict):
        raise InputError("job.json must be a JSON object")
    sentences = data.get("sentences")
    if not isinstance(sentences, list) or not sentences:
        raise InputError("job.sentences must be a non-empty list")
    for i, x in enumerate(sentences):
        where = "job.sentences[%d]" % i
        if not isinstance(x, dict):
            raise InputError("%s must be an object" % where)
        for key in ("start_ms", "end_ms"):
            if not _int(x.get(key)) or x[key] < 0:
                raise InputError("%s.%s must be a non-negative integer" % (where, key))
        if not isinstance(x.get("asr_text"), str):
            raise InputError("%s.asr_text must be a string" % where)
        ws = x.get("words")
        if not ws:
            raise InputError("%s has no word times (stored before they were kept); it "
                             "cannot be repaired without transcribing it again" % where)
        if not isinstance(ws, list) or not all(
                isinstance(w, dict) and isinstance(w.get("w"), str) and w["w"]
                and _int(w.get("s")) and _int(w.get("e")) for w in ws):
            raise InputError('%s.words must be a list of {"w", "s", "e"}' % where)
        lc = x.get("low_conf_words")
        if lc is not None and (not isinstance(lc, list) or not all(
                isinstance(c, dict) and _int(c.get("i")) and isinstance(c.get("w"), str)
                for c in lc)):
            raise InputError('%s.low_conf_words must be a list of {"i", "w"}' % where)
        if not isinstance(x.get("keep", False), bool):
            raise InputError("%s.keep must be true or false" % where)
    glossary = data.get("glossary")
    if glossary is None:
        glossary = []
    if not isinstance(glossary, list) or not all(isinstance(t, str) for t in glossary):
        raise InputError("job.glossary must be a list of strings")
    language = data.get("language")
    if language is None:
        language = "en"
    if not isinstance(language, str):
        raise InputError("job.language must be a string")
    terms = data.get("prompt_terms_used")
    if terms is None:
        terms = 0
    if not _int(terms) or terms < 0:
        raise InputError("job.prompt_terms_used must be a non-negative integer")
    return Job(sentences, glossary, language, terms)


def load_job(path: str) -> Job:
    try:
        with open(path, encoding="utf-8-sig") as f:
            data = json.load(f)
    except FileNotFoundError:
        raise InputError("job file not found: %s" % path)
    except (OSError, ValueError) as e:
        raise InputError("cannot read job file %s: %s" % (path, e))
    return parse_job(data)


def _write_atomic(path: str, result: dict) -> None:
    """Writes path + ".part" (fsync) and renames it to path: path is complete or absent."""
    tmp = path + ".part"
    try:
        with open(tmp, "w", encoding="utf-8", newline="\n") as f:
            json.dump(result, f, ensure_ascii=False)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)
    except BaseException:
        try:
            os.remove(tmp)
        except OSError:
            pass
        raise


def repair_job(job: Job) -> dict:
    """The result.json of a job (its "textrepair" without "ms")."""
    if not enabled_for(job.language):
        out = [_sentence_out(x) for x in job.sentences]
        stats = {"version": VERSION, "skipped": "language",
                 "words": sum(len(x["words"]) for x in job.sentences),
                 "kept_sentences": sum(1 for x in job.sentences if x.get("keep")),
                 "sentences_in": len(out), "sentences_out": len(out)}
    else:
        out, stats = repair_sentences(job.sentences, job.glossary, job.language)
    return {"prompt_terms_used": job.prompt_terms_used, "sentences": out, "textrepair": stats}


def run(in_path: str, out_path: str, emit: Callable[[str], None] = emit_stdout) -> None:
    job = load_job(in_path)
    t0 = time.monotonic()
    result = repair_job(job)
    st = result["textrepair"]
    st["ms"] = int(round((time.monotonic() - t0) * 1000.0))
    emit(format_progress(0.5))
    if st.get("skipped"):
        emit("textrepair: skipped (%s %r); %d sentences unchanged"
             % (st["skipped"], job.language, st["sentences_in"]))
    else:
        emit("textrepair: %d sentences (%d kept) -> %d; %d segments, +%d '.', +%d ',', "
             "%d words recased; %d ms"
             % (st["sentences_in"], st["kept_sentences"], st["sentences_out"], st["segments"],
                st["sentence_ends_added"], st["commas_added"], st["words_recased"], st["ms"]))
    _write_atomic(out_path, result)
    emit(format_progress(1.0))


def build_parser() -> ArgParser:
    p = ArgParser(prog="textrepair.py",
                  description="LectureFlow: repair the degenerate text of a stored lecture")
    p.add_argument("--in", dest="in_path", help="job.json")
    p.add_argument("--out", dest="out_path", help="result file to write (e.g. result.json.tmp)")
    return p


def main(argv: Optional[Sequence[str]] = None) -> int:
    try:
        args = build_parser().parse_args(argv)
        if not args.in_path or not args.out_path:
            raise InputError("--in and --out are required")
        run(args.in_path, args.out_path)
        return EXIT_OK
    except InputError as e:
        fail("input error: ", e)
        return EXIT_INPUT
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
