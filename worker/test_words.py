# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""Word-level timestamps in result.json (SPEC-v2 P4.2)."""
from types import SimpleNamespace

import asr
from asr import split_sentences


def mk(texts, start=0.0, dur=0.3, gap=0.0, probs=None):
    """Builds whisper-style words (leading space) with evenly spaced timestamps."""
    out, t = [], start
    for i, text in enumerate(texts):
        word = text if text.startswith(("-", "'")) else " " + text
        out.append({"word": word, "start": t, "end": t + dur,
                    "probability": 0.9 if probs is None else probs[i]})
        t += dur + gap
    return out


def raw(word, start, end, prob=0.9):
    return {"word": word, "start": start, "end": end, "probability": prob}


def ws(sentence):
    return [x["w"] for x in sentence["words"]]


def times(sentence):
    return [(x["w"], x["s"], x["e"]) for x in sentence["words"]]


def assert_consistent(sentence):
    """Invariants every sentence of result.json must satisfy."""
    words = sentence["words"]
    assert isinstance(words, list)
    prev_s = sentence["start_ms"]
    for x in words:
        assert set(x) == {"w", "s", "e"}
        assert isinstance(x["s"], int) and isinstance(x["e"], int)
        assert x["w"] and x["w"] == x["w"].strip()
        assert sentence["start_ms"] <= prev_s <= x["s"] <= x["e"] <= sentence["end_ms"]
        prev_s = x["s"]
    # Same words as the text: only the whitespace between them differs.
    assert "".join(ws(sentence)) == "".join(sentence["text"].split())


# --- words follow the text ----------------------------------------------------

def test_words_match_text():
    out = split_sentences(mk("Okay, let's get started.".split()) +
                          mk("Today we talk about caches.".split(), start=1.2))
    assert [s["text"] for s in out] == ["Okay, let's get started.",
                                        "Today we talk about caches."]
    for s in out:
        assert " ".join(ws(s)) == s["text"]
        assert_consistent(s)
    assert times(out[0]) == [("Okay,", 0, 300), ("let's", 300, 600), ("get", 600, 900),
                             ("started.", 900, 1200)]
    assert out[1]["words"][0]["s"] == out[1]["start_ms"] == 1200
    assert out[1]["words"][-1]["e"] == out[1]["end_ms"] == 2700


def test_words_are_whisper_words_with_attached_punctuation():
    # faster-whisper may split "two-way" into " two" + "-way" and "3.14" into " 3" + ".14":
    # each whisper word is one entry, stripped of whitespace, punctuation kept.
    words = [raw(" A", 0.0, 0.2), raw(" two", 0.2, 0.4), raw("-way", 0.4, 0.6),
             raw(" set", 0.6, 0.8), raw(" is", 0.8, 1.0), raw(" 3", 1.0, 1.2),
             raw(".14", 1.2, 1.4), raw(' "fast."', 1.4, 2.0)]
    out = split_sentences(words)
    assert len(out) == 1
    assert out[0]["text"] == 'A two-way set is 3.14 "fast."'
    assert ws(out[0]) == ["A", "two", "-way", "set", "is", "3", ".14", '"fast."']
    assert_consistent(out[0])


def test_words_cover_every_word_once_across_split_sentences():
    # Over-long sentences (forced and comma fallbacks) and the short-sentence merge:
    # every input word ends up in exactly one sentence, in order.
    texts = ["w%d" % i for i in range(130)]
    texts[39] += ","
    texts[99] += "."
    texts += ["Yes."]
    src = mk(texts, dur=0.2)
    out = split_sentences(src)
    assert len(out) > 2
    assert [w for s in out for w in ws(s)] == [w["word"].strip() for w in src]
    for s in out:
        assert len(s["words"]) == len(s["text"].split())
        assert_consistent(s)


def test_words_of_a_merged_short_sentence():
    words = mk("This is the first sentence.".split()) + mk(["Yes."], start=1.5, dur=0.4)
    out = split_sentences(words)
    assert [s["text"] for s in out] == ["This is the first sentence. Yes."]
    assert ws(out[0]) == ["This", "is", "the", "first", "sentence.", "Yes."]
    assert out[0]["words"][-1] == {"w": "Yes.", "s": 1500, "e": 1900}
    assert_consistent(out[0])


def test_low_conf_index_points_at_the_same_word():
    out = split_sentences(mk("The cash hierarchy matters.".split(), probs=[0.9, 0.3, 0.9, 0.4]))
    for lc in out[0]["low_conf_words"]:
        assert out[0]["words"][lc["i"]]["w"] == lc["w"]


# --- milliseconds ---------------------------------------------------------------

def test_word_times_are_integer_ms_rounded_half_up():
    words = [raw(" Caches", 1.2344, 1.0625 + 0.5), raw(" are", 2.25, 2.5),
             raw(" small.", 2.5, 4.2006), raw(" Tiny", 4.2006, 4.2006 + 0.0009765625),
             raw(" bit.", 5.0, 6.0)]
    out = split_sentences(words)
    assert times(out[0]) == [("Caches", 1234, 1563), ("are", 2250, 2500),
                             ("small.", 2500, 4201)]
    assert times(out[1]) == [("Tiny", 4201, 4202), ("bit.", 5000, 6000)]
    for s in out:
        assert s["words"][0]["s"] == s["start_ms"]
        assert s["words"][-1]["e"] == s["end_ms"]
        assert_consistent(s)


def test_word_times_are_relative_to_the_lecture_start():
    out = split_sentences(mk("Late in the lecture.".split(), start=3600.0))
    assert times(out[0])[0] == ("Late", 3600000, 3600300)
    assert out[0]["words"][-1]["e"] == out[0]["end_ms"] == 3601200


# --- monotonic correction --------------------------------------------------------

def test_word_ending_before_it_starts_is_corrected():
    words = [raw(" A", 1.0, 1.5), raw(" b", 1.4, 1.3), raw(" c.", 1.7, 2.0)]
    out = split_sentences(words)
    assert times(out[0]) == [("A", 1000, 1500), ("b", 1400, 1400), ("c.", 1700, 2000)]
    assert_consistent(out[0])


def test_word_starting_before_the_previous_word_is_corrected():
    words = [raw(" A", 1.0, 1.5), raw(" b", 1.4, 1.6), raw(" c", 1.2, 1.8),
             raw(" d.", 1.9, 2.0)]
    out = split_sentences(words)
    assert times(out[0]) == [("A", 1000, 1500), ("b", 1400, 1600), ("c", 1400, 1800),
                             ("d.", 1900, 2000)]
    assert_consistent(out[0])


def test_correction_after_rounding_to_ms():
    # "two" ends 0.2 ms before it starts: 1000 < 1001 after rounding, so e is raised to s.
    words = [raw(" One", 0.0, 1.0), raw(" two", 1.0006, 1.0004), raw(" three.", 1.5, 2.0)]
    out = split_sentences(words)
    assert times(out[0]) == [("One", 0, 1000), ("two", 1001, 1001), ("three.", 1500, 2000)]
    assert_consistent(out[0])


def test_words_stay_inside_the_sentence():
    # A middle word that ends after the last word, and a last word that starts after
    # it ends: the sentence end_ms (unchanged rule) caps every word.
    words = [raw(" Wide", 0.0, 5.0), raw(" end.", 1.0, 2.0)]
    out = split_sentences(words)
    assert (out[0]["start_ms"], out[0]["end_ms"]) == (0, 2000)
    assert times(out[0]) == [("Wide", 0, 2000), ("end.", 1000, 2000)]
    assert_consistent(out[0])

    words = [raw(" Odd", 0.0, 1.0), raw(" order.", 3.0, 2.5)]
    out = split_sentences(words)
    assert (out[0]["start_ms"], out[0]["end_ms"]) == (0, 2500)
    assert times(out[0]) == [("Odd", 0, 1000), ("order.", 2500, 2500)]
    assert_consistent(out[0])


# --- no words ---------------------------------------------------------------------

def test_whitespace_only_words_are_left_out():
    words = [raw(" We", 0.0, 0.3), raw(" ", 0.3, 0.4), raw(" go.", 0.4, 0.8)]
    out = split_sentences(words)
    assert out[0]["text"] == "We  go."
    assert times(out[0]) == [("We", 0, 300), ("go.", 400, 800)]
    assert_consistent(out[0])


def test_sentence_without_words_has_an_empty_list():
    out = split_sentences([raw(" ", 0.0, 0.5), raw("  ", 0.5, 2.0)])
    assert out == [{"start_ms": 0, "end_ms": 2000, "text": "", "low_conf_words": [],
                    "words": []}]


def test_no_speech_has_no_sentences():
    assert split_sentences([]) == []


# --- filters ----------------------------------------------------------------------

def W(text, t, dur=0.5):
    return {"word": " " + text, "start": float(t), "end": float(t) + dur, "probability": 0.9}


def test_filtered_words_are_not_left_in_words():
    """Dropped segments and collapsed repeats leave no trace in words."""
    segs = [
        SimpleNamespace(start=0.0, end=2.0, no_speech_prob=0.1, avg_logprob=-0.2,
                        words=[W("We", 0), W("start", 1), W("now.", 2)]),
        SimpleNamespace(start=2.0, end=4.0, no_speech_prob=0.95, avg_logprob=-1.6,
                        words=[W("Subtitles", 3), W("by", 4), W("Amara.", 5)]),
        SimpleNamespace(start=4.0, end=6.0, no_speech_prob=0.1, avg_logprob=-0.3,
                        words=[W("Thank", 6), W("you.", 7), W("Thank", 8)]),
        SimpleNamespace(start=6.0, end=8.0, no_speech_prob=0.1, avg_logprob=-0.3,
                        words=[W("you.", 9), W("Thank", 10), W("you.", 11)]),
        SimpleNamespace(start=8.0, end=13.0, no_speech_prob=0.1, avg_logprob=-0.3,
                        words=[W("the", 12, 0.1), W("the", 12.1, 0.1), W("the", 12.2, 0.1),
                               W("end.", 12.3, 0.7)]),
    ]

    class Model:
        def transcribe(self, audio, **kw):
            return iter(segs), SimpleNamespace(duration=13.0, language="en")

    job = asr.Job("a.wav", "large-v3", "float16", "en", "", "cuda", 4)
    result = asr.transcribe_job(Model(), [0.0] * 16000, job, lambda line: None,
                                lambda text: len(text.split()))
    sentences = result["sentences"]
    assert [s["text"] for s in sentences] == ["We start now.", "Thank you.", "the end."]
    assert [times(s) for s in sentences] == [
        [("We", 0, 500), ("start", 1000, 1500), ("now.", 2000, 2500)],
        [("Thank", 6000, 6500), ("you.", 7000, 7500)],       # the first copy is kept
        [("the", 12000, 12100), ("end.", 12300, 13000)],
    ]
    for s in sentences:
        assert " ".join(ws(s)) == s["text"]
        assert_consistent(s)
