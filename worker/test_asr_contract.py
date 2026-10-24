# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""One contract, two workers (SPEC-v2 I7, I11; brief 3b §4).

* The sentence splitting, repeat filter, prompt truncation and result writing of
  worker/asr.py and worker/asr_openvino.py are the same objects from asr_common.py.
* Fed the same words, both workers write the same result.json (apart from the
  OpenVINO worker's timing block, which asr.py writes only when asked).
No real model or device is used (fake faster-whisper model, fake openvino_genai).
"""
import inspect
import json
import os
from types import SimpleNamespace

import pytest

import asr
import asr_common
import asr_openvino
from conftest import write_wav_parts

SHARED = ["split_sentences", "split_word_groups", "is_sentence_end", "remove_repeats",
          "truncate_prompt", "format_progress", "write_result", "as_word", "_word_times",
          "_sentence_dict", "Word", "InputError", "make_timing"]

# A lecture with an abbreviation, a decimal, an initial, a repeated phrase, a clause
# break and a short trailing sentence; every word <= 2 s (no OpenVINO long-word trim).
WORDS = [(" Okay,", 0.0, 0.4), (" e.g.", 0.4, 0.8), (" a", 0.8, 0.9), (" cache", 0.9, 1.3),
         (" of", 1.3, 1.4), (" 3.5", 1.4, 1.9), (" MB.", 1.9, 2.4), (" J.", 2.6, 2.8),
         (" Smith", 2.8, 3.2), (" said", 3.2, 3.5), (" so", 3.5, 3.7), (" so", 3.7, 3.9),
         (" so", 3.9, 4.1), (" so", 4.1, 4.3), (" yes.", 4.3, 4.9), (" Right?", 5.0, 5.4),
         (" Ok.", 5.6, 5.8)]
PROMPT = "cache, write-back, TLB"


def test_shared_functions_are_one_implementation():
    for name in SHARED:
        a, o, c = getattr(asr, name), getattr(asr_openvino, name), getattr(asr_common, name)
        assert a is c and o is c, name
    src = os.path.abspath(inspect.getsourcefile(asr_common.split_sentences))
    assert src == os.path.abspath(inspect.getsourcefile(asr.split_sentences)) \
        == os.path.abspath(inspect.getsourcefile(asr_openvino.split_sentences))
    assert os.path.basename(src) == "asr_common.py"


def test_asr_common_needs_only_the_standard_library():
    import ast
    import sys
    with open(asr_common.__file__, encoding="utf-8") as f:
        tree = ast.parse(f.read())
    roots = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            roots |= {a.name.split(".")[0] for a in node.names}
        elif isinstance(node, ast.ImportFrom) and node.module and node.level == 0:
            roots.add(node.module.split(".")[0])
    assert roots <= set(sys.stdlib_module_names) | {"__future__"}, roots


class _FWTokenizer:
    def encode(self, text, add_special_tokens=True):
        return SimpleNamespace(ids=text.split())


class _FWModel:
    """faster-whisper shaped: one segment, words with probability 0.9 (all confident)."""

    def __init__(self):
        self.hf_tokenizer = _FWTokenizer()

    def transcribe(self, audio, **kwargs):
        words = [SimpleNamespace(word=w, start=s, end=e, probability=0.9) for w, s, e in WORDS]
        seg = SimpleNamespace(start=0.0, end=6.0, no_speech_prob=0.01, avg_logprob=-0.2,
                              words=words, text="".join(w for w, _, _ in WORDS))
        return iter([seg]), SimpleNamespace(duration=6.0, language="en")


@pytest.fixture
def both(tmp_path, monkeypatch, fake_ov):
    """Runs both workers on the same audio and words; returns their result.json dicts."""
    audio = write_wav_parts(tmp_path / "audio.wav", [(6.0, 8000)])
    monkeypatch.setattr(asr, "load_model", lambda *a: _FWModel())
    monkeypatch.setattr(asr, "setup_cuda_libs", lambda: [])
    fake_ov.words = list(WORDS)
    fake_ov.chunks = [(0.0, 6.0, "".join(w for w, _, _ in WORDS))]
    model_dir = tmp_path / "whisper-large-v3-turbo-fp16"
    model_dir.mkdir()
    for f in asr_openvino.MODEL_FILES:
        (model_dir / f).write_bytes(b"x")

    def run(fw_extra=None):
        fw_job = dict({"audio": audio, "model": "large-v3", "compute_type": "float16",
                       "language": "en", "initial_prompt": PROMPT, "device": "cuda"},
                      **(fw_extra or {}))
        ov_job = {"audio": audio, "model": "large-v3-turbo", "precision": "fp16",
                  "model_dir": str(model_dir), "device": "CPU", "language": "en",
                  "initial_prompt": PROMPT, "cache_dir": "", "hf_id": "openai/whisper-large-v3-turbo",
                  "hf_home": str(tmp_path / "hf"), "word_timestamps": True}
        out = {}
        for name, mod, job in (("fw", asr, fw_job), ("ov", asr_openvino, ov_job)):
            in_path = tmp_path / ("%s_job.json" % name)
            in_path.write_text(json.dumps(job), encoding="utf-8")
            out_path = tmp_path / ("%s_result.json.tmp" % name)
            mod.run(str(in_path), str(out_path), lambda line: None)
            with open(out_path, encoding="utf-8") as f:
                out[name] = json.load(f)
        return out

    return run


def test_same_words_give_the_same_result(both):
    r = both()
    assert set(r["fw"]) == {"prompt_terms_used", "sentences"}
    assert set(r["ov"]) == {"prompt_terms_used", "sentences", "timing"}
    ov_timing = r["ov"].pop("timing")
    assert r["fw"] == r["ov"]
    texts = [s["text"] for s in r["fw"]["sentences"]]
    # "so so so so" collapsed; the one-word sentences "Right?" and "Ok." (< 1 s) merged
    assert texts == ["Okay, e.g. a cache of 3.5 MB.", "J. Smith said so yes. Right? Ok."]
    assert r["fw"]["prompt_terms_used"] == 3
    assert set(ov_timing) == {"load_s", "transcribe_s", "audio_s", "first_compile",
                              "peak_working_set_mb"}


def test_timing_blocks_have_the_same_shape(both):
    r = both({"timing": True})
    fw, ov = r["fw"].pop("timing"), r["ov"].pop("timing")
    assert r["fw"] == r["ov"]
    assert set(fw) == set(ov)
    assert {k: type(v) for k, v in fw.items()} == {k: type(v) for k, v in ov.items()}
    assert fw["audio_s"] == ov["audio_s"] == 6.0
    assert fw["first_compile"] is False


# --- asr.py timing is opt-in (job.timing), so its old result.json is unchanged ------

def test_asr_timing_opt_in(tmp_path, monkeypatch, capsys):
    audio = write_wav_parts(tmp_path / "audio.wav", [(6.0, 8000)])
    monkeypatch.setattr(asr, "load_model", lambda *a: _FWModel())
    monkeypatch.setattr(asr, "setup_cuda_libs", lambda: [])
    job = {"audio": audio, "model": "large-v3", "compute_type": "float16", "language": "en",
           "initial_prompt": PROMPT}
    for timing, keys in ((None, {"prompt_terms_used", "sentences"}),
                         (False, {"prompt_terms_used", "sentences"}),
                         (True, {"prompt_terms_used", "sentences", "timing"})):
        data = dict(job) if timing is None else dict(job, timing=timing)
        in_path = tmp_path / "job.json"
        in_path.write_text(json.dumps(data), encoding="utf-8")
        out_path = tmp_path / "result.json.tmp"
        assert asr.main(["--in", str(in_path), "--out", str(out_path)]) == 0
        with open(out_path, encoding="utf-8") as f:
            assert set(json.load(f)) == keys
    in_path.write_text(json.dumps(dict(job, timing="yes")), encoding="utf-8")
    assert asr.main(["--in", str(in_path), "--out", str(tmp_path / "x.tmp")]) == 2
    assert "job.timing" in capsys.readouterr().err


def test_make_timing():
    t = asr_common.make_timing(1.234, 5.678, 60.0, True, 2683)
    assert t == {"load_s": 1.23, "transcribe_s": 5.68, "audio_s": 60.0, "first_compile": True,
                 "peak_working_set_mb": 2683}
    t = asr_common.make_timing(-1, 0, 1, 0)
    assert t["load_s"] == 0.0 and t["first_compile"] is False
    assert isinstance(t["peak_working_set_mb"], int) and t["peak_working_set_mb"] >= 0


def test_peak_working_set_is_read_on_windows():
    import sys
    mb = asr_common.peak_working_set_mb()
    assert isinstance(mb, int)
    if sys.platform == "win32":
        assert 5 < mb < 1 << 20


def test_model_files_and_defaults_match_the_go_side():
    """worker "model ready" = devices.ModelReady (same files); same default free space;
    the probe's device kinds and ignore reasons are the frozen Go constants."""
    import re
    from conftest import PROJECT_DIR
    with open(os.path.join(PROJECT_DIR, "internal", "devices", "paths.go"), encoding="utf-8") as f:
        paths = f.read()
    block = re.search(r"var ModelFiles = \[\]string\{(.*?)\}", paths, re.S).group(1)
    assert tuple(re.findall(r'"([^"]+)"', block)) == asr_openvino.MODEL_FILES
    assert "const PrepareMinFreeBytes int64 = 10 << 30" in paths
    assert asr_openvino.DEFAULT_MIN_FREE_BYTES == 10 << 30
    with open(os.path.join(PROJECT_DIR, "internal", "devices", "devices.go"), encoding="utf-8") as f:
        devices_go = f.read()
    for reason in (asr_openvino.REASON_NOT_INTEL, asr_openvino.REASON_SECOND_INTEL_GPU,
                   asr_openvino.REASON_ERROR):
        assert '"%s"' % reason in devices_go
    with open(os.path.join(PROJECT_DIR, "internal", "model", "v2_intel.go"), encoding="utf-8") as f:
        model_go = f.read()
    for kind in (asr_openvino.KIND_IGPU, asr_openvino.KIND_NPU, asr_openvino.KIND_CPU):
        assert '= "%s"' % kind in model_go


def test_fake_openvino_fixture_matches_result_schema():
    """testdata/fake_asr_openvino.json (Go's fake_result_openvino) has the worker's shape:
    no words, low_conf_words always [], a timing block like make_timing's."""
    from conftest import PROJECT_DIR
    with open(os.path.join(PROJECT_DIR, "testdata", "fake_asr_openvino.json"),
              encoding="utf-8") as f:
        data = json.load(f)
    assert set(data) == {"prompt_terms_used", "sentences", "timing"}
    assert set(data["timing"]) == set(asr_common.make_timing(0, 0, 0, False, 0))
    prev = 0
    for s in data["sentences"]:
        assert set(s) == {"start_ms", "end_ms", "text", "low_conf_words"}
        assert s["low_conf_words"] == [] and s["text"] == s["text"].strip()
        assert prev <= s["start_ms"] <= s["end_ms"]
        prev = s["end_ms"]
