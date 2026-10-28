# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""The text repair inside the three transcription workers (textrepair-int).

worker/asr.py, worker/asr_openvino.py and worker/asr_mlx.py run textrepair.repair_transcript
on the lecture's words after remove_repeats and before split_sentences: English lectures
only, with the job's whole term list (not the truncated prompt) as the glossary, and the
decoding windows' start times where the backend knows them (faster-whisper; OpenVINO and
MLX pass none). Its counters go into timing.postprocess.textrepair. Fake models only
(SPEC §14); the lecture is test_textrepair's (normal text, then degenerate windows).
"""
import copy
import json
import os
from types import SimpleNamespace

import pytest

import asr
import asr_mlx
import asr_openvino
import textrepair as tr
from asr_common import remove_repeats, split_sentences
from conftest import all_speech_vad, write_wav, write_wav_parts
from test_asr_mlx import fake_mlx  # noqa: F401  (the fixture)
from test_asr_mlx import make_model_dir as make_mlx_model_dir
from test_checkpoint import checkpoint_obj, read_json
from test_textrepair import DEGENERATE, degenerate_words, lecture as tr_lecture

# An all-caps window with an acronym the lecture's normal text never has (AES): only a
# glossary that has it keeps its capitals.
CAPS = ["NOW WE USE AES FOR EVERY BLOCK OF THE MESSAGE",
        "SO THE RECEIVER DOES THE SAME THING WITH THE KEY",
        "OKAY THIS GIVES US THE CIPHERTEXT RIGHT"]

WORDS = tr_lecture(lambda t: degenerate_words(DEGENERATE, t),
                   lambda t: degenerate_words(CAPS, t))
END_S = WORDS[-1].end + 1.0
# 250 terms: the first 200 fill the prompt (one token each with the fake tokenizers),
# AES (number 230) is only in the whole list
TERMS = ["term%d" % i for i in range(250)]
TERMS[230] = "AES"
PROMPT = ", ".join(TERMS)
WINDOW_S = 30.0


def texts(sentences):
    return [s["text"] for s in sentences]


def expected(words, glossary=TERMS, window_starts=None, language="en"):
    """(sentences, repair stats) as the workers should produce them from words."""
    words = remove_repeats(words)
    rep = tr.repair_words(words, glossary, window_starts, language)
    return split_sentences(rep.words), rep.stats


def without_ms(stats):
    out = dict(stats)
    assert isinstance(out.pop("ms"), int) and stats["ms"] >= 0
    return out


def test_the_lecture_needs_the_repair():
    """The fixture: the repair adds sentence ends and re-cases the all-caps window, and
    AES keeps its capitals only with the whole term list."""
    plain = split_sentences(remove_repeats(WORDS))
    repaired, stats = expected(WORDS)
    assert stats["sentence_ends_added"] >= 5 and stats["caps_segments"] == 1
    assert len(repaired) > len(plain)
    assert any("AES" in t for t in texts(repaired))
    short, _ = expected(WORDS, glossary=TERMS[:200])
    assert not any("AES" in t for t in texts(short)) and any(" aes " in t for t in texts(short))


# ---------------------------------------------------------------------------
# faster-whisper (worker/asr.py)
# ---------------------------------------------------------------------------

def windows_of(words, window_s=WINDOW_S, per_segment=8):
    """faster-whisper shaped segments of words: seek = 30 s windows (all of the audio is
    speech: the speech-only time line is the lecture's), at most per_segment words per
    segment, no segment across a window."""
    segs, cur, cur_seek = [], [], None
    for w in words:
        seek = int(w.start // window_s) * int(window_s * 100)
        if cur and (seek != cur_seek or len(cur) == per_segment):
            segs.append((cur_seek, cur))
            cur = []
        cur_seek = seek
        cur.append(w)
    if cur:
        segs.append((cur_seek, cur))
    out = []
    for seek, ws in segs:
        out.append(SimpleNamespace(
            seek=seek, start=ws[0].start, end=ws[-1].end, text="".join(w.word for w in ws),
            temperature=0.0, avg_logprob=-0.2, compression_ratio=1.3, no_speech_prob=0.05,
            words=[SimpleNamespace(word=w.word, start=w.start, end=w.end,
                                   probability=w.probability) for w in ws]))
    return out


def window_starts(segs):
    starts, last = [], None
    for s in segs:
        if s.seek != last:
            starts.append(s.words[0].start)
            last = s.seek
    return starts


class WordsModel:
    """faster-whisper shaped: the segments of windows_of(WORDS) from the clip on (the
    worker's VAD is all_speech_vad). fail_at = i raises when segment i is requested."""

    def __init__(self, words=WORDS, fail_at=None):
        self.segs = windows_of(words)
        self.fail_at = fail_at
        self.hf_tokenizer = SimpleNamespace(
            encode=lambda text, add_special_tokens=False: SimpleNamespace(ids=text.split()))
        self.calls = []

    def transcribe(self, audio, **kw):
        self.calls.append(kw)
        clip = int(round((kw.get("clip_timestamps") or [0.0])[0] * 100))
        segs = [copy.deepcopy(s) for s in self.segs if s.seek >= clip]

        def gen():
            for i, s in enumerate(segs):
                if i == self.fail_at:
                    raise RuntimeError("worker killed (fake)")
                yield s

        return gen(), SimpleNamespace(duration=len(audio) / 16000.0, language=kw["language"])


@pytest.fixture(scope="module")
def lecture_wav(tmp_path_factory):
    return write_wav(tmp_path_factory.mktemp("trw") / "audio.wav", END_S)


@pytest.fixture
def fw(tmp_path, monkeypatch, lecture_wav):
    """run(model, checkpoint=None, **job) runs asr.run in-process; returns (result, lines)."""
    state = SimpleNamespace(model=None, ckpt=str(tmp_path / "asr.ckpt.json"))
    state.vad_calls = all_speech_vad(monkeypatch, asr)
    monkeypatch.setattr(asr, "load_model", lambda *a: state.model)
    monkeypatch.setattr(asr, "setup_cuda_libs", lambda: [])
    monkeypatch.setattr(asr, "CHECKPOINT_INTERVAL_S", 0.0)

    def run(model, checkpoint=None, **overrides):
        state.model = model
        data = {"audio": lecture_wav, "model": "large-v3", "compute_type": "float16",
                "language": "en", "initial_prompt": PROMPT, "timing": True,
                "redecode": False}
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


def test_fw_repairs_the_words_before_the_split(fw):
    model = WordsModel()
    r, lines = fw.run(model)
    want, stats = expected(WORDS, window_starts=window_starts(model.segs))
    assert r["sentences"] == want
    assert r["prompt_terms_used"] == 200                     # the prompt is still cut
    post = r["timing"]["postprocess"]
    assert set(post) == {"redecode", "textrepair"}
    assert without_ms(post["textrepair"]) == stats
    assert post["redecode"]["skipped"] is True              # job.redecode false here
    assert any(l.startswith("textrepair: %d words, 2 degenerate segments" % stats["words"])
               for l in lines), lines
    # the windows' text as decoded is gone where the repair worked: AES from the term list
    assert any("AES" in t for t in texts(r["sentences"]))
    assert any(t.startswith("Now we use AES for every block of the message")
               for t in texts(r["sentences"]))


def test_fw_repair_gets_the_whole_term_list_and_the_window_starts(fw, monkeypatch):
    seen = []
    real = tr.repair_words

    def spy(words, glossary=None, window_starts=None, language="en", *a, **kw):
        seen.append((list(glossary), window_starts, language))
        return real(words, glossary, window_starts, language, *a, **kw)

    monkeypatch.setattr(tr, "repair_words", spy)
    model = WordsModel()
    fw.run(model)
    (glossary, starts, language), = seen
    assert glossary == TERMS and language == "en"
    assert starts == window_starts(model.segs) and len(starts) == len({s.seek for s in model.segs})


def test_fw_without_seek_repairs_without_window_starts(fw, monkeypatch):
    """Segments without seek (never faster-whisper's): no windows, no redecode counters;
    the repair still runs, without window starts."""
    class Plain(WordsModel):
        def transcribe(self, audio, **kw):
            gen, info = WordsModel.transcribe(self, audio, **kw)
            return (SimpleNamespace(**{k: v for k, v in vars(s).items() if k != "seek"})
                    for s in gen), info

    seen = []
    real = tr.repair_words
    monkeypatch.setattr(tr, "repair_words", lambda words, g=None, ws=None, lang="en", *a, **k:
                        seen.append(ws) or real(words, g, ws, lang, *a, **k))
    r, _ = fw.run(Plain())
    assert seen == [None]
    assert set(r["timing"]["postprocess"]) == {"textrepair"}
    assert r["sentences"] == expected(WORDS)[0]


@pytest.mark.parametrize("language", ["de", "zh", ""])
def test_fw_other_languages_are_not_repaired(fw, language):
    r, lines = fw.run(WordsModel(), language=language)
    assert r["sentences"] == split_sentences(remove_repeats(WORDS))
    assert without_ms(r["timing"]["postprocess"]["textrepair"]) == {
        "version": tr.VERSION, "skipped": "language", "words": len(WORDS)}
    assert any(l.startswith("textrepair: skipped (language") for l in lines)


def test_fw_english_variants_are_repaired(fw):
    r, _ = fw.run(WordsModel(), language="en-GB")
    assert r["timing"]["postprocess"]["textrepair"]["sentence_ends_added"] > 0


def test_fw_without_timing_the_repair_still_runs(fw):
    r, lines = fw.run(WordsModel(), timing=False)
    assert set(r) == {"prompt_terms_used", "sentences"}
    assert r["sentences"] == expected(WORDS, window_starts=window_starts(WordsModel().segs))[0]
    assert any(l.startswith("textrepair: ") for l in lines)


def test_fw_resumed_run_repairs_like_the_uninterrupted_one(fw):
    r0, _ = fw.run(WordsModel())
    n = len(WordsModel().segs)
    with pytest.raises(RuntimeError, match="worker killed"):
        fw.run(WordsModel(fail_at=n - 4), checkpoint_obj(fw.ckpt))
    data = read_json(fw.ckpt)
    assert 0 < data["resume_ms"] < END_S * 1000
    model = WordsModel()
    r, lines = fw.run(model, checkpoint_obj(fw.ckpt))
    assert model.calls[0]["clip_timestamps"] == [data["resume_seek"] / 100.0]
    assert r["sentences"] == r0["sentences"]                 # word for word
    assert without_ms(r["timing"]["postprocess"]["textrepair"]) == \
        without_ms(r0["timing"]["postprocess"]["textrepair"])
    assert any(l.startswith("resuming from checkpoint") for l in lines)


# ---------------------------------------------------------------------------
# OpenVINO (worker/asr_openvino.py) and MLX (worker/asr_mlx.py)
# ---------------------------------------------------------------------------

def ov_words(words):
    return [(w.word, w.start, w.end) for w in words]


@pytest.fixture
def ov(tmp_path, fake_ov):
    audio = write_wav_parts(tmp_path / "audio.wav", [(END_S, 8000)])
    model_dir = tmp_path / "ov-model"
    model_dir.mkdir()
    for f in asr_openvino.MODEL_FILES:
        (model_dir / f).write_bytes(b"x")
    fake_ov.words = ov_words(WORDS)
    fake_ov.chunks = [(WORDS[0].start, WORDS[-1].end, "".join(w.word for w in WORDS))]

    def run(**overrides):
        job = {"audio": audio, "model": "large-v3-turbo", "precision": "fp16",
               "model_dir": str(model_dir), "device": "CPU", "language": "en",
               "initial_prompt": PROMPT, "cache_dir": "",
               "hf_id": "openai/whisper-large-v3-turbo", "hf_home": str(tmp_path / "hf"),
               "word_timestamps": True}
        job.update(overrides)
        in_path = tmp_path / "ov_job.json"
        in_path.write_text(json.dumps(job), encoding="utf-8")
        out_path = tmp_path / "ov_result.json.tmp"
        lines = []
        asr_openvino.run(str(in_path), str(out_path), lines.append)
        return read_json(out_path), lines

    return run


@pytest.fixture
def mlx(tmp_path, fake_mlx):
    audio = write_wav_parts(tmp_path / "audio.wav", [(END_S, 8000)])
    fake_mlx.CFG.update(lecture=audio, script=[
        [WORDS[0].start, WORDS[-1].end, "".join(w.word for w in WORDS),
         [[w.word, w.start, w.end, w.probability] for w in WORDS], 0.01, -0.2]])
    model_dir = make_mlx_model_dir(tmp_path / "mlx-model")

    def run(**overrides):
        job = {"audio": audio, "model": "large-v3-turbo", "precision": "fp16",
               "model_dir": model_dir, "device": "mlx:gpu", "language": "en",
               "initial_prompt": PROMPT}
        job.update(overrides)
        in_path = tmp_path / "mlx_job.json"
        in_path.write_text(json.dumps(job), encoding="utf-8")
        out_path = tmp_path / "mlx_result.json.tmp"
        lines = []
        asr_mlx.run(str(in_path), str(out_path), lines.append)
        return read_json(out_path), lines

    return run


@pytest.mark.parametrize("backend", ["ov", "mlx"])
def test_backend_repairs_the_words_before_the_split(request, backend):
    run = request.getfixturevalue(backend)
    r, lines = run()
    want, stats = expected(WORDS)                            # no window starts
    assert texts(r["sentences"]) == texts(want)
    assert [s["start_ms"] for s in r["sentences"]] == [s["start_ms"] for s in want]
    assert set(r["timing"]["postprocess"]) == {"textrepair"}
    assert without_ms(r["timing"]["postprocess"]["textrepair"]) == stats
    assert stats["sentence_ends_added"] > 0
    assert any(l.startswith("textrepair: ") and l.endswith(" ms") and "(no decoding windows)" in l
               for l in lines), lines


@pytest.mark.parametrize("backend", ["ov", "mlx"])
def test_backend_repair_gets_the_whole_term_list_and_no_window_starts(request, backend,
                                                                      monkeypatch):
    seen = []
    real = tr.repair_words
    monkeypatch.setattr(tr, "repair_words", lambda words, g=None, ws=None, lang="en", *a, **k:
                        seen.append((list(g), ws, lang)) or real(words, g, ws, lang, *a, **k))
    r, _ = request.getfixturevalue(backend)()
    assert seen == [(TERMS, None, "en")]
    assert any("AES" in t for t in texts(r["sentences"]))


@pytest.mark.parametrize("backend", ["ov", "mlx"])
def test_backend_other_languages_are_not_repaired(request, backend):
    r, lines = request.getfixturevalue(backend)(language="de")
    assert texts(r["sentences"]) == texts(split_sentences(remove_repeats(WORDS)))
    assert without_ms(r["timing"]["postprocess"]["textrepair"]) == {
        "version": tr.VERSION, "skipped": "language", "words": len(WORDS)}
    assert any(l == "textrepair: skipped (language 'de')" for l in lines)


def test_the_three_workers_repair_alike(fw, request):
    """Fed the same words without windows, the three workers write the same text."""
    class Plain(WordsModel):
        def transcribe(self, audio, **kw):
            gen, info = WordsModel.transcribe(self, audio, **kw)
            return (SimpleNamespace(**{k: v for k, v in vars(s).items() if k != "seek"})
                    for s in gen), info

    r_fw, _ = fw.run(Plain())
    r_ov, _ = request.getfixturevalue("ov")()
    r_mlx, _ = request.getfixturevalue("mlx")()
    assert texts(r_fw["sentences"]) == texts(r_ov["sentences"]) == texts(r_mlx["sentences"])
    stats = [without_ms(r["timing"]["postprocess"]["textrepair"]) for r in (r_fw, r_ov, r_mlx)]
    assert stats[0] == stats[1] == stats[2]


# ---------------------------------------------------------------------------
# repair_transcript itself
# ---------------------------------------------------------------------------

def test_glossary_terms():
    assert tr.glossary_terms("") == [] and tr.glossary_terms(None) == []
    assert tr.glossary_terms("XOR, AES,  , Shannon cipher") == ["XOR", "AES", "Shannon cipher"]


def test_repair_transcript_stats_and_languages():
    words, stats = tr.repair_transcript(WORDS, PROMPT, "en")
    rep = tr.repair_words(WORDS, TERMS, None, "en")
    assert words == rep.words and without_ms(stats) == rep.stats
    for lang in ("de", None, "", "  "):
        words, stats = tr.repair_transcript(WORDS, PROMPT, lang)
        assert words == WORDS and stats["skipped"] == "language"
    words, stats = tr.repair_transcript(WORDS, PROMPT, " EN ")
    assert "skipped" not in stats and stats["sentence_ends_added"] > 0
