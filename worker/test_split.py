# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""Sentence splitting rules (SPEC §6.2, §14)."""
import pytest

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


def texts(sentences):
    return [s["text"] for s in sentences]


def split(*sentence_strings, **kw):
    words = mk(" ".join(sentence_strings).split(), **kw)
    return texts(split_sentences(words))


# --- basic breaking --------------------------------------------------------

def test_breaks_on_period_question_and_exclamation():
    assert split("This is a cache.", "Is it fast?", "It is fast!", "Okay then") == [
        "This is a cache.", "Is it fast?", "It is fast!", "Okay then"]


def test_empty_input():
    assert split_sentences([]) == []


def test_text_is_raw_concatenation_stripped():
    words = mk(["two"]) + [{"word": "-way", "start": 0.3, "end": 0.6, "probability": 0.9}] \
        + mk(["set", "associative."], start=0.6)
    out = split_sentences(words)
    assert texts(out) == ["two-way set associative."]


def test_closing_quote_after_period_still_breaks():
    assert split('He said "stop."', "Then we left.") == ['He said "stop."', "Then we left."]


# --- exceptions ------------------------------------------------------------

@pytest.mark.parametrize("abbr", ["e.g.", "i.e.", "etc.", "vs.", "Dr.", "Mr.", "Mrs.", "Ms.",
                                  "Prof.", "Fig.", "Eq.", "No.", "approx.", "E.G.", "(e.g.",
                                  "Approx."])
def test_abbreviations_do_not_break(abbr):
    assert split("See the", abbr, "result here today.") == [
        "See the %s result here today." % abbr]


def test_abbreviation_with_question_mark_still_breaks():
    # The abbreviation exception only applies to a trailing period.
    assert split("Is the answer yes or no?", "It is yes.") == [
        "Is the answer yes or no?", "It is yes."]


def test_single_capital_initial_does_not_break():
    assert split("Alan M. Turing proposed it.", "J. Smith agreed.") == [
        "Alan M. Turing proposed it.", "J. Smith agreed."]


def test_lowercase_single_letter_breaks():
    assert split("This is plan b.", "Next one here.") == ["This is plan b.", "Next one here."]


def test_decimal_number_does_not_break():
    assert split("Pi is 3.14 roughly here.", "The value is 2.5.", "Next point now.") == [
        "Pi is 3.14 roughly here.", "The value is 2.5.", "Next point now."]


# --- over-long sentences ---------------------------------------------------

def _numbered(n, punct=None, last="."):
    """w0 w1 ... with optional punctuation per index and a final terminal mark."""
    punct = punct or {}
    words = ["w%d%s" % (i, punct.get(i, "")) for i in range(n)]
    words[-1] += last
    return words


def test_over_60_words_falls_back_to_last_comma():
    words = mk(_numbered(70, {9: ",", 39: ","}), dur=0.2)   # 14 s, well under 30 s
    out = split_sentences(words)
    assert [len(s["text"].split()) for s in out] == [40, 30]
    assert out[0]["text"].endswith("w39,")
    assert out[1]["text"].startswith("w40")


def test_semicolon_counts_as_fallback_point():
    words = mk(_numbered(65, {20: ";"}), dur=0.2)
    out = split_sentences(words)
    assert [len(s["text"].split()) for s in out] == [21, 44]


def test_over_60_words_without_comma_forced_at_60th_word():
    words = mk(_numbered(130), dur=0.2)
    out = split_sentences(words)
    assert [len(s["text"].split()) for s in out] == [60, 60, 10]
    assert out[0]["text"].split()[-1] == "w59"
    assert out[1]["text"].split()[0] == "w60"


def test_comma_on_the_61st_word_is_not_used():
    # The fallback point must lie inside the first 60 words.
    words = mk(_numbered(70, {10: ",", 60: ","}), dur=0.2)
    out = split_sentences(words)
    assert [len(s["text"].split()) for s in out] == [11, 59]


def test_over_30_seconds_falls_back_to_last_comma():
    words = mk(_numbered(20, {9: ","}), dur=2.0)   # 40 s, 20 words
    out = split_sentences(words)
    assert [len(s["text"].split()) for s in out] == [10, 10]
    assert out[0]["end_ms"] == 20000
    assert out[1]["start_ms"] == 20000


def test_over_30_seconds_without_comma_is_not_cut_before_60_words():
    words = mk(_numbered(20), dur=2.0)   # 40 s, no ',' or ';'
    out = split_sentences(words)
    assert len(out) == 1
    assert out[0]["end_ms"] == 40000


def test_over_30_seconds_breaks_at_comma_that_appears_later():
    words = mk(_numbered(25, {17: ","}), dur=2.0)  # overflow at w15, first comma at w17
    out = split_sentences(words)
    assert [len(s["text"].split()) for s in out] == [18, 7]


# --- short sentence merge --------------------------------------------------

def test_short_single_word_sentence_merges_into_previous():
    words = mk("This is the first sentence.".split()) \
        + mk(["Yes."], start=1.5, dur=0.4) \
        + mk("And then a second one.".split(), start=2.0)
    out = split_sentences(words)
    assert texts(out) == ["This is the first sentence. Yes.", "And then a second one."]
    assert out[0]["end_ms"] == 1900


def test_single_word_lasting_one_second_is_kept():
    words = mk("This is the first sentence.".split()) + mk(["Okay."], start=1.5, dur=1.0)
    assert texts(split_sentences(words)) == ["This is the first sentence.", "Okay."]


def test_two_short_words_are_kept():
    words = mk("This is the first sentence.".split()) + mk(["Thank", "you."], start=1.5, dur=0.3)
    assert texts(split_sentences(words)) == ["This is the first sentence.", "Thank you."]


def test_short_first_sentence_has_nothing_to_merge_into():
    words = mk(["So."], dur=0.3) + mk("Let us begin now.".split(), start=0.5)
    assert texts(split_sentences(words)) == ["So.", "Let us begin now."]


def test_trailing_short_fragment_merges():
    words = mk("Any questions before we move on?".split()) + mk(["Right"], start=2.0, dur=0.2)
    assert texts(split_sentences(words)) == ["Any questions before we move on? Right"]


# --- timestamps and low-confidence words -----------------------------------

def test_start_and_end_ms_come_from_first_and_last_word():
    words = [
        {"word": " Caches", "start": 1.2344, "end": 1.6, "probability": 0.9},
        {"word": " are", "start": 1.6, "end": 1.8, "probability": 0.9},
        {"word": " small.", "start": 1.8, "end": 4.2006, "probability": 0.9},
        {"word": " Next", "start": 5.0, "end": 5.5, "probability": 0.9},
        {"word": " part.", "start": 5.5, "end": 6.0, "probability": 0.9},
    ]
    out = split_sentences(words)
    assert [(s["start_ms"], s["end_ms"]) for s in out] == [(1234, 4201), (5000, 6000)]
    assert all(isinstance(s["start_ms"], int) and isinstance(s["end_ms"], int) for s in out)


def test_low_conf_word_index_is_relative_to_sentence():
    words = mk("Okay let us start.".split()) + mk(
        "The cash hierarchy matters.".split(), start=2.0, probs=[0.9, 0.3, 0.5, 0.49])
    out = split_sentences(words)
    assert out[0]["low_conf_words"] == []
    assert out[1]["low_conf_words"] == [{"i": 1, "w": "cash"}, {"i": 3, "w": "matters."}]


def test_low_conf_index_after_merge():
    words = mk("This is the first sentence.".split()) + mk(["Yes."], start=1.5, probs=[0.2])
    out = split_sentences(words)
    assert out[0]["low_conf_words"] == [{"i": 5, "w": "Yes."}]


def test_accepts_faster_whisper_word_objects():
    from faster_whisper.transcribe import Word as FWWord
    dict_words = mk("Hello there.".split()) + mk("How are you?".split(), start=1.0,
                                                 probs=[0.9, 0.1, 0.9])
    fw_words = [FWWord(start=w["start"], end=w["end"], word=w["word"],
                       probability=w["probability"]) for w in dict_words]
    assert split_sentences(fw_words) == split_sentences(dict_words)
    tuple_words = [asr.Word(w["word"], w["start"], w["end"], w["probability"])
                   for w in dict_words]
    assert split_sentences(tuple_words) == split_sentences(dict_words)


def test_result_sentence_shape():
    out = split_sentences(mk("A small test sentence.".split(), probs=[0.9, 0.2, 0.9, 0.9]))
    assert out == [{"start_ms": 0, "end_ms": 1200, "text": "A small test sentence.",
                    "low_conf_words": [{"i": 1, "w": "small"}],
                    "words": [{"w": "A", "s": 0, "e": 300}, {"w": "small", "s": 300, "e": 600},
                              {"w": "test", "s": 600, "e": 900},
                              {"w": "sentence.", "s": 900, "e": 1200}]}]
