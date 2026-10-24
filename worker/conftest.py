# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""Shared pytest setup for the LectureFlow workers.

Tests marked `realgpu` use the real GPU / real faster-whisper models (CLAUDE.md,
revision 2). They are skipped unless the marker is selected explicitly:
    .venv\\Scripts\\python.exe -m pytest -q -m realgpu worker      (.\\make.ps1 realgpu)
"""
import math
import os
import struct
import sys
import types
import wave
from types import SimpleNamespace

import pytest

WORKER_DIR = os.path.dirname(os.path.abspath(__file__))
PROJECT_DIR = os.path.dirname(WORKER_DIR)
if WORKER_DIR not in sys.path:
    sys.path.insert(0, WORKER_DIR)


def pytest_configure(config):
    config.addinivalue_line(
        "markers", "realgpu: uses the real GPU and real whisper models; run with -m realgpu")
    config.addinivalue_line(
        "markers", "realmlx: uses real MLX on Apple Silicon (macOS CI); run with -m realmlx")


def pytest_collection_modifyitems(config, items):
    if "realgpu" in (config.getoption("markexpr") or ""):
        return
    skip = pytest.mark.skip(reason="realgpu test: run with -m realgpu (.\\make.ps1 realgpu)")
    for item in items:
        if "realgpu" in item.keywords:
            item.add_marker(skip)


def write_wav(path, seconds, rate=16000, tone_hz=0.0):
    """Writes a 16-bit mono PCM wav (silence, or a quiet sine tone) without ffmpeg."""
    n = int(seconds * rate)
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(rate)
        if tone_hz:
            frames = b"".join(
                struct.pack("<h", int(3000 * math.sin(2 * math.pi * tone_hz * i / rate)))
                for i in range(n))
        else:
            frames = b"\x00\x00" * n
        w.writeframes(frames)
    return str(path)


@pytest.fixture
def wav_factory(tmp_path):
    def make(name="audio.wav", seconds=5.0, **kw):
        return write_wav(tmp_path / name, seconds, **kw)
    return make


def write_wav_parts(path, parts, rate=16000):
    """Writes a 16-bit mono wav from (seconds, amplitude) parts: amplitude 0 is digital
    silence, otherwise a 440 Hz sine with that peak (int16 units)."""
    import numpy as np
    frames = []
    for seconds, amp in parts:
        n = int(round(seconds * rate))
        t = np.arange(n, dtype=np.float64) / rate
        frames.append((amp * np.sin(2 * np.pi * 440.0 * t)).astype("<i2").tobytes())
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(rate)
        w.writeframes(b"".join(frames))
    return str(path)


# ---------------------------------------------------------------------------
# Fake OpenVINO (worker/asr_openvino.py imports openvino / openvino_genai lazily, so the
# tests run in .venv without them; SPEC §14: no real model is loaded).
# ---------------------------------------------------------------------------

INTEL_GPU_PROPS = {"FULL_DEVICE_NAME": "Intel(R) Graphics (iGPU)",
                   "DEVICE_ARCHITECTURE": "GPU: vendor=0x8086 arch=v30.1.0",
                   "DEVICE_LUID": "e220010000000000"}
NVIDIA_GPU_PROPS = {"FULL_DEVICE_NAME": "NVIDIA GeForce RTX 5070 Ti Laptop GPU (dGPU)",
                    "DEVICE_ARCHITECTURE": "GPU: vendor=0x10de arch=v12.0.0",
                    "DEVICE_LUID": "f7d9270000000000"}
NPU_PROPS = {"FULL_DEVICE_NAME": "Intel(R) AI Boost", "DEVICE_ARCHITECTURE": "5010",
             "DEVICE_LUID": "d026010000000000", "NPU_DRIVER_VERSION": 1004841}
CPU_PROPS = {"FULL_DEVICE_NAME": "Intel(R) Core(TM) Ultra 9 386H",
             "DEVICE_ARCHITECTURE": "intel64"}


def default_ov_devices():
    """The 3a machine: CPU, Intel iGPU as GPU.0, the NVIDIA card as GPU.1, NPU."""
    return {"CPU": dict(CPU_PROPS), "GPU.0": dict(INTEL_GPU_PROPS),
            "GPU.1": dict(NVIDIA_GPU_PROPS), "NPU": dict(NPU_PROPS)}


class FakeShape(tuple):
    pass


class _Tokenized:
    def __init__(self, n):
        self.input_ids = SimpleNamespace(shape=FakeShape((1, n)))


class FakeOVTokenizer:
    """One token per whitespace-separated piece."""

    def encode(self, text, add_special_tokens=True):
        assert add_special_tokens is False
        return _Tokenized(len(text.split()))


class FakeGenerationConfig:
    def __init__(self):
        self.language = None
        self.task = None
        self.return_timestamps = False
        self.word_timestamps = False
        self.num_beams = 1
        self.initial_prompt = None
        self.hotwords = None
        self.lang_to_id = {"<|en|>": 50259, "<|zh|>": 50260, "<|de|>": 50261}

    def snapshot(self):
        return dict(vars(self))


def ov_words(words):
    """[(word, start, end)] -> objects shaped like openvino_genai.WhisperWordTiming."""
    return [SimpleNamespace(word=w, start_ts=s, end_ts=e) for w, s, e in words]


def ov_chunks(chunks):
    """[(start, end, text)] -> objects shaped like WhisperDecodedResultChunk."""
    return [SimpleNamespace(start_ts=s, end_ts=e, text=t) for s, e, t in chunks]


# A scripted lecture for the checkpoint tests (fake models that answer by position):
# the fake finds where the audio it is given lies in the lecture's audio and returns the
# script's segments inside it, with times relative to it, the way a model transcribing
# that piece would. Shared as source text by the in-process fake below and the fake
# openvino_genai package of worker processes (FAKE_GENAI_PACKAGE).
FAKE_SCRIPT_SRC = r'''
import wave as _wave
import numpy as _np


def script_read_wav(path):
    """A 16-bit mono wav as the OpenVINO worker reads it (int16 / 32768, float32)."""
    with _wave.open(str(path), "rb") as w:
        raw = w.readframes(w.getnframes())
    return _np.frombuffer(raw, dtype="<i2").astype(_np.float32) / 32768.0


def script_locate(full, piece):
    """Sample offset of piece inside full (exact match); None for an all-zero piece."""
    piece = _np.asarray(piece, dtype=_np.float32)
    nz = _np.flatnonzero(piece)
    if len(nz) == 0:
        return None
    j = int(nz[0])
    probe = piece[j:j + 64]
    for o in _np.flatnonzero(full == piece[j]) - j:
        o = int(o)
        if 0 <= o and o + len(piece) <= len(full) and \
                _np.array_equal(full[o + j:o + j + len(probe)], probe) and \
                _np.array_equal(full[o:o + len(piece)], piece):
            return o
    raise AssertionError("audio piece not found in the lecture")


def script_result(segments, offset_s, length_s):
    """(chunks, words) of the script segments [(start, end, text, [(word, s, e)])] that lie
    inside [offset_s, offset_s + length_s], times relative to offset_s."""
    chunks, words = [], []
    for start, end, text, ws in segments:
        if start >= offset_s - 1e-6 and end <= offset_s + length_s + 1e-6:
            chunks.append((start - offset_s, end - offset_s, text))
            words.extend((w, s - offset_s, e - offset_s) for w, s, e in ws)
    return chunks, words
'''
exec(FAKE_SCRIPT_SRC)


def make_fake_openvino(state):
    """(openvino, openvino_genai) fake modules driven by `state` (a SimpleNamespace):
    devices {name: {property: value}} (a value of Exception is raised), windows (streamer
    calls per generate), words / chunks / texts of the result, generate_error, on_init
    (called with the pipeline when it is constructed), on_window (called with the
    window number after each streamer call). Records pipelines, generate_calls.
    Checkpoint tests: script (FAKE_SCRIPT_SRC segments) with full_audio (the lecture as
    read_wav reads it) answers by position instead of words / chunks, recording each
    call's sample "offset"; on_generate(n) runs at the start of the n-th call;
    fail_on_call = n raises on the n-th call (after its streamer calls), like a killed
    worker; first_word_no_space returns each call's first word without its space."""
    ov = types.ModuleType("openvino")
    ov.__version__ = "2026.4.1-fake"
    genai = types.ModuleType("openvino_genai")
    genai.__version__ = "2026.4.1.0-fake"
    genai.StreamingStatus = SimpleNamespace(RUNNING="RUNNING", STOP="STOP", CANCEL="CANCEL")

    class Core:
        @property
        def available_devices(self):
            return list(state.devices)

        def get_property(self, device, key):
            props = state.devices[device]
            if key == "SUPPORTED_PROPERTIES":
                if isinstance(props.get("SUPPORTED_PROPERTIES"), Exception):
                    raise props["SUPPORTED_PROPERTIES"]
                return [k for k in props if k != "SUPPORTED_PROPERTIES"] + ["SUPPORTED_PROPERTIES"]
            if key not in props:
                raise RuntimeError("Unsupported property %s by device %s" % (key, device))
            v = props[key]
            if isinstance(v, Exception):
                raise v
            return v

    class WhisperPipeline:
        def __init__(self, model_dir, device, **props):
            self.model_dir, self.device, self.props = model_dir, device, props
            state.pipelines.append(self)
            if state.on_init:
                state.on_init(self)

        def get_generation_config(self):
            return FakeGenerationConfig()

        def get_tokenizer(self):
            return FakeOVTokenizer()

        def generate(self, audio, config, streamer=None):
            call = {"audio": audio, "config": config.snapshot(), "streamer": streamer}
            state.generate_calls.append(call)
            if state.on_generate:
                state.on_generate(len(state.generate_calls))
            for i in range(state.windows):
                if streamer is not None:
                    assert streamer("some text") == genai.StreamingStatus.RUNNING
                if state.on_window:
                    state.on_window(i + 1)
            if state.fail_on_call == len(state.generate_calls):
                raise RuntimeError("worker killed (fake)")
            if state.generate_error:
                raise state.generate_error
            if state.script is not None:
                off = script_locate(state.full_audio, audio)
                call["offset"] = off
                chunks, words = ([], []) if off is None else script_result(
                    state.script, off / 16000.0, len(audio) / 16000.0)
                if state.first_word_no_space and words:
                    words[0] = (words[0][0].lstrip(),) + tuple(words[0][1:])
                return SimpleNamespace(texts=["".join(c[2] for c in chunks)],
                                       chunks=ov_chunks(chunks),
                                       words=ov_words(words) if config.word_timestamps
                                       else [], scores=[1.0])
            return SimpleNamespace(texts=state.texts, chunks=ov_chunks(state.chunks),
                                   words=ov_words(state.words) if config.word_timestamps
                                   else ov_words(state.words_without_ts),
                                   scores=[1.0])

    ov.Core = Core
    genai.WhisperPipeline = WhisperPipeline
    return ov, genai


def new_ov_state(**kw):
    s = SimpleNamespace(devices=default_ov_devices(), windows=1, words=[], chunks=[],
                        texts=[""], words_without_ts=[], generate_error=None, on_init=None,
                        on_window=None, pipelines=[], generate_calls=[], script=None,
                        full_audio=None, on_generate=None, fail_on_call=None,
                        first_word_no_space=False)
    for k, v in kw.items():
        setattr(s, k, v)
    return s


@pytest.fixture
def fake_ov(monkeypatch):
    """Installs fake openvino / openvino_genai modules; returns their state."""
    state = new_ov_state()
    ov, genai = make_fake_openvino(state)
    monkeypatch.setitem(sys.modules, "openvino", ov)
    monkeypatch.setitem(sys.modules, "openvino_genai", genai)
    return state


# Fake packages for worker processes started by the tests (PYTHONPATH = their dir).
# LF_FAKE_OV (JSON): {"import_error": "..."} makes `import openvino` fail; {"hang": true}
# makes generate() call the streamer once, print "GENERATING" and sleep for a minute
# ({"hang_on_call": n}: the same on the n-th call only); {"script": {"audio": wav path,
# "segments": FAKE_SCRIPT_SRC segments}} answers by position (checkpoint tests), and
# {"calls_log": path} appends {"offset", "initial_prompt"} of every call there.
FAKE_OV_PACKAGE = r'''
import json, os, sys, time
from types import SimpleNamespace
cfg = json.loads(os.environ.get("LF_FAKE_OV", "{}"))
if cfg.get("import_error"):
    raise ImportError(cfg["import_error"])
__version__ = "2026.4.1-fake"
DEVICES = {
    "CPU": {"FULL_DEVICE_NAME": "Intel(R) Core(TM) Ultra 9 386H"},
    "GPU.0": {"FULL_DEVICE_NAME": "Intel(R) Graphics (iGPU)",
              "DEVICE_ARCHITECTURE": "GPU: vendor=0x8086 arch=v30.1.0",
              "DEVICE_LUID": "e220010000000000"},
    "GPU.1": {"FULL_DEVICE_NAME": "NVIDIA GeForce RTX 5070 Ti Laptop GPU (dGPU)",
              "DEVICE_ARCHITECTURE": "GPU: vendor=0x10de arch=v12.0.0",
              "DEVICE_LUID": "f7d9270000000000"},
    "NPU": {"FULL_DEVICE_NAME": "Intel(R) AI Boost", "DEVICE_LUID": "d026010000000000",
            "NPU_DRIVER_VERSION": 1004841},
}
class Core:
    available_devices = list(DEVICES)
    def get_property(self, device, key):
        if key == "SUPPORTED_PROPERTIES":
            return list(DEVICES[device]) + ["SUPPORTED_PROPERTIES"]
        return DEVICES[device][key]
'''

FAKE_GENAI_PACKAGE = r'''
import json, os, sys, time
from types import SimpleNamespace
import openvino
__version__ = "2026.4.1.0-fake"
cfg = json.loads(os.environ.get("LF_FAKE_OV", "{}"))
StreamingStatus = SimpleNamespace(RUNNING="RUNNING", STOP="STOP", CANCEL="CANCEL")
SCRIPT = cfg.get("script")
CALLS = [0]
if SCRIPT:
    exec(FAKE_SCRIPT_SRC)
    FULL = script_read_wav(SCRIPT["audio"])


def _scripted(audio, config):
    off = script_locate(FULL, audio)
    chunks, words = ([], []) if off is None else script_result(
        SCRIPT["segments"], off / 16000.0, len(audio) / 16000.0)
    if cfg.get("calls_log"):
        with open(cfg["calls_log"], "a", encoding="utf-8") as f:
            f.write(json.dumps({"offset": off, "initial_prompt": config.initial_prompt}) + "\n")
    return SimpleNamespace(
        texts=["".join(c[2] for c in chunks)],
        chunks=[SimpleNamespace(start_ts=s, end_ts=e, text=t) for s, e, t in chunks],
        words=[SimpleNamespace(word=w, start_ts=s, end_ts=e) for w, s, e in words]
        if config.word_timestamps else [])
class _Cfg:
    def __init__(self):
        self.language = None; self.task = None; self.return_timestamps = False
        self.word_timestamps = False; self.num_beams = 1; self.initial_prompt = None
        self.lang_to_id = {"<|en|>": 50259}
class _Tok:
    def encode(self, text, add_special_tokens=True):
        return SimpleNamespace(input_ids=SimpleNamespace(shape=(1, len(text.split()))))
class WhisperPipeline:
    def __init__(self, model_dir, device, **props):
        self.device = device
    def get_generation_config(self):
        return _Cfg()
    def get_tokenizer(self):
        return _Tok()
    def generate(self, audio, config, streamer=None):
        CALLS[0] += 1
        if streamer is not None:
            streamer("text")
        if cfg.get("hang") or cfg.get("hang_on_call") == CALLS[0]:
            print("GENERATING", flush=True)
            time.sleep(60)
        if SCRIPT:
            return _scripted(audio, config)
        w = SimpleNamespace(word=" Hello.", start_ts=0.0, end_ts=0.5)
        c = SimpleNamespace(start_ts=0.0, end_ts=0.5, text=" Hello.")
        return SimpleNamespace(texts=[" Hello."], chunks=[c], words=[w])
'''.replace("exec(FAKE_SCRIPT_SRC)", "exec(%r)" % FAKE_SCRIPT_SRC)


def write_fake_ov_packages(root):
    """Writes the fake openvino / openvino_genai packages under root; returns root."""
    for name, src in (("openvino", FAKE_OV_PACKAGE), ("openvino_genai", FAKE_GENAI_PACKAGE)):
        d = os.path.join(str(root), name)
        os.makedirs(d, exist_ok=True)
        with open(os.path.join(d, "__init__.py"), "w", encoding="utf-8", newline="\n") as f:
            f.write(src)
    return str(root)
