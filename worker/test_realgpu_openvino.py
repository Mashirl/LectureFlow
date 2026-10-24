# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""Real-device tests of worker/asr_openvino.py (CLAUDE.md revision 2). Skipped unless run
with -m realgpu:

    .venv\\Scripts\\python.exe -m pytest -q -m realgpu worker\\test_realgpu_openvino.py

They start the .venv-ov interpreter (env LF_REALGPU_OV_PYTHON, default
.venv-ov\\Scripts\\python.exe) on the Intel GPU and the NPU with the 10 s sample, using
the product's model and compile-cache locations under data/models/openvino (a missing
model is downloaded and converted first; a cold NPU cache compiles for minutes). Model:
env LF_REALGPU_OV_MODEL (default large-v3-turbo); precision: LF_REALGPU_OV_PRECISION
(default fp16).
"""
import json
import os
import re
import shutil
import subprocess

import pytest

from conftest import PROJECT_DIR, WORKER_DIR

pytestmark = pytest.mark.realgpu

OV_PYTHON = os.environ.get("LF_REALGPU_OV_PYTHON",
                           os.path.join(PROJECT_DIR, ".venv-ov", "Scripts", "python.exe"))
WORKER = os.path.join(WORKER_DIR, "asr_openvino.py")
SAMPLE10 = os.path.join(PROJECT_DIR, "sample", "sample10.wav")
MODEL = os.environ.get("LF_REALGPU_OV_MODEL", "large-v3-turbo")
PRECISION = os.environ.get("LF_REALGPU_OV_PRECISION", "fp16")
MODELS = os.path.join(PROJECT_DIR, "data", "models", "openvino")


@pytest.fixture(scope="module")
def ov_python():
    if not os.path.isfile(OV_PYTHON):
        pytest.skip("no OpenVINO interpreter at %s" % OV_PYTHON)
    return OV_PYTHON


@pytest.fixture(scope="module")
def probe(ov_python):
    p = subprocess.run([ov_python, WORKER, "--probe"], capture_output=True, timeout=120)
    assert p.returncode == 0, p.stderr.decode("utf-8", "replace")[-2000:]
    (line,) = [l for l in p.stdout.decode("utf-8").splitlines() if l.startswith("PROBE ")]
    return json.loads(line[len("PROBE "):])


def test_probe_lists_the_intel_devices(probe):
    kinds = {d["kind"]: d for d in probe["devices"]}
    assert set(kinds) == {"igpu", "npu", "cpu"}, probe
    assert kinds["igpu"]["name"].startswith("GPU") and kinds["igpu"]["vendor"] == "0x8086"
    assert kinds["npu"]["name"] == "NPU" and kinds["cpu"]["name"] == "CPU"
    assert re.fullmatch(r"[0-9a-f]{16}", kinds["igpu"]["luid"])
    for d in probe["ignored"]:
        assert d["reason"] in ("not_intel", "second_intel_gpu", "error")
        if "NVIDIA" in d["full_name"]:
            assert d["reason"] == "not_intel"
    print(json.dumps(probe, indent=1, ensure_ascii=False))


@pytest.mark.parametrize("device", ["GPU", "NPU"])
def test_transcribe_10s_sample(ov_python, probe, tmp_path, device):
    if not os.path.isfile(SAMPLE10):
        pytest.skip("sample/sample10.wav not found (copy a 10 s excerpt of a lecture there)")
    audio = tmp_path / "audio.wav"
    ffmpeg = shutil.which("ffmpeg")
    if ffmpeg:   # same conversion as the prep stage (SPEC §6.1)
        subprocess.run([ffmpeg, "-y", "-loglevel", "error", "-i", SAMPLE10, "-vn", "-ac", "1",
                        "-ar", "16000", "-c:a", "pcm_s16le", "-t", "10", str(audio)],
                       check=True, timeout=120)
    else:
        shutil.copyfile(SAMPLE10, audio)
    job = tmp_path / "job.json"
    job.write_text(json.dumps({
        "audio": str(audio), "model": MODEL, "precision": PRECISION,
        "model_dir": os.path.join(MODELS, "whisper-%s-%s" % (MODEL, PRECISION)),
        "device": device, "language": "en", "initial_prompt": "cache, pipeline, TLB",
        "cache_dir": os.path.join(MODELS, "cache", "%s-%s-%s" % (device, MODEL, PRECISION)),
        "hf_id": "openai/whisper-" + MODEL, "hf_home": os.path.join(MODELS, "hf_cache"),
        "word_timestamps": True, "min_free_bytes": 10 << 30}), encoding="utf-8")
    out = tmp_path / "result.json.tmp"

    p = subprocess.run([ov_python, WORKER, "--in", str(job), "--out", str(out)],
                       capture_output=True, cwd=str(tmp_path), timeout=3600)
    assert p.returncode == 0, p.stderr.decode("utf-8", "replace")[-2000:]
    lines = p.stdout.decode("utf-8").splitlines()
    progress = [l for l in lines if l.startswith("PROGRESS ")]
    assert progress[0] == "PROGRESS 0.00" and progress[-1] == "PROGRESS 1.00"
    assert all(re.fullmatch(r"PROGRESS (0\.\d\d|1\.00)", l) for l in progress)
    assert all(float(l[9:]) <= 0.95 for l in progress[:-1])
    assert progress == sorted(progress)
    # STATUS lines come before the first PROGRESS line
    first = lines.index("PROGRESS 0.00")
    assert all(i < first for i, l in enumerate(lines) if l.startswith("STATUS "))

    with open(out, encoding="utf-8") as f:
        result = json.load(f)
    assert result["prompt_terms_used"] == 3
    timing = result["timing"]
    assert timing["audio_s"] == pytest.approx(10.0, abs=0.1)
    assert timing["first_compile"] == ("STATUS compiling_model" in lines)
    sentences = result["sentences"]
    assert sentences, "no speech recognised in the 10 s sample"
    prev_start = 0
    for s in sentences:
        assert s["text"] and s["text"] == s["text"].strip()
        assert prev_start <= s["start_ms"] <= s["end_ms"] <= 10500
        prev_start = s["start_ms"]
        assert s["low_conf_words"] == []
        words = s["words"]
        assert words
        w_prev = s["start_ms"]
        for x in words:
            assert isinstance(x["s"], int) and isinstance(x["e"], int)
            assert w_prev <= x["s"] <= x["e"] <= s["end_ms"]
            w_prev = x["s"]
        assert "".join(x["w"] for x in words) == "".join(s["text"].split())
    print(device, timing)
    print("\n".join(s["text"] for s in sentences))
