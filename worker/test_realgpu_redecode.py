# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""Real-GPU check of the second decode of degenerate windows (redecode, 2026-10-27).
Skipped unless run with -m realgpu (CLAUDE.md revision 2):

    .venv\\Scripts\\python.exe -m pytest -q -m realgpu worker -k redecode

sample/clip_Oct14.mp4 (5 minutes of a CSC3631 lecture whose large-v3 transcript has
windows all in capitals and windows without punctuation) is transcribed by the worker
as the app runs it, once with the switch off and once on: off, nothing is decoded again;
on, every degenerate window is decoded again, at least one is replaced, and fewer words
are left in unpunctuated runs. Model: env
LF_REALGPU_MODEL (default large-v3); compute type: LF_REALGPU_COMPUTE_TYPE (float16).
"""
import json
import os
import shutil
import subprocess
import sys

import pytest

from conftest import PROJECT_DIR, WORKER_DIR

pytestmark = pytest.mark.realgpu

CLIP = os.path.join(PROJECT_DIR, "sample", "clip_Oct14.mp4")
PROMPT = "cryptography, encryption, AES, DES, XOR, stream cipher, LFSR, one-time pad"


def unpunctuated_share(sentences):
    """Words in runs of 25 or more words without .,?!;: at the end of a word."""
    words = [w["w"] for s in sentences for w in s["words"]]
    run = inside = 0
    for w in words + ["."]:
        if w.rstrip("\"')]}").endswith(tuple(".,?!;:")):
            inside += run if run >= 25 else 0
            run = 0
        else:
            run += 1
    return inside / max(1, len(words))


def upper_sentences(sentences):
    n = 0
    for s in sentences:
        letters = [c for c in s["text"] if c.isalpha()]
        n += len(letters) >= 10 and sum(c.isupper() for c in letters) >= 0.8 * len(letters)
    return n


def run_worker(tmp_path, audio, redecode):
    job = tmp_path / ("job_%s.json" % redecode)
    job.write_text(json.dumps({
        "audio": str(audio), "model": os.environ.get("LF_REALGPU_MODEL", "large-v3"),
        "compute_type": os.environ.get("LF_REALGPU_COMPUTE_TYPE", "float16"),
        "language": "en", "initial_prompt": PROMPT, "device": "cuda", "timing": True,
        "redecode": redecode}), encoding="utf-8")
    out = tmp_path / ("result_%s.json" % redecode)
    p = subprocess.run([sys.executable, os.path.join(WORKER_DIR, "asr.py"), "--in", str(job),
                        "--out", str(out)], capture_output=True, cwd=str(tmp_path), timeout=1800)
    assert p.returncode == 0, p.stderr.decode("utf-8", "replace")[-2000:]
    with open(out, encoding="utf-8") as f:
        return json.load(f), p.stdout.decode("utf-8")


def test_redecode_on_a_lecture_clip(tmp_path):
    if not os.path.isfile(CLIP):
        pytest.skip("sample/clip_Oct14.mp4 not found")
    ffmpeg = shutil.which("ffmpeg")
    if not ffmpeg:
        pytest.skip("ffmpeg not on PATH")
    audio = tmp_path / "audio.wav"
    subprocess.run([ffmpeg, "-y", "-loglevel", "error", "-i", CLIP, "-vn", "-ac", "1", "-ar",
                    "16000", "-c:a", "pcm_s16le", str(audio)], check=True, timeout=300)

    off, _ = run_worker(tmp_path, audio, False)
    on, log = run_worker(tmp_path, audio, True)
    c_off = off["timing"]["postprocess"]["redecode"]
    c_on = on["timing"]["postprocess"]["redecode"]
    print(json.dumps({"off": c_off, "on": c_on}))
    print("unpunctuated %.2f -> %.2f, upper-case sentences %d -> %d"
          % (unpunctuated_share(off["sentences"]), unpunctuated_share(on["sentences"]),
             upper_sentences(off["sentences"]), upper_sentences(on["sentences"])))
    assert c_off["skipped"] is True and c_off["redecoded"] == 0
    assert (c_off["unpunctuated"], c_off["uppercase"]) == (c_on["unpunctuated"], c_on["uppercase"])
    degenerate = c_on["unpunctuated"] + c_on["uppercase"]
    # the clip degenerates with this prompt (2026-10-27: 2 degenerate windows; the lecture's
    # full 55-term prompt gave 8 in the research run)
    assert degenerate >= 1, c_on
    assert c_on["redecoded"] == degenerate and c_on["accepted"] >= 1, c_on
    assert upper_sentences(on["sentences"]) <= upper_sentences(off["sentences"])
    assert unpunctuated_share(on["sentences"]) < unpunctuated_share(off["sentences"])
    n_off = sum(len(s["words"]) for s in off["sentences"])
    n_on = sum(len(s["words"]) for s in on["sentences"])
    assert 0.9 * n_off <= n_on <= 1.1 * n_off
