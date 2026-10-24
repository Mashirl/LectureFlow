# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
# Additional permission under AGPL-3.0 section 7: see LICENSE-EXCEPTION.md
"""LectureFlow OpenVINO Whisper worker (SPEC-v2 I7; phase 3b brief §4).

Usage (interpreter = config python_openvino, i.e. .venv-ov):
    python worker/asr_openvino.py --in job.json --out result.json.tmp
    python worker/asr_openvino.py --probe

Same protocol as worker/asr.py (SPEC §6.0):
  * Exit code 0 = success; 2 = input error, or the device / environment is not usable
    (the caller must not retry); anything else (1) = retryable.
  * stdout (UTF-8, one line per event, flushed immediately):
      "PROGRESS 0.42"            fraction transcribed; the first line comes once the
                                 model is loaded, never more than 0.95 before the end,
                                 "PROGRESS 1.00" after result.json is written
      "STATUS preparing_model"   the converted model is missing: downloading/converting
      "STATUS compiling_model"   GPU / NPU: no compiled blob in cache_dir yet
      "ERROR_CODE <name> [json]" at most one, just before a failing exit: device_unavailable
                                 (exit 2), model_prepare_failed (exit 1 for download /
                                 conversion errors, 2 when optimum / torch are missing),
                                 no_space {"free_bytes", "need_bytes"} (exit 2)
      "PROBE {json}"             --probe only (devices.OVProbe)
      anything else              plain log line
  * stderr: diagnostics; on failure the last line is "ERROR: <reason>".
  * Only the --out path is written (the caller renames it). The worker never starts
    subprocesses (the model conversion runs optimum-intel in this process) and never
    touches the database. Cancelling = the caller kills the process; nothing is written
    to --out before the transcription has finished.

job.json: {"audio", "model", "precision" (fp16|int8|int4), "model_dir",
           "device" (GPU|NPU|CPU; GPU = the Intel GPU, vendor 0x8086), "language",
           "initial_prompt", "cache_dir" ("" = no CACHE_DIR), "hf_id", "hf_home",
           "word_timestamps" (default true), "min_free_bytes" (default 10 GiB),
           optional "checkpoint" ({"path", "job_id", "input_hash", "device_id", "model",
           "precision", "chunk_s"}, power brief §4)}
result.json: as worker/asr.py, plus "timing": {"load_s", "transcribe_s", "audio_s" (the
             audio this run transcribed), "first_compile", "peak_working_set_mb"}.
             low_conf_words is always [] (OpenVINO gives no word probabilities). Without
             word timestamps the sentences have no "words" (the Go side interpolates,
             SPEC-v2 P4.4). "parts" when job.checkpoint is set (asr_common.Checkpointer).

Checkpoints (power brief §4): generate() transcribes its whole input at once and gives
no timed partial results, so checkpoints need chunks. checkpoint.chunk_s = 0: no
chunks and no checkpoint file (one generate() over the lecture, as without a checkpoint
object). chunk_s > 0: the lecture is cut at the quietest point within +-10 s of every
chunk_s seconds (next_cut), each chunk is one generate() (initial_prompt on the first
chunk only), and the checkpoint is written after every chunk but the last. A matching
checkpoint is continued from its end (any chunk_s).

Sentence splitting, the repeat filter, prompt truncation, result writing and the
checkpoint file come from worker/asr_common.py, the same code as worker/asr.py (I7).
openvino, openvino_genai, numpy and optimum are imported lazily, so the module imports in
any environment.
"""
from __future__ import annotations

import argparse
import gc
import glob
import json
import math
import os
import re
import shutil
import sys
import time
import traceback
import wave
from collections import namedtuple
from typing import Callable, List, Optional, Sequence, Tuple

from asr_common import (  # noqa: F401  (re-exported: one implementation, I11)
    EXIT_INPUT, EXIT_OK, EXIT_RETRY, SAMPLE_RATE, ArgParser, InputError, Word, _sentence_dict,
    _word_times, as_word, emit_stdout, fail, format_progress, is_sentence_end, make_timing,
    remove_repeats, setup_stdio, split_sentences, split_word_groups, truncate_prompt,
    write_result,
)
from asr_common import (
    BACKEND_OPENVINO, Checkpointer, Resume, audio_ms, clock, encode_segment,
    parse_checkpoint_job, samples_at,
)

PRECISIONS = ("fp16", "int8", "int4")
OV_DEVICES = ("GPU", "NPU", "CPU")
INTEL_VENDOR = "0x8086"

# Device kinds and probe reasons (Go: model.DeviceKind*, devices.ProbeReason*).
KIND_IGPU, KIND_NPU, KIND_CPU = "igpu", "npu", "cpu"
REASON_NOT_INTEL = "not_intel"
REASON_SECOND_INTEL_GPU = "second_intel_gpu"
REASON_ERROR = "error"

# The files of a converted model (Go: devices.ModelFiles): all present and non-empty
# = ready.
MODEL_FILES = (
    "config.json", "generation_config.json", "preprocessor_config.json",
    "openvino_encoder_model.xml", "openvino_encoder_model.bin",
    "openvino_decoder_model.xml", "openvino_decoder_model.bin",
    "openvino_tokenizer.xml", "openvino_tokenizer.bin",
    "openvino_detokenizer.xml", "openvino_detokenizer.bin",
)
DEFAULT_MIN_FREE_BYTES = 10 << 30          # Go: devices.PrepareMinFreeBytes
EXPORT_TASK = "automatic-speech-recognition-with-past"   # as the 3a conversions

# Progress: the streamer fires about once per 30-second window (3a), without times.
WINDOW_SECONDS = 30.0
PROGRESS_CAP = 0.95

# Word timing repair without VAD (3a: words next to silences are stretched to 7-19 s).
FRAME_SECONDS = 0.02
MAX_WORD_SECONDS = 2.0         # longer words are trimmed to their voiced frames
WORD_PAD_SECONDS = 0.05
VOICED_REL = 0.2               # voiced = above noise + 20% of (speech - noise) level
SILENCE_FLOOR_RMS = 0.001      # -60 dBFS: a whole segment below this is silence

# Checkpoint chunks (checkpoint.chunk_s > 0; docs/benchmarks/power-checkpoint.md §2.1):
# the cut is the middle of the quietest 0.4 s (CUT_SMOOTH_FRAMES frames on either side of
# a frame boundary, mean RMS) within +-CUT_SEARCH_SECONDS of the target; the last chunk
# is at least MIN_LAST_CHUNK_SECONDS long.
CUT_SEARCH_SECONDS = 10.0
CUT_SMOOTH_FRAMES = 10
MIN_LAST_CHUNK_SECONDS = 10.0


class WorkerError(Exception):
    """A failure with an exit code and, when code is set, an ERROR_CODE line."""

    def __init__(self, code: Optional[str], message: str, exit_code: int,
                 params: Optional[dict] = None):
        super().__init__(message)
        self.code = code
        self.exit_code = exit_code
        self.params = params


# ---------------------------------------------------------------------------
# Job input and audio
# ---------------------------------------------------------------------------

# checkpoint (power brief §4): asr_common.CheckpointJob or None (default).
Job = namedtuple("Job", ["audio", "model", "precision", "model_dir", "device", "language",
                         "initial_prompt", "cache_dir", "hf_id", "hf_home",
                         "word_timestamps", "min_free_bytes", "checkpoint"],
                 defaults=(None,))


def _opt_str(data: dict, key: str) -> str:
    v = data.get(key)
    if v is None:
        return ""
    if not isinstance(v, str):
        raise InputError("job.%s must be a string" % key)
    return v


def _req_str(data: dict, key: str) -> str:
    v = data.get(key)
    if not isinstance(v, str) or not v:
        raise InputError("job.%s must be a non-empty string" % key)
    return v


def parse_job(data) -> Job:
    if not isinstance(data, dict):
        raise InputError("job.json must contain a JSON object")
    audio = _req_str(data, "audio")
    model = _req_str(data, "model")
    precision = data.get("precision")
    if precision not in PRECISIONS:
        raise InputError("unsupported precision %r (expected fp16, int8 or int4)"
                         % (precision,))
    model_dir = _req_str(data, "model_dir")
    device = data.get("device")
    if device not in OV_DEVICES:
        raise InputError("unsupported device %r (expected GPU, NPU or CPU)" % (device,))
    language = _opt_str(data, "language")
    prompt = _opt_str(data, "initial_prompt")
    cache_dir = _opt_str(data, "cache_dir")
    hf_id = _opt_str(data, "hf_id")
    hf_home = _opt_str(data, "hf_home")
    word_ts = data.get("word_timestamps", True)
    if not isinstance(word_ts, bool):
        raise InputError("job.word_timestamps must be true or false")
    min_free = data.get("min_free_bytes", DEFAULT_MIN_FREE_BYTES)
    if isinstance(min_free, bool) or not isinstance(min_free, int) or min_free < 0:
        raise InputError("job.min_free_bytes must be a non-negative integer")
    checkpoint = parse_checkpoint_job(data.get("checkpoint"))
    return Job(audio, model, precision, model_dir, device, language or None, prompt,
               cache_dir, hf_id, hf_home, word_ts, min_free, checkpoint)


def load_job(path: str) -> Job:
    try:
        with open(path, encoding="utf-8-sig") as f:
            data = json.load(f)
    except FileNotFoundError:
        raise InputError("job file not found: %s" % path)
    except (OSError, ValueError) as e:
        raise InputError("cannot read job file %s: %s" % (path, e))
    job = parse_job(data)
    if not os.path.isfile(job.audio):
        raise InputError("audio file not found: %s" % job.audio)
    return job


def read_wav(path: str):
    """Reads a 16 kHz PCM wav (the prep stage's audio.wav) to mono float32 in [-1, 1].

    Standard library wave + numpy (no faster_whisper / PyAV in .venv-ov). 8-, 16-, 24-
    and 32-bit PCM; several channels are averaged. Anything else is an input error.
    """
    import numpy as np
    try:
        with wave.open(path, "rb") as w:
            channels, width, rate = w.getnchannels(), w.getsampwidth(), w.getframerate()
            raw = w.readframes(w.getnframes())
    except (wave.Error, EOFError, OSError, ValueError) as e:
        raise InputError("cannot decode audio %s: %s" % (path, e))
    if rate != SAMPLE_RATE:
        raise InputError("cannot decode audio %s: expected %d Hz, got %d Hz"
                         % (path, SAMPLE_RATE, rate))
    n = len(raw) // (width * channels) * (width * channels)
    raw = raw[:n]
    if width == 2:
        a = np.frombuffer(raw, dtype="<i2").astype(np.float32) / 32768.0
    elif width == 1:
        a = (np.frombuffer(raw, dtype=np.uint8).astype(np.float32) - 128.0) / 128.0
    elif width == 4:
        a = (np.frombuffer(raw, dtype="<i4").astype(np.float64) / 2147483648.0).astype(
            np.float32)
    elif width == 3:
        b = np.frombuffer(raw, dtype=np.uint8).reshape(-1, 3).astype(np.int32)
        v = b[:, 0] | (b[:, 1] << 8) | (b[:, 2] << 16)
        v = np.where(v >= 1 << 23, v - (1 << 24), v)
        a = v.astype(np.float32) / 8388608.0
    else:
        raise InputError("cannot decode audio %s: unsupported sample width %d"
                         % (path, width))
    del raw
    if channels > 1:
        a = a.reshape(-1, channels).mean(axis=1)
    if len(a) == 0:
        raise InputError("cannot decode audio %s: no samples" % path)
    return np.ascontiguousarray(a, dtype=np.float32)


# ---------------------------------------------------------------------------
# OpenVINO: import, devices, probe
# ---------------------------------------------------------------------------

def import_openvino():
    """Imports openvino and openvino_genai (lazily: the module itself needs neither)."""
    try:
        import openvino as ov
        import openvino_genai as genai
    except Exception as e:  # ImportError, or OSError when a DLL fails to load
        raise WorkerError(None, "cannot import openvino / openvino_genai: %s: %s"
                          % (type(e).__name__, e), EXIT_INPUT) from e
    return ov, genai


def _supported(core, name: str) -> set:
    return {str(p) for p in core.get_property(name, "SUPPORTED_PROPERTIES")}


def _prop(core, name: str, supported: set, key: str) -> str:
    if key not in supported:
        return ""
    try:
        v = core.get_property(name, key)
    except Exception:
        return ""
    return "" if v is None else str(v)


def _vendor_of(arch: str, full_name: str) -> str:
    m = re.search(r"vendor\s*=\s*(0x[0-9a-fA-F]+)", arch or "")
    if m:
        return m.group(1).lower()
    fn = (full_name or "").lower()
    if "intel" in fn:
        return INTEL_VENDOR
    if "nvidia" in fn:
        return "0x10de"
    if "amd" in fn or "radeon" in fn:
        return "0x1002"
    return ""


def _device_entry(core, name: str) -> dict:
    """{name, kind (filled by the caller), full_name, vendor, luid, driver_version}."""
    base = name.split(".")[0]
    sp = _supported(core, name)
    full_name = str(core.get_property(name, "FULL_DEVICE_NAME"))
    if base == "GPU":
        vendor = _vendor_of(_prop(core, name, sp, "DEVICE_ARCHITECTURE"), full_name)
        driver = _prop(core, name, sp, "GPU_DRIVER_VERSION")
    elif base == "NPU":
        vendor = INTEL_VENDOR            # OpenVINO's NPU plugin drives Intel NPUs only
        driver = _prop(core, name, sp, "NPU_DRIVER_VERSION")
    else:
        fn = full_name.lower()
        vendor = INTEL_VENDOR if "intel" in fn else ("0x1022" if "amd" in fn else "")
        driver = ""
    luid = _prop(core, name, sp, "DEVICE_LUID").lower() if base in ("GPU", "NPU") else ""
    return {"name": name, "kind": "", "full_name": full_name, "vendor": vendor,
            "luid": luid, "driver_version": driver}


def classify_devices(core) -> Tuple[List[dict], List[dict]]:
    """(devices, ignored) of OpenVINO's device list (brief 3b §4 --probe, ruling 10).

    devices: at most one per kind, in OpenVINO's order: the first Intel GPU (vendor
    0x8086) as igpu, the NPU, the CPU. ignored: GPUs of other vendors (not_intel, e.g.
    the NVIDIA card OpenVINO lists as GPU.1), further Intel GPUs (second_intel_gpu)
    and devices whose properties cannot be read (error). Other device types are left
    out.
    """
    devices: List[dict] = []
    ignored: List[dict] = []
    seen = set()
    for name in list(core.available_devices):
        base = str(name).split(".")[0]
        kind = {"GPU": KIND_IGPU, "NPU": KIND_NPU, "CPU": KIND_CPU}.get(base)
        if kind is None:
            continue
        try:
            entry = _device_entry(core, name)
        except Exception as e:
            ignored.append({"name": name, "kind": "", "full_name": "", "vendor": "",
                            "luid": "", "driver_version": "", "reason": REASON_ERROR,
                            "error": ("%s: %s" % (type(e).__name__, e))[:300]})
            continue
        if kind == KIND_IGPU and entry["vendor"] != INTEL_VENDOR:
            ignored.append(dict(entry, reason=REASON_NOT_INTEL))
        elif kind in seen:
            if kind == KIND_IGPU:
                ignored.append(dict(entry, reason=REASON_SECOND_INTEL_GPU))
        else:
            seen.add(kind)
            devices.append(dict(entry, kind=kind))
    return devices, ignored


def resolve_device(core, device: str) -> str:
    """job.device (GPU|NPU|CPU) -> OpenVINO device name; GPU is the Intel GPU found by
    vendor (never OpenVINO's bare "GPU", which may be another vendor's card)."""
    kind = {"GPU": KIND_IGPU, "NPU": KIND_NPU, "CPU": KIND_CPU}[device]
    devices, _ = classify_devices(core)
    for d in devices:
        if d["kind"] == kind:
            return d["name"]
    raise WorkerError("device_unavailable", "OpenVINO device %s not found (available: %s)"
                      % (device, ", ".join(map(str, core.available_devices)) or "none"),
                      EXIT_INPUT)


def can_convert() -> bool:
    """optimum-intel imports and torch is installed: missing models can be prepared."""
    import importlib.util
    try:
        import optimum.intel  # noqa: F401
    except Exception:
        return False
    try:
        return importlib.util.find_spec("torch") is not None
    except Exception:
        return False


def probe(emit: Callable[[str], None] = emit_stdout) -> None:
    ov, genai = import_openvino()
    core = ov.Core()
    devices, ignored = classify_devices(core)
    out = {"openvino": str(getattr(ov, "__version__", "")),
           "genai": str(getattr(genai, "__version__", "")),
           "devices": devices, "ignored": ignored, "can_convert": can_convert()}
    emit("PROBE " + json.dumps(out, ensure_ascii=False))


# ---------------------------------------------------------------------------
# Model preparation (download + conversion, lead rulings 4 and 11)
# ---------------------------------------------------------------------------

def model_ready(model_dir: str) -> bool:
    """Every MODEL_FILES entry is a non-empty file (same rule as devices.ModelReady)."""
    for f in MODEL_FILES:
        p = os.path.join(model_dir, f)
        try:
            if not os.path.isfile(p) or os.path.getsize(p) == 0:
                return False
        except OSError:
            return False
    return True


def compile_cached(cache_dir: str) -> bool:
    """cache_dir holds a non-empty *.blob (same rule as devices.CompileCached)."""
    if not cache_dir or not os.path.isdir(cache_dir):
        return False
    try:
        for e in os.scandir(cache_dir):
            if e.is_file() and e.name.lower().endswith(".blob") and e.stat().st_size > 0:
                return True
    except OSError:
        return False
    return False


def disk_free(path: str) -> int:
    """Free bytes on the drive of path (its nearest existing ancestor)."""
    p = os.path.abspath(path)
    while not os.path.exists(p):
        parent = os.path.dirname(p)
        if parent == p:
            break
        p = parent
    return int(shutil.disk_usage(p).free)


def _pid_alive(pid: int) -> bool:
    if pid <= 0:
        return False
    if sys.platform == "win32":
        import ctypes
        from ctypes import wintypes
        k32 = ctypes.WinDLL("kernel32", use_last_error=True)
        k32.OpenProcess.restype = wintypes.HANDLE
        k32.OpenProcess.argtypes = [wintypes.DWORD, wintypes.BOOL, wintypes.DWORD]
        h = k32.OpenProcess(0x1000, False, pid)        # PROCESS_QUERY_LIMITED_INFORMATION
        if not h:
            return ctypes.get_last_error() == 5           # access denied: it exists
        try:
            code = wintypes.DWORD()
            if k32.GetExitCodeProcess(h, ctypes.byref(code)):
                return code.value == 259                  # STILL_ACTIVE
            return True
        finally:
            k32.CloseHandle(h)
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True


def remove_stale_temp(model_dir: str, emit: Callable[[str], None]) -> None:
    """Removes {model_dir}.tmp-{pid} / .old-{pid} left by killed workers (cancel = kill)."""
    for p in glob.glob(glob.escape(model_dir) + ".tmp-*") + \
            glob.glob(glob.escape(model_dir) + ".old-*"):
        m = re.search(r"\.(?:tmp|old)-(\d+)$", p)
        if not m or int(m.group(1)) == os.getpid() or _pid_alive(int(m.group(1))):
            continue
        shutil.rmtree(p, ignore_errors=True)
        emit("removed stale %s" % os.path.basename(p))


def hf_repo_dir(hf_home: str, hf_id: str) -> str:
    """huggingface_hub's cache folder of a model under HF_HOME."""
    return os.path.join(hf_home, "hub", "models--" + hf_id.replace("/", "--"))


def download_marker(hf_home: str, hf_id: str) -> str:
    """Marks a Hugging Face download this worker started (deleted after conversion)."""
    return os.path.join(hf_home, "lectureflow_downloads", "models--" + hf_id.replace("/", "--"))


def set_hf_env(hf_home: str) -> None:
    """Points huggingface_hub at hf_home; must run before huggingface_hub is imported."""
    os.environ["HF_HOME"] = hf_home
    for key in ("HF_HUB_CACHE", "HUGGINGFACE_HUB_CACHE", "TRANSFORMERS_CACHE"):
        os.environ.pop(key, None)
    os.environ.setdefault("HF_HUB_DISABLE_TELEMETRY", "1")
    os.environ.setdefault("HF_HUB_DISABLE_SYMLINKS_WARNING", "1")
    os.environ.setdefault("HF_HUB_DISABLE_PROGRESS_BARS", "1")


class _ExportArgParser(argparse.ArgumentParser):
    def error(self, message):
        raise ValueError("optimum export arguments: %s" % message)


def export_model(hf_id: str, precision: str, out_dir: str, cache_dir: str) -> None:
    """`optimum-cli export openvino --model {hf_id} --task automatic-speech-recognition-
    with-past --weight-format {precision} {out_dir}` (the 3a conversion), run in this
    process through optimum-intel's command class, so int8 / int4 get exactly the CLI's
    quantization settings. The tokenizer and detokenizer are converted too (default)."""
    from optimum.commands.export.openvino import OVExportCommand, parse_args_openvino
    parser = _ExportArgParser(prog="optimum-cli export openvino")
    parse_args_openvino(parser)
    args = parser.parse_args(["--model", hf_id, "--task", EXPORT_TASK,
                              "--weight-format", precision, "--cache_dir", cache_dir,
                              out_dir])
    OVExportCommand(None, args, from_defaults_factory=True, parser=parser).run()


def _delete_download(hf_home: str, hf_id: str, emit: Callable[[str], None]) -> None:
    repo = hf_repo_dir(hf_home, hf_id)
    gc.collect()                         # release memory-mapped weights (Windows locks)
    shutil.rmtree(repo, ignore_errors=True)
    shutil.rmtree(os.path.join(hf_home, "hub", ".locks", os.path.basename(repo)),
                  ignore_errors=True)
    if os.path.isdir(repo):
        emit("could not delete the downloaded weights in %s (retried next run)" % repo)
        return
    try:
        os.remove(download_marker(hf_home, hf_id))
    except OSError:
        pass
    emit("deleted the downloaded weights of %s" % hf_id)


def _install(tmp: str, target: str, emit: Callable[[str], None]) -> None:
    """Atomically renames tmp to target; an existing complete target is reused."""
    try:
        os.replace(tmp, target)
        return
    except OSError:
        if model_ready(target):
            emit("%s was prepared meanwhile; reusing it" % os.path.basename(target))
            return
        if not os.path.exists(target):
            raise
    aside = "%s.old-%d" % (target, os.getpid())    # an incomplete older copy
    os.replace(target, aside)
    try:
        os.replace(tmp, target)
    except OSError:
        if not model_ready(target):
            raise
    finally:
        shutil.rmtree(aside, ignore_errors=True)


def ensure_model(job: Job, emit: Callable[[str], None]) -> bool:
    """Makes job.model_dir a converted model; returns True when it was prepared now.

    Not ready: "STATUS preparing_model" -> free space on the model's drive must be at
    least job.min_free_bytes (else ERROR_CODE no_space) -> HF_HOME = job.hf_home ->
    optimum-intel exports into {model_dir}.tmp-{pid} -> os.replace to model_dir (a
    complete model_dir that appeared meanwhile is reused, lead ruling 11) -> the
    Hugging Face download is deleted when this worker started it (a marker file
    remembers that across retries); downloads that were already there (3a) stay.
    """
    hf_home, hf_id = job.hf_home, job.hf_id
    if model_ready(job.model_dir):
        if hf_home and hf_id and os.path.isfile(download_marker(hf_home, hf_id)):
            _delete_download(hf_home, hf_id, emit)   # left over by an earlier run
        return False
    emit("STATUS preparing_model")
    if not hf_id or not hf_home:
        raise InputError("model %s is not converted (%s) and job.hf_id / job.hf_home are "
                         "missing" % (job.model, job.model_dir))
    free = disk_free(job.model_dir)
    if free < job.min_free_bytes:
        raise WorkerError("no_space", "not enough free space to prepare the model: %d bytes "
                          "free, %d needed" % (free, job.min_free_bytes), EXIT_INPUT,
                          {"free_bytes": free, "need_bytes": job.min_free_bytes})
    parent = os.path.dirname(os.path.abspath(job.model_dir))
    os.makedirs(parent, exist_ok=True)
    remove_stale_temp(job.model_dir, emit)
    set_hf_env(hf_home)
    marker = download_marker(hf_home, hf_id)
    owned = not os.path.isdir(hf_repo_dir(hf_home, hf_id)) or os.path.isfile(marker)
    if owned:
        os.makedirs(os.path.dirname(marker), exist_ok=True)
        with open(marker, "w", encoding="utf-8") as f:
            f.write(hf_id + "\n")
    tmp = "%s.tmp-%d" % (job.model_dir, os.getpid())
    shutil.rmtree(tmp, ignore_errors=True)
    emit("preparing %s (%s) from %s" % (job.model, job.precision, hf_id))
    t0 = time.monotonic()
    try:
        try:
            export_model(hf_id, job.precision, tmp, os.path.join(hf_home, "hub"))
        except ImportError as e:
            raise WorkerError("model_prepare_failed", "converting a model needs optimum-intel "
                              "and torch in the OpenVINO environment: %s" % e, EXIT_INPUT) from e
        except Exception as e:
            raise WorkerError("model_prepare_failed", "cannot download or convert %s: %s: %s"
                              % (hf_id, type(e).__name__, e), EXIT_RETRY) from e
        if not model_ready(tmp):
            missing = [f for f in MODEL_FILES if not os.path.isfile(os.path.join(tmp, f))]
            raise WorkerError("model_prepare_failed", "the conversion of %s left out %s"
                              % (hf_id, ", ".join(missing) or "non-empty files"), EXIT_RETRY)
        _install(tmp, job.model_dir, emit)
    finally:
        shutil.rmtree(tmp, ignore_errors=True)
    emit("model %s (%s) prepared in %.1fs" % (job.model, job.precision, time.monotonic() - t0))
    if owned:
        _delete_download(hf_home, hf_id, emit)
    return True


# ---------------------------------------------------------------------------
# Words: pieces, silence, long words, order
# ---------------------------------------------------------------------------

def glue_pieces(raw: Sequence[Tuple[str, float, float]]) -> List[Word]:
    """OpenVINO splits some words into pieces without a leading space ("two" + "-time");
    faster-whisper starts every word with a space. A piece without one is appended to
    the previous word (its end extended) so splitting sees the same units."""
    out: List[Word] = []
    for text, start, end in raw:
        if not text or not text.strip():
            continue
        if out and not text[:1].isspace():
            p = out[-1]
            out[-1] = p._replace(word=p.word + text, end=max(p.end, float(end)))
            continue
        out.append(Word(text if text[:1].isspace() else " " + text, float(start),
                        float(end), 1.0))
    return out


def frame_rms(audio):
    """RMS of FRAME_SECONDS frames (the last, shorter frame included)."""
    import numpy as np
    n = int(round(FRAME_SECONDS * SAMPLE_RATE))
    full = len(audio) // n
    parts = []
    block = 3000                              # frames per block (60 s): bounded temporaries
    for i in range(0, full, block):
        x = audio[i * n:min(full, i + block) * n].reshape(-1, n)
        parts.append(np.sqrt(np.mean(np.square(x, dtype=np.float32), axis=1)))
    if len(audio) > full * n:
        tail = audio[full * n:]
        parts.append(np.array([math.sqrt(float(np.mean(np.square(tail))))], dtype=np.float32))
    if not parts:
        return np.zeros(0, dtype=np.float32)
    return np.concatenate(parts)


def voiced_threshold(frames) -> float:
    import numpy as np
    if len(frames) == 0:
        return SILENCE_FLOOR_RMS
    lo, hi = (float(v) for v in np.percentile(frames, [10, 90]))
    return max(SILENCE_FLOOR_RMS, lo + VOICED_REL * (hi - lo))


def _frame_range(frames, start: float, end: float) -> Tuple[int, int]:
    i0 = max(0, int(math.floor(start / FRAME_SECONDS)))
    i1 = min(len(frames), int(math.ceil(end / FRAME_SECONDS)))
    return i0, max(i0, i1)


def silent_spans(chunks, frames) -> List[Tuple[float, float]]:
    """Segments whose loudest frame is below SILENCE_FLOOR_RMS (-60 dBFS): with no VAD,
    only such truly silent segments are treated as hallucinations (rather keep, §0.1)."""
    spans = []
    for start, end, _ in chunks:
        i0, i1 = _frame_range(frames, start, end)
        if i1 > i0 and float(frames[i0:i1].max()) < SILENCE_FLOOR_RMS:
            spans.append((start, end))
    return spans


def drop_silent(words: Sequence[Word], spans) -> List[Word]:
    if not spans:
        return list(words)
    out = []
    for w in words:
        mid = (w.start + w.end) / 2.0
        if not any(s <= mid <= e for s, e in spans):
            out.append(w)
    return out


def clamp_long_words(words: Sequence[Word], frames, threshold: float) -> List[Word]:
    """Words longer than MAX_WORD_SECONDS are trimmed to their voiced frames (+ a small
    pad); still too long, MAX_WORD_SECONDS are kept on the louder side (the end when
    nothing is voiced: stretched words mostly start at a window start, 3a)."""
    import numpy as np
    out = []
    for w in words:
        s, e = w.start, w.end
        if e - s > MAX_WORD_SECONDS:
            i0, i1 = _frame_range(frames, s, e)
            seg = frames[i0:i1]
            voiced = np.nonzero(seg >= threshold)[0]
            if len(voiced):
                s = max(s, (i0 + int(voiced[0])) * FRAME_SECONDS - WORD_PAD_SECONDS)
                e = min(e, (i0 + int(voiced[-1]) + 1) * FRAME_SECONDS + WORD_PAD_SECONDS)
            if e - s > MAX_WORD_SECONDS:
                a0, a1 = _frame_range(frames, s, s + MAX_WORD_SECONDS)
                b0, b1 = _frame_range(frames, e - MAX_WORD_SECONDS, e)
                head = float(np.sum(frames[a0:a1] >= threshold))
                tail = float(np.sum(frames[b0:b1] >= threshold))
                if head > tail:
                    e = s + MAX_WORD_SECONDS
                else:
                    s = e - MAX_WORD_SECONDS
            w = w._replace(start=s, end=e)
        out.append(w)
    return out


def make_monotonic(words: Sequence[Word], duration: float) -> List[Word]:
    """Times inside [0, duration], starts never decreasing, no overlap (an earlier word
    ending after the next one starts is cut there), end >= start."""
    out: List[Word] = []
    for w in words:
        s = min(max(0.0, w.start), duration)
        e = min(max(s, w.end), duration)
        if out:
            p = out[-1]
            if s < p.start:
                s = p.start
                e = max(e, s)
            if p.end > s:
                out[-1] = p._replace(end=max(p.start, s))
        out.append(w._replace(start=s, end=e))
    return out


def words_from_chunks(chunks) -> List[Word]:
    """Pseudo-words of segments (no word timestamps): times by character position, only
    for sentence splitting; the result then carries no "words" (P4.4 interpolation)."""
    out: List[Word] = []
    for start, end, text in chunks:
        toks = (text or "").split()
        total = sum(len(t) for t in toks)
        if not toks or total == 0:
            continue
        span = max(0.0, end - start)
        pos = 0
        for t in toks:
            ts = start + span * pos / total
            pos += len(t)
            out.append(Word(" " + t, ts, start + span * pos / total, 1.0))
    return out


def chunk_words(res, start: float, end: float, frames, threshold: float,
                word_timestamps: bool, emit: Callable[[str], None]):
    """The words of one WhisperDecodedResults for the audio [start, end) s of the lecture,
    on the lecture's timeline: pieces glued, words of silent segments dropped, long words
    trimmed (frames and threshold of the whole lecture), not yet ordered (make_monotonic)
    nor de-duplicated. A chunk's first word always starts with a space (glue_pieces adds
    one), so it is never glued onto the previous chunk's last word. Returns (words,
    with_words, counts): with_words False = pseudo-words from the segments (no word
    timestamps); counts = [segments, silent segments, glued pieces, dropped silent words
    or segments]."""
    chunks = [(start + float(c.start_ts), start + float(c.end_ts), str(c.text))
              for c in (res.chunks or [])]
    raw = [(str(w.word), start + float(w.start_ts), start + float(w.end_ts))
           for w in (res.words or [])] if word_timestamps else []
    if not chunks and not raw:
        text = (res.texts[0] if getattr(res, "texts", None) else "") or ""
        if text.strip():
            chunks = [(start, end, text)]
    spans = silent_spans(chunks, frames)
    if raw:
        words = glue_pieces(raw)
        n_glued = len(raw) - len(words)
        n_pieces = len(words)
        words = drop_silent(words, spans)
        n_silent = n_pieces - len(words)
        words = clamp_long_words(words, frames, threshold)
        return words, True, [len(chunks), len(spans), n_glued, n_silent]
    if word_timestamps and chunks:
        emit("no word timestamps returned; sentences from segments")
    silent = set(spans)
    kept = [c for c in chunks if (c[0], c[1]) not in silent]
    return words_from_chunks(kept), False, [len(chunks), len(spans), 0, len(chunks) - len(kept)]


def finish_sentences(words: Sequence[Word], duration: float, with_words: bool, counts,
                     emit: Callable[[str], None]) -> List[dict]:
    """Orders the words (make_monotonic), removes repeats, splits sentences; without
    word timestamps the sentences carry no "words"."""
    words = make_monotonic(words, duration)
    n_before = len(words)
    words = remove_repeats(words)
    sentences = split_sentences(words)
    if not with_words:
        for s in sentences:
            s.pop("words", None)
    emit("segments %d (%d silent), words %d (%d pieces glued, %d silent dropped, "
         "%d repeated removed), sentences %d"
         % (counts[0], counts[1], n_before, counts[2], counts[3], n_before - len(words),
            len(sentences)))
    return sentences


def sentences_from_result(res, audio, word_timestamps: bool,
                          emit: Callable[[str], None]) -> List[dict]:
    """result.json sentences of a WhisperDecodedResults (brief 3b §4 "转写")."""
    duration = len(audio) / float(SAMPLE_RATE)
    frames = frame_rms(audio)
    words, with_words, counts = chunk_words(res, 0.0, duration, frames,
                                            voiced_threshold(frames), word_timestamps, emit)
    return finish_sentences(words, duration, with_words, counts, emit)


def next_cut(frames, start: int, n: int, chunk_s: float) -> int:
    """End sample of the checkpoint chunk that starts at sample `start` (n = all samples).

    The rest of the audio when it is at most chunk_s + CUT_SEARCH_SECONDS long. Otherwise
    the 20 ms frame boundary between max(start + chunk_s / 2, target - 10 s) and
    min(target + 10 s, end - MIN_LAST_CHUNK_SECONDS) (target = start + chunk_s) where the
    mean RMS of the CUT_SMOOTH_FRAMES frames on either side (0.4 s) is lowest. Several
    boundaries in a row with the same lowest value (a pause of digital silence) count as
    one candidate at their middle; of equal candidates the one nearest the target wins.
    Frames are those of the whole lecture, so the cuts after a resume are the cuts of an
    uninterrupted run. Boundaries are multiples of 20 ms: a cut is whole milliseconds."""
    import numpy as np
    step = int(round(FRAME_SECONDS * SAMPLE_RATE))
    if (n - start) / float(SAMPLE_RATE) <= chunk_s + CUT_SEARCH_SECONDS:
        return n
    start_s = start / float(SAMPLE_RATE)
    target = start_s + chunk_s
    lo = max(start_s + chunk_s / 2.0, target - CUT_SEARCH_SECONDS)
    hi = min(target + CUT_SEARCH_SECONDS, n / float(SAMPLE_RATE) - MIN_LAST_CHUNK_SECONDS)
    ks = np.arange(int(math.ceil(lo / FRAME_SECONDS - 1e-9)),
                   int(math.floor(hi / FRAME_SECONDS + 1e-9)) + 1)
    csum = np.concatenate([[0.0], np.cumsum(frames, dtype=np.float64)])
    a = np.clip(ks - CUT_SMOOTH_FRAMES, 0, len(frames))
    b = np.clip(ks + CUT_SMOOTH_FRAMES, 0, len(frames))
    mean = (csum[b] - csum[a]) / np.maximum(b - a, 1)
    low = np.flatnonzero(mean <= mean.min())
    gaps = np.flatnonzero(np.diff(low) > 1)
    first = low[np.concatenate([[0], gaps + 1])]
    last = low[np.concatenate([gaps, [len(low) - 1]])]
    middles = (ks[first] + ks[last]) // 2
    k = int(middles[np.argmin(np.abs(middles * FRAME_SECONDS - target))])
    return min(n, k * step)


# ---------------------------------------------------------------------------
# Transcription
# ---------------------------------------------------------------------------

def load_pipeline(genai, job: Job, ov_device: str):
    props = {}
    if job.word_timestamps:
        props["word_timestamps"] = True
    if job.cache_dir:
        os.makedirs(job.cache_dir, exist_ok=True)
        props["CACHE_DIR"] = job.cache_dir
    return genai.WhisperPipeline(job.model_dir, ov_device, **props)


def make_token_counter(pipe) -> Callable[[str], int]:
    """Counts tokens like the initial prompt is encoded (" " + prompt.strip())."""
    tok = pipe.get_tokenizer()

    def count(text: str) -> int:
        ids = tok.encode(" " + text.strip(), add_special_tokens=False).input_ids
        return int(ids.shape[1])

    return count


def generation_config(pipe, job: Job, prompt: str):
    """Greedy (GPU / NPU support no beam search; CPU too, as in 3a), transcribe,
    segment and word timestamps; initial_prompt only (G4: first window), no hotwords."""
    cfg = pipe.get_generation_config()
    if job.language:
        tag = "<|%s|>" % job.language
        known = getattr(cfg, "lang_to_id", None) or {}
        if known and tag not in known:
            raise InputError("unsupported language %r for model %s" % (job.language, job.model))
        cfg.language = tag
    cfg.task = "transcribe"
    cfg.return_timestamps = True
    cfg.word_timestamps = bool(job.word_timestamps)
    cfg.num_beams = 1
    if prompt:
        cfg.initial_prompt = prompt
    return cfg


class WindowProgress:
    """Streamer callback: about one call per 30-second window, so progress is
    calls x 30 s / duration, capped at PROGRESS_CAP; printed when it changes.

    Chunks (checkpoints): chunk(start, end) counts the calls from the chunk's start and
    never past its end; reach(pos) reports a position (a chunk done). `start` = where a
    resumed run begins (run() prints that line once the model is loaded)."""

    def __init__(self, duration: float, emit: Callable[[str], None], running=None,
                 start: float = 0.0):
        self.duration = duration
        self.emit = emit
        self.running = running
        self.calls = 0
        self.base = 0.0              # the current chunk's start (s)
        self.limit = None            # ... and end (None = the end of the audio)
        self.chunk_calls = 0
        self.last = format_progress(self._fraction(start) if start > 0 else 0.0)

    def _fraction(self, pos: float) -> float:
        if self.duration > 0:
            return min(PROGRESS_CAP, pos / self.duration)
        return PROGRESS_CAP

    def _show(self, p: float) -> None:
        line = format_progress(p)
        if line > self.last:             # same width ("PROGRESS 0.xx"): string order works
            self.last = line
            self.emit(line)

    def chunk(self, start: float, end: float) -> None:
        self.base, self.limit, self.chunk_calls = start, end, 0

    def reach(self, pos: float) -> None:
        self._show(self._fraction(pos))

    def __call__(self, _text):
        self.calls += 1
        self.chunk_calls += 1
        pos = self.base + self.chunk_calls * WINDOW_SECONDS
        if self.limit is not None:
            pos = min(pos, self.limit)
        self._show(self._fraction(pos))
        return self.running


def _streamer(genai, duration: float, emit: Callable[[str], None],
              start: float = 0.0) -> WindowProgress:
    status = getattr(genai, "StreamingStatus", None)
    return WindowProgress(duration, emit, status.RUNNING if status is not None else False,
                          start)


def transcribe_chunks(pipe, genai, audio, job: Job, emit: Callable[[str], None], prompt: str,
                      terms_used: int, checkpointer: Optional[Checkpointer],
                      resume: Optional[Resume], chunk_s: int) -> Tuple[List[dict], float]:
    """Sentences of the audio from the resume point (or 0) on, chunk by chunk (one chunk
    to the end when chunk_s is 0), and the transcribe seconds.

    Each chunk is one generate() of audio[a:b]; its words are moved onto the lecture's
    timeline and processed with the lecture's frames (chunk_words), so a resumed run
    computes the same chunks, words and sentences as an uninterrupted one on the same
    device. initial_prompt only reaches the first chunk of a run that starts at 0 (a
    resumed run has none, G4). After every chunk but the last the checkpoint is written:
    resume_ms = the chunk's end, the segments so far (one per chunk). Sentences keep
    their "words" exactly when job.word_timestamps is set (a chunk that returns no word
    timestamps then contributes the pseudo-words of its segments)."""
    n = len(audio)
    duration = n / float(SAMPLE_RATE)
    frames = frame_rms(audio)
    threshold = voiced_threshold(frames)
    cfg_first = generation_config(pipe, job, prompt)
    cfg_rest = generation_config(pipe, job, "") if prompt else cfg_first
    pos = samples_at(resume.resume_ms) if resume is not None else 0
    streamer = _streamer(genai, duration, emit, pos / float(SAMPLE_RATE))
    words: List[Word] = list(resume.words) if resume is not None else []
    segments: List[str] = list(resume.segments) if resume is not None else []
    counts = [0, 0, 0, 0]
    n_chunks = 0
    t0 = time.monotonic()
    while pos < n:
        end = next_cut(frames, pos, n, chunk_s) if chunk_s > 0 else n
        a, b = pos / float(SAMPLE_RATE), end / float(SAMPLE_RATE)
        streamer.chunk(a, b)
        res = pipe.generate(audio[pos:end], cfg_first if n_chunks == 0 else cfg_rest,
                            streamer=streamer)
        n_chunks += 1
        ws, _, c = chunk_words(res, a, b, frames, threshold, job.word_timestamps, emit)
        words.extend(ws)
        counts = [x + y for x, y in zip(counts, c)]
        if checkpointer is not None and end < n:
            segments.append(encode_segment(audio_ms(pos), audio_ms(end), ws))
            checkpointer.save(audio_ms(end), terms_used,
                              checkpointer.parts(resume, audio_ms(end)), segments)
        streamer.reach(b)
        pos = end
    transcribe_s = time.monotonic() - t0
    emit("audio duration %.1fs, transcribed in %.1fs (%d chunks, %d streamer calls%s)"
         % (duration, transcribe_s, n_chunks, streamer.calls,
            ", %d checkpoints written" % checkpointer.writes if checkpointer is not None
            else ""))
    sentences = finish_sentences(words, duration, job.word_timestamps, counts, emit)
    return sentences, transcribe_s


def transcribe(pipe, genai, audio, job: Job, emit: Callable[[str], None],
               count_tokens: Optional[Callable[[str], int]] = None,
               checkpointer: Optional[Checkpointer] = None,
               resume: Optional[Resume] = None) -> Tuple[dict, float]:
    """Returns ({"prompt_terms_used", "sentences"[, "parts"]}, transcribe seconds).

    Without checkpointer, or with checkpoint.chunk_s 0 and nothing to resume: one
    generate() over the whole lecture, as always (no checkpoint file); the checkpointer
    only adds "parts". Otherwise transcribe_chunks()."""
    if resume is not None:
        prompt, terms_used = "", resume.prompt_terms_used
        emit("continuing at %s without initial_prompt (first chunk only)"
             % clock(resume.resume_ms))
    else:
        if count_tokens is None:
            count_tokens = make_token_counter(pipe)
        prompt, terms_used = truncate_prompt(job.initial_prompt, count_tokens)
        if job.initial_prompt:
            emit("initial_prompt: %d terms, %d tokens"
                 % (terms_used, count_tokens(prompt) if prompt else 0))
    chunk_s = checkpointer.chunk_s if checkpointer is not None else 0
    if resume is None and chunk_s == 0:
        cfg = generation_config(pipe, job, prompt)
        duration = len(audio) / float(SAMPLE_RATE)
        streamer = _streamer(genai, duration, emit)
        t0 = time.monotonic()
        res = pipe.generate(audio, cfg, streamer=streamer)
        transcribe_s = time.monotonic() - t0
        emit("audio duration %.1fs, transcribed in %.1fs (%d streamer calls)"
             % (duration, transcribe_s, streamer.calls))
        sentences = sentences_from_result(res, audio, job.word_timestamps, emit)
    else:
        sentences, transcribe_s = transcribe_chunks(pipe, genai, audio, job, emit, prompt,
                                                    terms_used, checkpointer, resume, chunk_s)
    result = {"prompt_terms_used": terms_used, "sentences": sentences}
    if checkpointer is not None:
        result["parts"] = checkpointer.parts(resume, audio_ms(len(audio)))
    return result, transcribe_s


def run(in_path: str, out_path: str, emit: Callable[[str], None] = emit_stdout) -> None:
    job = load_job(in_path)
    audio = read_wav(job.audio)
    checkpointer = resume = None
    if job.checkpoint is not None:
        checkpointer = Checkpointer(job.checkpoint, BACKEND_OPENVINO, emit)
        resume = checkpointer.load(audio_ms(len(audio)))
    start = samples_at(resume.resume_ms) if resume is not None else 0
    ov, genai = import_openvino()
    core = ov.Core()
    ov_device = resolve_device(core, job.device)
    emit("device %s = %s" % (job.device, ov_device))
    prepared = ensure_model(job, emit)
    first_compile = job.device in ("GPU", "NPU") and not compile_cached(job.cache_dir)
    if first_compile:
        emit("STATUS compiling_model")
    t0 = time.monotonic()
    pipe = load_pipeline(genai, job, ov_device)
    load_s = time.monotonic() - t0
    emit("model %s (%s) loaded on %s in %.1fs%s" % (job.model, job.precision, ov_device,
                                                   load_s, " (compiled)" if first_compile else ""))
    # the first PROGRESS line: 0.00, or where a resumed run begins
    emit(format_progress(min(PROGRESS_CAP, start / float(len(audio)))))
    result, transcribe_s = transcribe(pipe, genai, audio, job, emit,
                                      checkpointer=checkpointer, resume=resume)
    # A run that converted the model peaked in torch, not in transcription: report 0
    # (unknown) so it does not inflate the device's observed memory.
    result["timing"] = make_timing(load_s, transcribe_s,
                                   (len(audio) - start) / float(SAMPLE_RATE),
                                   first_compile, 0 if prepared else None)
    write_result(out_path, result)
    emit(format_progress(1.0))


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def build_parser() -> argparse.ArgumentParser:
    p = ArgParser(prog="asr_openvino.py", description="LectureFlow OpenVINO Whisper worker")
    p.add_argument("--in", dest="in_path", help="job.json")
    p.add_argument("--out", dest="out_path", help="result file to write (e.g. result.json.tmp)")
    p.add_argument("--probe", action="store_true",
                   help='print "PROBE {json}" with the usable OpenVINO devices')
    return p


def main(argv: Optional[Sequence[str]] = None) -> int:
    try:
        args = build_parser().parse_args(argv)
        if args.probe:
            probe()
            return EXIT_OK
        if not args.in_path or not args.out_path:
            raise InputError("--in and --out are required")
        run(args.in_path, args.out_path)
        return EXIT_OK
    except InputError as e:
        fail("input error: ", e)
        return EXIT_INPUT
    except WorkerError as e:
        if e.__cause__ is not None:
            traceback.print_exception(type(e.__cause__), e.__cause__, e.__cause__.__traceback__,
                                      file=sys.stderr)
        if e.code:
            emit_stdout("ERROR_CODE " + e.code
                        + (" " + json.dumps(e.params, sort_keys=True) if e.params else ""))
        fail("", e)
        return e.exit_code
    except SystemExit:
        raise
    except KeyboardInterrupt:
        fail("", "interrupted")
        return EXIT_RETRY
    except Exception as e:  # every other failure is retryable (SPEC §6.0)
        traceback.print_exc(file=sys.stderr)
        fail("", "%s: %s" % (type(e).__name__, e))
        return EXIT_RETRY


if __name__ == "__main__":
    setup_stdio()
    sys.exit(main())
