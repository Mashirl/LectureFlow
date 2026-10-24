# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""Real-GPU integration tests (CLAUDE.md revision 2). Skipped unless run with -m realgpu:

    .\\make.ps1 realgpu        (or: .venv\\Scripts\\python.exe -m pytest -q -m realgpu worker)

They load real faster-whisper models (downloaded on first use). Model for the 10 s
sample test: env LF_REALGPU_MODEL (default large-v3); compute type: env
LF_REALGPU_COMPUTE_TYPE (default float16).
"""
import json
import os
import re
import shutil
import subprocess
import sys

import pytest

from conftest import PROJECT_DIR, WORKER_DIR

pytestmark = pytest.mark.realgpu

ASR = os.path.join(WORKER_DIR, "asr.py")
SAMPLE10 = os.path.join(PROJECT_DIR, "sample", "sample10.wav")
COMPUTE_TYPE = os.environ.get("LF_REALGPU_COMPUTE_TYPE", "float16")


def test_selftest_cuda():
    p = subprocess.run([sys.executable, ASR, "--selftest", "--device", "cuda",
                        "--compute-type", COMPUTE_TYPE],
                       capture_output=True, cwd=PROJECT_DIR, timeout=900)
    out = p.stdout.decode("utf-8", "replace")
    assert p.returncode == 0, p.stderr.decode("utf-8", "replace")[-2000:]
    assert re.search(r"^SELFTEST ok device=cuda", out, re.M), out


def test_transcribe_10s_sample(tmp_path):
    if not os.path.isfile(SAMPLE10):
        pytest.skip("sample/sample10.wav not found (copy a 10 s excerpt of a lecture there)")
    ffmpeg = shutil.which("ffmpeg")
    if not ffmpeg:
        pytest.skip("ffmpeg not on PATH")
    audio = tmp_path / "audio.wav"
    # Same conversion as the prep stage (SPEC §6.1).
    subprocess.run([ffmpeg, "-y", "-loglevel", "error", "-i", SAMPLE10, "-vn", "-ac", "1",
                    "-ar", "16000", "-c:a", "pcm_s16le", "-t", "10", str(audio)],
                   check=True, timeout=120)
    job = tmp_path / "job.json"
    job.write_text(json.dumps({
        "audio": str(audio), "model": os.environ.get("LF_REALGPU_MODEL", "large-v3"),
        "compute_type": COMPUTE_TYPE, "language": "en",
        "initial_prompt": "cache, pipeline, TLB", "device": "cuda"}), encoding="utf-8")
    out = tmp_path / "result.json.tmp"

    p = subprocess.run([sys.executable, ASR, "--in", str(job), "--out", str(out)],
                       capture_output=True, cwd=str(tmp_path), timeout=3600)
    assert p.returncode == 0, p.stderr.decode("utf-8", "replace")[-2000:]
    lines = p.stdout.decode("utf-8").splitlines()
    progress = [l for l in lines if l.startswith("PROGRESS ")]
    assert progress and progress[-1] == "PROGRESS 1.00"
    assert all(re.fullmatch(r"PROGRESS (0\.\d\d|1\.00)", l) for l in progress)

    with open(out, encoding="utf-8") as f:
        result = json.load(f)
    assert result["prompt_terms_used"] == 3
    sentences = result["sentences"]
    assert sentences, "no speech recognised in the 10 s sample"
    prev_start = 0
    for s in sentences:
        assert s["text"] and s["text"] == s["text"].strip()
        assert prev_start <= s["start_ms"] <= s["end_ms"] <= 10500
        prev_start = s["start_ms"]
        for lc in s["low_conf_words"]:
            assert lc["i"] >= 0 and lc["w"] in s["text"]
        # SPEC-v2 P4.2: word times inside the sentence, ordered, same words as the text.
        words = s["words"]
        assert words
        w_prev = s["start_ms"]
        for x in words:
            assert isinstance(x["s"], int) and isinstance(x["e"], int)
            assert w_prev <= x["s"] <= x["e"] <= s["end_ms"]
            w_prev = x["s"]
        assert "".join(x["w"] for x in words) == "".join(s["text"].split())
    print("\n".join(s["text"] for s in sentences))
