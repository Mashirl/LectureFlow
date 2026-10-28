# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""The workers' checks for the diagnostics page (ext-step1, .claude/ext-brief.md §2):
asr.py --check-cudnn, asr_mlx.py --selftest and asr_openvino.py --selftest, with fake
libraries (SPEC §14: no real model, no real GPU). The real ones are marked realgpu /
realmlx and skipped unless selected."""
import os
import re
import subprocess
import sys

import numpy as np
import pytest

import asr
import asr_mlx
import asr_openvino as ov_worker
from conftest import PROJECT_DIR, WORKER_DIR
from test_asr_mlx import fake_mlx, make_model_dir as make_mlx_model  # noqa: F401 (fixture)
from test_asr_openvino import make_model_dir as make_ov_model

SELFTEST_RE = re.compile(r"^SELFTEST ok device=(\S+) load_s=\d+\.\d transcribe_s=\d+\.\d\d segments=(\d+)$")


def run_main(module, capsys, *argv):
    rc = module.main(list(argv))
    out, err = capsys.readouterr()
    return rc, out.splitlines(), err.splitlines()


# ---------------------------------------------------------------------------
# asr.py --check-cudnn
# ---------------------------------------------------------------------------

@pytest.mark.parametrize("v, text", [(90100, "9.1.0"), (90501, "9.5.1"), (8902, "8.9.2"), (8600, "8.6.0")])
def test_cudnn_version_text(v, text):
    assert asr.cudnn_version_text(v) == text


class _FakeFn:
    def __init__(self, v):
        self.v = v
        self.restype = None

    def __call__(self):
        return self.v


class _FakeLib:
    def __init__(self, v=None):
        if v is not None:
            self.cudnnGetVersion = _FakeFn(v)


def test_check_cudnn_first_loadable_library():
    loaded = []

    def load(path):
        loaded.append(path)
        if path == "missing.dll":
            raise OSError("[WinError 126] The specified module could not be found")
        if path == "other.dll":
            return _FakeLib()          # not cuDNN
        return _FakeLib(90100)

    lines = []
    asr.check_cudnn(lines.append, candidates=["missing.dll", "other.dll", "cudnn64_9.dll"], load=load)
    assert lines == ["CUDNN ok version=9.1.0"]
    assert loaded == ["missing.dll", "other.dll", "cudnn64_9.dll"]


def test_check_cudnn_none_loads_is_check_failed():
    def load(path):
        raise OSError("not found")

    with pytest.raises(asr.CheckFailed) as e:
        asr.check_cudnn(lambda line: None, candidates=["a/cudnn64_9.dll"], load=load)
    assert "cudnn64_9.dll: not found" in str(e.value)
    with pytest.raises(asr.CheckFailed) as e:
        asr.check_cudnn(lambda line: None, candidates=[], load=load)
    assert "not installed" in str(e.value)


def test_check_cudnn_cli_failure_exit_2(monkeypatch, capsys):
    monkeypatch.setattr(asr, "cudnn_candidates", lambda dirs=None: [])
    rc, out, err = run_main(asr, capsys, "--check-cudnn")
    assert rc == 2
    assert out == []
    assert err[-1].startswith("ERROR: cuDNN cannot be loaded")


def test_check_cudnn_cli_success(monkeypatch, capsys):
    monkeypatch.setattr(asr, "cudnn_candidates", lambda dirs=None: ["cudnn64_9.dll"])
    real = asr.check_cudnn
    monkeypatch.setattr(asr, "check_cudnn", lambda emit=asr.emit_stdout: real(emit, load=lambda p: _FakeLib(90100)))
    rc, out, err = run_main(asr, capsys, "--check-cudnn")
    assert rc == 0, err
    assert out == ["CUDNN ok version=9.1.0"]


def test_cudnn_candidates_from_the_package_dirs(tmp_path):
    names = ["cudnn64_9.dll", "libcudnn.so.9", "cublas64_12.dll"]
    for n in names:
        (tmp_path / n).write_bytes(b"x")
    found = [os.path.basename(p) for p in asr.cudnn_candidates([str(tmp_path)]) if os.path.dirname(p) == str(tmp_path)]
    if sys.platform == "win32":
        assert found == ["cudnn64_9.dll"]
    elif sys.platform == "darwin":
        assert found == []
    else:
        assert found == ["libcudnn.so.9"]


def test_check_cudnn_does_not_import_faster_whisper(monkeypatch, capsys):
    monkeypatch.delitem(sys.modules, "faster_whisper", raising=False)
    monkeypatch.setattr(asr, "cudnn_candidates", lambda dirs=None: [])
    run_main(asr, capsys, "--check-cudnn")
    assert "faster_whisper" not in sys.modules


# ---------------------------------------------------------------------------
# asr_mlx.py --selftest
# ---------------------------------------------------------------------------

def test_mlx_selftest_one_second_of_silence(fake_mlx, tmp_path, capsys):  # noqa: F811
    model_dir = make_mlx_model(tmp_path / "large-v3-turbo")
    rc, out, err = run_main(asr_mlx, capsys, "--selftest", "--model", model_dir)
    assert rc == 0, err
    m = SELFTEST_RE.match(out[-1])
    assert m and m.group(1) == "mlx:gpu" and m.group(2) == "0", out
    assert len(fake_mlx.CALLS) == 1
    call = fake_mlx.CALLS[0]
    assert call["samples"] == 16000
    assert call["path_or_hf_repo"] == model_dir
    assert call["word_timestamps"] is False and call["language"] == "en"
    assert call["condition_on_previous_text"] is False and call["fp16"] is True
    assert fake_mlx.load_models.LOADS == [(model_dir, "float16")]


def test_mlx_selftest_fp32(fake_mlx, tmp_path, capsys):  # noqa: F811
    model_dir = make_mlx_model(tmp_path / "small")
    rc, out, err = run_main(asr_mlx, capsys, "--selftest", "--model", model_dir, "--precision", "fp32")
    assert rc == 0, err
    assert fake_mlx.CALLS[0]["fp16"] is False
    assert fake_mlx.load_models.LOADS == [(model_dir, "float32")]


def test_mlx_selftest_input_errors_exit_2(fake_mlx, tmp_path, capsys):  # noqa: F811
    rc, out, err = run_main(asr_mlx, capsys, "--selftest")
    assert rc == 2 and "--model is required" in err[-1] and err[-1].startswith("ERROR: ")
    incomplete = tmp_path / "incomplete"
    incomplete.mkdir()
    (incomplete / "config.json").write_text("{}", encoding="utf-8")
    rc, out, err = run_main(asr_mlx, capsys, "--selftest", "--model", str(incomplete))
    assert rc == 2 and "not installed" in err[-1]
    rc, out, err = run_main(asr_mlx, capsys, "--selftest", "--model", make_mlx_model(tmp_path / "m"), "--precision", "q3")
    assert rc == 2 and "unsupported precision" in err[-1]
    assert fake_mlx.CALLS == []


def test_mlx_selftest_without_metal_is_device_unavailable(fake_mlx, tmp_path, capsys, monkeypatch):  # noqa: F811
    monkeypatch.setattr(fake_mlx.core.metal, "AVAILABLE", [False])
    rc, out, err = run_main(asr_mlx, capsys, "--selftest", "--model", make_mlx_model(tmp_path / "m"))
    assert rc == 2
    assert "ERROR_CODE device_unavailable" in out
    assert err[-1] == "ERROR: MLX reports no Metal GPU"


def test_mlx_selftest_failure_is_exit_1(fake_mlx, tmp_path, capsys):  # noqa: F811
    fake_mlx.CFG["error"] = True
    rc, out, err = run_main(asr_mlx, capsys, "--selftest", "--model", make_mlx_model(tmp_path / "m"))
    assert rc == 1
    assert err[-1].startswith("ERROR: RuntimeError: fake mlx failure")
    assert not any(l.startswith("SELFTEST") for l in out)


# ---------------------------------------------------------------------------
# asr_openvino.py --selftest
# ---------------------------------------------------------------------------

def test_openvino_selftest_cpu(fake_ov, tmp_path, capsys):
    model_dir = make_ov_model(tmp_path / "whisper-small-int8")
    fake_ov.chunks = [(0.0, 1.0, " .")]
    rc, out, err = run_main(ov_worker, capsys, "--selftest", "--device", "CPU", "--model", model_dir)
    assert rc == 0, err
    m = SELFTEST_RE.match(out[-1])
    assert m and m.group(1) == "CPU" and m.group(2) == "1", out
    assert len(fake_ov.pipelines) == 1
    pipe = fake_ov.pipelines[0]
    assert pipe.model_dir == model_dir and pipe.device == "CPU"
    assert pipe.props == {}                      # no CACHE_DIR on the CPU, no word timestamps
    call = fake_ov.generate_calls[0]
    audio = np.asarray(call["audio"])
    assert audio.shape == (16000,) and not audio.any()
    cfg = call["config"]
    assert cfg["word_timestamps"] is False and cfg["num_beams"] == 1 and cfg["language"] == "<|en|>"
    assert cfg["task"] == "transcribe" and not cfg.get("initial_prompt")


def test_openvino_selftest_intel_gpu_with_cache_dir(fake_ov, tmp_path, capsys):
    model_dir = make_ov_model(tmp_path / "whisper-large-v3-turbo-fp16")
    cache = tmp_path / "cache" / "GPU-large-v3-turbo-fp16"
    rc, out, err = run_main(ov_worker, capsys, "--selftest", "--device", "GPU", "--model", model_dir,
                            "--cache-dir", str(cache))
    assert rc == 0, err
    assert SELFTEST_RE.match(out[-1]).group(1) == "GPU"
    pipe = fake_ov.pipelines[0]
    assert pipe.device == "GPU.0"                # the Intel GPU, not the NVIDIA card (GPU.1)
    assert pipe.props == {"CACHE_DIR": str(cache)}


@pytest.mark.parametrize("argv, needle", [
    (["--selftest", "--device", "CPU"], "--model is required"),
    (["--selftest", "--device", "TPU", "--model", "x"], "unsupported device"),
])
def test_openvino_selftest_input_errors_exit_2(fake_ov, capsys, argv, needle):
    rc, out, err = run_main(ov_worker, capsys, *argv)
    assert rc == 2 and needle in err[-1] and err[-1].startswith("ERROR: ")
    assert fake_ov.pipelines == []


def test_openvino_selftest_incomplete_model_exit_2(fake_ov, tmp_path, capsys):
    model_dir = make_ov_model(tmp_path / "m", skip=("openvino_decoder_model.bin",))
    rc, out, err = run_main(ov_worker, capsys, "--selftest", "--device", "CPU", "--model", model_dir)
    assert rc == 2 and "model is not ready" in err[-1]
    assert fake_ov.pipelines == []


def test_openvino_selftest_missing_device_is_device_unavailable(fake_ov, tmp_path, capsys):
    del fake_ov.devices["NPU"]
    rc, out, err = run_main(ov_worker, capsys, "--selftest", "--device", "NPU", "--model", make_ov_model(tmp_path / "m"))
    assert rc == 2
    assert "ERROR_CODE device_unavailable" in out


def test_openvino_selftest_failure_is_exit_1(fake_ov, tmp_path, capsys):
    fake_ov.generate_error = RuntimeError("inference failed (fake)")
    rc, out, err = run_main(ov_worker, capsys, "--selftest", "--device", "CPU", "--model", make_ov_model(tmp_path / "m"))
    assert rc == 1
    assert err[-1] == "ERROR: RuntimeError: inference failed (fake)"


def test_openvino_selftest_without_openvino_exit_2(monkeypatch, tmp_path, capsys):
    monkeypatch.setitem(sys.modules, "openvino", None)   # import openvino raises ImportError
    rc, out, err = run_main(ov_worker, capsys, "--selftest", "--device", "CPU", "--model", make_ov_model(tmp_path / "m"))
    assert rc == 2
    assert "cannot import openvino" in err[-1]


# ---------------------------------------------------------------------------
# Real libraries (opt-in)
# ---------------------------------------------------------------------------

@pytest.mark.realgpu
def test_real_check_cudnn():
    """The .venv's nvidia-cudnn-cu12 loads (no model, no inference)."""
    p = subprocess.run([sys.executable, os.path.join(WORKER_DIR, "asr.py"), "--check-cudnn"],
                       capture_output=True, cwd=PROJECT_DIR, timeout=120)
    out = p.stdout.decode("utf-8", "replace")
    assert p.returncode == 0, p.stderr.decode("utf-8", "replace")[-2000:]
    assert re.search(r"^CUDNN ok version=\d+\.\d+\.\d+$", out, re.M), out


@pytest.mark.realmlx
def test_real_mlx_selftest(request):
    """A real MLX model on the Metal GPU (macOS on Apple silicon, LECTUREFLOW_MLX_MODEL_DIR)."""
    from test_asr_mlx import _realmlx_skip
    _realmlx_skip(request)
    p = subprocess.run([sys.executable, os.path.join(WORKER_DIR, "asr_mlx.py"), "--selftest",
                        "--model", os.environ["LECTUREFLOW_MLX_MODEL_DIR"]],
                       capture_output=True, cwd=PROJECT_DIR, timeout=600)
    out = p.stdout.decode("utf-8", "replace")
    assert p.returncode == 0, p.stderr.decode("utf-8", "replace")[-2000:]
    assert re.search(r"^SELFTEST ok device=mlx:gpu load_s=\S+ transcribe_s=\S+ segments=\d+$", out, re.M), out
