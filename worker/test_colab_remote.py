# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""worker/colab_remote.py (SPEC-v3 V14.3 step 3) with a fake faster_whisper and a fake
ctranslate2: standard output holds only the allowed JSON events and never any text
derived from the audio; result.json has the local worker's structure."""
import hashlib
import json
import os
import re
import sys
import types
from types import SimpleNamespace

import numpy as np
import pytest

import asr
import asr_common
import colab_remote

# Words of the fake transcript: none of them may ever reach stdout.
SECRET_WORDS = ["Zebrafish", "quokka", "Vigenère", "pterodactyl"]

# The contract as internal/colab/events.go checks it.
ALLOWED = {
    "progress": ({"type", "ts", "fraction"}, set()),
    "stage": ({"type", "ts", "stage"}, set()),
    "done": ({"type", "ts", "result", "sha256", "bytes", "audio_s", "load_s",
              "transcribe_s", "sentences"}, set()),
    "error": ({"type", "ts", "code", "retryable"}, {"exc"}),
}


def check_stdout(out):
    """Every line is one allowed event; returns the events."""
    assert out.endswith("\n") or out == ""
    events = []
    for line in out.splitlines():
        ev = json.loads(line)
        assert isinstance(ev, dict), line
        required, optional = ALLOWED[ev["type"]]
        assert required <= set(ev) <= required | optional, line
        for k, v in ev.items():
            assert isinstance(v, (str, int, float, bool)), line
        assert isinstance(ev["ts"], (int, float)) and ev["ts"] > 0
        if ev["type"] == "stage":
            assert ev["stage"] in colab_remote.STAGES
        if ev["type"] == "progress":
            assert 0 <= ev["fraction"] <= 1
        if ev["type"] == "done":
            assert re.fullmatch(r"[0-9a-f]{64}", ev["sha256"])
            assert re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}", ev["result"])
        if ev["type"] == "error":
            assert ev["code"] in colab_remote.ERROR_CODES
            if "exc" in ev:
                assert re.fullmatch(r"[A-Za-z_][A-Za-z0-9_.]{0,79}", ev["exc"])
        events.append(ev)
    for w in SECRET_WORDS:
        assert w not in out
        assert w.encode("ascii", "backslashreplace").decode() not in out
        assert json.dumps(w)[1:-1] not in out  # not even JSON-escaped
    return events


# ---------------------------------------------------------------------------
# Fakes
# ---------------------------------------------------------------------------

class FakeWord:
    def __init__(self, word, start, end, probability=0.9):
        self.word, self.start, self.end, self.probability = word, start, end, probability


class FakeSegment:
    def __init__(self, seek, start, end, words):
        self.seek, self.start, self.end, self.words = seek, start, end, words
        self.no_speech_prob, self.avg_logprob = 0.01, -0.2
        self.text = "".join(w.word for w in words)


def fake_segments():
    segs, t = [], 0.0
    for i, w in enumerate(SECRET_WORDS * 3):
        words = [FakeWord(" " + w, t, t + 0.4), FakeWord(" is.", t + 0.4, t + 0.8)]
        segs.append(FakeSegment(i * 100, t, t + 0.8, words))
        t += 1.0
    return segs


class FakeTokenizer:
    def encode(self, text, add_special_tokens=False):
        return SimpleNamespace(ids=text.split())


def make_fake_fw(state):
    fw = types.ModuleType("faster_whisper")
    fw.__version__ = "1.2.1-fake"

    class WhisperModel:
        def __init__(self, path, device, compute_type, cpu_threads):
            state.loaded.append((path, device, compute_type, cpu_threads))
            if state.load_error:
                raise state.load_error
            self.hf_tokenizer = FakeTokenizer()

        def transcribe(self, audio, language=None, initial_prompt=None, **kw):
            state.transcribe_calls.append({"language": language, "initial_prompt": initial_prompt, **kw})
            if state.chatty:
                # a library writing to stdout, at the Python and the fd level
                print("debug: Zebrafish")
                sys.stdout.flush()
                os.write(1, b"native: quokka\n")
            if state.transcribe_error:
                raise state.transcribe_error
            info = SimpleNamespace(duration=len(audio) / 16000.0, language=language or "en")
            return iter(fake_segments()), info

    def decode_audio(path, sampling_rate=16000):
        if state.decode_error:
            raise state.decode_error
        return np.zeros(int(state.audio_s * sampling_rate), dtype=np.float32)

    utils = types.ModuleType("faster_whisper.utils")

    def available_models():
        return ["tiny", "large-v3"]

    def download_model(name, local_files_only=False):
        d = os.path.join(state.models_dir, name)
        if local_files_only and not state.cached:
            raise RuntimeError("not cached")
        os.makedirs(d, exist_ok=True)
        with open(os.path.join(d, "model.bin"), "wb") as f:
            f.write(b"x")
        state.downloads.append(name)
        return d

    utils.available_models = available_models
    utils.download_model = download_model
    fw.WhisperModel = WhisperModel
    fw.decode_audio = decode_audio
    fw.utils = utils
    return fw, utils


@pytest.fixture
def remote(tmp_path, monkeypatch):
    state = SimpleNamespace(loaded=[], transcribe_calls=[], downloads=[], load_error=None,
                            transcribe_error=None, decode_error=None, chatty=False, cached=False,
                            audio_s=20.0, models_dir=str(tmp_path / "models"), cuda=1,
                            installs=[], install_rc=0)
    fw, utils = make_fake_fw(state)
    monkeypatch.setitem(sys.modules, "faster_whisper", fw)
    monkeypatch.setitem(sys.modules, "faster_whisper.utils", utils)
    ct2 = types.ModuleType("ctranslate2")
    ct2.get_cuda_device_count = lambda: state.cuda
    monkeypatch.setitem(sys.modules, "ctranslate2", ct2)
    monkeypatch.setattr(asr, "setup_cuda_libs", lambda: [])

    job_dir = tmp_path / "jobs" / "j1"
    job_dir.mkdir(parents=True)
    audio = job_dir / "audio.flac"
    audio.write_bytes(b"fLaC fake audio bytes")
    job = {"audio": "audio.flac", "audio_sha256": hashlib.sha256(audio.read_bytes()).hexdigest(),
           "model": "large-v3", "compute_type": "float16", "language": "en",
           "initial_prompt": "Vigenère, pterodactyl", "result": "result.json", "requirements": []}
    state.job_dir = job_dir
    state.job = job
    state.log_dir = str(tmp_path)

    def write_job(**changes):
        j = dict(job, **changes)
        (job_dir / "job.json").write_text(json.dumps(j), encoding="utf-8")
        return str(job_dir / "job.json")

    def install(reqs, log_path):
        state.installs.append((list(reqs), log_path))
        with open(log_path, "a", encoding="utf-8") as f:
            f.write("Collecting quokka\n")  # pip's chatter goes to the log only
        return state.install_rc

    state.write_job = write_job
    state.install = install
    return state


def run_main(remote, capfd, job_path=None, **kw):
    job_path = job_path or remote.write_job()
    rc = colab_remote.main(["--job", job_path, "--log-dir", remote.log_dir], install=remote.install, **kw)
    out, err = capfd.readouterr()
    return rc, out, err


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

def test_success_stdout_is_only_events_and_result_matches(remote, capfd):
    remote.chatty = True
    rc, out, err = run_main(remote, capfd)
    assert rc == 0
    events = check_stdout(out)
    stages = [e["stage"] for e in events if e["type"] == "stage"]
    assert stages == ["check", "install", "download_model", "load_model", "transcribe", "write"]
    assert any(e["type"] == "progress" for e in events)
    assert events[-1]["type"] == "done"
    done = events[-1]
    assert [e["type"] for e in events].count("done") == 1
    # the chatty library's output went to stderr, not stdout
    assert "Zebrafish" in err and "quokka" in err
    res_path = remote.job_dir / "result.json"
    data = res_path.read_bytes()
    assert done["sha256"] == hashlib.sha256(data).hexdigest()
    assert done["bytes"] == len(data)
    assert done["audio_s"] == pytest.approx(20.0)
    result = json.loads(data)
    assert set(result) == {"prompt_terms_used", "sentences", "timing"}
    assert done["sentences"] == len(result["sentences"]) > 0
    s0 = result["sentences"][0]
    assert set(s0) == {"start_ms", "end_ms", "text", "low_conf_words", "words"}
    assert s0["words"] and set(s0["words"][0]) == {"w", "s", "e"}
    assert "Zebrafish" in s0["text"]  # the text is in the result file, never on stdout
    assert result["prompt_terms_used"] == 2
    # the same transcription parameters as the local worker
    call = remote.transcribe_calls[0]
    assert call["initial_prompt"] == "Vigenère, pterodactyl"
    for k, v in asr.TRANSCRIBE_KWARGS.items():
        assert call[k] == v
    assert remote.loaded[0][1:] == ("cuda", "float16", 4)
    # the uploaded audio is removed once decoded
    assert not (remote.job_dir / "audio.flac").exists()


def test_shared_sentence_splitting(remote, capfd):
    # colab_remote transcribes through asr.transcribe_job, which uses asr_common's
    # split_sentences: one implementation for every worker (SPEC-v2 I11).
    assert colab_remote.run.__globals__["__name__"] == "colab_remote"
    assert asr.split_sentences is asr_common.split_sentences
    rc, out, _ = run_main(remote, capfd)
    assert rc == 0
    result = json.loads((remote.job_dir / "result.json").read_text(encoding="utf-8"))
    from asr_common import remove_repeats, split_sentences
    words = [asr_common.as_word(w) for s in fake_segments() for w in s.words]
    assert result["sentences"] == split_sentences(remove_repeats(words))


def test_cached_model_has_no_download_stage(remote, capfd):
    remote.cached = True
    rc, out, _ = run_main(remote, capfd)
    assert rc == 0
    stages = [e["stage"] for e in check_stdout(out) if e["type"] == "stage"]
    assert "download_model" not in stages


def test_progress_is_throttled_and_monotonic(remote, capfd):
    rc, out, _ = run_main(remote, capfd)
    assert rc == 0
    fr = [e["fraction"] for e in check_stdout(out) if e["type"] == "progress"]
    assert fr == sorted(fr) and fr[-1] == 1.0
    assert len(fr) <= 101


def test_audio_hash_mismatch(remote, capfd):
    rc, out, _ = run_main(remote, capfd, job_path=remote.write_job(audio_sha256="0" * 64))
    assert rc == 1
    ev = check_stdout(out)
    assert ev[-1] == {"type": "error", "ts": ev[-1]["ts"], "code": "audio_hash_mismatch", "retryable": True}
    assert not remote.loaded


@pytest.mark.parametrize("change", [
    {"audio": "../escape.flac"}, {"result": "sub/result.json"}, {"audio_sha256": "xyz"},
    {"model": ""}, {"requirements": ["faster-whisper>=1"]}, {"requirements": "faster-whisper==1.2.1"},
])
def test_bad_job_is_input_error(remote, capfd, change):
    rc, out, _ = run_main(remote, capfd, job_path=remote.write_job(**change))
    assert rc == 2
    ev = check_stdout(out)
    assert ev[-1]["type"] == "error" and ev[-1]["code"] == "bad_job" and ev[-1]["retryable"] is False


def test_unreadable_job_file(remote, capfd, tmp_path):
    p = tmp_path / "broken.json"
    p.write_text("{not json", encoding="utf-8")
    rc, out, _ = run_main(remote, capfd, job_path=str(p))
    assert rc == 2
    assert check_stdout(out)[-1]["code"] == "bad_job"


def test_missing_audio(remote, capfd):
    os.remove(remote.job_dir / "audio.flac")
    rc, out, _ = run_main(remote, capfd)
    assert rc == 2
    assert check_stdout(out)[-1]["code"] == "audio_missing"


def test_install_runs_pip_into_the_log_only(remote, capfd, monkeypatch):
    installed = {}
    monkeypatch.setattr(colab_remote, "installed_version", lambda name: installed.get(name))

    def install(reqs, log_path):
        remote.installs.append((list(reqs), log_path))
        with open(log_path, "a", encoding="utf-8") as f:
            f.write("Collecting quokka\n")
        for r in reqs:
            n, v = r.split("==")
            installed[n] = v
        return 0

    remote.install = install
    rc, out, err = run_main(remote, capfd, job_path=remote.write_job(requirements=["faster-whisper==1.2.1", "av==15.1.0"]))
    assert rc == 0
    check_stdout(out)
    assert remote.installs == [(["faster-whisper==1.2.1", "av==15.1.0"], os.path.join(remote.log_dir, "install.log"))]
    with open(os.path.join(remote.log_dir, "install.log"), encoding="utf-8") as f:
        assert "quokka" in f.read()


def test_install_skipped_when_pins_present(remote, capfd, monkeypatch):
    monkeypatch.setattr(colab_remote, "installed_version", lambda name: {"faster-whisper": "1.2.1"}.get(name))
    rc, out, _ = run_main(remote, capfd, job_path=remote.write_job(requirements=["faster-whisper==1.2.1"]))
    assert rc == 0 and remote.installs == []


def test_install_failure(remote, capfd, monkeypatch):
    monkeypatch.setattr(colab_remote, "installed_version", lambda name: None)
    remote.install_rc = 1
    rc, out, _ = run_main(remote, capfd, job_path=remote.write_job(requirements=["faster-whisper==1.2.1"]))
    assert rc == 1
    assert check_stdout(out)[-1]["code"] == "install_failed"


def test_no_gpu(remote, capfd):
    remote.cuda = 0
    rc, out, _ = run_main(remote, capfd)
    assert rc == 1
    assert check_stdout(out)[-1]["code"] == "no_gpu"


def test_exception_text_never_reaches_stdout(remote, capfd):
    remote.transcribe_error = RuntimeError("could not handle Zebrafish quokka segment")
    rc, out, err = run_main(remote, capfd)
    assert rc == 1
    ev = check_stdout(out)[-1]
    assert ev["code"] == "transcribe_failed" and ev["exc"] == "RuntimeError"


def test_out_of_memory(remote, capfd):
    remote.load_error = RuntimeError("CUDA failed with error out of memory")
    rc, out, _ = run_main(remote, capfd)
    assert check_stdout(out)[-1]["code"] == "out_of_memory"


def test_internal_error_has_class_name_only(remote, capfd, monkeypatch):
    def boom(*a, **k):
        raise ValueError("Vigenère pterodactyl")
    monkeypatch.setattr(asr, "transcribe_job", boom)
    rc, out, err = run_main(remote, capfd)
    assert rc == 1
    ev = check_stdout(out)[-1]
    assert ev["code"] == "transcribe_failed" and ev["exc"] == "ValueError"

    monkeypatch.setattr(colab_remote, "load_job", boom)
    rc, out, err = run_main(remote, capfd)
    ev = check_stdout(out)[-1]
    assert ev["code"] == "internal" and ev["exc"] == "ValueError"


def test_events_refuse_fields_outside_the_contract():
    class Buf:
        def __init__(self):
            self.text = ""

        def write(self, s):
            self.text += s

        def flush(self):
            pass

    ev = colab_remote.Events(Buf())
    with pytest.raises(ValueError):
        ev._emit("progress", fraction=0.5, text="Zebrafish")
    with pytest.raises(ValueError):
        ev._emit("stage", stage=["check"])
    with pytest.raises(AssertionError):
        ev.stage("Zebrafish")


def test_stdout_restored_after_isolation(remote, capfd):
    run_main(remote, capfd)
    print("after")
    out, _ = capfd.readouterr()
    assert out == "after\n"


def test_contract_tables_match_go():
    """EVENT_KEYS, STAGES and ERROR_CODES equal internal/colab/events.go's tables."""
    src = open(os.path.join(os.path.dirname(__file__), "..", "internal", "colab", "events.go"),
               encoding="utf-8").read()
    stages = re.search(r"remoteStages = \[\]string\{([^}]*)\}", src).group(1)
    assert tuple(re.findall(r'"([^"]+)"', stages)) == colab_remote.STAGES
    codes = re.search(r"remoteErrorCodes = \[\]string\{([^}]*)\}", src, re.S).group(1)
    assert tuple(re.findall(r'"([^"]+)"', codes)) == colab_remote.ERROR_CODES
    for typ, (required, optional) in colab_remote.EVENT_KEYS.items():
        m = re.search(r'"%s":\s*\{required: \[\]string\{([^}]*)\}(?:, optional: \[\]string\{([^}]*)\})?\}' % typ, src)
        assert m, typ
        assert tuple(re.findall(r'"([^"]+)"', m.group(1))) == required
        assert tuple(re.findall(r'"([^"]+)"', m.group(2) or "")) == optional
