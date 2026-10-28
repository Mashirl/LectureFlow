# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""Statistical repair of degenerate ASR text (worker/textrepair.py)."""
import itertools
import json
import os
import subprocess
import sys

import pytest

import textrepair as tr
from asr_common import MAX_SENTENCE_WORDS, Word, is_sentence_end, split_sentences

DUR = 0.25     # every word lasts this long ...
PAUSE = 0.6    # ... and a sentence end is followed by this pause

# Sentences of the "normal" text the repair learns from: a pause after each sentence,
# sentences often start with so / now / okay, end with ... right / okay.
NORMAL = [
    "So the key is a random string of bits.",
    "Now we take the message and the key.",
    "Okay this gives us the ciphertext right.",
    "The attacker cannot learn anything about the message.",
    "We use XOR for this step okay.",
    "I think this is very important for the exam.",
    "So we repeat this for every block of the message.",
    "Now the receiver does the same thing with Python.",
]

# The same kind of sentences, as a degenerate window writes them: no punctuation, no
# sentence-initial capitals ("I" and acronyms keep theirs), the pause after a sentence
# inside the next word.
DEGENERATE = [
    "so the attacker takes the ciphertext and the key",
    "now the message is a random string of bits right",
    "okay we use XOR for every block of the message",
    "so I think the receiver does the same thing",
    "now this is very important for the exam okay",
    "so we repeat this step for the ciphertext",
]


def normal_words(sentences, t0=0.0):
    out, t = [], t0
    for s in sentences:
        for tok in s.split():
            out.append(Word(" " + tok, t, t + DUR, 0.9))
            t += DUR
        t += PAUSE
    return out, t


def degenerate_words(sentences, t0, pause=PAUSE):
    """No gap between words; the first word of each sentence but the first absorbs the
    pause (it starts where the previous word ends)."""
    out, t = [], t0
    for si, s in enumerate(sentences):
        for wi, tok in enumerate(s.split()):
            extra = pause if (wi == 0 and si > 0) else 0.0
            out.append(Word(" " + tok, t, t + DUR + extra, 0.9))
            t += DUR + extra
    return out, t + PAUSE


def lecture(*parts, normal_rounds=4):
    """Normal text (NORMAL repeated), then each part, separated by normal sentences."""
    words, t = normal_words(NORMAL * normal_rounds)
    for part in parts:
        w, t = part(t)
        words += w
        w, t = normal_words(NORMAL[:2], t)
        words += w
    return words


def same_shape(inp, out):
    """No word added or dropped, times and probabilities unchanged, only case and an
    appended mark changed."""
    assert len(inp) == len(out)
    for a, b in zip(inp, out):
        assert (a.start, a.end, a.probability) == (b.start, b.end, b.probability)
        ta, tb = a.word, b.word
        assert tb.startswith(ta[:len(ta) - len(ta.lstrip())])
        base = tb[:-1] if len(tb) == len(ta) + 1 and tb[-1] in ".," else tb
        assert base.lower() == ta.lower(), (ta, tb)


def added_marks(inp, out):
    return [(k, b.word[-1]) for k, (a, b) in enumerate(zip(inp, out)) if a.word != b.word
            and len(b.word) == len(a.word) + 1]


# --- normal text and the basics ------------------------------------------------

def test_empty_input():
    rep = tr.repair_words([])
    assert rep.words == [] and rep.stats["words"] == 0


def test_normal_text_is_unchanged():
    words = lecture()
    rep = tr.repair_words(words, glossary=["XOR", "Python"])
    assert [w.word for w in rep.words] == [w.word for w in words]
    assert rep.words == words
    assert rep.stats["sentence_ends_added"] == 0
    assert rep.stats["commas_added"] == 0
    assert rep.stats["words_recased"] == 0
    assert rep.stats["segments"] == 0
    assert rep.stats["train_sentences"] >= 20


def test_accepts_dicts_like_whisper_words():
    words = lecture(lambda t: degenerate_words(DEGENERATE, t))
    dicts = [{"word": w.word, "start": w.start, "end": w.end, "probability": w.probability}
             for w in words]
    assert tr.repair_words(dicts).words == tr.repair_words(words).words


def test_deterministic():
    words = lecture(lambda t: degenerate_words(DEGENERATE, t))
    a = tr.repair_words(words, glossary=["XOR"])
    b = tr.repair_words(list(words), glossary=["XOR"])
    assert a.words == b.words and a.stats == b.stats


# --- sentence ends ----------------------------------------------------------------

def test_unpunctuated_stretch_gets_sentence_ends():
    words = lecture(lambda t: degenerate_words(DEGENERATE, t))
    rep = tr.repair_words(words)
    same_shape(words, rep.words)
    start = len(normal_words(NORMAL * 4)[0])
    lengths = [len(s.split()) for s in DEGENERATE]
    # the stretch runs on into the normal sentence after it ("So the key ... bits."),
    # so the end of the last degenerate sentence is found as well
    expected = [start + n - 1 for n in itertools.accumulate(lengths)]
    marks = added_marks(words, rep.words)
    assert [k for k, m in marks if m == "."] == expected
    assert rep.stats["sentence_ends_added"] == len(expected)
    assert rep.stats["unpunctuated_segments"] == 1
    # each new sentence starts with a capital; "I" and XOR keep theirs
    texts = [s["text"] for s in split_sentences(rep.words)]
    for s in DEGENERATE:
        want = s[0].upper() + s[1:] + "."
        assert want in texts, want
    assert "So I think the receiver does the same thing." in texts
    assert "Okay we use XOR for every block of the message." in texts


def test_no_sentence_shorter_than_the_minimum():
    # a pause after every second word: the model would cut everywhere
    pairs = ["so then", "now this", "okay we", "so that", "now it", "okay so"] * 4
    words = lecture(lambda t: degenerate_words(pairs, t))
    rep = tr.repair_words(words)
    same_shape(words, rep.words)
    dots = [k for k, m in added_marks(words, rep.words) if m == "."]
    assert dots, "strong pauses: some sentence ends expected"
    first = len(normal_words(NORMAL * 4)[0])
    edges = [first - 1] + dots
    assert min(b - a for a, b in zip(edges, edges[1:])) >= tr.MIN_SENTENCE_WORDS


def test_long_stretch_is_cut_at_a_comma_not_at_the_60th_word():
    # 150 words without any pause or cue: no '.' inside, but ',' so §6.2 never cuts
    # mid-phrase (the pause and "So" after the last word still end that sentence)
    filler = ("the key and the message of the block with the bits " * 15).split()
    words = lecture(lambda t: degenerate_words([" ".join(filler)], t))
    rep = tr.repair_words(words)
    same_shape(words, rep.words)
    first = len(normal_words(NORMAL * 4)[0])
    last = first + len(filler) - 1
    marks = added_marks(words, rep.words)
    assert marks[-1] == (last, ".")
    inside = marks[:-1]
    assert len(inside) >= 2 and all(m == "," for _, m in inside)
    assert rep.stats["commas_added"] == len(inside)
    for s in split_sentences(rep.words):
        assert len(s["words"]) <= MAX_SENTENCE_WORDS
        assert s["text"][-1] in ".?!,", s["text"]


def test_uncertain_stretch_is_left_alone():
    # 40 words (no §6.2 overflow), evenly timed, no sentence-start words, and the normal
    # text after it starts without a pause or capital: no mark is added
    filler = ("the key and the message of the block with the bits " * 4).split()

    def build(t):
        w, _ = degenerate_words([" ".join(filler)], t)
        t = w[-1].end
        tail = []
        for tok in "of the message and the key.".split():
            tail.append(Word(" " + tok, t, t + DUR, 0.9))
            t += DUR
        return w + tail, t + PAUSE

    words = lecture(build)
    rep = tr.repair_words(words)
    assert added_marks(words, rep.words) == []
    assert rep.stats["unpunctuated_segments"] == 1
    assert rep.stats["sentence_ends_added"] == rep.stats["commas_added"] == 0
    # only the stretch's first word changes: it follows a full stop
    first = len(normal_words(NORMAL * 4)[0])
    changed = [k for k, (a, b) in enumerate(zip(words, rep.words)) if a != b]
    assert changed == [first] and rep.words[first].word == " The"


def test_without_enough_normal_text_no_sentence_end_is_added():
    words, _ = degenerate_words(DEGENERATE * 3, 0.0)
    rep = tr.repair_words(words)
    assert rep.stats["train_sentences"] < tr.MIN_TRAIN_SENTENCES
    assert rep.stats["sentence_ends_added"] == 0
    same_shape(words, rep.words)


def test_short_stretch_is_not_touched():
    # fewer than RUN_MIN_WORDS words without punctuation (9 + the 8 of the normal
    # sentence after them) count as normal text
    words = lecture(lambda t: degenerate_words(DEGENERATE[:1], t))
    rep = tr.repair_words(words)
    assert rep.words == words


def test_no_period_after_an_abbreviation_or_inside_a_joined_word():
    part = ["so the attacker takes the ciphertext and the key etc",
            "now the message is a random string of bits right",
            "okay we use the two", "-way exchange for the message"]

    def build(t):
        w, t = degenerate_words(part, t)
        # " two" + "-way" is one word: no space before "-way", no pause inside it
        for k, x in enumerate(w):
            if x.word == " -way":
                w[k] = Word("-way", w[k - 1].end, w[k - 1].end + DUR, 0.9)
                for j in range(k + 1, len(w)):
                    w[j] = Word(w[j].word, w[j].start - PAUSE, w[j].end - PAUSE, 0.9)
        return w, t

    words = lecture(build)
    rep = tr.repair_words(words)
    same_shape(words, rep.words)
    for a, b in zip(words, rep.words):
        if a.word in (" etc", " two"):
            assert not is_sentence_end(b.word) and not b.word.endswith("."), b.word


# --- casing -----------------------------------------------------------------------

def test_all_caps_span_is_recased_and_glossary_terms_keep_their_capitals():
    caps = "SO WE USE XOR AND THE LFSR FOR THE KEY. NOW THE ATTACKER CANNOT LEARN ANYTHING."

    def build(t):
        out = []
        for tok in caps.split():
            out.append(Word(" " + tok, t, t + DUR, 0.9))
            t += DUR + (PAUSE if tok.endswith(".") else 0.0)
        return out, t

    words = lecture(build)
    rep = tr.repair_words(words, glossary=["LFSR", "XOR"])
    same_shape(words, rep.words)
    text = "".join(w.word for w in rep.words)
    assert "So we use XOR and the LFSR for the key. Now the attacker cannot learn anything." \
        in text
    assert rep.stats["caps_segments"] == 1
    assert rep.stats["sentence_ends_added"] == 0


def test_unknown_caps_words_become_lower_case_and_digits_stay():
    caps = "SO THE ATTACKER USES RC4 WITH A FROBNICATED KEY."
    words = lecture(lambda t: normal_words([caps], t))
    rep = tr.repair_words(words)
    text = "".join(w.word for w in rep.words)
    assert "So the attacker uses RC4 with a frobnicated key." in text


def test_acronyms_alone_are_not_an_all_caps_span():
    words = lecture(lambda t: normal_words(["We compare AES DES RSA and XOR today."], t))
    rep = tr.repair_words(words)
    assert rep.words == words


def test_all_caps_window_with_a_stray_lower_case_word():
    # lower-case "x" splits the window into runs of two ordinary words each: too short
    # for an all-caps span on their own, but the decoding window as a whole is all caps
    caps = "THE KEY x THE MESSAGE x AND KEY."
    words = lecture(lambda t: normal_words([caps], t))
    start = len(normal_words(NORMAL * 4)[0])
    assert tr.repair_words(words).words == words
    win = [0.0, words[start].start, words[start + 8].start]
    rep = tr.repair_words(words, window_starts=win)
    same_shape(words, rep.words)
    text = "".join(w.word for w in rep.words[start:start + 8])
    assert text == " The key x the message x and key."
    assert rep.stats["caps_segments"] == 1
    # a window of acronyms only is not all caps
    words = lecture(lambda t: normal_words(["AES DES RSA XOR."], t))
    win = [0.0, words[start].start, words[start + 4].start]
    assert tr.repair_words(words, window_starts=win).words == words


def test_lower_case_stretch_gets_usual_capitals():
    part = ["so i think the receiver uses python and xor",
            "now i'm sure the attacker cannot learn anything about the message",
            "okay we take the key"]
    words = lecture(lambda t: degenerate_words(part, t))
    rep = tr.repair_words(words, glossary=["XOR"])
    same_shape(words, rep.words)
    text = "".join(w.word for w in rep.words)
    assert "So I think the receiver uses Python and XOR." in text
    assert "Now I'm sure the attacker" in text


def test_pronoun_rule_only_for_english():
    part = ["so i think the receiver uses the key and the message of the block for i"]
    words = lecture(lambda t: degenerate_words(part * 2, t))
    en = "".join(w.word for w in tr.repair_words(words, language="en").words)
    xx = "".join(w.word for w in tr.repair_words(words, language="de").words)
    assert " I think" in en and " i think" in xx


# --- stored results ---------------------------------------------------------------

def test_words_from_sentences_round_trip():
    words = [Word(" So", 0.0, 0.2, 0.9), Word(" a", 0.2, 0.3, 0.3), Word(" two", 0.3, 0.5, 0.9),
             Word("-way", 0.5, 0.7, 0.9), Word(" handshake.", 0.7, 1.0, 0.9),
             Word(" Then", 1.5, 1.7, 0.9), Word(" done,", 1.7, 1.9, 0.2),
             Word(" right?", 1.9, 2.3, 0.9)]
    sents = split_sentences(words)
    back = tr.words_from_sentences(sents)
    assert [w.word for w in back] == [w.word for w in words]
    assert split_sentences(back) == sents


def test_words_from_sentences_needs_word_times():
    with pytest.raises(ValueError):
        tr.words_from_sentences([{"start_ms": 0, "end_ms": 10, "text": "Hi.",
                                  "low_conf_words": []}])


def rows(sentences, keep=()):
    """Stored sentences as the database gives them (asr_text), keep for the indexes in keep."""
    out = []
    for i, x in enumerate(sentences):
        r = {"start_ms": x["start_ms"], "end_ms": x["end_ms"], "asr_text": x["text"],
             "low_conf_words": x["low_conf_words"], "words": x["words"]}
        if i in keep:
            r["keep"] = True
        out.append(r)
    return out


def forced(sentences):
    return [x for x in sentences if x["text"][-1] not in ".?!"]


def test_words_from_database_rows_round_trip():
    words = lecture(lambda t: degenerate_words(DEGENERATE * 2, t))
    before = split_sentences(words)
    assert tr.words_from_sentences(rows(before)) == tr.words_from_sentences(before)
    assert split_sentences(tr.words_from_sentences(rows(before))) == before


def test_repair_sentences_of_a_stored_result():
    words = lecture(lambda t: degenerate_words(DEGENERATE * 2, t))
    before = split_sentences(words)
    assert len(forced(before)) >= 1
    for given in (before, rows(before)):        # result.json or database rows
        after, stats = tr.repair_sentences(given)
        assert stats["sentence_ends_added"] > 0
        assert stats["kept_sentences"] == 0
        assert stats["sentences_in"] == len(before) and stats["sentences_out"] == len(after)
        assert forced(after) == []
        assert sum(len(x["words"]) for x in after) == sum(len(x["words"]) for x in before)
        # the normal sentences come back exactly as they were
        normal = [x for x in before if x["start_ms"] < words[len(normal_words(NORMAL * 4)[0])
                                                              ].start * 1000 - 1]
        assert after[:len(normal)] == normal
        assert after[-1] == before[-1]


def test_kept_sentence_is_unchanged_and_its_neighbours_are_repaired():
    words = lecture(lambda t: degenerate_words(DEGENERATE * 3, t))
    before = split_sentences(words)
    chunks = [i for i, x in enumerate(before) if x in forced(before)]
    assert len(chunks) >= 2
    k = chunks[1]                       # a 60-word chunk in the middle of the stretch
    after, stats = tr.repair_sentences(rows(before, keep={k}))
    assert stats["kept_sentences"] == 1
    kept = before[k]
    assert after.count(kept) == 1       # boundaries, text, low_conf_words, words: as they were
    at = after.index(kept)
    # nothing moved into or out of it; the words around it were repaired
    assert all(x["end_ms"] <= kept["start_ms"] for x in after[:at])
    assert all(x["start_ms"] >= kept["end_ms"] for x in after[at + 1:])
    assert any(x not in before for x in after[:at])
    assert any(x not in before for x in after[at + 1:])
    # the kept chunk starts and ends mid-sentence, so the sentences touching it end
    # without a full stop; no other one does
    assert [x for x in forced(after) if x not in after[at - 1:at + 2]] == []
    assert sum(len(x["words"]) for x in after) == sum(len(x["words"]) for x in before)


def test_kept_sentences_still_teach_the_statistics():
    # every normal sentence kept: the model still learns from them and repairs the rest
    words = lecture(lambda t: degenerate_words(DEGENERATE * 2, t))
    before = split_sentences(words)
    normal = {i for i, x in enumerate(before) if x["text"][-1] == "." and
              len(x["words"]) < 15}
    after, stats = tr.repair_sentences(rows(before, keep=normal))
    assert stats["train_sentences"] >= tr.MIN_TRAIN_SENTENCES
    assert stats["kept_sentences"] == len(normal)
    assert stats["sentence_ends_added"] > 0
    for i in normal:
        assert before[i] in after


def test_every_sentence_kept_means_nothing_changes():
    words = lecture(lambda t: degenerate_words(DEGENERATE * 2, t))
    before = split_sentences(words)
    after, stats = tr.repair_sentences(rows(before, keep=set(range(len(before)))))
    assert after == before
    assert stats["sentence_ends_added"] == stats["words_recased"] == stats["segments"] == 0


# --- command line (SPEC §6.0) -----------------------------------------------------

def write_job(tmp_path, job):
    path = tmp_path / "job.json"
    path.write_text(json.dumps(job), encoding="utf-8")
    return str(path)


def last_error(err):
    return [line for line in err.splitlines() if line][-1]


def test_cli_writes_the_repaired_result(tmp_path, capsys):
    words = lecture(lambda t: degenerate_words(DEGENERATE * 2, t))
    before = split_sentences(words)
    job = write_job(tmp_path, {"sentences": rows(before, keep={0}), "glossary": ["XOR"],
                               "language": "en", "prompt_terms_used": 7})
    out = tmp_path / "result.json.tmp"
    assert tr.main(["--in", job, "--out", str(out)]) == 0
    assert not (tmp_path / "result.json.tmp.part").exists()
    res = json.loads(out.read_text(encoding="utf-8"))
    want, stats = tr.repair_sentences(rows(before, keep={0}), ["XOR"], "en")
    assert res["sentences"] == want
    assert res["prompt_terms_used"] == 7
    st = res["textrepair"]
    assert isinstance(st.pop("ms"), int)
    assert st == stats and st["kept_sentences"] == 1
    lines = capsys.readouterr().out.splitlines()
    assert lines[0] == "PROGRESS 0.50" and lines[-1] == "PROGRESS 1.00"
    assert any(line.startswith("textrepair: ") for line in lines)


def test_cli_skips_lectures_that_are_not_english(tmp_path):
    words = lecture(lambda t: degenerate_words(DEGENERATE * 2, t))
    before = split_sentences(words)
    job = write_job(tmp_path, {"sentences": rows(before), "language": "de"})
    out = tmp_path / "result.json.tmp"
    assert tr.main(["--in", job, "--out", str(out)]) == 0
    res = json.loads(out.read_text(encoding="utf-8"))
    assert res["sentences"] == before
    assert res["prompt_terms_used"] == 0
    assert res["textrepair"]["skipped"] == "language"
    assert tr.enabled_for("en") and tr.enabled_for("en-GB") and tr.enabled_for(" EN")
    assert not tr.enabled_for("") and not tr.enabled_for(None) and not tr.enabled_for("de")


@pytest.mark.parametrize("job", [
    "not json",
    {"sentences": []},
    {"sentences": [{"start_ms": 0, "end_ms": 10, "asr_text": "Hi.", "low_conf_words": []}]},
    {"sentences": [{"start_ms": 0, "end_ms": 10, "asr_text": "Hi.",
                    "words": [{"w": "Hi.", "s": 0, "e": 10}], "keep": "yes"}]},
    {"sentences": [{"start_ms": -1, "end_ms": 10, "asr_text": "Hi.",
                    "words": [{"w": "Hi.", "s": 0, "e": 10}]}]},
    {"sentences": [{"start_ms": 0, "end_ms": 10, "asr_text": "Hi.",
                    "words": [{"w": "Hi.", "s": 0.5, "e": 10}]}]},
    {"sentences": [{"start_ms": 0, "end_ms": 10, "text": "Hi.",
                    "words": [{"w": "Hi.", "s": 0, "e": 10}]}]},
    {"sentences": [{"start_ms": 0, "end_ms": 10, "asr_text": "Hi.",
                    "words": [{"w": "Hi.", "s": 0, "e": 10}]}], "glossary": "XOR"},
    {"sentences": [{"start_ms": 0, "end_ms": 10, "asr_text": "Hi.",
                    "words": [{"w": "Hi.", "s": 0, "e": 10}]}], "prompt_terms_used": -1},
])
def test_cli_input_errors_exit_2(tmp_path, capsys, job):
    path = tmp_path / "job.json"
    path.write_text(job if isinstance(job, str) else json.dumps(job), encoding="utf-8")
    out = tmp_path / "result.json.tmp"
    assert tr.main(["--in", str(path), "--out", str(out)]) == 2
    assert last_error(capsys.readouterr().err).startswith("ERROR: input error: ")
    assert not out.exists() and not (tmp_path / "result.json.tmp.part").exists()


def test_cli_missing_arguments_and_job(tmp_path, capsys):
    assert tr.main(["--in", str(tmp_path / "job.json")]) == 2
    assert tr.main(["--in", str(tmp_path / "missing.json"), "--out", str(tmp_path / "o")]) == 2
    assert "job file not found" in last_error(capsys.readouterr().err)


def test_cli_write_failure_is_retryable_and_leaves_nothing(tmp_path, capsys):
    words = lecture(lambda t: degenerate_words(DEGENERATE, t))
    job = write_job(tmp_path, {"sentences": rows(split_sentences(words))})
    out = tmp_path / "no-such-dir" / "result.json.tmp"
    assert tr.main(["--in", job, "--out", str(out)]) == 1
    assert last_error(capsys.readouterr().err).startswith("ERROR: ")
    assert not (tmp_path / "no-such-dir").exists()


def test_cli_as_a_process(tmp_path):
    words = lecture(lambda t: degenerate_words(DEGENERATE, t))
    job = write_job(tmp_path, {"sentences": rows(split_sentences(words))})
    out = tmp_path / "result.json.tmp"
    proc = subprocess.run([sys.executable, os.path.join(os.path.dirname(tr.__file__),
                                                        "textrepair.py"),
                           "--in", job, "--out", str(out)],
                          capture_output=True, text=True, encoding="utf-8", timeout=120)
    assert proc.returncode == 0, proc.stderr
    assert "PROGRESS 1.00" in proc.stdout.splitlines()
    assert json.loads(out.read_text(encoding="utf-8"))["textrepair"]["sentence_ends_added"] > 0
