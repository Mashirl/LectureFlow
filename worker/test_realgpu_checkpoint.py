# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""Real-device checkpoint tests (power management, v2-step20; CLAUDE.md revision 2).
Skipped unless run with -m realgpu:

    .venv\\Scripts\\python.exe -m pytest -q -s -m realgpu worker\\test_realgpu_checkpoint.py

Each test runs the real worker process on a real lecture excerpt: uninterrupted, then
killed after a checkpoint was written and resumed from it, and requires the resumed
result to be the uninterrupted one, word for word (faster-whisper: scheme B,
docs/benchmarks/power-checkpoint.md §3; OpenVINO: 300 s chunks, §2.7). Devices are used
one after another.

Audio: env LF_REALGPU_CKPT_AUDIO (default sample\\CSC1003-L02-01 Tue Sep 29
first-10min.mp4, converted with ffmpeg as the prep stage does; a .wav is used as it is).
faster-whisper: model LF_REALGPU_MODEL (default large-v3), compute type
LF_REALGPU_COMPUTE_TYPE (default float16), on CUDA. OpenVINO: interpreter
LF_REALGPU_OV_PYTHON (default .venv-ov\\Scripts\\python.exe), model LF_REALGPU_OV_MODEL
(default large-v3-turbo), precision LF_REALGPU_OV_PRECISION (default fp16), device
LF_REALGPU_OV_DEVICE (default GPU), chunk length LF_REALGPU_OV_CHUNK_S (default 300, the
product's), with the product's converted model under data/models/openvino (read only;
the compile cache goes to a temporary directory, so nothing is written under data/).
"""
import difflib
import json
import os
import shutil
import subprocess
import sys
import time

import pytest

from conftest import PROJECT_DIR, WORKER_DIR

pytestmark = pytest.mark.realgpu

AUDIO = os.environ.get("LF_REALGPU_CKPT_AUDIO", os.path.join(
    PROJECT_DIR, "sample", "CSC1003-L02-01 Tue Sep 29 first-10min.mp4"))
PROMPT = "cache, pipeline, TLB"
OV_PYTHON = os.environ.get("LF_REALGPU_OV_PYTHON",
                           os.path.join(PROJECT_DIR, ".venv-ov", "Scripts", "python.exe"))
OV_MODEL = os.environ.get("LF_REALGPU_OV_MODEL", "large-v3-turbo")
OV_PRECISION = os.environ.get("LF_REALGPU_OV_PRECISION", "fp16")
OV_DEVICE = os.environ.get("LF_REALGPU_OV_DEVICE", "GPU")
OV_CHUNK_S = int(os.environ.get("LF_REALGPU_OV_CHUNK_S", "300"))
OV_MODELS = os.path.join(PROJECT_DIR, "data", "models", "openvino")


@pytest.fixture(scope="module")
def audio(tmp_path_factory):
    if not os.path.isfile(AUDIO):
        pytest.skip("no lecture excerpt at %s" % AUDIO)
    if AUDIO.lower().endswith(".wav"):
        return AUDIO
    ffmpeg = shutil.which("ffmpeg")
    if not ffmpeg:
        pytest.skip("ffmpeg not on PATH")
    out = tmp_path_factory.mktemp("audio") / "audio.wav"
    # same conversion as the prep stage (SPEC §6.1)
    subprocess.run([ffmpeg, "-y", "-loglevel", "error", "-i", AUDIO, "-vn", "-ac", "1",
                    "-ar", "16000", "-c:a", "pcm_s16le", str(out)], check=True, timeout=600)
    return str(out)


def _checkpoint(path, device_id, model, precision, chunk_s):
    return {"path": str(path), "job_id": "j-asr-realgpu", "input_hash": "realgpu",
            "device_id": device_id, "model": model, "precision": precision,
            "chunk_s": chunk_s}


def _run(argv, cwd, timeout=3600):
    t0 = time.monotonic()
    p = subprocess.run(argv, capture_output=True, cwd=str(cwd), timeout=timeout)
    assert p.returncode == 0, p.stderr.decode("utf-8", "replace")[-3000:]
    print("  run in %s: %.1f s" % (os.path.basename(str(cwd)), time.monotonic() - t0))
    return p.stdout.decode("utf-8").splitlines()


def _read(path):
    with open(path, encoding="utf-8") as f:
        return json.load(f)


def _checkpoint_state(path):
    try:
        data = _read(path)
        return data["resume_ms"], data.get("resume_seek")
    except (OSError, ValueError, KeyError):
        return None                       # not there yet, or being replaced


def _run_until_checkpoint(argv, cwd, ckpt, nth, timeout=3600):
    """Starts the worker and kills it once the nth distinct checkpoint was seen; returns
    the checkpoint's (resume_ms, resume_seek)."""
    proc = subprocess.Popen(argv, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
                            cwd=str(cwd))
    seen = []
    deadline = time.monotonic() + timeout
    try:
        while time.monotonic() < deadline:
            state = _checkpoint_state(ckpt)
            if state is not None and (not seen or seen[-1] != state):
                seen.append(state)
                if len(seen) >= nth:
                    break
            if proc.poll() is not None:
                pytest.fail("the worker finished before checkpoint %d (exit %s, seen %s): %s"
                            % (nth, proc.returncode, seen,
                               proc.stderr.read().decode("utf-8", "replace")[-2000:]))
            time.sleep(0.02)
        else:
            pytest.fail("no checkpoint %d within %d s" % (nth, timeout))
    finally:
        if proc.poll() is None:
            proc.kill()
            proc.wait(timeout=30)
        proc.stderr.close()
    assert proc.returncode != 0, "killed after it had finished; use a longer excerpt"
    print("  killed at checkpoint %s (seen %s)" % (seen[-1], seen))
    return _checkpoint_state(ckpt)


def _diff(full, resumed):
    ta = [s["text"] for s in full["sentences"]]
    tb = [s["text"] for s in resumed["sentences"]]
    return "\n".join(difflib.unified_diff(ta, tb, "uninterrupted", "resumed", lineterm="", n=1))


@pytest.fixture(scope="module")
def fw_runs(audio, tmp_path_factory):
    """The faster-whisper job and its uninterrupted results (plain and checkpointed)."""
    model = os.environ.get("LF_REALGPU_MODEL", "large-v3")
    compute_type = os.environ.get("LF_REALGPU_COMPUTE_TYPE", "float16")
    root = tmp_path_factory.mktemp("fw")

    def job(name, checkpoint=True):
        d = root / name
        d.mkdir(exist_ok=True)
        data = {"audio": audio, "model": model, "compute_type": compute_type,
                "language": "en", "initial_prompt": PROMPT, "device": "cuda"}
        if checkpoint:
            data["checkpoint"] = _checkpoint(d / "asr.ckpt.json", "cuda:0", model, "auto", 0)
        (d / "job.json").write_text(json.dumps(data), encoding="utf-8")
        return [sys.executable, os.path.join(WORKER_DIR, "asr.py"), "--in",
                str(d / "job.json"), "--out", str(d / "result.json.tmp")], d

    argv, d = job("plain", checkpoint=False)
    _run(argv, d)
    plain = _read(d / "result.json.tmp")
    argv, d = job("full")
    lines = _run(argv, d)
    full = _read(d / "result.json.tmp")
    print("  uninterrupted: %d sentences; %s" % (
        len(full["sentences"]), [l for l in lines if l.startswith("checkpoints written")]))
    return {"job": job, "plain": plain, "full": full, "model": model}


def test_faster_whisper_checkpoint_object_changes_nothing(fw_runs):
    full = dict(fw_runs["full"])
    parts = full.pop("parts")
    assert parts == [{"from_ms": 0, "to_ms": parts[0]["to_ms"], "device_id": "cuda:0",
                      "model": fw_runs["model"], "precision": "auto"}]
    assert full == fw_runs["plain"], _diff(fw_runs["plain"], full)


@pytest.mark.parametrize("nth", [1, 2])
def test_faster_whisper_resumed_is_word_for_word_the_uninterrupted(fw_runs, nth):
    argv, d = fw_runs["job"]("cut%d" % nth)
    resume_ms, resume_seek = _run_until_checkpoint(argv, d, d / "asr.ckpt.json", nth)
    lines = _run(argv, d)
    assert any(l.startswith("resuming from checkpoint") for l in lines)
    assert any(l.startswith("VAD of the whole lecture") for l in lines)
    resumed = _read(d / "result.json.tmp")
    full = fw_runs["full"]
    print("  resumed at %d ms (seek %d): %d sentences" % (resume_ms, resume_seek,
                                                         len(resumed["sentences"])))
    assert resumed["prompt_terms_used"] == full["prompt_terms_used"]
    assert resumed["parts"] == full["parts"]                   # one part: the same device
    assert resumed["sentences"] == full["sentences"], _diff(full, resumed)


def test_openvino_300s_chunks_resumed_is_word_for_word_the_uninterrupted(audio, tmp_path):
    if not os.path.isfile(OV_PYTHON):
        pytest.skip("no OpenVINO interpreter at %s" % OV_PYTHON)
    worker = os.path.join(WORKER_DIR, "asr_openvino.py")
    model_dir = os.path.join(OV_MODELS, "whisper-%s-%s" % (OV_MODEL, OV_PRECISION))
    if not os.path.isfile(os.path.join(model_dir, "openvino_encoder_model.bin")):
        pytest.skip("no converted model at %s" % model_dir)
    cache = str(tmp_path / "ovcache")
    device_id = {"GPU": "ov:GPU", "NPU": "ov:NPU", "CPU": "ov:CPU"}[OV_DEVICE]

    def job(d):
        d.mkdir(exist_ok=True)
        (d / "job.json").write_text(json.dumps({
            "audio": audio, "model": OV_MODEL, "precision": OV_PRECISION,
            "model_dir": model_dir, "device": OV_DEVICE, "language": "en", "initial_prompt": PROMPT,
            "cache_dir": "" if OV_DEVICE == "CPU" else cache,
            "hf_id": "openai/whisper-" + OV_MODEL, "hf_home": os.path.join(OV_MODELS, "hf_cache"),
            "word_timestamps": True, "min_free_bytes": 10 << 30,
            "checkpoint": _checkpoint(d / "asr.ckpt.json", device_id, OV_MODEL, OV_PRECISION,
                                      OV_CHUNK_S)}), encoding="utf-8")
        return [OV_PYTHON, worker, "--in", str(d / "job.json"), "--out",
                str(d / "result.json.tmp")]

    full_dir = tmp_path / "full"
    lines = _run(job(full_dir), full_dir)
    full = _read(full_dir / "result.json.tmp")
    print("  uninterrupted: %d sentences; %s" % (
        len(full["sentences"]), [l for l in lines if "chunks" in l]))
    cut = tmp_path / "cut"
    argv = job(cut)
    resume_ms, _ = _run_until_checkpoint(argv, cut, cut / "asr.ckpt.json", 1)
    lines = _run(argv, cut)
    assert any(l.startswith("resuming from checkpoint") for l in lines)
    resumed = _read(cut / "result.json.tmp")
    print("  resumed at %d ms: %d sentences" % (resume_ms, len(resumed["sentences"])))
    # same cuts, same device, initial_prompt on the first chunk only in both runs
    assert resumed["prompt_terms_used"] == full["prompt_terms_used"]
    assert resumed["parts"] == full["parts"]
    assert resumed["sentences"] == full["sentences"], _diff(full, resumed)
