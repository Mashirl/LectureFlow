# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""Degenerate windows decoded once more (redecode: worker-builder, 2026-10-27).

asr_common.window_style / degeneration (the shared judgement), the second decode in
worker/asr.py (prompt, clip, acceptance, waiting for a prompt at the start of a
lecture), its counters in timing.postprocess, progress, checkpoints and resuming, and a
worker process killed during a second decode. Fake models only (SPEC §14): the lecture
is scripted (FAKE_LECTURE_SRC); its audio holds each sample's index, so the fake knows
which window a redecode piece is.
"""
import json
import os
import subprocess
import sys
import time
from types import SimpleNamespace

import numpy as np
import pytest

import asr
import asr_common
from asr_common import DEGENERATE_UNPUNCTUATED, DEGENERATE_UPPERCASE, Word, degeneration, \
    window_style
from conftest import WORKER_DIR, write_wav
from test_checkpoint import FAKE_FW_TRANSCRIBE, FAKE_FW_VAD, FW_FAKE_SRC, JOB_ID, \
    checkpoint_obj, progress_of, read_json

# ===========================================================================
# window_style / degeneration
# ===========================================================================


def ws(text):
    return [" " + w for w in text.split()]


LOWER_40 = ws(" ".join("word%d" % i for i in range(40)))


def test_unpunctuated_window():
    assert degeneration(LOWER_40) == DEGENERATE_UNPUNCTUATED
    st = window_style(LOWER_40)
    assert (st.words, st.punct, st.sentence_ends, st.kind) == (40, 0, 0, "unpunctuated")
    # 29 words are too few to judge (the end of a talk, a short pause)
    assert degeneration(LOWER_40[:29]) == ""
    assert degeneration(LOWER_40[:30]) == DEGENERATE_UNPUNCTUATED
    # a sentence end anywhere makes it normal text
    assert degeneration(LOWER_40 + [" done."]) == ""
    assert degeneration(LOWER_40[:10] + [' "really?"'] + LOWER_40[10:]) == ""
    # a stray comma or two do not: at most one mark per 10 words without a sentence end
    four = [w + "," if i % 10 == 9 else w for i, w in enumerate(LOWER_40)]
    assert window_style(four).punct == 4 and degeneration(four) == DEGENERATE_UNPUNCTUATED
    five = four[:20] + [" so;"] + four[20:]
    assert window_style(five).words == 41 and degeneration(five) == ""
    seven = [w + "," if i % 7 == 6 else w for i, w in enumerate(LOWER_40[:30])]
    assert window_style(seven).punct == 4 and degeneration(seven) == ""   # 4 marks in 30
    assert degeneration(seven[:27] + [" x", " y", " z"]) == DEGENERATE_UNPUNCTUATED  # 3 in 30


def test_punctuation_inside_a_word_does_not_count():
    words = LOWER_40[:15] + [" 3.14", " e.g", " it's", " two-way", " x86-64"] + LOWER_40[15:]
    assert degeneration(words) == DEGENERATE_UNPUNCTUATED
    # stray punctuation tokens are no words; a lone "-" does not punctuate
    assert window_style(LOWER_40 + [" -", " ..."]).words == 40
    assert degeneration(LOWER_40 + [" -"]) == DEGENERATE_UNPUNCTUATED


def test_uppercase_window():
    caps = ws("SO YOU JUST XOR THE KEY STREAM WITH THE PLAIN TEXT. ONE OF THEM IS THE AES.")
    assert degeneration(caps) == DEGENERATE_UPPERCASE
    st = window_style(caps)
    assert st.upper == st.letters and st.punct == 2 and st.sentence_ends == 2
    # punctuated or not, all upper case is degenerate; apostrophes do not matter
    assert degeneration(ws("IT'S THE SAME IDEA WE HAD LAST WEEK WITH THE LFSR")) == \
        DEGENERATE_UPPERCASE


def test_acronym_dense_text_is_normal():
    text = ("AES, DES and 3DES use XOR. In CBC mode the IV, in CTR mode a nonce; the "
            "LFSR in RC4? No, RC4 is no LFSR. NIST, FIPS and ISO.")
    st = window_style(ws(text))
    assert st.kind == "" and st.upper < 0.8 * st.letters
    # all acronyms: the heaviest case still has lower-case words around it
    assert degeneration(ws("AES DES XOR CBC CTR IV and the LFSR of RC4 and NIST FIPS")) == ""


def test_short_windows_are_not_judged():
    assert degeneration(ws("OK. AES.")) == ""                           # 2 words
    assert degeneration(ws("SO THAT'S IT FOR TODAY. THANKS.")) == ""     # 6 words
    assert degeneration(ws("SO THAT IS IT FOR TODAY THANK YOU")) == DEGENERATE_UPPERCASE
    assert degeneration([]) == ""


def test_word_shapes_accepted():
    words = [Word(w, 0.0, 1.0, 0.9) for w in LOWER_40]
    assert degeneration(words) == DEGENERATE_UNPUNCTUATED
    assert degeneration([{"word": w} for w in LOWER_40]) == DEGENERATE_UNPUNCTUATED
    assert degeneration([{"w": w.strip(), "s": 0, "e": 1} for w in LOWER_40]) == \
        DEGENERATE_UNPUNCTUATED
    assert degeneration([SimpleNamespace(word=w) for w in LOWER_40]) == DEGENERATE_UNPUNCTUATED


# ===========================================================================
# The scripted lecture
# ===========================================================================

# Shared as source by the in-process fake and the fake faster_whisper package of worker
# processes. The lecture: 160 s, two speech chunks (0-70 s, 80-160 s): 150 s of speech.
# Times in the script are on the speech-only time line; the main pass maps them back
# (restore), a redecode answers relative to its piece.
FAKE_LECTURE_SRC = r'''
import copy as _copy
from types import SimpleNamespace as _NS

SR = 16000
TOTAL = 160 * SR
CHUNKS = [{"start": 0, "end": 70 * SR}, {"start": 80 * SR, "end": 160 * SR}]
SPEECH = sum(c["end"] - c["start"] for c in CHUNKS)        # the speech-only audio


def lecture_to_speech(i):
    """Sample index of the lecture -> of the speech-only audio."""
    return i if i < 70 * SR else i - 10 * SR


def text_words(style, tag, n):
    """n words: "normal" (capitalised sentences of six words with a comma), "lower" (no
    punctuation) or "upper" (upper case, full stops)."""
    out = []
    for i in range(n):
        w = "%s%d" % (tag, i)
        if style == "normal":
            if i % 6 == 0:
                w = w.capitalize()
            if i % 6 == 5 or i == n - 1:
                w += "."
            elif i % 6 == 2:
                w += ","
        elif style == "upper":
            w = w.upper() + ("." if i % 6 == 5 or i == n - 1 else "")
        out.append(" " + w)
    return out


def window_segs(seek, texts, start=0.5, step=0.5, temperature=0.0, parts=2, no_speech=0.05,
                logprob=-0.2):
    """A window's segments (parts of them): its words every step seconds from
    seek / 100 + start on the speech-only time line."""
    t0 = seek / 100.0 + start
    words = [_NS(word=w, start=round(t0 + i * step, 2), end=round(t0 + i * step + 0.4, 2),
                 probability=0.9) for i, w in enumerate(texts)]
    k = max(1, -(-len(words) // parts))
    segs = []
    for j in range(0, len(words), k):
        part = words[j:j + k]
        segs.append(_NS(seek=seek, start=part[0].start, end=part[-1].end,
                        text="".join(w.word for w in part), temperature=temperature,
                        avg_logprob=logprob, compression_ratio=1.4, no_speech_prob=no_speech,
                        words=part))
    return segs


def shifted(segs, offset):
    out = []
    for s in segs:
        c = _copy.deepcopy(s)
        c.start, c.end = round(c.start - offset, 2), round(c.end - offset, 2)
        for w in c.words:
            w.start, w.end = round(w.start - offset, 2), round(w.end - offset, 2)
        out.append(c)
    return out


class Lecture:
    """windows: [(seek, segs)] of the main pass; redo: {seek: segs or prompt -> segs}
    (the second decode, absolute speech-only times). transcribe() answers like
    faster-whisper: the main pass on the whole speech-only audio (no clip; asr.py runs
    the VAD itself and maps the times back, textrepair-int) or on the whole lecture
    (vad_filter=True, mapped back with restore here), a resumed main pass
    (clip_timestamps) from that seek on, a redecode (vad_filter=False, no clip, a piece
    of at most 30 s) relative to its piece. calls: what was asked; on_redecode(n) runs
    before the n-th redecode answers (n from 1)."""

    def __init__(self, windows, redo=None, restore=None, on_redecode=None, fail_main_at=None):
        self.windows, self.redo = windows, dict(redo or {})
        self.restore, self.on_redecode = restore, on_redecode
        self.fail_main_at = fail_main_at
        self.calls = []
        self.redecodes = 0
        self.hf_tokenizer = _NS(encode=lambda text, add_special_tokens=False:
                                _NS(ids=text.split()))

    def transcribe(self, audio, **kw):
        info = _NS(duration=len(audio) / float(SR), language=kw.get("language"))
        if kw.get("vad_filter") or ("clip_timestamps" not in kw and len(audio) == SPEECH):
            self.calls.append({"pass": "main", "initial_prompt": kw.get("initial_prompt")})
            segs = [_copy.deepcopy(s) for _, ss in self.windows for s in ss]
            if kw.get("vad_filter"):
                assert len(audio) == TOTAL
                restored = list(self.restore(segs, CHUNKS, SR))
            else:
                restored = segs                 # the caller maps the times back

            def gen():
                for i, seg in enumerate(restored):
                    if i == self.fail_main_at:
                        raise RuntimeError("worker killed (fake)")
                    yield seg

            return gen(), info
        if "clip_timestamps" in kw:
            clip = int(round(kw["clip_timestamps"][0] * 100))
            self.calls.append({"pass": "resume", "seek": clip,
                               "initial_prompt": kw.get("initial_prompt")})
            return iter([_copy.deepcopy(s) for seek, ss in self.windows if seek >= clip
                         for s in ss]), info
        start = lecture_to_speech(int(audio[0]))
        assert start % 160 == 0
        seek = start // 160
        self.redecodes += 1
        call = {"pass": "redecode", "seek": seek, "samples": len(audio),
                "initial_prompt": kw.get("initial_prompt"),
                "kwargs": {k: v for k, v in kw.items() if k != "initial_prompt"}}
        self.calls.append(call)
        if self.on_redecode:
            self.on_redecode(self.redecodes)
        segs = self.redo.get(seek, [])
        if callable(segs):
            segs = segs(kw.get("initial_prompt"))

        def gen():
            for seg in shifted(segs, seek / 100.0):
                call["yielded"] = call.get("yielded", 0) + 1
                yield seg

        return gen(), info


def default_script():
    """Five windows: normal, unpunctuated (across the pause at 70 s of speech), upper
    case, normal, unpunctuated (the last); the second decodes of windows 1, 2 and 4 are
    in normal style with the same word times."""
    windows = [(0, window_segs(0, text_words("normal", "a", 40))),
               (3000, window_segs(3000, text_words("lower", "b", 40))),
               (6000, window_segs(6000, text_words("upper", "c", 40))),
               (9000, window_segs(9000, text_words("normal", "d", 40))),
               (12000, window_segs(12000, text_words("lower", "e", 36)))]
    redo = {3000: window_segs(3000, text_words("normal", "b", 40)),
            6000: window_segs(6000, text_words("normal", "c", 40)),
            12000: window_segs(12000, text_words("normal", "e", 36))}
    return windows, redo
'''
exec(FAKE_LECTURE_SRC)

WINDOWS, REDO = default_script()
W0, W1, W2, W3, W4 = [segs for _, segs in WINDOWS]


def lecture(windows=None, redo=None, **kw):
    from faster_whisper.transcribe import restore_speech_timestamps
    if windows is None:
        windows = WINDOWS
    return Lecture(windows, REDO if redo is None else redo, restore=restore_speech_timestamps,
                   **kw)


def tail_text(segs, n=None):
    words = [w.word for s in segs for w in s.words]
    return "".join(words[-n:] if n else words).strip()


@pytest.fixture
def rd(tmp_path, monkeypatch):
    """run(model, checkpoint=None, **job) runs asr.run in-process on the scripted
    lecture (decode_audio gives sample indexes, the VAD the script's chunks) and returns
    (result, log lines)."""
    import faster_whisper.vad
    state = SimpleNamespace(model=None, ckpt=str(tmp_path / "asr.ckpt.json"), vad_calls=0)
    monkeypatch.setattr(asr, "load_model", lambda *a: state.model)
    monkeypatch.setattr(asr, "setup_cuda_libs", lambda: [])
    monkeypatch.setattr(asr, "decode_audio", lambda path: np.arange(TOTAL, dtype=np.float32))
    monkeypatch.setattr(asr, "CHECKPOINT_INTERVAL_S", 0.0)

    def vad(audio, options=None, **kw):
        state.vad_calls += 1
        return [dict(c) for c in CHUNKS]

    monkeypatch.setattr(faster_whisper.vad, "get_speech_timestamps", vad)
    wav = write_wav(tmp_path / "audio.wav", 0.1)

    def run(model, checkpoint=None, **overrides):
        state.model = model
        data = {"audio": wav, "model": "large-v3", "compute_type": "float16",
                "language": "en", "initial_prompt": "cache, write-back, TLB", "timing": True}
        data.update(overrides)
        if checkpoint is not None:
            data["checkpoint"] = checkpoint
        in_path = tmp_path / "job.json"
        in_path.write_text(json.dumps(data), encoding="utf-8")
        out_path = tmp_path / "result.json.tmp"
        if out_path.exists():
            out_path.unlink()
        lines = []
        asr.run(str(in_path), str(out_path), lines.append)
        return read_json(out_path), lines

    state.run = run
    return state


def texts(result):
    return " ".join(s["text"] for s in result["sentences"])


def redecodes(model):
    return [c for c in model.calls if c["pass"] == "redecode"]


def counters(result):
    return result["timing"]["postprocess"]["redecode"]


# ===========================================================================
# The second decode
# ===========================================================================

def test_degenerate_windows_are_decoded_again_with_the_text_before(rd):
    model = lecture()
    r, lines = rd.run(model)
    calls = redecodes(model)
    assert [c["seek"] for c in calls] == [3000, 6000, 12000]
    # the piece: the window's speech-only audio up to the next window (the last: the end)
    assert [c["samples"] for c in calls] == [3000 * 160, 3000 * 160, 3000 * 160]
    # otherwise as the main pass: no VAD of its own, no hotwords, no clip
    assert calls[0]["kwargs"] == dict(asr.RESUME_KWARGS, language="en")
    assert "hotwords" not in calls[0]["kwargs"]
    # the prompt ends with the text before the window (window 2: window 1 as decoded again)
    assert calls[0]["initial_prompt"].endswith(tail_text(W0))
    assert calls[1]["initial_prompt"].endswith(tail_text(REDO[3000]))
    assert calls[2]["initial_prompt"].endswith(tail_text(W3))
    # every degenerate window replaced: normal sentences only
    words = texts(r).split()
    assert words[40:43] == ["B0", "b1", "b2,"] and words[80:83] == ["C0", "c1", "c2,"]
    assert words[160:163] == ["E0", "e1", "e2,"]
    assert all(s["text"][-1] in ".?!" for s in r["sentences"])
    assert not any(degeneration(ws(s["text"])) for s in r["sentences"])
    assert counters(r) == {"windows": 5, "unpunctuated": 2, "uppercase": 1, "redecoded": 3,
                           "accepted": 3, "redecode_s": counters(r)["redecode_s"]}
    assert counters(r)["redecode_s"] >= 0
    assert rd.vad_calls == 1          # once per run: the main pass's, the redecode reuses it
    assert any(l.startswith("redecode: 3 of 5 windows degenerate (2 unpunctuated, "
                            "1 uppercase), 3 decoded again, 3 accepted") for l in lines)
    assert any(l.startswith("window at 0:30 (seek 3000) unpunctuated: decoded again in ")
               and l.endswith(", accepted") for l in lines)


def test_redecoded_words_keep_the_lecture_times(rd):
    """The second decode's times go through the same mapping as the main pass: window 2
    spans the pause (70 s of speech = 80 s of the lecture)."""
    r0, _ = rd.run(lecture(), timing=False)
    asr_off = asr.REDECODE
    try:
        asr.REDECODE = False
        r1, _ = rd.run(lecture(), timing=False)
    finally:
        asr.REDECODE = asr_off
    times = lambda r: [(w["s"], w["e"]) for s in r["sentences"] for w in s["words"]]
    assert times(r0) == times(r1)
    assert texts(r0) != texts(r1)
    assert any(w["s"] >= 80000 and w["w"].lower().startswith("c") for s in r0["sentences"]
               for w in s["words"])


def test_without_timing_the_result_has_no_counters(rd):
    r, lines = rd.run(lecture(), timing=False)
    assert set(r) == {"prompt_terms_used", "sentences"}
    assert any(l.startswith("redecode: 3 of 5 windows degenerate") for l in lines)


def test_normal_lecture_decodes_nothing_again(rd):
    model = lecture(windows=[(0, W0), (3000, W3)])
    r, lines = rd.run(model)
    assert redecodes(model) == [] and rd.vad_calls == 1      # the main pass's VAD only
    assert counters(r) == {"windows": 2, "unpunctuated": 0, "uppercase": 0, "redecoded": 0,
                           "accepted": 0, "redecode_s": 0.0}


def test_short_and_acronym_windows_are_left_alone(rd):
    short = window_segs(3000, text_words("lower", "s", 20))          # 20 words, no commas
    acr = window_segs(6000, [" AES", " DES,", " and", " the", " LFSR", " XOR.", " NIST",
                             " CBC", " and", " CTR", " use", " the", " IV."])
    model = lecture(windows=[(0, W0), (3000, short), (6000, acr)])
    r, _ = rd.run(model)
    assert redecodes(model) == []
    assert counters(r)["unpunctuated"] == counters(r)["uppercase"] == 0


@pytest.mark.parametrize("name,redo,why", [
    ("dropped speech", window_segs(3000, [" Thank", " you", " very", " much."]),
     "4 words instead of 40"),
    ("still lower case", window_segs(3000, text_words("lower", "x", 40)),
     "still unpunctuated"),
    ("upper case", window_segs(3000, text_words("upper", "x", 40)), "still uppercase"),
    ("invented words", window_segs(3000, text_words("normal", "b", 48), step=0.45),
     "48 words instead of 40"),
    ("a repeat loop", window_segs(3000, text_words("normal", "b", 38) + [" so", " so", " so"]),
     "repeats a phrase"),
    ("temperature fallback", window_segs(3000, text_words("normal", "b", 40), temperature=0.4),
     "temperature fallback to 0.4"),
    ("no words", [], "no words"),
])
def test_second_decode_rejected_keeps_the_original(rd, name, redo, why):
    model = lecture(windows=[(0, W0), (3000, W1), (6000, W3)], redo={3000: redo})
    r, lines = rd.run(model)
    # the window's words as decoded; the text repair after it (textrepair-int) only
    # capitalises the first one, which follows a sentence end (14 sentence ends of normal
    # text are too few for it to add full stops)
    kept = [w.strip() for w in text_words("lower", "b", 40)]
    assert texts(r).split()[40:80] == ["B0"] + kept[1:]
    assert r["timing"]["postprocess"]["textrepair"]["sentence_ends_added"] == 0
    assert counters(r)["redecoded"] == 1 and counters(r)["accepted"] == 0
    assert any(l.startswith("window at 0:30 (seek 3000) unpunctuated: decoded again in ")
               and l.endswith("kept the original (%s)" % why) for l in lines), lines


def test_second_decode_copying_the_prompt_is_rejected(rd):
    copied = [w.word for s in W0 for w in s.words][-asr.REDECODE_ECHO_WORDS:]
    echo = window_segs(3000, copied + text_words("normal", "b", 40 - len(copied)))
    model = lecture(windows=[(0, W0), (3000, W1), (6000, W3)], redo={3000: echo})
    r, lines = rd.run(model)
    assert counters(r)["accepted"] == 0
    assert any(l.endswith("kept the original (repeats the prompt)") for l in lines)
    # seven words of the prompt are no echo (a phrase the lecturer repeats)
    seven = window_segs(3000, copied[1:] + text_words("normal", "b", 41 - len(copied)))
    model = lecture(windows=[(0, W0), (3000, W1), (6000, W3)], redo={3000: seven})
    r, _ = rd.run(model)
    assert counters(r)["accepted"] == 1


def test_echo_later_in_the_window_is_no_echo(rd):
    """A phrase of the prompt the lecturer says again later in the window is kept."""
    copied = [w.word for s in W0 for w in s.words][-asr.REDECODE_ECHO_WORDS:]
    words = text_words("normal", "b", 40)
    later = window_segs(3000, words[:24] + copied + words[24 + len(copied):])
    model = lecture(windows=[(0, W0), (3000, W1), (6000, W3)], redo={3000: later})
    r, _ = rd.run(model)
    assert counters(r)["accepted"] == 1


def test_second_decode_hears_the_whole_window_and_keeps_its_words(rd):
    """The piece is the 30 s the main pass decoded the window from, also when the next
    window begins earlier; words from the next window's seek on (word middle) are
    dropped, and no segment after it is asked for."""
    nxt = window_segs(5500, text_words("normal", "n", 20))
    redo = window_segs(3000, text_words("normal", "b", 40) + [" Next", " window", " words."],
                       parts=1)
    # the last three words lie at 50.5-51.9 s: move them past the next window's seek
    for w, t in zip(redo[0].words[-3:], (54.6, 55.4, 56.0)):
        w.start, w.end = t, t + 0.4
    redo[0].end = 56.4
    after = window_segs(3000, [" Far", " later."], start=32.0)
    model = lecture(windows=[(0, W0), (3000, W1), (5500, nxt)], redo={3000: redo + after})
    r, _ = rd.run(model)
    (call,) = redecodes(model)
    assert call["samples"] == 3000 * 160 and call["yielded"] == 1
    words = texts(r).split()
    # 54.6-55.0 has its middle before 55.0 s: kept; the other two belong to window 2
    assert words[40:82] == [w.strip() for w in text_words("normal", "b", 40)] + ["Next", "N0"]
    assert counters(r)["accepted"] == 1


def test_second_decode_leaves_out_what_it_rates_as_no_speech(rd):
    """In the pause after the window's last word the second decode makes up a phrase it
    rates as no speech itself: left out, the rest accepted."""
    ghost = window_segs(3000, [" Thank", " you", " very", " much."], start=21.0,
                        no_speech=0.96, logprob=-0.87)
    model = lecture(windows=[(0, W0), (3000, W1), (6000, W3)], redo={3000: REDO[3000] + ghost})
    r, _ = rd.run(model)
    assert counters(r)["accepted"] == 1
    assert "Thank" not in texts(r)
    assert texts(r).split()[40:80] == [w.strip() for w in text_words("normal", "b", 40)]


def test_temperature_no_higher_than_the_original_is_accepted(rd):
    hot = window_segs(3000, text_words("lower", "b", 40), temperature=0.4)
    redo = window_segs(3000, text_words("normal", "b", 40), temperature=0.2)
    model = lecture(windows=[(0, W0), (3000, hot), (6000, W3)], redo={3000: redo})
    r, _ = rd.run(model)
    assert counters(r)["accepted"] == 1


def test_hallucinated_segments_do_not_count(rd):
    """The words the hallucination filter drops count neither for the judgement nor for
    the comparison."""
    ghost = window_segs(3000, [" Subtitles", " by", " Amara."], start=25.0, no_speech=0.9,
                        logprob=-1.5)
    model = lecture(windows=[(0, W0), (3000, W1 + ghost), (6000, W3)])
    r, _ = rd.run(model)
    assert counters(r)["accepted"] == 1 and "Amara" not in texts(r)


def test_window_at_the_start_waits_for_a_prompt(rd):
    """No text before the first window: it is decoded again with the first window after
    it that can be a prompt, and the windows stay in order."""
    normal = window_segs(6000, text_words("normal", "a", 40))
    f_redo = window_segs(0, text_words("normal", "f", 40))
    model = lecture(windows=[(0, window_segs(0, text_words("lower", "f", 40))),
                             (3000, window_segs(3000, text_words("upper", "g", 40))),
                             (6000, normal),
                             (9000, window_segs(9000, text_words("lower", "b", 40)))],
                    redo={0: f_redo, 3000: window_segs(3000, text_words("normal", "g", 40)),
                          9000: window_segs(9000, text_words("normal", "b", 40))})
    r, _ = rd.run(model)
    calls = redecodes(model)
    assert [c["seek"] for c in calls] == [0, 3000, 9000]
    assert calls[0]["initial_prompt"].endswith(tail_text(normal))
    # window 1: window 0 as decoded again comes before it
    assert calls[1]["initial_prompt"].endswith(tail_text(f_redo))
    assert calls[2]["initial_prompt"].endswith(tail_text(normal))
    words = [w["w"] for s in r["sentences"] for w in s["words"]]
    assert [w[:1] for w in words[::40]] == ["F", "G", "A", "B"]
    assert counters(r)["accepted"] == 3


def test_windows_wait_in_order_behind_a_window_too_short_to_prompt(rd):
    """A normal window too short to be a prompt does not resolve the waiting ones and
    stays behind them."""
    short = window_segs(3000, [" Okay.", " So"])
    normal = window_segs(6000, text_words("normal", "a", 40))
    f_redo = window_segs(0, text_words("normal", "f", 40))
    model = lecture(windows=[(0, window_segs(0, text_words("lower", "f", 40))),
                             (3000, short), (6000, normal)], redo={0: f_redo})
    r, _ = rd.run(model)
    (call,) = redecodes(model)
    assert call["seek"] == 0 and call["initial_prompt"].endswith(tail_text(normal))
    words = [w["w"] for s in r["sentences"] for w in s["words"]]
    assert words[:1] == ["F0"] and words[40:42] == ["Okay.", "So"] and words[42] == "A0"


def test_no_text_to_prompt_with_keeps_the_windows(rd):
    model = lecture(windows=[(0, W1), (3000, W4)], redo={})
    r, lines = rd.run(model)
    assert redecodes(model) == []
    assert counters(r) == {"windows": 2, "unpunctuated": 2, "uppercase": 0, "redecoded": 0,
                           "accepted": 0, "redecode_s": 0.0}
    # kept as decoded; the text repair (textrepair-int) capitalises the lecture's first
    # word and, with no normal text to learn from, adds no sentence end
    assert texts(r).split()[:3] == ["B0", "b1", "b2"]
    assert r["timing"]["postprocess"]["textrepair"]["sentence_ends_added"] == 0


def test_progress_is_monotonic_with_second_decodes(rd):
    r, lines = rd.run(lecture())
    progress = progress_of(lines)
    values = [float(l.split()[1]) for l in progress]
    assert values == sorted(values)
    # one line per main-pass segment (10) and the final one
    assert len(progress) == 11 and progress[-1] == "PROGRESS 1.00"


def test_terms_and_text_fit_the_prompt_limit():
    count = lambda text: len(text.split())          # one token per word
    style = [Word(w, 0.0, 1.0, 0.9) for w in text_words("normal", "z", 300)]
    terms = ", ".join("term%d" % i for i in range(300))
    p = asr.redecode_prompt(style, terms, count)
    assert count(p) <= asr.REDECODE_PROMPT_TOKEN_LIMIT
    head, _, context = p.rpartition(". ")
    assert p.startswith("term0, term1, ")
    assert p.endswith("".join(w.word for w in style[-20:]).strip())
    # without terms: the text alone, its last whole words within the context limit
    q = asr.redecode_prompt(style, "", count)
    assert count(q) == asr.REDECODE_CONTEXT_TOKEN_LIMIT
    assert q == "".join(w.word for w in style[-asr.REDECODE_CONTEXT_TOKEN_LIMIT:]).strip()


# ===========================================================================
# Checkpoints and resuming
# ===========================================================================

def _without_seconds(c):
    return {k: v for k, v in c.items() if k != "redecode_s"}


def test_checkpoint_holds_the_final_windows_and_their_counters(rd):
    seen = []

    def on_redecode(n):
        seen.append(read_json(rd.ckpt) if os.path.exists(rd.ckpt) else None)

    rd.run(lecture(on_redecode=on_redecode), checkpoint_obj(rd.ckpt))
    first, second, third = seen
    # window 1 is decoded again when window 2 begins: only window 0 is in the file
    assert first["resume_seek"] == 3000 and len(first["segments"]) == 2
    assert _without_seconds(first["redecode"]) == {
        "windows": 1, "unpunctuated": 0, "uppercase": 0, "redecoded": 0, "accepted": 0}
    # then window 1 as decoded again, with its counters
    assert second["resume_seek"] == 6000 and len(second["segments"]) == 4
    assert [w["word"] for w in second["segments"][2]["words"]][:3] == [" B0", " b1", " b2,"]
    assert {s["seek"] for s in second["segments"][2:]} == {3000}
    assert _without_seconds(second["redecode"]) == {
        "windows": 2, "unpunctuated": 1, "uppercase": 0, "redecoded": 1, "accepted": 1}
    assert third["resume_seek"] == 12000
    data = read_json(rd.ckpt)              # after the run: windows 0-3 (4 is the last)
    assert data["resume_seek"] == 12000 and len(data["segments"]) == 8
    assert _without_seconds(data["redecode"]) == {
        "windows": 4, "unpunctuated": 1, "uppercase": 1, "redecoded": 2, "accepted": 2}


def test_interrupted_during_a_second_decode_resumes_identically(rd):
    r0, _ = rd.run(lecture())
    uninterrupted = lecture()
    rd.run(uninterrupted, checkpoint_obj(rd.ckpt))
    os.remove(rd.ckpt)

    def kill(n):
        if n == 2:
            raise RuntimeError("worker killed (fake)")

    with pytest.raises(RuntimeError, match="worker killed"):
        rd.run(lecture(on_redecode=kill), checkpoint_obj(rd.ckpt))
    assert read_json(rd.ckpt)["resume_seek"] == 6000
    model = lecture()
    r, lines = rd.run(model, checkpoint_obj(rd.ckpt))
    assert r["sentences"] == r0["sentences"]
    assert _without_seconds(counters(r)) == _without_seconds(counters(r0))
    # window 1 is final in the checkpoint: not decoded again; the prompt of window 2 is
    # the one of the uninterrupted run (window 1 as decoded again, from the checkpoint)
    assert [(c["pass"], c.get("seek")) for c in model.calls] == [
        ("resume", 6000), ("redecode", 6000), ("redecode", 12000)]
    assert [c["initial_prompt"] for c in redecodes(model)] == \
        [c["initial_prompt"] for c in redecodes(uninterrupted)][1:]
    # each run's own VAD (the resumed one's too) serves its second decodes
    assert rd.vad_calls == 1 + 1 + 1 + 1        # r0, uninterrupted, killed, resumed
    values = [float(l.split()[1]) for l in progress_of(lines)]
    assert values == sorted(values) and values[-1] == 1.0


def _old_checkpoint(rd, fail_main_at):
    """A checkpoint as the worker wrote it before the redecode: no "redecode" counters,
    degenerate windows as decoded."""
    old = asr.REDECODE
    asr.REDECODE = False
    try:
        with pytest.raises(RuntimeError, match="worker killed"):
            rd.run(lecture(fail_main_at=fail_main_at), checkpoint_obj(rd.ckpt))
    finally:
        asr.REDECODE = old
    data = read_json(rd.ckpt)
    del data["redecode"]
    with open(rd.ckpt, "w", encoding="utf-8") as f:
        json.dump(data, f)
    return data


def test_old_checkpoint_with_a_degenerate_window_starts_over(rd):
    r0, _ = rd.run(lecture())
    data = _old_checkpoint(rd, fail_main_at=8)     # windows 0-2 in it, 1 and 2 degenerate
    assert data["resume_seek"] == 9000
    model = lecture()
    r, lines = rd.run(model, checkpoint_obj(rd.ckpt))
    assert "checkpoint asr.ckpt.json not used (its degenerate windows were not decoded " \
           "again); starting over" in lines
    assert model.calls[0] == {"pass": "main", "initial_prompt": "cache, write-back, TLB"}
    assert r["sentences"] == r0["sentences"]
    assert r["parts"] == [{"from_ms": 0, "to_ms": 160000, "device_id": "cuda:0",
                           "model": "large-v3", "precision": "auto"}]


def test_old_checkpoint_without_degenerate_windows_is_continued(rd):
    r0, _ = rd.run(lecture())
    data = _old_checkpoint(rd, fail_main_at=4)     # window 0 only
    assert data["resume_seek"] == 3000
    model = lecture()
    r, lines = rd.run(model, checkpoint_obj(rd.ckpt))
    assert model.calls[0]["pass"] == "resume"
    assert r["sentences"] == r0["sentences"]
    # no counters in the file: this run's windows only
    assert counters(r)["windows"] == 4 and counters(r)["accepted"] == 3


def test_checkpoint_redecode_counters_are_checked(tmp_path):
    from test_checkpoint import _checkpointer, _fw_segment, _words
    ck, lines = _checkpointer(tmp_path)
    ok = {"windows": 3, "unpunctuated": 1, "uppercase": 0, "redecoded": 1, "accepted": 1,
          "redecode_s": 1.5}
    ck.save(1800, 3, ck.parts(None, 1800), [_fw_segment(500, 1620, _words())], 180, ok)
    assert read_json(ck.path)["redecode"] == ok
    assert ck.load(10000).redecode == ok
    data = read_json(ck.path)
    data["redecode"] = {"windows": -1}
    with open(ck.path, "w", encoding="utf-8") as f:
        json.dump(data, f)
    assert ck.load(10000) is None and "malformed redecode" in lines[-1]


# ===========================================================================
# A worker process killed during a second decode (as Go cancels: kill)
# ===========================================================================

FAKE_PACKAGE = r'''
import json, os, sys, time
import numpy as np
cfg = json.loads(os.environ.get("LF_FAKE_REDECODE", "{}"))
exec(%r)
exec(%r)
__version__ = "1.2.1-fake"


def decode_audio(path, sampling_rate=16000):
    return np.arange(TOTAL, dtype=np.float32)


def _on_redecode(n):
    if cfg.get("hang_on_redecode") == n:
        print("REDECODING", flush=True)
        time.sleep(60)


class WhisperModel(Lecture):
    def __init__(self, path, device="cuda", compute_type="default", cpu_threads=0):
        windows, redo = default_script()
        Lecture.__init__(self, windows, redo, restore=fw_restore, on_redecode=_on_redecode)

    def transcribe(self, audio, **kw):
        out = Lecture.transcribe(self, audio, **kw)
        if cfg.get("calls_log"):
            c = dict(self.calls[-1])
            c.pop("kwargs", None)
            with open(cfg["calls_log"], "a", encoding="utf-8") as f:
                f.write(json.dumps(c) + "\n")
        return out
''' % (FW_FAKE_SRC, FAKE_LECTURE_SRC)


def _process_env(tmp_path, **cfg):
    pkg = tmp_path / "fakefw" / "faster_whisper"
    pkg.mkdir(parents=True, exist_ok=True)
    for name, src in (("__init__.py", FAKE_PACKAGE), ("vad.py", FAKE_FW_VAD),
                      ("transcribe.py", FAKE_FW_TRANSCRIBE)):
        with open(pkg / name, "w", encoding="utf-8", newline="\n") as f:
            f.write(src)
    return dict(os.environ, PYTHONIOENCODING="utf-8", PYTHONUNBUFFERED="1",
                PYTHONPATH=str(tmp_path / "fakefw"), LF_FAKE_REDECODE=json.dumps(cfg))


def test_process_killed_during_a_second_decode_then_resumed(tmp_path):
    from test_checkpoint import _kill, _model_dir, _wait_for
    wav = write_wav(tmp_path / "audio.wav", 0.1)

    def job(work):
        work.mkdir(exist_ok=True)
        p = work / "job.json"
        p.write_text(json.dumps({
            "audio": wav, "model": _model_dir(tmp_path), "compute_type": "int8",
            "device": "cpu", "cpu_threads": 1, "language": "en", "timing": True,
            "initial_prompt": "cache, write-back, TLB",
            "checkpoint": checkpoint_obj(work / "asr.ckpt.json")}), encoding="utf-8")
        return [sys.executable, os.path.join(WORKER_DIR, "asr.py"), "--in", str(p), "--out",
                str(work / "result.json.tmp")]

    # the uninterrupted run
    ref = tmp_path / "ref"
    p = subprocess.run(job(ref), capture_output=True, cwd=str(tmp_path), timeout=120,
                       env=_process_env(tmp_path))
    assert p.returncode == 0, p.stderr.decode("utf-8", "replace")
    r0 = read_json(ref / "result.json.tmp")
    assert _without_seconds(counters(r0)) == {"windows": 5, "unpunctuated": 2, "uppercase": 1,
                                              "redecoded": 3, "accepted": 3}

    work = tmp_path / "lecture"
    calls = tmp_path / "calls.jsonl"
    argv = job(work)
    proc = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            cwd=str(work), env=_process_env(tmp_path, hang_on_redecode=2))
    try:
        _wait_for(proc, "REDECODING")
        data = read_json(work / "asr.ckpt.json")
        # the first write: window 0 (the next ones wait for the 10 s interval)
        assert data["resume_seek"] == 3000 and "redecode" in data
    finally:
        if proc.poll() is None:
            _kill(proc)                          # within 3 s
    assert proc.returncode != 0
    assert sorted(os.listdir(work)) == ["asr.ckpt.json", "job.json"]

    p = subprocess.run(argv, capture_output=True, cwd=str(work), timeout=120,
                       env=_process_env(tmp_path, calls_log=str(calls)))
    assert p.returncode == 0, p.stderr.decode("utf-8", "replace")
    r = read_json(work / "result.json.tmp")
    assert r["sentences"] == r0["sentences"]
    assert _without_seconds(counters(r)) == _without_seconds(counters(r0))
    logged = [json.loads(l) for l in calls.read_text(encoding="utf-8").splitlines()]
    assert [(c["pass"], c.get("seek")) for c in logged] == [
        ("resume", 3000), ("redecode", 3000), ("redecode", 6000), ("redecode", 12000)]
    values = [float(l.split()[1]) for l in progress_of(p.stdout.decode("utf-8").splitlines())]
    assert values == sorted(values) and values[-1] == 1.0


# ===========================================================================
# The switch (job.redecode; Go: kv asr.redecode, settings page)
# ===========================================================================

def test_switch_off_counts_the_windows_and_decodes_none_again(rd):
    model = lecture()
    r, lines = rd.run(model, redecode=False)
    assert redecodes(model) == [] and rd.vad_calls == 1      # the main pass's VAD only
    assert counters(r) == {"skipped": True, "windows": 5, "unpunctuated": 2, "uppercase": 1,
                           "redecoded": 0, "accepted": 0, "redecode_s": 0.0}
    assert any(l.startswith("redecode off (job.redecode false): 3 of 5 windows degenerate")
               for l in lines)
    old = asr.REDECODE
    asr.REDECODE = False
    try:
        r0, _ = rd.run(lecture())
    finally:
        asr.REDECODE = old
    assert r["sentences"] == r0["sentences"]          # the main pass as it is


def test_switch_on_and_default_are_the_same(rd):
    r1, _ = rd.run(lecture(), redecode=True)
    r2, _ = rd.run(lecture())
    assert r1["sentences"] == r2["sentences"] and "skipped" not in counters(r2)
    assert counters(r2)["accepted"] == 3


@pytest.mark.parametrize("bad", ["false", 0, None, "off"])
def test_switch_must_be_a_boolean(rd, bad):
    with pytest.raises(asr_common.InputError, match="job.redecode must be true or false"):
        rd.run(lecture(), redecode=bad)


def test_checkpoint_of_a_run_with_the_switch_off(rd):
    """A checkpoint written with the switch off holds degenerate windows as decoded: a run
    with the switch on starts over, one with it off continues it."""
    r_on, _ = rd.run(lecture())
    r_off, _ = rd.run(lecture(), redecode=False)
    for on, want_first, want in ((True, "main", r_on), (False, "resume", r_off)):
        with pytest.raises(RuntimeError, match="worker killed"):
            rd.run(lecture(fail_main_at=8), checkpoint_obj(rd.ckpt), redecode=False)
        assert read_json(rd.ckpt)["redecode"]["skipped"] is True
        model = lecture()
        r, lines = rd.run(model, checkpoint_obj(rd.ckpt), redecode=on)
        assert model.calls[0]["pass"] == want_first
        assert r["sentences"] == want["sentences"]
        assert ("skipped" in counters(r)) is (not on)
        os.remove(rd.ckpt)


def test_switch_on_continues_a_checkpoint_of_the_switch_on(rd):
    with pytest.raises(RuntimeError, match="worker killed"):
        rd.run(lecture(fail_main_at=8), checkpoint_obj(rd.ckpt))
    assert "skipped" not in read_json(rd.ckpt)["redecode"]
    model = lecture()
    rd.run(model, checkpoint_obj(rd.ckpt))
    assert model.calls[0]["pass"] == "resume"
