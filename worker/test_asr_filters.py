# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""initial_prompt truncation and hallucination filters (SPEC §6.2)."""
from types import SimpleNamespace

import pytest

import asr
from asr import keep_segment, remove_repeats, truncate_prompt


def count_words(text):
    """Fake token counter: one token per whitespace-separated piece."""
    return len(text.split())


# --- initial_prompt truncation ----------------------------------------------

def test_prompt_under_limit_is_kept_whole():
    prompt = "cache, write-back, TLB"
    assert truncate_prompt(prompt, count_words) == (prompt, 3)


def test_prompt_truncated_to_200_tokens():
    terms = ["t%d" % i for i in range(250)]
    text, used = truncate_prompt(", ".join(terms), count_words)
    assert used == 200
    assert text == ", ".join(terms[:200])
    assert count_words(text) <= 200


def test_prompt_truncates_on_whole_terms():
    terms = ["a%d b%d c%d" % (i, i, i) for i in range(100)]  # 3 tokens per term
    text, used = truncate_prompt(", ".join(terms), count_words)
    assert used == 66  # 66 * 3 = 198 <= 200 < 201
    assert text == ", ".join(terms[:66])


def test_prompt_keeps_priority_order_and_stops_at_first_overflow():
    big = " ".join("x%d" % i for i in range(150))
    terms = ["first", big, "small"]
    # "first" + big = 151 tokens fit; "small" fits too
    assert truncate_prompt(", ".join(terms), count_words) == (", ".join(terms), 3)
    huge = " ".join("y%d" % i for i in range(250))
    # A term that does not fit ends the prompt; later shorter terms are not used.
    assert truncate_prompt(", ".join(["first", huge, "small"]), count_words) == ("first", 1)


def test_prompt_custom_limit():
    assert truncate_prompt("a, b, c, d", count_words, limit=2) == ("a, b", 2)


@pytest.mark.parametrize("prompt", [None, "", ", ,  "])
def test_empty_prompt(prompt):
    assert truncate_prompt(prompt, count_words) == ("", 0)


def test_first_term_too_long_gives_empty_prompt():
    huge = " ".join("y%d" % i for i in range(250))
    assert truncate_prompt(huge + ", cache", count_words) == ("", 0)


def test_token_counter_matches_faster_whisper_prompt_encoding():
    calls = []

    class FakeTokenizer:
        def encode(self, text, add_special_tokens=True):
            calls.append((text, add_special_tokens))
            return SimpleNamespace(ids=list(range(len(text.split()) + 1)))

    count = asr.make_token_counter(SimpleNamespace(hf_tokenizer=FakeTokenizer()))
    assert count("  cache, TLB ") == 3
    assert calls == [(" cache, TLB", False)]


# --- segment filter ---------------------------------------------------------

@pytest.mark.parametrize("no_speech,logprob,kept", [
    (0.7, -1.2, False),
    (0.9, -3.0, False),
    (0.7, -0.5, True),    # confident text despite no_speech
    (0.3, -1.5, True),    # low logprob but speech
    (0.6, -1.5, True),    # thresholds are strict: > 0.6
    (0.9, -1.0, True),    # ... and < -1.0
])
def test_keep_segment(no_speech, logprob, kept):
    seg = SimpleNamespace(no_speech_prob=no_speech, avg_logprob=logprob)
    assert keep_segment(seg) is kept


# --- repeated phrases -------------------------------------------------------

def W(text, i=0):
    return {"word": " " + text, "start": float(i), "end": float(i) + 0.5, "probability": 0.9}


def words(s):
    return [W(t, i) for i, t in enumerate(s.split())]


def joined(ws):
    return " ".join(w.word.strip() for w in ws)


@pytest.mark.parametrize("src,expected", [
    ("the the the cache", "the cache"),
    ("the the cache", "the the cache"),                          # twice is allowed
    ("so so so so so so we start", "so we start"),
    ("thank you thank you thank you thank you", "thank you"),
    ("a b a b a b c a b", "a b c a b"),
    ("x a b c a b c a b c y", "x a b c y"),
    ("Okay. okay, OKAY! we go", "Okay. we go"),                  # case/punctuation-insensitive
    ("one two three four", "one two three four"),
    ("", ""),
])
def test_remove_repeats(src, expected):
    assert joined(remove_repeats(words(src))) == expected


def test_remove_repeats_keeps_first_copy_with_its_timestamps():
    out = remove_repeats(words("we we we go"))
    assert [(w.word, w.start, w.end) for w in out] == [(" we", 0.0, 0.5), (" go", 3.0, 3.5)]


def test_remove_repeats_long_phrase():
    phrase = "thanks for watching and see you in the next video"   # 10 words
    out = remove_repeats(words(" ".join([phrase] * 4) + " end"))
    assert joined(out) == phrase + " end"


def test_remove_repeats_phrase_longer_than_limit_is_kept():
    phrase = " ".join("p%d" % i for i in range(asr.REPEAT_MAX_NGRAM + 1))
    src = " ".join([phrase] * 3)
    assert joined(remove_repeats(words(src))) == src


def test_remove_repeats_reaches_fixpoint():
    # Collapsing "x x x" exposes "x y" repeated three times.
    assert joined(remove_repeats(words("x x x y x x x y x x x y"))) == "x y"


def test_filters_then_split_in_transcribe_job():
    """Segments are filtered, words flattened across segments, repeats removed, split."""
    segs = [
        SimpleNamespace(start=0.0, end=2.0, no_speech_prob=0.1, avg_logprob=-0.2,
                        words=[W("We", 0), W("start", 1), W("now.", 2)]),
        SimpleNamespace(start=2.0, end=4.0, no_speech_prob=0.95, avg_logprob=-1.6,
                        words=[W("Subtitles", 3), W("by", 4), W("Amara.", 5)]),
        SimpleNamespace(start=4.0, end=6.0, no_speech_prob=0.1, avg_logprob=-0.3,
                        words=[W("Thank", 6), W("you.", 7), W("Thank", 8)]),
        SimpleNamespace(start=6.0, end=8.0, no_speech_prob=0.1, avg_logprob=-0.3,
                        words=[W("you.", 9), W("Thank", 10), W("you.", 11)]),
    ]

    class Model:
        def transcribe(self, audio, **kw):
            return iter(segs), SimpleNamespace(duration=8.0, language="en")

    job = asr.Job("a.wav", "large-v3", "float16", "en", "", "cuda", 4)
    lines = []
    result = asr.transcribe_job(Model(), [0.0] * 16000, job, lines.append, count_words)
    assert [s["text"] for s in result["sentences"]] == ["We start now.", "Thank you."]
    assert result["prompt_terms_used"] == 0
    assert [l for l in lines if l.startswith("PROGRESS")] == [
        "PROGRESS 0.25", "PROGRESS 0.50", "PROGRESS 0.75", "PROGRESS 1.00"]
