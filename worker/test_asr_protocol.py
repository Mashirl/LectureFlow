# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""asr.py worker protocol (SPEC §6.0, §6.2, §17.2, §17.3) with a fake model.

No real whisper model is loaded here; load_model is replaced by a fake object.
"""
import json
import os
import re
import subprocess
import sys
from types import SimpleNamespace

import pytest

import asr
from conftest import PROJECT_DIR, WORKER_DIR

PROGRESS_RE = re.compile(r"^PROGRESS (0\.\d\d|1\.00)$")


def W(text, start, end, prob=0.9):
    return SimpleNamespace(word=" " + text, start=start, end=end, probability=prob)


def seg(start, end, words, no_speech=0.05, logprob=-0.2):
    return SimpleNamespace(start=start, end=end, text="".join(w.word for w in words),
                           no_speech_prob=no_speech, avg_logprob=logprob, words=words)


DEFAULT_SEGMENTS = [
    seg(0.0, 3.8, [W("Okay,", 0.0, 0.5), W("let's", 0.5, 0.9), W("get", 0.9, 1.2),
                   W("started.", 1.2, 3.8)]),
    seg(3.8, 8.2, [W("Today", 3.8, 4.2), W("we", 4.2, 4.4), W("talk", 4.4, 4.8),
                   W("about", 4.8, 5.1), W("the", 5.1, 5.3), W("cash", 5.3, 5.8, 0.31),
                   W("hierarchy.", 5.8, 8.2)]),
    # Hallucination on silence: must be dropped.
    seg(8.2, 9.0, [W("Subtitles", 8.2, 8.5), W("by", 8.5, 8.7), W("Amara.", 8.7, 9.0)],
        no_speech=0.92, logprob=-1.4),
    seg(9.0, 10.0, [W("the", 9.0, 9.1), W("the", 9.1, 9.2), W("the", 9.2, 9.3),
                    W("end.", 9.3, 10.0)]),
]


class FakeTokenizer:
    def encode(self, text, add_special_tokens=True):
        return SimpleNamespace(ids=text.split())


class FakeModel:
    def __init__(self, segments=None, duration=10.0, error=None):
        self.segments = DEFAULT_SEGMENTS if segments is None else segments
        self.duration = duration
        self.error = error
        self.hf_tokenizer = FakeTokenizer()
        self.calls = []

    def transcribe(self, audio, **kwargs):
        self.calls.append((audio, kwargs))
        if self.error:
            raise self.error

        def gen():
            for s in self.segments:
                yield s

        return gen(), SimpleNamespace(duration=self.duration, language="en",
                                      duration_after_vad=self.duration)


@pytest.fixture
def fake(monkeypatch):
    """Installs a FakeModel in place of the real model loader."""
    state = SimpleNamespace(model=FakeModel(), load_args=None)

    def fake_load(model_name, device, compute_type, cpu_threads, emit):
        state.load_args = (model_name, device, compute_type, cpu_threads)
        return state.model

    monkeypatch.setattr(asr, "load_model", fake_load)
    monkeypatch.setattr(asr, "setup_cuda_libs", lambda: [])
    return state


@pytest.fixture
def job(tmp_path, wav_factory):
    """Writes job.json (with overrides) and returns (in_path, out_path)."""
    audio = wav_factory("audio.wav", seconds=10.0)

    def make(**overrides):
        data = {"audio": audio, "model": "large-v3", "compute_type": "float16",
                "language": "en", "initial_prompt": "cache, write-back, TLB"}
        data.update(overrides)
        data = {k: v for k, v in data.items() if v is not _DROP}
        in_path = tmp_path / "job.json"
        in_path.write_text(json.dumps(data), encoding="utf-8")
        return str(in_path), str(tmp_path / "result.json.tmp")

    return make


_DROP = object()


def run_main(capsys, *argv):
    rc = asr.main(list(argv))
    out, err = capsys.readouterr()
    return rc, out.splitlines(), err


# --- success path -----------------------------------------------------------

def test_success_writes_result_and_progress(fake, job, capsys, tmp_path):
    in_path, out_path = job()
    rc, lines, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 0, err

    progress = [l for l in lines if l.startswith("PROGRESS")]
    assert all(PROGRESS_RE.match(l) for l in progress), progress
    # one line per segment (dropped ones included) plus the final 1.00
    assert progress == ["PROGRESS 0.38", "PROGRESS 0.82", "PROGRESS 0.90",
                        "PROGRESS 1.00", "PROGRESS 1.00"]
    assert "STATUS downloading_model" not in lines

    with open(out_path, encoding="utf-8") as f:
        result = json.load(f)
    assert set(result) == {"prompt_terms_used", "sentences"}
    assert result["prompt_terms_used"] == 3
    assert result["sentences"] == [
        {"start_ms": 0, "end_ms": 3800, "text": "Okay, let's get started.",
         "low_conf_words": [],
         "words": [{"w": "Okay,", "s": 0, "e": 500}, {"w": "let's", "s": 500, "e": 900},
                   {"w": "get", "s": 900, "e": 1200}, {"w": "started.", "s": 1200, "e": 3800}]},
        {"start_ms": 3800, "end_ms": 8200, "text": "Today we talk about the cash hierarchy.",
         "low_conf_words": [{"i": 5, "w": "cash"}],
         "words": [{"w": "Today", "s": 3800, "e": 4200}, {"w": "we", "s": 4200, "e": 4400},
                   {"w": "talk", "s": 4400, "e": 4800}, {"w": "about", "s": 4800, "e": 5100},
                   {"w": "the", "s": 5100, "e": 5300}, {"w": "cash", "s": 5300, "e": 5800},
                   {"w": "hierarchy.", "s": 5800, "e": 8200}]},
        # "Subtitles by Amara." (dropped segment) and the collapsed repeats of "the"
        # leave no words behind.
        {"start_ms": 9000, "end_ms": 10000, "text": "the end.", "low_conf_words": [],
         "words": [{"w": "the", "s": 9000, "e": 9100}, {"w": "end.", "s": 9300, "e": 10000}]},
    ]
    # The worker writes nothing but --out next to its inputs.
    assert sorted(os.listdir(tmp_path)) == ["audio.wav", "job.json", "result.json.tmp"]


def test_transcribe_parameters(fake, job, capsys):
    in_path, out_path = job()
    rc, _, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 0, err
    assert fake.load_args == ("large-v3", "cuda", "float16", 4)
    audio, kwargs = fake.model.calls[0]
    assert kwargs == {"language": "en", "initial_prompt": "cache, write-back, TLB",
                      "vad_filter": True, "word_timestamps": True, "beam_size": 5,
                      "condition_on_previous_text": False}
    assert "hotwords" not in kwargs
    # audio decoded in-process to 16 kHz mono float32
    assert audio.dtype.name == "float32" and len(audio) == 160000


def test_prompt_truncated_and_terms_counted(fake, job, capsys):
    terms = ["term%d" % i for i in range(300)]
    in_path, out_path = job(initial_prompt=", ".join(terms))
    rc, _, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 0, err
    _, kwargs = fake.model.calls[0]
    assert kwargs["initial_prompt"] == ", ".join(terms[:200])
    with open(out_path, encoding="utf-8") as f:
        assert json.load(f)["prompt_terms_used"] == 200


def test_no_prompt(fake, job, capsys):
    in_path, out_path = job(initial_prompt="")
    rc, _, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 0, err
    _, kwargs = fake.model.calls[0]
    assert kwargs["initial_prompt"] is None
    with open(out_path, encoding="utf-8") as f:
        assert json.load(f)["prompt_terms_used"] == 0


def test_cpu_device_and_threads(fake, job, capsys):
    in_path, out_path = job(device="cpu", compute_type="int8")
    assert run_main(capsys, "--in", in_path, "--out", out_path)[0] == 0
    assert fake.load_args == ("large-v3", "cpu", "int8", max(1, asr.physical_cores() - 1))
    in_path, out_path = job(device="cpu", compute_type="int8", cpu_threads=3)
    assert run_main(capsys, "--in", in_path, "--out", out_path)[0] == 0
    assert fake.load_args[3] == 3


def test_no_speech_gives_empty_sentence_list(fake, job, capsys):
    fake.model = FakeModel(segments=[])
    in_path, out_path = job()
    rc, lines, _ = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 0
    with open(out_path, encoding="utf-8") as f:
        assert json.load(f) == {"prompt_terms_used": 3, "sentences": []}
    assert [l for l in lines if l.startswith("PROGRESS")] == ["PROGRESS 1.00"]


def test_progress_is_monotonic_and_clamped(fake, job, capsys):
    fake.model = FakeModel(duration=4.0, segments=[
        seg(0.0, 2.0, [W("One", 0.0, 1.0), W("two.", 1.0, 2.0)]),
        seg(2.0, 1.5, [W("Odd", 1.4, 1.5), W("order.", 1.5, 1.5)]),
        seg(2.0, 9.0, [W("Past", 2.0, 3.0), W("end.", 3.0, 9.0)]),
    ])
    in_path, out_path = job()
    rc, lines, _ = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 0
    assert [l for l in lines if l.startswith("PROGRESS")] == [
        "PROGRESS 0.50", "PROGRESS 0.50", "PROGRESS 1.00", "PROGRESS 1.00"]


# --- input errors: exit code 2, no output file -------------------------------

@pytest.mark.parametrize("overrides,needle", [
    ({"compute_type": "auto"}, "auto"),
    ({"compute_type": "default"}, "compute_type"),
    ({"compute_type": "float64"}, "compute_type"),
    ({"compute_type": _DROP}, "compute_type"),
    ({"device": "tpu"}, "device"),
    ({"model": ""}, "model"),
    ({"audio": "does/not/exist.wav"}, "audio file not found"),
    ({"audio": _DROP}, "audio"),
    ({"cpu_threads": 0}, "cpu_threads"),
    ({"cpu_threads": "4"}, "cpu_threads"),
    ({"language": 5}, "language"),
    ({"initial_prompt": ["a", "b"]}, "initial_prompt"),
])
def test_input_errors_exit_2(fake, job, capsys, overrides, needle):
    in_path, out_path = job(**overrides)
    rc, _, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 2
    assert err.strip().splitlines()[-1].startswith("ERROR: input error")
    assert needle in err
    assert fake.load_args is None          # failed before loading the model
    assert not os.path.exists(out_path)


def test_bad_job_json_exit_2(fake, tmp_path, capsys):
    bad = tmp_path / "job.json"
    bad.write_text("{not json", encoding="utf-8")
    rc, _, err = run_main(capsys, "--in", str(bad), "--out", str(tmp_path / "r.tmp"))
    assert rc == 2 and "cannot read job file" in err
    bad.write_text("[1, 2]", encoding="utf-8")
    assert run_main(capsys, "--in", str(bad), "--out", str(tmp_path / "r.tmp"))[0] == 2


def test_missing_job_file_exit_2(fake, tmp_path, capsys):
    rc, _, err = run_main(capsys, "--in", str(tmp_path / "nope.json"), "--out",
                          str(tmp_path / "r.tmp"))
    assert rc == 2 and "job file not found" in err


def test_missing_arguments_exit_2(capsys):
    assert run_main(capsys)[0] == 2
    assert run_main(capsys, "--in", "job.json")[0] == 2
    assert run_main(capsys, "--bogus")[0] == 2


def test_undecodable_audio_exit_2(fake, job, tmp_path, capsys):
    garbage = tmp_path / "broken.wav"
    garbage.write_bytes(b"this is not audio at all" * 100)
    in_path, out_path = job(audio=str(garbage))
    rc, _, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 2 and "cannot decode audio" in err
    assert fake.load_args is None


# --- retryable errors ---------------------------------------------------------

def test_transcribe_failure_is_retryable(fake, job, capsys):
    fake.model = FakeModel(error=RuntimeError("CUDA failed with error out of memory"))
    in_path, out_path = job()
    rc, _, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 1
    assert err.strip().splitlines()[-1] == \
        "ERROR: RuntimeError: CUDA failed with error out of memory"
    assert not os.path.exists(out_path)


def test_failure_in_the_middle_writes_no_result(fake, job, capsys):
    def boom():
        yield DEFAULT_SEGMENTS[0]
        raise RuntimeError("decoder crashed")

    class Model(FakeModel):
        def transcribe(self, audio, **kwargs):
            return boom(), SimpleNamespace(duration=10.0, language="en")

    fake.model = Model()
    in_path, out_path = job()
    rc, lines, _ = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 1
    assert "PROGRESS 0.38" in lines
    assert not os.path.exists(out_path)


# --- model loading ------------------------------------------------------------

def _model_dir(path):
    os.makedirs(path, exist_ok=True)
    open(os.path.join(path, "model.bin"), "wb").close()
    return str(path)


def test_cached_model_loads_without_download_status(tmp_path):
    cached = _model_dir(tmp_path / "snap")
    calls, lines = [], []

    def download(name, local_files_only=False):
        calls.append((name, local_files_only))
        return cached

    assert asr.resolve_model_path("large-v3", lines.append, download) == cached
    assert calls == [("large-v3", True)]
    assert lines == []


def test_uncached_model_prints_status_before_download(tmp_path):
    target = tmp_path / "snap"
    calls, lines = [], []

    def download(name, local_files_only=False):
        calls.append((name, local_files_only, list(lines)))
        if local_files_only:
            raise FileNotFoundError("not in cache")
        return _model_dir(target)

    assert asr.resolve_model_path("large-v3", lines.append, download) == str(target)
    assert lines == ["STATUS downloading_model"]
    # the status line was printed before the real download started
    assert calls == [("large-v3", True, []),
                     ("large-v3", False, ["STATUS downloading_model"])]


def test_incomplete_cache_triggers_download(tmp_path):
    partial = tmp_path / "partial"
    partial.mkdir()
    lines = []

    def download(name, local_files_only=False):
        return str(partial) if local_files_only else _model_dir(tmp_path / "full")

    assert asr.resolve_model_path("tiny", lines.append, download) == str(tmp_path / "full")
    assert lines == ["STATUS downloading_model"]


def test_model_directory_is_used_directly(tmp_path):
    d = _model_dir(tmp_path / "local-model")

    def download(*a, **k):
        raise AssertionError("must not download")

    assert asr.resolve_model_path(d, lambda l: None, download) == d
    with pytest.raises(asr.InputError):
        asr.resolve_model_path(str(tmp_path), lambda l: None, download)


def test_unknown_model_name_is_input_error():
    with pytest.raises(asr.InputError, match="unknown model"):
        asr.resolve_model_path("large-v99", lambda l: None)


def test_load_model_passes_device_settings(monkeypatch, tmp_path):
    import faster_whisper
    created = {}

    class FakeWhisperModel:
        def __init__(self, path, **kw):
            created.update(path=path, **kw)

    d = _model_dir(tmp_path / "m")
    monkeypatch.setattr(asr, "resolve_model_path", lambda name, emit: d)
    monkeypatch.setattr(faster_whisper, "WhisperModel", FakeWhisperModel)
    lines = []
    asr.load_model("large-v3", "cuda", "int8_float16", 4, lines.append)
    assert created == {"path": d, "device": "cuda", "compute_type": "int8_float16",
                       "cpu_threads": 4}
    assert lines and lines[0].startswith("model large-v3 loaded")


def test_unsupported_compute_type_on_device_is_input_error(monkeypatch, tmp_path):
    import faster_whisper

    class FakeWhisperModel:
        def __init__(self, path, **kw):
            raise ValueError("Requested float16 compute type, but the target device "
                             "or backend do not support efficient float16 computation.")

    d = _model_dir(tmp_path / "m")
    monkeypatch.setattr(asr, "resolve_model_path", lambda name, emit: d)
    monkeypatch.setattr(faster_whisper, "WhisperModel", FakeWhisperModel)
    with pytest.raises(asr.InputError, match="float16"):
        asr.load_model("large-v3", "cpu", "float16", 4, lambda l: None)


# --- selftest -----------------------------------------------------------------

def test_selftest_with_fake_model(fake, capsys):
    rc, lines, err = run_main(capsys, "--selftest", "--device", "cpu")
    assert rc == 0, err
    assert fake.load_args[:3] == ("tiny", "cpu", "int8")
    assert any(l.startswith("SELFTEST ok device=cpu compute_type=int8") for l in lines)
    audio, kwargs = fake.model.calls[0]
    assert len(audio) == 16000 and not audio.any()       # 1 s of silence
    assert kwargs["vad_filter"] is False                  # make the model actually run


def test_selftest_defaults_to_cuda_float16(fake, capsys):
    rc, lines, _ = run_main(capsys, "--selftest")
    assert rc == 0
    assert fake.load_args == ("tiny", "cuda", "float16", 4)


@pytest.mark.parametrize("argv", [["--selftest", "--device", "tpu"],
                                  ["--selftest", "--compute-type", "auto"]])
def test_selftest_bad_arguments_exit_2(fake, capsys, argv):
    assert run_main(capsys, *argv)[0] == 2
    assert fake.load_args is None


def test_selftest_failure_is_retryable(fake, capsys):
    fake.model = FakeModel(error=RuntimeError("no CUDA-capable device is detected"))
    rc, _, err = run_main(capsys, "--selftest")
    assert rc == 1 and "no CUDA-capable device" in err


# --- environment helpers --------------------------------------------------------

def test_cpu_threads_defaults():
    assert asr.default_cpu_threads("cuda") == 4
    n = asr.physical_cores()
    assert 1 <= n <= (os.cpu_count() or n)
    assert asr.default_cpu_threads("cpu") == max(1, n - 1)


def test_nvidia_lib_dirs_are_existing_directories():
    dirs = asr._nvidia_lib_dirs()
    sub = "bin" if sys.platform == "win32" else "lib"
    assert all(os.path.isdir(d) and os.path.basename(d) == sub for d in dirs)
    assert len(dirs) == len({os.path.normcase(os.path.abspath(d)) for d in dirs})


# --- real process (exit codes as seen by Go), still no model --------------------

def _run_cli(*args):
    return subprocess.run([sys.executable, os.path.join(WORKER_DIR, "asr.py"), *args],
                          capture_output=True, cwd=PROJECT_DIR, timeout=120)


def test_cli_compute_type_auto_exits_2(job):
    in_path, out_path = job(compute_type="auto")
    p = _run_cli("--in", in_path, "--out", out_path)
    assert p.returncode == 2
    assert p.stderr.decode("utf-8").strip().splitlines()[-1].startswith("ERROR: input error")
    assert not os.path.exists(out_path)


def test_cli_missing_arguments_exit_2():
    p = _run_cli()
    assert p.returncode == 2


def test_cli_stderr_is_utf8(job):
    in_path, out_path = job(audio="缺失的音频.wav")
    p = _run_cli("--in", in_path, "--out", out_path)
    assert p.returncode == 2
    assert "缺失的音频.wav" in p.stderr.decode("utf-8")


# --- shared fixture contract ------------------------------------------------------

def test_fake_asr_fixture_matches_result_schema():
    """testdata/fake_asr.json (used by Go when asr.fake_result is set) has our shape."""
    with open(os.path.join(PROJECT_DIR, "testdata", "fake_asr.json"), encoding="utf-8") as f:
        data = json.load(f)
    assert set(data) == {"prompt_terms_used", "sentences"}
    assert isinstance(data["prompt_terms_used"], int)
    prev_end = 0
    n_exact = n_without = 0
    for s in data["sentences"]:
        # "words" (SPEC-v2 P4.2) is optional here: results of the v1 worker have none.
        assert set(s) - {"words"} == {"start_ms", "end_ms", "text", "low_conf_words"}
        assert isinstance(s["start_ms"], int) and isinstance(s["end_ms"], int)
        assert prev_end <= s["start_ms"] <= s["end_ms"]
        prev_end = s["end_ms"]
        assert s["text"] == s["text"].strip() and s["text"]
        n_words = len(s["text"].split())
        for lc in s["low_conf_words"]:
            assert set(lc) == {"i", "w"}
            assert 0 <= lc["i"] < n_words
            assert s["text"].split()[lc["i"]].strip(".,?!") == lc["w"].strip(".,?!")
        if "words" not in s:
            n_without += 1
            continue
        words = s["words"]
        assert words, "a sentence with text has words"
        prev_s = s["start_ms"]
        for x in words:
            assert set(x) == {"w", "s", "e"}
            assert isinstance(x["s"], int) and isinstance(x["e"], int)
            assert x["w"] and x["w"] == x["w"].strip()
            assert prev_s <= x["s"] <= x["e"] <= s["end_ms"]
            prev_s = x["s"]
        assert words[0]["s"] == s["start_ms"] and words[-1]["e"] == s["end_ms"]
        assert "".join(x["w"] for x in words) == "".join(s["text"].split())
        for lc in s["low_conf_words"]:
            assert words[lc["i"]]["w"] == lc["w"]
        if " ".join(x["w"] for x in words) == s["text"]:
            n_exact += 1
    # Go tests need both paths: words that match the text, and no words at all.
    assert n_exact >= 1 and n_without >= 1
