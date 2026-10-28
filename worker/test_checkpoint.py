# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""Checkpoints of both workers (power management, v2-step20; power brief §4): job.json
"checkpoint", asr.ckpt.json, result.json "parts", continuing after an interruption.

Fake models only (SPEC §14): faster-whisper is a scripted generator that answers by the
length of the audio it gets (a resumed run gets the audio cut at resume_ms), OpenVINO
the scripted fake of conftest.py that answers by where its audio lies in the lecture.
test_realgpu_checkpoint.py has the opt-in real-device comparisons.
"""
import json
import os
import subprocess
import sys
import time
from pathlib import Path
from types import SimpleNamespace

import numpy as np
import pytest

import asr
import asr_common
import asr_openvino as ov_worker
from asr_common import Word
from conftest import WORKER_DIR, script_read_wav, write_fake_ov_packages, write_wav

JOB_ID = "j-asr-0001"
INPUT_HASH = "v3:0123456789abcdef"


def read_json(path):
    with open(path, encoding="utf-8") as f:
        return json.load(f)


def progress_of(lines):
    return [l for l in lines if l.startswith("PROGRESS")]


def assert_monotonic(progress):
    values = [float(l.split()[1]) for l in progress]
    assert values == sorted(values), progress


def checkpoint_obj(path, device_id="cuda:0", model="large-v3", precision="auto", chunk_s=0,
                   **overrides):
    obj = {"path": str(path), "job_id": JOB_ID, "input_hash": INPUT_HASH,
           "device_id": device_id, "model": model, "precision": precision, "chunk_s": chunk_s}
    obj.update(overrides)
    return obj


# ===========================================================================
# asr_common: the checkpoint object, the file, parts
# ===========================================================================

def test_parse_checkpoint_job():
    assert asr_common.parse_checkpoint_job(None) is None
    cj = asr_common.parse_checkpoint_job(checkpoint_obj("C:\\l\\asr.ckpt.json", chunk_s=30))
    assert cj == asr_common.CheckpointJob("C:\\l\\asr.ckpt.json", JOB_ID, INPUT_HASH, "cuda:0",
                                          "large-v3", "auto", 30)
    # chunk_s and the descriptive fields may be missing (faster-whisper ignores chunk_s)
    cj = asr_common.parse_checkpoint_job({"path": "p", "job_id": "j", "input_hash": "h"})
    assert cj == asr_common.CheckpointJob("p", "j", "h", "", "", "", 0)


@pytest.mark.parametrize("bad,needle", [
    ([], "job.checkpoint must be an object"),
    ({"job_id": "j", "input_hash": "h"}, "checkpoint.path"),
    ({"path": "p", "job_id": "", "input_hash": "h"}, "checkpoint.job_id"),
    ({"path": "p", "job_id": "j", "input_hash": 5}, "checkpoint.input_hash"),
    ({"path": "p", "job_id": "j", "input_hash": "h", "device_id": 1}, "checkpoint.device_id"),
    ({"path": "p", "job_id": "j", "input_hash": "h", "chunk_s": -1}, "checkpoint.chunk_s"),
    ({"path": "p", "job_id": "j", "input_hash": "h", "chunk_s": True}, "checkpoint.chunk_s"),
    ({"path": "p", "job_id": "j", "input_hash": "h", "chunk_s": 30.0}, "checkpoint.chunk_s"),
])
def test_parse_checkpoint_job_errors(bad, needle):
    with pytest.raises(asr_common.InputError, match=needle.replace(".", r"\.")):
        asr_common.parse_checkpoint_job(bad)


def test_merge_part():
    a = {"from_ms": 0, "to_ms": 1000, "device_id": "ov:GPU", "model": "m", "precision": "fp16"}
    same = dict(a, from_ms=1000, to_ms=3000)
    other = dict(same, device_id="ov:NPU")
    assert asr_common.merge_part([a], same) == [dict(a, to_ms=3000)]
    assert asr_common.merge_part([a], other) == [a, other]
    assert asr_common.merge_part([a], dict(same, precision="int8"))[1]["precision"] == "int8"
    assert asr_common.merge_part([a], dict(same, from_ms=1500)) == [a, dict(same, from_ms=1500)]
    assert asr_common.merge_part([], same) == [same]
    assert a["to_ms"] == 1000                     # copies: the input is not changed


ENGINE = "faster-whisper 9.9.9-test"
VAD = "VadOptions(threshold=0.5)"


def _checkpointer(tmp_path, backend=asr_common.BACKEND_FASTER_WHISPER, engine=ENGINE, vad=VAD,
                  **kw):
    lines = []
    cj = asr_common.parse_checkpoint_job(checkpoint_obj(tmp_path / "asr.ckpt.json", **kw))
    if backend != asr_common.BACKEND_FASTER_WHISPER:
        engine = vad = ""
    return asr_common.Checkpointer(cj, backend, lines.append, engine, vad), lines


def _words():
    return [Word(" Hello", 0.5, 0.9, 0.93), Word(" there.", 0.9, 1.62, 0.41)]


def _fw_segment(start_ms, end_ms, words, seek=0, no_speech=0.05, logprob=-0.2):
    return asr_common.encode_segment(start_ms, end_ms, words, seek, no_speech, logprob)


def test_save_then_load_round_trips_exactly(tmp_path):
    ck, lines = _checkpointer(tmp_path)
    words = _words() + [Word(" x", 1.0 / 3.0, 0.1 + 0.2, 1.0)]     # floats survive JSON
    segs = [_fw_segment(500, 1620, words[:2]),
            _fw_segment(1700, 1800, words[2:], seek=0, no_speech=0.92, logprob=-1.4)]
    parts = ck.parts(None, 1800)
    assert ck.save(1800, 3, parts, segs, resume_seek=2950) is True
    data = read_json(ck.path)
    assert data == {
        "version": 2, "job_id": JOB_ID, "input_hash": INPUT_HASH, "backend": "faster-whisper",
        "engine": ENGINE, "vad": VAD, "resume_ms": 1800, "resume_seek": 2950,
        "prompt_terms_used": 3,
        "parts": [{"from_ms": 0, "to_ms": 1800, "device_id": "cuda:0", "model": "large-v3",
                   "precision": "auto"}],
        "segments": [
            {"start_ms": 500, "end_ms": 1620, "seek": 0, "no_speech_prob": 0.05,
             "avg_logprob": -0.2, "words": [
                 {"word": " Hello", "start": 0.5, "end": 0.9, "probability": 0.93},
                 {"word": " there.", "start": 0.9, "end": 1.62, "probability": 0.41}]},
            {"start_ms": 1700, "end_ms": 1800, "seek": 0, "no_speech_prob": 0.92,
             "avg_logprob": -1.4, "words": [
                 {"word": " x", "start": 1.0 / 3.0, "end": 0.1 + 0.2, "probability": 1.0}]}]}
    assert not os.path.exists(ck.tmp)
    resume = ck.load(10000)
    assert resume.resume_ms == 1800 and resume.resume_seek == 2950
    assert resume.prompt_terms_used == 3
    assert resume.words == words                  # bit for bit
    assert resume.segments == segs and resume.parts == parts
    assert resume.raw[1] == asr_common.CheckpointSegment(1700, 1800, 0, 0.92, -1.4, words[2:])
    # the raw segments go through the hallucination filter like faster-whisper's
    assert [asr.keep_segment(s) for s in resume.raw] == [True, False]
    assert any(l.startswith("resuming from checkpoint asr.ckpt.json at 0:01 (seek 2950)")
               for l in lines)
    # this run's part continues the last one; another device makes a second part
    assert ck.parts(resume, 9000) == [dict(parts[0], to_ms=9000)]
    ck2, _ = _checkpointer(tmp_path, device_id="fw:cpu")
    assert ck2.parts(resume, 9000) == [parts[0], {"from_ms": 1800, "to_ms": 9000,
                                                  "device_id": "fw:cpu", "model": "large-v3",
                                                  "precision": "auto"}]


def test_openvino_file_has_no_faster_whisper_fields(tmp_path):
    ck, _ = _checkpointer(tmp_path, backend=asr_common.BACKEND_OPENVINO)
    ck.save(300000, 0, ck.parts(None, 300000), [asr_common.encode_segment(0, 300000, _words())])
    data = read_json(ck.path)
    assert set(data) == {"version", "job_id", "input_hash", "backend", "resume_ms",
                         "prompt_terms_used", "parts", "segments"}
    assert set(data["segments"][0]) == {"start_ms", "end_ms", "words"}
    resume = ck.load(600000)
    assert resume.resume_seek is None and resume.raw[0].seek is None


def test_load_without_file_and_stale_tmp_is_removed(tmp_path):
    ck, lines = _checkpointer(tmp_path)
    assert ck.load(1000) is None and lines == []
    with open(ck.tmp, "w", encoding="utf-8") as f:
        f.write('{"version": 2, "trunc')               # a killed write
    assert ck.load(1000) is None
    assert not os.path.exists(ck.tmp)


def _valid_file(tmp_path, **changes):
    ck, _ = _checkpointer(tmp_path)
    ck.save(1800, 3, ck.parts(None, 1800), [_fw_segment(500, 1620, _words())], resume_seek=180)
    data = read_json(ck.path)
    for k, v in changes.items():
        if v is _DELETE:
            del data[k]
        else:
            data[k] = v
    with open(ck.path, "w", encoding="utf-8") as f:
        json.dump(data, f)
    return ck.path


_DELETE = object()


def _seg(**changes):
    s = {"start_ms": 0, "end_ms": 1, "seek": 0, "no_speech_prob": 0.1, "avg_logprob": -0.2,
         "words": [{"word": "x", "start": 0, "end": 1, "probability": 1}]}
    for k, v in changes.items():
        if v is _DELETE:
            del s[k]
        else:
            s[k] = v
    return s


@pytest.mark.parametrize("changes,reason", [
    ({"version": 1}, "version 1, expected 2"),             # the format before scheme B
    ({"version": 3}, "version 3"),
    ({"version": True}, "version True"),
    ({"version": _DELETE}, "version None"),
    ({"job_id": "j-asr-other"}, "job_id 'j-asr-other'"),
    ({"input_hash": "v3:changed"}, "input_hash differs"),
    ({"backend": "openvino"}, "backend 'openvino'"),
    ({"engine": "faster-whisper 1.3.0"}, "engine 'faster-whisper 1.3.0'"),
    ({"engine": _DELETE}, "engine ''"),
    ({"vad": "VadOptions(threshold=0.6)"}, "vad 'VadOptions(threshold=0.6)'"),
    ({"resume_ms": 0}, "resume_ms 0"),
    ({"resume_ms": 10001}, "resume_ms 10001 outside the audio (10000 ms)"),
    ({"resume_ms": 1800.0}, "resume_ms 1800.0"),
    ({"resume_seek": _DELETE}, "resume_seek None"),
    ({"resume_seek": 0}, "resume_seek 0"),
    ({"resume_seek": 180.0}, "resume_seek 180.0"),
    ({"prompt_terms_used": -1}, "prompt_terms_used"),
    ({"parts": []}, "malformed parts"),
    ({"parts": [{"from_ms": 0, "to_ms": "1800"}]}, "malformed parts"),
    ({"segments": {}}, "malformed segments"),
    ({"segments": []}, "no segments"),
    ({"segments": [_seg(words=[{"word": "x", "start": "0"}])]}, "malformed segments"),
    ({"segments": [_seg(words=[{"word": "x", "start": 0, "end": float("nan"),
                                "probability": 1}])]}, "malformed segments"),
    ({"segments": [_seg(seek=_DELETE)]}, "malformed segments"),
    ({"segments": [_seg(seek=-1)]}, "malformed segments"),
    ({"segments": [_seg(no_speech_prob=_DELETE)]}, "malformed segments"),
    ({"segments": [_seg(avg_logprob="x")]}, "malformed segments"),
])
def test_checkpoint_that_does_not_match_is_deleted(tmp_path, changes, reason):
    path = _valid_file(tmp_path, **changes)
    ck, lines = _checkpointer(tmp_path)
    assert ck.load(10000) is None
    assert not os.path.exists(path)
    (line,) = lines
    assert line.startswith("checkpoint asr.ckpt.json not used (") and reason in line, line
    assert line.endswith("; starting over")


def test_valid_file_loads(tmp_path):
    _valid_file(tmp_path)
    ck, _ = _checkpointer(tmp_path)
    assert ck.load(10000).resume_seek == 180


def test_unreadable_checkpoint_is_deleted(tmp_path):
    ck, lines = _checkpointer(tmp_path)
    with open(ck.path, "w", encoding="utf-8") as f:
        f.write("{not json")
    assert ck.load(10000) is None and not os.path.exists(ck.path)
    assert "unreadable" in lines[0]
    with open(ck.path, "w", encoding="utf-8") as f:
        f.write("[1, 2]")
    assert ck.load(10000) is None and "not a JSON object" in lines[1]


def test_save_writes_tmp_then_replaces(tmp_path, monkeypatch):
    ck, _ = _checkpointer(tmp_path)
    ck.save(1000, 0, ck.parts(None, 1000), [], resume_seek=100)
    seen = []
    real_replace = os.replace

    def replace(src, dst):
        seen.append((src, dst, read_json(src)["resume_ms"], read_json(dst)["resume_ms"]))
        real_replace(src, dst)

    monkeypatch.setattr(asr_common.os, "replace", replace)
    ck.save(2000, 0, ck.parts(None, 2000), [], resume_seek=200)
    # the complete new file sits in .tmp; the old one is untouched until the rename
    assert seen == [(ck.tmp, ck.path, 2000, 1000)]
    assert read_json(ck.path)["resume_ms"] == 2000 and not os.path.exists(ck.tmp)


def test_failed_save_is_logged_and_not_fatal(tmp_path, monkeypatch):
    ck, lines = _checkpointer(tmp_path)

    def replace(src, dst):
        raise PermissionError(13, "Access is denied")

    monkeypatch.setattr(asr_common.os, "replace", replace)
    assert ck.save(1000, 0, ck.parts(None, 1000), [], resume_seek=100) is False
    assert lines[-1].startswith("checkpoint not written (PermissionError") and \
        lines[-1].endswith("the run goes on")
    assert not os.path.exists(ck.tmp) and not os.path.exists(ck.path)
    assert ck.writes == 0


def test_go_side_matches():
    """devices.CheckpointVersion, the file's JSON names, OVCheckpointChunkS = 300 (lead,
    after docs/benchmarks/power-checkpoint.md), model.Backend*."""
    from conftest import PROJECT_DIR
    with open(os.path.join(PROJECT_DIR, "internal", "devices", "checkpoint.go"),
              encoding="utf-8") as f:
        go = f.read()
    assert "const CheckpointVersion = %d" % asr_common.CHECKPOINT_VERSION in go
    assert "const OVCheckpointChunkS = 300" in go
    for field in ("path", "job_id", "input_hash", "device_id", "model", "precision", "chunk_s",
                  "version", "backend", "resume_ms", "prompt_terms_used", "parts", "segments",
                  "start_ms", "end_ms", "words", "word", "start", "end", "probability",
                  "engine,omitempty", "vad,omitempty", "resume_seek,omitempty",
                  "seek,omitempty", "no_speech_prob,omitempty", "avg_logprob,omitempty"):
        assert 'json:"%s"' % field in go, field
    with open(os.path.join(PROJECT_DIR, "internal", "model", "v2_intel.go"), encoding="utf-8") as f:
        model_go = f.read()
    assert 'BackendFasterWhisper = "%s"' % asr_common.BACKEND_FASTER_WHISPER in model_go
    assert 'BackendOpenVINO      = "%s"' % asr_common.BACKEND_OPENVINO in model_go
    with open(os.path.join(PROJECT_DIR, "internal", "model", "v2_power.go"), encoding="utf-8") as f:
        power_go = f.read()
    for key in ("from_ms", "to_ms", "device_id", "model", "precision"):
        assert 'json:"%s"' % key in power_go


def test_engine_names_the_installed_faster_whisper():
    import faster_whisper
    from faster_whisper.vad import VadOptions
    engine, vad = asr.checkpoint_engine()
    assert engine == "faster-whisper %s" % faster_whisper.__version__
    assert vad == repr(VadOptions()) and vad.startswith("VadOptions(threshold=0.5")
    assert asr.CHECKPOINT_INTERVAL_S == 10


# ===========================================================================
# faster-whisper
# ===========================================================================

TOTAL_S = 150.0
TOTAL_SAMPLES = int(TOTAL_S * 16000)

# The fake faster-whisper, shared by the in-process fake model and the fake faster_whisper
# package of the worker processes (exec'd in both). Speech chunks: every script segment
# padded by 0.3 s. The speech-only time line leaves out the audio between them; windows
# are 30 s of it (seek = 3000 x the window number), and a run decodes from clip_seek on.
FW_FAKE_SRC = r'''
import bisect as _bisect
from types import SimpleNamespace as _NS


def fw_speech_chunks(script, pad=0.3, sr=16000):
    """The VAD's speech chunks: [{"start", "end"}] in samples."""
    return [{"start": int(round((s - pad) * sr)), "end": int(round((e + pad) * sr))}
            for s, e, _, _, _ in script]


def _silence_before(chunks, sr=16000):
    out, prev, silent = [], 0, 0
    for c in chunks:
        silent += c["start"] - prev
        prev = c["end"]
        out.append(silent / float(sr))
    return out


def fw_compact_segments(script, chunks, clip_seek=0, window_s=30.0, sr=16000):
    """The script's segments [(start, end, [(word, s, e, p)], no_speech_prob, avg_logprob)]
    as faster-whisper decodes the speech-only audio from mel frame clip_seek on: times on
    the speech-only time line, seek = the first frame of their window there."""
    before = _silence_before(chunks, sr)
    out = []
    for start, end, words, no_speech, logprob in script:
        j = next(i for i, c in enumerate(chunks) if c["start"] <= start * sr <= c["end"])
        off = before[j]
        seek = int((start - off) // window_s) * int(window_s * 100)
        if seek < clip_seek:
            continue
        out.append(_NS(seek=seek, start=start - off, end=end - off,
                       text="".join(w for w, _, _, _ in words), no_speech_prob=no_speech,
                       avg_logprob=logprob,
                       words=[_NS(word=w, start=s - off, end=e - off, probability=p)
                              for w, s, e, p in words]))
    return out


def fw_restore(segments, chunks, sampling_rate=16000):
    """faster_whisper.transcribe.restore_speech_timestamps (the fake package's copy)."""
    ends, before, prev, silent = [], [], 0, 0
    for c in chunks:
        silent += c["start"] - prev
        prev = c["end"]
        ends.append(c["end"] - silent)
        before.append(silent / sampling_rate)

    def index(t):
        return min(_bisect.bisect(ends, int(t * sampling_rate)), len(ends) - 1)

    for seg in segments:
        if seg.words:
            for w in seg.words:
                j = index((w.start + w.end) / 2)
                w.start, w.end = round(before[j] + w.start, 2), round(before[j] + w.end, 2)
            seg.start, seg.end = seg.words[0].start, seg.words[-1].end
        else:
            seg.start = round(before[index(seg.start)] + seg.start, 2)
            seg.end = round(before[index(seg.end)] + seg.end, 2)
        yield seg
'''
exec(FW_FAKE_SRC)


def fw_script():
    """A 150 s lecture: 31 segments of 2.5 s every 4.7 s, three words each (a sentence
    ends every third segment), one hallucination on silence (segment 7), and "so so" +
    "so ..." repeated across segments 19 / 20, the end of the second window."""
    script = []
    for i in range(31):
        t = round(1.0 + 4.7 * i, 2)
        end = round(t + 2.5, 2)
        if i == 19:
            words = [(" so", round(t + 0.2, 2), round(t + 1.0, 2), 0.9),
                     (" so", round(t + 1.2, 2), round(t + 2.4, 2), 0.9)]
        elif i == 20:
            words = [(" so", round(t + 0.1, 2), round(t + 0.8, 2), 0.9),
                     (" next", round(t + 0.9, 2), round(t + 1.6, 2), 0.3),
                     (" part.", round(t + 1.7, 2), end, 0.95)]
        else:
            last = " w%dc." % i if i % 3 == 2 else " w%dc" % i
            words = [(" w%da" % i, round(t + 0.1, 2), round(t + 0.8, 2), 0.9),
                     (" w%db" % i, round(t + 0.9, 2), round(t + 1.6, 2), 0.42 if i == 5 else 0.9),
                     (last, round(t + 1.7, 2), end, 0.95)]
        if i == 7:
            script.append((t, end, [(" Subtitles", t, round(t + 1.0, 2), 0.2),
                                    (" by", round(t + 1.0, 2), round(t + 1.5, 2), 0.2),
                                    (" Amara.", round(t + 1.5, 2), end, 0.2)], 0.92, -1.4))
        else:
            script.append((t, end, words, 0.05, -0.2))
    return script


SCRIPT = fw_script()
CHUNKS = fw_speech_chunks(SCRIPT)
SPEECH_SAMPLES = sum(c["end"] - c["start"] for c in CHUNKS)


def _restored(segments):
    from faster_whisper.transcribe import restore_speech_timestamps
    return list(restore_speech_timestamps(segments, CHUNKS, 16000))


SEGS = _restored(fw_compact_segments(SCRIPT, CHUNKS))       # the uninterrupted run's
WINDOW_STARTS = [i for i, s in enumerate(SEGS) if i == 0 or s.seek != SEGS[i - 1].seek]


def _ms(t):
    return asr_common._to_ms(t)


# the end of the last segment of the first three windows (lecture timeline; a restored
# segment ends with its last word: segment 19 at 92.7 s)
END_W = [_ms(SEGS[9].end), _ms(SEGS[19].end), _ms(SEGS[28].end)]


def test_script_windows():
    """3.1 s of speech per segment: windows begin at segments 0, 10, 20 and 29; restoring
    the speech-only times gives the script's word times back (a segment then spans its
    words, as restore_speech_timestamps makes it)."""
    assert WINDOW_STARTS == [0, 10, 20, 29]
    assert [SEGS[i].seek for i in WINDOW_STARTS] == [0, 3000, 6000, 9000]
    assert END_W == [45800, 92700, 135100]
    assert [(s.start, s.end) for s in SEGS] == [(ws[0][1], ws[-1][2]) for _, _, ws, _, _ in SCRIPT]
    assert [w.start for s in SEGS for w in s.words] == \
        [w[1] for _, _, ws, _, _ in SCRIPT for w in ws]


class _Tok:
    def encode(self, text, add_special_tokens=True):
        return SimpleNamespace(ids=text.split())


class FakeFW:
    """faster-whisper shaped. transcribe(audio, vad_filter=True): the whole lecture,
    decoded on the speech-only time line and mapped back (as faster-whisper does);
    transcribe(speech-only audio, vad_filter=False[, clip_timestamps=[seek / 100]]): the
    windows from that seek (from the start without clip_timestamps), times on the
    speech-only line; asr.py makes every call this way since it runs the VAD itself
    (textrepair-int). fail_at = i raises when segment i of the run is requested (a
    killed worker); on_segment(i) runs before segment i is yielded."""

    def __init__(self, script=SCRIPT, fail_at=None, on_segment=None):
        self.script, self.fail_at, self.on_segment = script, fail_at, on_segment
        self.hf_tokenizer = _Tok()
        self.calls = []

    def transcribe(self, audio, **kwargs):
        self.calls.append(dict(kwargs, samples=len(audio)))
        if kwargs["vad_filter"]:
            assert len(audio) == TOTAL_SAMPLES
            segs = _restored(fw_compact_segments(self.script, CHUNKS))
        else:
            assert len(audio) == SPEECH_SAMPLES          # the speech-only audio
            (clip,) = kwargs.get("clip_timestamps", [0.0])
            segs = fw_compact_segments(self.script, CHUNKS, int(round(clip * 100)))

        def gen():
            for i, s in enumerate(segs):
                if self.fail_at is not None and i == self.fail_at:
                    raise RuntimeError("worker killed (fake)")
                if self.on_segment:
                    self.on_segment(i)
                yield s

        return gen(), SimpleNamespace(duration=len(audio) / 16000.0, language="en")


@pytest.fixture(scope="module")
def lecture_wav(tmp_path_factory):
    return write_wav(tmp_path_factory.mktemp("fw") / "audio.wav", TOTAL_S)


@pytest.fixture
def fw(tmp_path, monkeypatch, lecture_wav):
    """run(model, checkpoint=..., **job) runs asr.run in-process with a fake model (and the
    fake's speech chunks as the VAD's) and returns (result, log lines). Checkpoints are
    written after every complete window (interval 0) unless a test sets it."""
    import faster_whisper.vad
    state = SimpleNamespace(model=None, ckpt=str(tmp_path / "asr.ckpt.json"), vad_calls=[])
    monkeypatch.setattr(asr, "load_model", lambda *a: state.model)
    monkeypatch.setattr(asr, "setup_cuda_libs", lambda: [])
    monkeypatch.setattr(asr, "CHECKPOINT_INTERVAL_S", 0.0)

    def vad(audio, options=None, **kw):
        state.vad_calls.append((len(audio), options))
        return [dict(c) for c in CHUNKS]

    monkeypatch.setattr(faster_whisper.vad, "get_speech_timestamps", vad)

    def run(model, checkpoint=None, **overrides):
        state.model = model
        data = {"audio": lecture_wav, "model": "large-v3", "compute_type": "float16",
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


def expected_fw_sentences():
    words = [asr_common.as_word(w) for s in SEGS if asr.keep_segment(s) for w in s.words]
    return asr_common.split_sentences(asr_common.remove_repeats(words))


def test_fw_result_unchanged_by_the_checkpoint_object(fw):
    r0, lines0 = fw.run(FakeFW())
    assert not os.path.exists(fw.ckpt)
    model = FakeFW()
    r1, lines1 = fw.run(model, checkpoint_obj(fw.ckpt))
    # the uninterrupted run is the same call as without checkpoints: the worker's VAD of
    # the whole lecture (once per run, textrepair-int), its speech-only audio decoded
    # from the start with the prompt
    (call,) = model.calls
    assert call["vad_filter"] is False and "clip_timestamps" not in call
    assert call["samples"] == SPEECH_SAMPLES
    assert call["initial_prompt"] == "cache, write-back, TLB"
    from faster_whisper.vad import VadOptions
    assert fw.vad_calls == [(TOTAL_SAMPLES, VadOptions())] * 2
    t0, t1 = r0.pop("timing"), r1.pop("timing")
    assert t0["audio_s"] == t1["audio_s"] == TOTAL_S
    parts = r1.pop("parts")
    assert r1 == r0                                            # same sentences, same terms
    assert r0["sentences"] == expected_fw_sentences()
    assert parts == [{"from_ms": 0, "to_ms": 150000, "device_id": "cuda:0", "model": "large-v3",
                      "precision": "auto"}]
    assert progress_of(lines1) == progress_of(lines0)
    # repeats across segments 19 / 20 collapsed, the hallucination dropped
    texts = [s["text"] for s in r0["sentences"]]
    assert "w18a w18b w18c so next part." in texts
    assert not any("Amara" in t for t in texts)


def test_fw_segments_without_seek_write_no_checkpoint(fw):
    """Segments without seek (not faster-whisper's) give no resume point."""
    class Plain(FakeFW):
        def transcribe(self, audio, **kwargs):
            gen, info = FakeFW.transcribe(self, audio, **kwargs)
            return (SimpleNamespace(**{k: v for k, v in vars(s).items() if k != "seek"})
                    for s in gen), info

    r0, _ = fw.run(Plain())
    r1, lines = fw.run(Plain(), checkpoint_obj(fw.ckpt))
    r1.pop("parts")
    r0.pop("timing"), r1.pop("timing")
    assert r1 == r0
    assert not os.path.exists(fw.ckpt)
    assert "segments without seek: no checkpoints in this run" in lines
    assert "checkpoints written: 0" in lines


def test_fw_checkpoint_after_every_complete_window(fw):
    seen = {}

    def on_segment(i):
        seen[i] = (read_json(fw.ckpt)["resume_ms"], read_json(fw.ckpt)["resume_seek"]) \
            if os.path.exists(fw.ckpt) else None

    r, lines = fw.run(FakeFW(on_segment=on_segment), checkpoint_obj(fw.ckpt))
    # window 0 is complete once segment 10 arrived (seen before segment 11 is yielded)
    assert all(seen[i] is None for i in range(11))
    assert all(seen[i] == (END_W[0], 3000) for i in range(11, 21))
    assert all(seen[i] == (END_W[1], 6000) for i in range(21, 30))
    assert seen[30] == (END_W[2], 9000)
    data = read_json(fw.ckpt)            # the last one: after window 2 (window 3 = the end)
    assert data["version"] == 2 and data["job_id"] == JOB_ID
    assert data["input_hash"] == INPUT_HASH and data["backend"] == "faster-whisper"
    assert (data["engine"], data["vad"]) == asr.checkpoint_engine()
    assert data["resume_ms"] == END_W[2] and data["resume_seek"] == 9000
    # seek counts speech-only frames: window 3 starts at 90 s of speech, at 137.3 s here
    assert data["resume_seek"] * 10 != data["resume_ms"]
    assert data["prompt_terms_used"] == 3
    assert data["parts"] == [{"from_ms": 0, "to_ms": END_W[2], "device_id": "cuda:0",
                              "model": "large-v3", "precision": "auto"}]
    # all segments of windows 0..2, raw: the hallucination too, with its probabilities
    assert len(data["segments"]) == 29
    assert data["segments"][0] == {
        "start_ms": 1100, "end_ms": 3500, "seek": 0, "no_speech_prob": 0.05, "avg_logprob": -0.2,
        "words": [{"word": w.word, "start": w.start, "end": w.end, "probability": w.probability}
                  for w in SEGS[0].words]}
    assert data["segments"][7]["no_speech_prob"] == 0.92
    assert data["segments"][7]["avg_logprob"] == -1.4
    assert [w["word"] for w in data["segments"][7]["words"]] == [" Subtitles", " by", " Amara."]
    assert [s["seek"] for s in data["segments"]] == [s.seek for s in SEGS[:29]]
    assert not os.path.exists(fw.ckpt + ".tmp")
    assert "checkpoints written: 3" in lines
    assert any(l.startswith("checkpoint asr.ckpt.json written at 0:45") for l in lines)


def test_fw_checkpoints_at_most_every_interval_and_the_last_window_written(fw, monkeypatch):
    """With the interval: a window 5 s after the last write waits; the last complete
    window is written when the transcription ends."""
    now = [0.0]
    monkeypatch.setattr(asr, "CHECKPOINT_INTERVAL_S", 10.0)
    monkeypatch.setattr(asr, "_monotonic", lambda: now[0])
    seen = []

    def on_segment(i):
        now[0] = 0.5 * i                 # windows complete at 5 s, 10 s and 14.5 s
        seen.append(read_json(fw.ckpt)["resume_ms"] if os.path.exists(fw.ckpt) else None)

    r, lines = fw.run(FakeFW(on_segment=on_segment), checkpoint_obj(fw.ckpt))
    # written at 5 s (the first), not at 10 s or 14.5 s; then at the end
    assert sorted(set(v for v in seen if v)) == [END_W[0]]
    data = read_json(fw.ckpt)
    assert data["resume_ms"] == END_W[2] and data["resume_seek"] == 9000
    assert len(data["segments"]) == 29
    assert "checkpoints written: 2" in lines
    assert r["sentences"] == expected_fw_sentences()


def _interrupt(fw, fail_at, **ck):
    with pytest.raises(RuntimeError, match="worker killed"):
        fw.run(FakeFW(fail_at=fail_at), checkpoint_obj(fw.ckpt, **ck))
    assert not os.path.exists(str(fw.ckpt).replace("asr.ckpt.json", "result.json.tmp"))


def test_fw_interrupted_then_resumed_matches_the_uninterrupted_run(fw):
    r0, _ = fw.run(FakeFW())
    _interrupt(fw, 22)                        # killed in window 2: window 1 is checkpointed
    assert read_json(fw.ckpt)["resume_seek"] == 6000
    model = FakeFW()
    r2, lines = fw.run(model, checkpoint_obj(fw.ckpt, device_id="fw:cpu", precision="int8"))
    assert r2["sentences"] == r0["sentences"]                 # word for word, every time
    assert r2["prompt_terms_used"] == 3                       # the first part's prompt
    assert r2["parts"] == [
        {"from_ms": 0, "to_ms": END_W[1], "device_id": "cuda:0", "model": "large-v3",
         "precision": "auto"},
        {"from_ms": END_W[1], "to_ms": 150000, "device_id": "fw:cpu", "model": "large-v3",
         "precision": "int8"}]
    # scheme B: the VAD of the whole lecture (default options), its speech-only audio
    # decoded from the checkpoint's seek, without initial_prompt and without VAD of its own
    # (one VAD per run: the uninterrupted, the interrupted and the resumed one)
    from faster_whisper.vad import VadOptions
    assert fw.vad_calls == [(TOTAL_SAMPLES, VadOptions())] * 3
    (call,) = model.calls
    assert call["samples"] == SPEECH_SAMPLES
    assert call["clip_timestamps"] == [60.0] and call["initial_prompt"] is None
    assert call["vad_filter"] is False
    assert {k: call[k] for k in asr.TRANSCRIBE_KWARGS} == asr.RESUME_KWARGS
    assert call["language"] == "en"
    # timing covers this run's audio only (RTF of this run)
    assert r2["timing"]["audio_s"] == round((150000 - END_W[1]) / 1000.0, 2)
    progress = progress_of(lines)
    assert progress[0] == "PROGRESS 0.62" == asr_common.format_progress(END_W[1] / 150000.0)
    assert progress[-1] == "PROGRESS 1.00"
    assert_monotonic(progress)
    assert any(l.startswith("resuming from checkpoint asr.ckpt.json at 1:32 (seek 6000)")
               for l in lines)
    assert "continuing at 1:32 (seek 6000) without initial_prompt (first window only)" in lines


def test_fw_resumed_words_keep_their_times_and_probabilities(fw):
    _interrupt(fw, 22)
    r, _ = fw.run(FakeFW(), checkpoint_obj(fw.ckpt))
    last = r["sentences"][-1]
    assert last["text"] == "w30a w30b w30c"
    t = 1.0 + 4.7 * 30
    assert last["words"][0] == {"w": "w30a", "s": _ms(t + 0.1), "e": _ms(t + 0.8)}
    assert last["end_ms"] == _ms(t + 2.5) == 144500
    # low-confidence words: from the checkpoint (w5b) and from the resumed part (next)
    assert any(lc["w"] == "w5b" for s in r["sentences"] for lc in s["low_conf_words"])
    assert any(lc["w"] == "next" for s in r["sentences"] for lc in s["low_conf_words"])
    # the hallucination in the checkpoint's raw segments is still filtered out
    assert not any("Amara" in s["text"] for s in r["sentences"])


def test_fw_interrupted_twice_on_the_same_device_is_one_part(fw):
    r0, _ = fw.run(FakeFW())
    _interrupt(fw, 12)                       # window 0 checkpointed
    assert read_json(fw.ckpt)["resume_seek"] == 3000
    # the resumed run starts at window 1 (segment 10); killed in its window 2
    with pytest.raises(RuntimeError):
        fw.run(FakeFW(fail_at=12), checkpoint_obj(fw.ckpt))
    first = read_json(fw.ckpt)
    assert (first["resume_ms"], first["resume_seek"]) == (END_W[1], 6000)
    assert first["parts"] == [{"from_ms": 0, "to_ms": END_W[1], "device_id": "cuda:0",
                               "model": "large-v3", "precision": "auto"}]
    assert len(first["segments"]) == 20
    r, _ = fw.run(FakeFW(), checkpoint_obj(fw.ckpt))
    assert r["sentences"] == r0["sentences"]
    assert r["parts"] == [{"from_ms": 0, "to_ms": 150000, "device_id": "cuda:0",
                           "model": "large-v3", "precision": "auto"}]


@pytest.mark.parametrize("changes", [{"job_id": "j-asr-rerun"}, {"input_hash": "v3:other"},
                                     {"backend": "openvino"}, {"version": 1},
                                     {"engine": "faster-whisper 1.1.0"},
                                     {"vad": "VadOptions(threshold=0.4)"}])
def test_fw_checkpoint_of_another_job_or_engine_starts_over(fw, changes):
    r0, _ = fw.run(FakeFW())
    _interrupt(fw, 22)
    data = read_json(fw.ckpt)
    data.update(changes)
    with open(fw.ckpt, "w", encoding="utf-8") as f:
        json.dump(data, f)
    model = FakeFW()
    r, lines = fw.run(model, checkpoint_obj(fw.ckpt))
    (call,) = model.calls
    assert call["initial_prompt"] == "cache, write-back, TLB" and call["vad_filter"] is False
    assert "clip_timestamps" not in call                     # from the start
    assert call["samples"] == SPEECH_SAMPLES
    assert r["sentences"] == r0["sentences"]
    assert r["parts"] == [{"from_ms": 0, "to_ms": 150000, "device_id": "cuda:0",
                           "model": "large-v3", "precision": "auto"}]
    assert any(l.startswith("checkpoint asr.ckpt.json not used") for l in lines)
    # the old file was deleted; the one there now is this run's own
    data = read_json(fw.ckpt)
    assert data["job_id"] == JOB_ID and data["version"] == 2
    assert data["input_hash"] == INPUT_HASH and data["engine"] == asr.checkpoint_engine()[0]


def test_fw_checkpoint_object_errors_exit_2(fw, capsys, lecture_wav, tmp_path):
    job = {"audio": lecture_wav, "model": "large-v3", "compute_type": "float16",
           "language": "en", "checkpoint": {"path": "", "job_id": JOB_ID, "input_hash": "h"}}
    p = tmp_path / "job.json"
    p.write_text(json.dumps(job), encoding="utf-8")
    assert asr.main(["--in", str(p), "--out", str(tmp_path / "r.tmp")]) == 2
    assert "job.checkpoint.path" in capsys.readouterr().err


def _write_checkpoint(fw, resume_ms, resume_seek, segments, terms=2):
    engine, vad = asr.checkpoint_engine()
    ck, _ = _checkpointer(Path(os.path.dirname(fw.ckpt)), engine=engine, vad=vad)
    ck.save(resume_ms, terms, ck.parts(None, resume_ms), segments, resume_seek=resume_seek)


def test_fw_resume_after_the_last_window_decodes_nothing_new(fw):
    words = [Word(" All", 1.0, 1.5, 0.9), Word(" done.", 1.5, 2.0, 0.9)]
    _write_checkpoint(fw, 150000, 12000, [_fw_segment(1000, 2000, words)])
    model = FakeFW()
    r, lines = fw.run(model, checkpoint_obj(fw.ckpt))
    assert model.calls[0]["clip_timestamps"] == [120.0]          # past the last window
    assert [s["text"] for s in r["sentences"]] == ["All done."]
    assert r["prompt_terms_used"] == 2
    assert progress_of(lines) == ["PROGRESS 1.00", "PROGRESS 1.00"]


def test_fw_resume_without_speech_chunks_does_not_call_the_model(fw, monkeypatch):
    import faster_whisper.vad
    monkeypatch.setattr(faster_whisper.vad, "get_speech_timestamps", lambda audio, o=None: [])
    words = [Word(" Only", 1.0, 1.5, 0.9), Word(" this.", 1.5, 2.0, 0.9)]
    _write_checkpoint(fw, 2000, 3000, [_fw_segment(1000, 2000, words)])
    model = FakeFW()
    r, _ = fw.run(model, checkpoint_obj(fw.ckpt))
    assert model.calls == []
    assert [s["text"] for s in r["sentences"]] == ["Only this."]


# --- a real worker process killed mid-run (as Go cancels: kill), then resumed -------

FAKE_FW_PACKAGE = r'''
import json, os, sys, time, wave
from types import SimpleNamespace
import numpy as np
cfg = json.loads(os.environ.get("LF_FAKE_FW", "{}"))
exec(%r)
__version__ = "1.2.1-fake"
SCRIPT = [(s, e, [tuple(w) for w in ws], n, l) for s, e, ws, n, l in cfg["script"]]
CHUNKS = fw_speech_chunks(SCRIPT)


def decode_audio(path, sampling_rate=16000):
    with wave.open(path, "rb") as w:
        raw = w.readframes(w.getnframes())
    return np.frombuffer(raw, dtype="<i2").astype(np.float32) / 32768.0


class _Tok:
    def encode(self, text, add_special_tokens=True):
        return SimpleNamespace(ids=text.split())


class WhisperModel:
    def __init__(self, path, device="cuda", compute_type="default", cpu_threads=0):
        self.hf_tokenizer = _Tok()

    def transcribe(self, audio, **kw):
        if cfg.get("calls_log"):
            with open(cfg["calls_log"], "a", encoding="utf-8") as f:
                f.write(json.dumps({"samples": len(audio), "vad_filter": kw["vad_filter"],
                                    "clip_timestamps": kw.get("clip_timestamps"),
                                    "initial_prompt": kw.get("initial_prompt")}) + "\n")
        if kw["vad_filter"]:
            segs = list(fw_restore(fw_compact_segments(SCRIPT, CHUNKS), CHUNKS))
        else:
            clip = (kw.get("clip_timestamps") or [0.0])[0]
            segs = fw_compact_segments(SCRIPT, CHUNKS, int(round(clip * 100)))

        def gen():
            for i, s in enumerate(segs):
                if cfg.get("hang_at") == i:
                    print("HANG", flush=True)
                    time.sleep(60)
                yield s

        return gen(), SimpleNamespace(duration=len(audio) / 16000.0, language="en")
''' % FW_FAKE_SRC

FAKE_FW_VAD = r'''
from dataclasses import dataclass
import numpy as np
from faster_whisper import CHUNKS


@dataclass
class VadOptions:
    threshold: float = 0.5


def get_speech_timestamps(audio, vad_options=None, sampling_rate=16000, **kw):
    return [dict(c) for c in CHUNKS]


def collect_chunks(audio, chunks, sampling_rate=16000, max_duration=float("inf")):
    return [np.concatenate([audio[c["start"]:c["end"]] for c in chunks])], [{}]
'''

FAKE_FW_TRANSCRIBE = r'''
from faster_whisper import fw_restore as restore_speech_timestamps  # noqa: F401
'''


def _fw_process_env(tmp_path, **cfg):
    pkg = tmp_path / "fakefw" / "faster_whisper"
    pkg.mkdir(parents=True, exist_ok=True)
    for name, src in (("__init__.py", FAKE_FW_PACKAGE), ("vad.py", FAKE_FW_VAD),
                      ("transcribe.py", FAKE_FW_TRANSCRIBE)):
        with open(pkg / name, "w", encoding="utf-8", newline="\n") as f:
            f.write(src)
    cfg = dict({"script": SCRIPT}, **cfg)
    return dict(os.environ, PYTHONIOENCODING="utf-8", PYTHONUNBUFFERED="1",
                PYTHONPATH=str(tmp_path / "fakefw"), LF_FAKE_FW=json.dumps(cfg))


def _model_dir(tmp_path):
    d = tmp_path / "model"
    d.mkdir(exist_ok=True)
    (d / "model.bin").write_bytes(b"x")
    return str(d)


def _wait_for(proc, marker, timeout=60):
    seen = []
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        line = proc.stdout.readline().decode("utf-8").strip()
        if not line and proc.poll() is not None:
            break
        seen.append(line)
        if line == marker:
            return seen
    raise AssertionError("no %s line: %s" % (marker, seen))


def _kill(proc):
    t0 = time.monotonic()
    proc.kill()
    proc.wait(timeout=10)
    assert time.monotonic() - t0 < 3
    proc.stdout.close()
    proc.stderr.close()


def test_fw_process_killed_then_resumed(tmp_path, lecture_wav):
    work = tmp_path / "lecture"
    work.mkdir()
    ckpt = work / "asr.ckpt.json"
    calls = tmp_path / "calls.jsonl"

    def job(**ck):
        p = work / "job.json"
        p.write_text(json.dumps({
            "audio": lecture_wav, "model": _model_dir(tmp_path), "compute_type": "int8",
            "device": "cpu", "cpu_threads": 1, "language": "en",
            "initial_prompt": "cache, write-back, TLB",
            "checkpoint": checkpoint_obj(ckpt, **ck)}), encoding="utf-8")
        return str(p)

    argv = [sys.executable, os.path.join(WORKER_DIR, "asr.py"), "--in", job(), "--out",
            str(work / "result.json.tmp")]
    # segment 21 requested = segment 20 (window 2) processed: window 1 is complete, but
    # only 0 s after the first write (window 0), so the file still holds window 0
    proc = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            cwd=str(work), env=_fw_process_env(tmp_path, hang_at=21,
                                                               calls_log=str(calls)))
    try:
        _wait_for(proc, "HANG")
        data = read_json(ckpt)
        assert (data["resume_ms"], data["resume_seek"]) == (END_W[0], 3000)
        assert data["engine"] == "faster-whisper 1.2.1-fake"
        assert data["vad"] == "VadOptions(threshold=0.5)"
    finally:
        if proc.poll() is None:
            _kill(proc)
    assert proc.returncode != 0
    assert sorted(os.listdir(work)) == ["asr.ckpt.json", "job.json"]   # no result, no .tmp

    argv[3] = job(device_id="fw:cpu")
    p = subprocess.run(argv, capture_output=True, cwd=str(work), timeout=120,
                       env=_fw_process_env(tmp_path, calls_log=str(calls)))
    assert p.returncode == 0, p.stderr.decode("utf-8", "replace")
    result = read_json(work / "result.json.tmp")
    assert result["sentences"] == expected_fw_sentences()
    assert [x["device_id"] for x in result["parts"]] == ["cuda:0", "fw:cpu"]
    assert result["parts"][0]["to_ms"] == result["parts"][1]["from_ms"] == END_W[0]
    logged = [json.loads(l) for l in calls.read_text(encoding="utf-8").splitlines()]
    assert logged == [{"samples": SPEECH_SAMPLES, "vad_filter": False, "clip_timestamps": None,
                       "initial_prompt": "cache, write-back, TLB"},
                      {"samples": SPEECH_SAMPLES, "vad_filter": False,
                       "clip_timestamps": [30.0], "initial_prompt": None}]
    lines = p.stdout.decode("utf-8").splitlines()
    assert_monotonic(progress_of(lines))
    assert progress_of(lines)[0] == "PROGRESS 0.31"


# ===========================================================================
# OpenVINO
# ===========================================================================

OV_WORDS = [(" Okay,", 0.0, 0.5), (" let's", 0.5, 0.9), (" get", 0.9, 1.2),
            (" started.", 1.2, 2.0), (" Today", 3.8, 4.2), (" we", 4.2, 4.4)]
OV_CHUNKS = [(0.0, 3.8, " Okay, let's get started."), (3.8, 10.0, " Today we")]


def make_model_dir(path):
    os.makedirs(path, exist_ok=True)
    for f in ov_worker.MODEL_FILES:
        with open(os.path.join(path, f), "wb") as fh:
            fh.write(b"x")
    return str(path)


@pytest.fixture
def ov(tmp_path, fake_ov):
    """run(audio, checkpoint=..., **job) runs asr_openvino.run in-process with the fake
    openvino_genai; returns (result, log lines)."""
    model_dir = make_model_dir(tmp_path / "models" / "whisper-large-v3-turbo-fp16")
    state = SimpleNamespace(fake=fake_ov, ckpt=str(tmp_path / "lecture" / "asr.ckpt.json"))
    os.makedirs(tmp_path / "lecture")

    def run(audio, checkpoint=None, **overrides):
        data = {"audio": audio, "model": "large-v3-turbo", "precision": "fp16",
                "model_dir": model_dir, "device": "GPU", "language": "en",
                "initial_prompt": "cache, write-back, TLB",
                "cache_dir": str(tmp_path / "models" / "cache" / "GPU"),
                "hf_id": "openai/whisper-large-v3-turbo", "hf_home": str(tmp_path / "hf"),
                "word_timestamps": True, "min_free_bytes": 1}
        data.update(overrides)
        if checkpoint is not None:
            data["checkpoint"] = checkpoint
        in_path = tmp_path / "lecture" / "job.json"
        in_path.write_text(json.dumps(data), encoding="utf-8")
        out_path = tmp_path / "lecture" / "result.json.tmp"
        if out_path.exists():
            out_path.unlink()
        lines = []
        fake_ov.generate_calls.clear()
        ov_worker.run(str(in_path), str(out_path), lines.append)
        return read_json(out_path), lines

    state.run = run
    return state


def ov_ck(path, device_id="ov:GPU", chunk_s=30, **kw):
    return checkpoint_obj(path, device_id=device_id, model="large-v3-turbo", precision="fp16",
                          chunk_s=chunk_s, **kw)


def test_ov_chunk_s_0_is_the_old_run_plus_parts(ov, tmp_path):
    from conftest import write_wav_parts
    audio = write_wav_parts(tmp_path / "audio.wav", [(10.0, 8000)])
    ov.fake.words, ov.fake.chunks = list(OV_WORDS), list(OV_CHUNKS)
    ov.fake.windows = 2
    r0, lines0 = ov.run(audio)
    r1, lines1 = ov.run(audio, ov_ck(ov.ckpt, chunk_s=0))
    (call,) = ov.fake.generate_calls
    assert len(call["audio"]) == 160000                       # one generate, whole lecture
    assert call["config"]["initial_prompt"] == "cache, write-back, TLB"
    t0, t1 = r0.pop("timing"), r1.pop("timing")
    assert t0["audio_s"] == t1["audio_s"] == 10.0
    assert r1.pop("parts") == [{"from_ms": 0, "to_ms": 10000, "device_id": "ov:GPU",
                                "model": "large-v3-turbo", "precision": "fp16"}]
    assert r1 == r0
    assert progress_of(lines1) == progress_of(lines0) == ["PROGRESS 0.00", "PROGRESS 0.95",
                                                          "PROGRESS 1.00"]
    assert not os.path.exists(ov.ckpt) and not os.path.exists(ov.ckpt + ".tmp")
    assert sorted(os.listdir(os.path.dirname(ov.ckpt))) == ["job.json", "result.json.tmp"]


# --- the cut search --------------------------------------------------------------------

def _frames(parts, seed=3):
    """frame_rms of (seconds, amplitude) parts: amplitude 0 = digital silence, otherwise
    noise with that standard deviation (int16 units)."""
    rng = np.random.default_rng(seed)
    out = []
    for seconds, amp in parts:
        n = int(round(seconds * 16000))
        out.append(np.zeros(n, np.float32) if amp == 0 else
                   (rng.standard_normal(n) * amp / 32768.0).astype(np.float32))
    audio = np.concatenate(out)
    return ov_worker.frame_rms(audio), len(audio)


def test_next_cut_short_rest_is_one_chunk():
    frames, n = _frames([(40.0, 3000)])
    assert ov_worker.next_cut(frames, 0, n, 30) == n           # 40 s <= 30 + 10
    frames, n = _frames([(40.1, 3000)])
    assert ov_worker.next_cut(frames, 0, n, 30) < n


def test_next_cut_middle_of_the_silent_pause_nearest_the_target():
    # silent pauses at 21.0-21.8 and 37.0-37.8, a quiet (not silent) pause at 29.6-30.4
    frames, n = _frames([(21.0, 3000), (0.8, 0), (7.8, 3000), (0.8, 150), (6.6, 3000),
                         (0.8, 0), (30.0, 3000)])
    # target 30: silence beats the quieter pause at the target; of the two silent
    # pauses the nearer one (37.4 is 7.4 s away, 21.4 is 8.6 s), cut at its middle
    assert ov_worker.next_cut(frames, 0, n, 30) == int(37.4 * 16000)
    # target 28 (window 18-38): now 21.4 is the nearer one
    assert ov_worker.next_cut(frames, 0, n, 28) == int(21.4 * 16000)
    # from 10 s: target 40, window 30-50: only the pause at 37.0-37.8
    assert ov_worker.next_cut(frames, 10 * 16000, n, 30) == int(37.4 * 16000)


def test_next_cut_ignores_pauses_outside_the_search_window():
    # a silent pause at 18 s (outside 20-40) and a quiet one at 33 s
    frames, n = _frames([(18.0, 3000), (0.8, 0), (14.2, 3000), (0.8, 300), (40.0, 3000)])
    cut = ov_worker.next_cut(frames, 0, n, 30)
    assert 32.9 * 16000 <= cut <= 34.0 * 16000
    assert cut % 320 == 0                      # on the 20 ms grid: whole milliseconds


def test_next_cut_short_chunk_never_before_half_the_chunk():
    frames, n = _frames([(1.0, 0), (60.0, 3000)])
    cut = ov_worker.next_cut(frames, 0, n, 5)  # silence at 0-1 s is before 2.5 s
    assert 2.5 * 16000 <= cut <= 15 * 16000


def test_next_cut_leaves_at_least_10_s_for_the_last_chunk():
    # 41 s: silent pauses at 25.0-25.8 and 37.0-37.8; the later one is nearer the target
    # but would leave a 3.6 s last chunk
    frames, n = _frames([(25.0, 3000), (0.8, 0), (11.2, 3000), (0.8, 0), (3.2, 3000)])
    assert ov_worker.next_cut(frames, 0, n, 30) == int(25.4 * 16000)


def test_next_cut_300_s_chunks():
    """The product's chunk length (devices.OVCheckpointChunkS = 300): a 700 s lecture with
    silent pauses near 295 s and 600 s is cut there; the rest (100 s) stays one chunk."""
    frames, n = _frames([(294.6, 3000), (0.8, 0), (304.0, 3000), (0.8, 0), (99.8, 3000)])
    cut = ov_worker.next_cut(frames, 0, n, 300)
    assert cut == int(295.0 * 16000)
    cut2 = ov_worker.next_cut(frames, cut, n, 300)
    assert cut2 == int(599.8 * 16000)         # the middle of the pause 599.4-600.2
    assert ov_worker.next_cut(frames, cut2, n, 300) == n


def test_chunk_words_first_word_gets_a_space():
    """A chunk's first word without a leading space is not glued onto the previous
    chunk's last word (docs/benchmarks/power-checkpoint.md §2.1): it gets one."""
    res = SimpleNamespace(texts=["two words"], chunks=[SimpleNamespace(start_ts=0.0, end_ts=1.0,
                                                                       text="two words")],
                          words=[SimpleNamespace(word="two", start_ts=0.1, end_ts=0.4),
                                 SimpleNamespace(word=" words", start_ts=0.5, end_ts=0.9)])
    frames = ov_worker.frame_rms(np.full(16000 * 400, 0.1, dtype=np.float32))
    words, with_words, counts = ov_worker.chunk_words(res, 300.0, 301.0, frames, 0.01, True,
                                                      lambda line: None)
    assert words == [Word(" two", 300.1, 300.4, 1.0), Word(" words", 300.5, 300.9, 1.0)]
    assert with_words and counts == [1, 0, 0, 0]


# --- chunked transcription and checkpoints ----------------------------------------------

OV_TOTAL_S = 150.0


def ov_lecture(path):
    """150 s: 39 speech bursts of 3 s (noise) every 3.8 s from 0.5 s, separated by quiet
    pauses (noise at -47 dBFS) except pauses 6, 14, 22, 30 (digital silence at 26.3-27.1,
    56.7-57.5, 87.1-87.9, 117.5-118.3 s). Returns (wav path, script segments)."""
    rng = np.random.default_rng(7)
    audio = (rng.standard_normal(int(OV_TOTAL_S * 16000)) * 150).astype(np.int16)
    segments = []
    for i in range(39):
        t = round(0.5 + 3.8 * i, 2)
        a, b = int(round(t * 16000)), int(round((t + 3.0) * 16000))
        audio[a:b] = np.clip(rng.standard_normal(b - a) * 6000, -32000, 32000).astype(np.int16)
        if i in (6, 14, 22, 30):
            audio[b:int(round((t + 3.8) * 16000))] = 0
        last = " s%dc." % i if i % 3 == 2 else " s%dc" % i
        ws = [(" s%da" % i, round(t + 0.2, 2), round(t + 1.0, 2)),
              (" s%db" % i, round(t + 1.1, 2), round(t + 1.9, 2)),
              (last, round(t + 2.0, 2), round(t + 2.9, 2))]
        segments.append((t, round(t + 3.0, 2), "".join(w for w, _, _ in ws), ws))
    import wave
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(16000)
        w.writeframes(audio.astype("<i2").tobytes())
    return str(path), segments


CUTS_MS = [26700, 57100, 87500, 117900]       # the middles of the silent pauses


@pytest.fixture
def scripted(ov, tmp_path):
    path, segments = ov_lecture(tmp_path / "audio.wav")
    ov.fake.script = segments
    ov.fake.full_audio = ov_worker.read_wav(path)
    ov.audio = path
    ov.segments = segments
    return ov


def expected_ov_sentences(segments, with_words=True):
    words = [Word(w, s, e, 1.0) for _, _, _, ws in segments for w, s, e in ws]
    sentences = asr_common.split_sentences(asr_common.remove_repeats(words))
    if not with_words:
        for s in sentences:
            s.pop("words")
    return sentences


def test_ov_chunked_run_cuts_at_pauses_and_checkpoints_every_chunk(scripted):
    seen = []
    scripted.fake.on_generate = lambda n: seen.append(
        read_json(scripted.ckpt)["resume_ms"] if os.path.exists(scripted.ckpt) else None)
    r, lines = scripted.run(scripted.audio, ov_ck(scripted.ckpt))
    calls = scripted.fake.generate_calls
    starts = [c["offset"] for c in calls]
    assert starts == [0] + [ms * 16 for ms in CUTS_MS]
    assert [len(c["audio"]) for c in calls] == [
        b - a for a, b in zip(starts, starts[1:] + [int(OV_TOTAL_S * 16000)])]
    # initial_prompt on the first chunk only (G4)
    assert [c["config"]["initial_prompt"] for c in calls] == \
        ["cache, write-back, TLB", None, None, None, None]
    # a checkpoint after every chunk but the last
    assert seen == [None] + CUTS_MS
    data = read_json(scripted.ckpt)
    assert data["backend"] == "openvino" and data["resume_ms"] == CUTS_MS[-1]
    assert data["parts"] == [{"from_ms": 0, "to_ms": CUTS_MS[-1], "device_id": "ov:GPU",
                              "model": "large-v3-turbo", "precision": "fp16"}]
    assert [(s["start_ms"], s["end_ms"]) for s in data["segments"]] == \
        list(zip([0] + CUTS_MS[:-1], CUTS_MS))
    assert all(w["probability"] == 1.0 for s in data["segments"] for w in s["words"])
    # every word on the lecture's timeline, sentences across chunk cuts intact
    assert r["sentences"] == expected_ov_sentences(scripted.segments)
    assert r["parts"] == [{"from_ms": 0, "to_ms": 150000, "device_id": "ov:GPU",
                           "model": "large-v3-turbo", "precision": "fp16"}]
    assert r["timing"]["audio_s"] == OV_TOTAL_S
    progress = progress_of(lines)
    assert progress[0] == "PROGRESS 0.00" and progress[-1] == "PROGRESS 1.00"
    assert all(float(l.split()[1]) <= 0.95 for l in progress[:-1])
    assert_monotonic(progress)
    assert any("5 chunks" in l and "4 checkpoints written" in l for l in lines)


def test_ov_chunked_interrupted_then_resumed_on_the_npu_is_identical(scripted):
    r0, _ = scripted.run(scripted.audio, ov_ck(scripted.ckpt))
    os.remove(scripted.ckpt)
    scripted.fake.fail_on_call = 3                     # killed during the third chunk
    with pytest.raises(RuntimeError, match="worker killed"):
        scripted.run(scripted.audio, ov_ck(scripted.ckpt))
    assert read_json(scripted.ckpt)["resume_ms"] == CUTS_MS[1]
    scripted.fake.fail_on_call = None
    r, lines = scripted.run(scripted.audio, ov_ck(scripted.ckpt, device_id="ov:NPU"),
                            device="NPU", cache_dir="")
    assert r["sentences"] == r0["sentences"] == expected_ov_sentences(scripted.segments)
    assert r["prompt_terms_used"] == r0["prompt_terms_used"] == 3
    assert r["parts"] == [
        {"from_ms": 0, "to_ms": CUTS_MS[1], "device_id": "ov:GPU", "model": "large-v3-turbo",
         "precision": "fp16"},
        {"from_ms": CUTS_MS[1], "to_ms": 150000, "device_id": "ov:NPU",
         "model": "large-v3-turbo", "precision": "fp16"}]
    calls = scripted.fake.generate_calls
    # the cuts of the uninterrupted run, from the checkpoint on, without initial_prompt
    assert [c["offset"] for c in calls] == [ms * 16 for ms in CUTS_MS[1:]]
    assert all(c["config"]["initial_prompt"] is None for c in calls)
    assert r["timing"]["audio_s"] == round((150000 - CUTS_MS[1]) / 1000.0, 2)
    progress = progress_of(lines)
    assert progress[0] == asr_common.format_progress(CUTS_MS[1] / 150000.0) == "PROGRESS 0.38"
    assert_monotonic(progress)
    assert all(float(l.split()[1]) <= 0.95 for l in progress[:-1])
    assert "continuing at 0:57 without initial_prompt (first chunk only)" in lines


def test_ov_chunk_s_0_continues_a_matching_checkpoint_in_one_piece(scripted):
    """A checkpoint left by a chunked run (e.g. before chunk_s was set to 0) is still
    continued: one generate() from resume_ms to the end."""
    r0, _ = scripted.run(scripted.audio, ov_ck(scripted.ckpt))
    os.remove(scripted.ckpt)
    scripted.fake.fail_on_call = 2
    with pytest.raises(RuntimeError):
        scripted.run(scripted.audio, ov_ck(scripted.ckpt))
    scripted.fake.fail_on_call = None
    r, _ = scripted.run(scripted.audio, ov_ck(scripted.ckpt, chunk_s=0))
    (call,) = scripted.fake.generate_calls
    assert call["offset"] == CUTS_MS[0] * 16 and call["config"]["initial_prompt"] is None
    assert r["sentences"] == r0["sentences"]
    assert read_json(scripted.ckpt)["resume_ms"] == CUTS_MS[0]   # nothing new written


def test_ov_faster_whisper_checkpoint_is_not_continued(scripted):
    """Another backend's checkpoint (the CUDA run of this job): deleted, from the start."""
    ck, _ = _checkpointer(Path(os.path.dirname(scripted.ckpt)))
    ck.save(60000, 3, ck.parts(None, 60000), [])
    r, lines = scripted.run(scripted.audio, ov_ck(scripted.ckpt))
    assert scripted.fake.generate_calls[0]["offset"] == 0
    assert scripted.fake.generate_calls[0]["config"]["initial_prompt"] == "cache, write-back, TLB"
    assert any("backend 'faster-whisper', this worker is 'openvino'" in l for l in lines)
    assert r["sentences"] == expected_ov_sentences(scripted.segments)
    assert read_json(scripted.ckpt)["backend"] == "openvino"


def test_ov_chunk_starting_without_a_space_is_not_glued(scripted):
    scripted.fake.first_word_no_space = True
    r, _ = scripted.run(scripted.audio, ov_ck(scripted.ckpt))
    assert r["sentences"] == expected_ov_sentences(scripted.segments)


def test_ov_chunked_without_word_timestamps(scripted):
    r, _ = scripted.run(scripted.audio, ov_ck(scripted.ckpt), word_timestamps=False)
    sentences = r["sentences"]
    assert all("words" not in s for s in sentences)
    assert [s["text"] for s in sentences] == \
        [s["text"] for s in expected_ov_sentences(scripted.segments, with_words=False)]
    # pseudo-words of each chunk's segments: sentences start at their segment's start
    assert sentences[0]["start_ms"] == 500


def test_ov_chunked_streamer_progress_stays_inside_the_chunk(scripted):
    scripted.fake.windows = 3              # 3 x 30 s per call: more than a chunk is long
    r, lines = scripted.run(scripted.audio, ov_ck(scripted.ckpt))
    # never ahead of the chunk being transcribed (without the cap: 0.20, 0.40, 0.60 ...):
    # the chunk ends 26.7, 57.1, 87.5, 117.9 s (the fourth chunk's first window ends at
    # 117.5 s: 0.78), then 0.95 at most until the result
    assert progress_of(lines) == ["PROGRESS 0.00", "PROGRESS 0.18", "PROGRESS 0.38",
                                  "PROGRESS 0.58", "PROGRESS 0.78", "PROGRESS 0.79",
                                  "PROGRESS 0.95", "PROGRESS 1.00"]


def test_window_progress_chunks_unit():
    lines = []
    p = ov_worker.WindowProgress(100.0, lines.append, "RUNNING", start=40.0)
    assert p.last == "PROGRESS 0.40"
    p.chunk(40.0, 50.0)
    assert p("x") == "RUNNING"              # 40 + 30 capped at the chunk end 50
    p("x")
    p.reach(50.0)
    p.chunk(50.0, 100.0)
    p("x"), p("x")                          # 80, then 100 capped at 0.95
    p.reach(100.0)
    assert lines == ["PROGRESS 0.50", "PROGRESS 0.80", "PROGRESS 0.95"]


# --- a real worker process killed mid-run, then resumed ----------------------------------

def _ov_env(tmp_path, **cfg):
    return dict(os.environ, PYTHONIOENCODING="utf-8", PYTHONUNBUFFERED="1",
                PYTHONPATH=write_fake_ov_packages(tmp_path / "fakepkgs"),
                LF_FAKE_OV=json.dumps(cfg))


def test_ov_process_killed_then_resumed(tmp_path):
    audio, segments = ov_lecture(tmp_path / "audio.wav")
    work = tmp_path / "lecture"
    work.mkdir()
    ckpt = work / "asr.ckpt.json"
    calls = tmp_path / "calls.jsonl"
    model_dir = make_model_dir(tmp_path / "models" / "whisper-large-v3-turbo-fp16")

    def job(device_id):
        p = work / "job.json"
        p.write_text(json.dumps({
            "audio": audio, "model": "large-v3-turbo", "precision": "fp16",
            "model_dir": model_dir, "device": "CPU", "language": "en",
            "initial_prompt": "cache, write-back, TLB", "cache_dir": "",
            "hf_id": "openai/whisper-large-v3-turbo", "hf_home": str(tmp_path / "hf"),
            "word_timestamps": True, "min_free_bytes": 1,
            "checkpoint": ov_ck(ckpt, device_id=device_id)}), encoding="utf-8")
        return str(p)

    script = {"audio": audio, "segments": segments}
    argv = [sys.executable, os.path.join(WORKER_DIR, "asr_openvino.py"), "--in", job("ov:GPU"),
            "--out", str(work / "result.json.tmp")]
    proc = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=str(work),
                            env=_ov_env(tmp_path, script=script, hang_on_call=3,
                                        calls_log=str(calls)))
    try:
        _wait_for(proc, "GENERATING")
        assert read_json(ckpt)["resume_ms"] == CUTS_MS[1]
    finally:
        if proc.poll() is None:
            _kill(proc)
    assert proc.returncode != 0
    assert sorted(os.listdir(work)) == ["asr.ckpt.json", "job.json"]

    argv[3] = job("ov:NPU")
    p = subprocess.run(argv, capture_output=True, cwd=str(work), timeout=120,
                       env=_ov_env(tmp_path, script=script, calls_log=str(calls)))
    assert p.returncode == 0, p.stderr.decode("utf-8", "replace")
    result = read_json(work / "result.json.tmp")
    assert result["sentences"] == expected_ov_sentences(segments)
    assert [x["device_id"] for x in result["parts"]] == ["ov:GPU", "ov:NPU"]
    logged = [json.loads(l) for l in calls.read_text(encoding="utf-8").splitlines()]
    prompt = "cache, write-back, TLB"
    # (the killed third call hung before logging; the resumed run starts at its offset)
    assert logged == [{"offset": 0, "initial_prompt": prompt},
                      {"offset": CUTS_MS[0] * 16, "initial_prompt": None},
                      {"offset": CUTS_MS[1] * 16, "initial_prompt": None},
                      {"offset": CUTS_MS[2] * 16, "initial_prompt": None},
                      {"offset": CUTS_MS[3] * 16, "initial_prompt": None}]
    lines = p.stdout.decode("utf-8").splitlines()
    assert_monotonic(progress_of(lines))
    assert progress_of(lines)[0] == "PROGRESS 0.38" and lines[-1] == "PROGRESS 1.00"


def test_script_fake_locates_pieces(tmp_path):
    path, _ = ov_lecture(tmp_path / "a.wav")
    from conftest import script_locate
    full = script_read_wav(path)
    assert np.array_equal(full, ov_worker.read_wav(path))
    assert script_locate(full, full[123456:200000]) == 123456
    assert script_locate(full, np.zeros(100, np.float32)) is None
