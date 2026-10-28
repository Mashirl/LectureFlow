# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
# Additional permission under AGPL-3.0 section 7: see LICENSE-EXCEPTION.md
"""LectureFlow remote transcription worker for Google Colab (SPEC-v3 V14.3).

LectureFlow uploads this file, asr.py, asr_common.py and textrepair.py to the Colab runtime
and runs, for every lecture,
    python colab_remote.py --attach --base BASE --job-dir BASE/jobs/ID --model M
        --compute-type C --requirements R --workers N [--memory]
in a kernel (internal/colab/job.go cellCode): the attaching process starts the runtime's
resident worker (--serve, see "The resident worker" below) when none runs and copies its
events to stdout; the worker prepares the environment and the model while the audio
uploads and keeps the model for the next lectures (colab-batch). The single-lecture mode
    python colab_remote.py --job job.json [--log-dir DIR]
(one process: install, model, transcription) stays for tests and experiments. The
transcription is the local
faster-whisper worker's own code (asr.transcribe_job: the same parameters,
hallucination and repeat filters, the second decode of degenerate windows, sentence
splitting and word times), so result.json has the structure the local worker writes
(prompt_terms_used, sentences with words, timing with its postprocess counters).

Standard output carries ONLY events, one JSON object per line (V14.3 step 3):
    {"type": "stage",    "ts": <unix s>, "stage": <STAGES>}
    {"type": "progress", "ts": <unix s>, "fraction": <0..1>}
    {"type": "done",     "ts": <unix s>, "result": "result.json", "sha256": <hex>,
     "bytes": <int>, "audio_s": <s>, "load_s": <s>, "transcribe_s": <s>,
     "sentences": <int>}
    {"type": "error",    "ts": <unix s>, "code": <ERROR_CODES>, "retryable": <bool>,
     ["exc": <exception class name>]}
Never any transcript text, prompt text or anything derived from the audio: event
values are numbers, booleans and strings from fixed tables (EVENT_KEYS is the same
table as internal/colab/events.go eventKeys, which rejects anything else). To make sure
nothing else reaches stdout, file descriptor 1 is pointed at stderr for the whole run
(libraries, pip, tqdm, C extensions) and events go to a duplicate of the original
stdout. stderr is a file on the runtime (stderr.log) that LectureFlow never downloads
and deletes with the job.

job.json: {"audio": file name in the job directory, "audio_sha256", "model",
           "compute_type", "language", "initial_prompt", "result": file name,
           "requirements": ["faster-whisper==1.2.1", ...], ["memory": true],
           ["redecode": false] (asr.py's job.redecode; absent = true),
           ["split": 2..4] ("极速模式": cut into parts at silences; absent = 1)}
Installing: with the runtime's uv when it has one (exactly the missing pins and
UV_EXTRAS, --no-deps, then checked), else or when that fails with pip as before.
Exit code 0 = done, 2 = input error, 1 = other error (as asr.py).

Memory mode (SPEC-ext "Colab 内存运行（实验）", kv colab.memory_mode = auto): before anything
is uploaded, LectureFlow runs this file's source as
    python -I -c <source> --memory-probe --model M --audio-bytes N --requirements R
(nothing is written to the runtime before the decision). The probe prints one "memory" event:
"memory" when model x 2 + Python environment + recording x 2 + 3 GB (the transcription process)
+ 2 GB fit MemAvailable and no swap is enabled, with the RAM disk to use (/dev/shm/lectureflow, or a tmpfs it mounts at
/mnt/lectureflow when /dev/shm is too small); "disk" with a reason otherwise. In memory mode the
scripts, the job directory, the extra packages (pip --target, --no-cache-dir), the Hugging Face
cache (HF_HOME: faster-whisper's model download) and every temporary or cache directory of the
worker live on that RAM disk, so the recording, the model and the result never reach the
runtime's persistent disk. The machine is still Google's: memory mode keeps data off the disk and
leaves less behind, it does not hide anything from Google.
"""
from __future__ import annotations

import argparse
import contextlib
import hashlib
import importlib
import json
import math
import os
import queue
import re
import subprocess
import sys
import tempfile
import threading
import time
from typing import Callable, Dict, List, Optional

EXIT_OK, EXIT_RETRY, EXIT_INPUT = 0, 1, 2

# No usage reporting (NOTES.md "无遥测"): the same list as worker/asr_common.py
# TELEMETRY_OFF_ENV and internal/pyenv TelemetryOff, set by main() before anything imports
# huggingface_hub (this file also runs as `python -I -c`, where asr_common cannot be imported
# yet); pip and the transcription process inherit it.
TELEMETRY_OFF_ENV = {
    "HF_HUB_DISABLE_TELEMETRY": "1",
    "HF_HUB_DISABLE_IMPLICIT_TOKEN": "1",
    "HF_XET_TELEMETRY_ENABLED": "0",
    "DO_NOT_TRACK": "1",
}

STAGES =("check", "install", "download_model", "load_model", "transcribe", "write")
ERROR_CODES = ("bad_job", "audio_missing", "audio_hash_mismatch", "audio_unreadable",
               "install_failed", "no_gpu", "model_load_failed", "out_of_memory",
               "transcribe_failed", "write_failed", "internal")

# Same keys as internal/colab/events.go eventKeys: (required, optional).
EVENT_KEYS = {
    "progress": (("type", "ts", "fraction"), ()),
    "stage": (("type", "ts", "stage"), ()),
    "done": (("type", "ts", "result", "sha256", "bytes", "audio_s", "load_s",
              "transcribe_s", "sentences"), ("mem_peak_mb", "mem_total_mb")),
    "error": (("type", "ts", "code", "retryable"), ("exc",)),
    "memory": (("type", "ts", "mode", "base", "reason", "mem_total_mb", "mem_available_mb",
                "need_mb", "free_mb"), ()),
}

# The runtime's own readings (colab-obs-core): the same tables as internal/remote/sample.go
# MachineEventKeys / SampleEventKeys (every field required; -1 / "" = unknown). The attaching
# process that holds BASE/sample.lock sends one machine event, then a sample every 2 s.
MACHINE_KEYS = ("gpu_name", "gpu_arch", "compute_cap", "driver", "cuda", "gpu_count",
                "vram_total_mb", "vcpus", "mem_total_mb", "storage")
SAMPLE_KEYS = ("cpu_pct", "mem_total_mb", "mem_available_mb", "ramdisk_mb", "data_mb",
               "data_disk_mb", "whisper_mb", "gpu_util_pct", "vram_used_mb", "vram_reserved_mb",
               "vram_whisper_mb", "gpu_temp_c", "gpu_power_w")
EVENT_KEYS["machine"] = (("type", "ts") + MACHINE_KEYS, ())
EVENT_KEYS["sample"] = (("type", "ts") + SAMPLE_KEYS, ())
# internal/remote/sample.go's patterns and ranges (fullmatch: Go's $ never matches before a
# trailing newline).
READING_STRINGS = {
    "gpu_name": re.compile(r"([A-Za-z0-9][A-Za-z0-9 ._()/+-]{0,63})?"),
    "gpu_arch": re.compile(r"[A-Za-z0-9 ]{0,32}"),
    "compute_cap": re.compile(r"([0-9]{1,2}\.[0-9]{1,2})?"),
    "driver": re.compile(r"([0-9]{1,4}(\.[0-9]{1,4}){1,3})?"),
    "cuda": re.compile(r"([0-9]{1,2}\.[0-9]{1,2})?"),
    "storage": re.compile(r"(memory|disk|disk_fallback)?"),
}
MAX_PCT, MAX_MB, MAX_POWER_W, MAX_GPUS, MAX_VCPUS, MAX_TEMP_C = 100, 1 << 40, 10000, 64, 4096, 200
# Integer fields and their upper bounds; the others of SAMPLE_KEYS are numbers.
READING_INTS = {"gpu_count": MAX_GPUS, "vcpus": MAX_VCPUS}
READING_NUMBERS = {"cpu_pct": MAX_PCT, "gpu_util_pct": MAX_PCT, "gpu_temp_c": MAX_TEMP_C,
                   "gpu_power_w": MAX_POWER_W}


def check_reading(typ: str, fields: dict) -> None:
    """fields (without type and ts) are a valid machine / sample event; ValueError otherwise."""
    keys = MACHINE_KEYS if typ == "machine" else SAMPLE_KEYS
    if set(fields) != set(keys):
        raise ValueError("fields of a %s event" % typ)
    for k, v in fields.items():
        if k in READING_STRINGS:
            if not isinstance(v, str) or not READING_STRINGS[k].fullmatch(v):
                raise ValueError(k)
        elif k in READING_NUMBERS:
            bad = isinstance(v, bool) or not isinstance(v, (int, float)) or math.isnan(v)
            if bad or (v != -1 and not 0 <= v <= READING_NUMBERS[k]):
                raise ValueError(k)
        else:  # integers: MB, counts
            if isinstance(v, bool) or not isinstance(v, int) or not -1 <= v <= READING_INTS.get(k, MAX_MB):
                raise ValueError(k)

# Memory mode (same tables as internal/colab/memory.go). The RAM disks the worker may use:
# /dev/shm (a tmpfs on Linux) or a tmpfs the probe mounts itself.
MEMORY_BASES = ("/dev/shm/lectureflow", "/mnt/lectureflow")
MEMORY_MODES = ("memory", "disk")
# "ok" with mode memory; why the disk is used otherwise.
MEMORY_REASONS = ("ok", "low_memory", "swap_enabled", "no_proc", "tmpfs_failed", "error")
# What must fit MemAvailable (SPEC-ext's model + environment + recording x 2 + 2 GB margin, with
# the transcription process added by the lead's ruling of 2026-10-27 after the Colab experiment:
# the process held about 5.9 GB with large-v3 float16, i.e. the model's host copy + ~3 GB of
# CUDA / cuDNN host memory and buffers): model x 2 + environment + recording x 2 + 3 GB + 2 GB.
MEMORY_MARGIN_MB = 2048
PROCESS_MB = 3072
# The pins with every dependency installed with pip --target (ignores the system's packages):
# 430 MiB measured on Colab (Python 3.13, 2026-10-26), with some room.
ENV_MB = 512
# result.json, stderr.log and install.log on the RAM disk.
RESULT_MB = 64
MIB = 1 << 20
# Size on disk of the faster-whisper models (CTranslate2 float16 weights and tokenizer, MiB);
# unknown names count as a large model.
MODEL_MB = {
    "tiny": 75, "tiny.en": 75, "base": 145, "base.en": 145, "small": 470, "small.en": 470,
    "medium": 1460, "medium.en": 1460, "large-v1": 2950, "large-v2": 2950, "large-v3": 2950,
    "large": 2950, "distil-large-v2": 1460, "distil-large-v3": 1460, "distil-large-v3.5": 1460,
    "distil-medium.en": 760, "distil-small.en": 320, "large-v3-turbo": 1550, "turbo": 1550,
}
DEFAULT_MODEL_MB = 2950
# faster_whisper.utils._MODELS 1.2.1: the Hugging Face repository of each name (the probe
# runs before faster-whisper is installed).
MODEL_REPOS = {
    "tiny.en": "Systran/faster-whisper-tiny.en", "tiny": "Systran/faster-whisper-tiny",
    "base.en": "Systran/faster-whisper-base.en", "base": "Systran/faster-whisper-base",
    "small.en": "Systran/faster-whisper-small.en", "small": "Systran/faster-whisper-small",
    "medium.en": "Systran/faster-whisper-medium.en", "medium": "Systran/faster-whisper-medium",
    "large-v1": "Systran/faster-whisper-large-v1", "large-v2": "Systran/faster-whisper-large-v2",
    "large-v3": "Systran/faster-whisper-large-v3", "large": "Systran/faster-whisper-large-v3",
    "distil-large-v2": "Systran/faster-distil-whisper-large-v2",
    "distil-medium.en": "Systran/faster-distil-whisper-medium.en",
    "distil-small.en": "Systran/faster-distil-whisper-small.en",
    "distil-large-v3": "Systran/faster-distil-whisper-large-v3",
    "distil-large-v3.5": "distil-whisper/distil-large-v3.5-ct2",
    "large-v3-turbo": "mobiuslabsgmbh/faster-whisper-large-v3-turbo",
    "turbo": "mobiuslabsgmbh/faster-whisper-large-v3-turbo",
}
MODEL_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._/-]{0,79}$")

FILE_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
REQUIREMENT_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*==[A-Za-z0-9][A-Za-z0-9.+_-]*$")
EXC_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_.]{0,79}$")

# "极速模式" (colab-batch, user decision 2026-10-27 05:40): a lecture cut into at most this
# many parts at silences, transcribed at once (job.json "split").
MAX_SPLIT = 4

# Progress events: at most every PROGRESS_MIN_S seconds unless the fraction moved by
# PROGRESS_MIN_STEP (asr.py reports every segment).
PROGRESS_MIN_S = 2.0
PROGRESS_MIN_STEP = 0.01

# faster-whisper runs on the runtime's GPU with asr.py's default CPU threads for cuda.
DEVICE = "cuda"
CPU_THREADS = 4


class RemoteError(Exception):
    """A failure with an ERROR_CODES code (exit 2 when input, else 1)."""

    def __init__(self, code: str, input_error: bool = False, exc: Optional[BaseException] = None):
        super().__init__(code)
        assert code in ERROR_CODES
        self.code = code
        self.input_error = input_error
        self.exc = exc


# ---------------------------------------------------------------------------
# Events
# ---------------------------------------------------------------------------

class Events:
    """Writes the stdout events (and nothing else) to `stream`."""

    def __init__(self, stream, clock: Callable[[], float] = time.time):
        self.stream = stream
        self.clock = clock
        self.last_fraction = -1.0
        self.last_progress_t = 0.0
        self.final = False
        self.last_stage = ""

    def _emit(self, typ: str, **fields) -> None:
        required, optional = EVENT_KEYS[typ]
        record = {"type": typ, "ts": round(self.clock(), 3)}
        record.update(fields)
        for k, v in record.items():
            if k not in required and k not in optional:
                raise ValueError("event field %r not allowed in %s" % (k, typ))
            if not isinstance(v, (str, int, float, bool)):
                raise ValueError("event field %r must be a scalar" % k)
        for k in required:
            if k not in record:
                raise ValueError("event field %r missing in %s" % (k, typ))
        self.stream.write(json.dumps(record, ensure_ascii=True, separators=(",", ":")) + "\n")
        self.stream.flush()

    def stage(self, name: str) -> None:
        """A stage; the same stage twice in a row is said once (the parallel model download
        still finishing, then asr.py's own download, both say download_model)."""
        assert name in STAGES
        if name == self.last_stage:
            return
        self.last_stage = name
        self._emit("stage", stage=name)

    def progress(self, fraction: float, force: bool = False) -> None:
        f = round(min(1.0, max(0.0, float(fraction))), 4)
        now = self.clock()
        if not force and f < 1.0 and f - self.last_fraction < PROGRESS_MIN_STEP and \
                now - self.last_progress_t < PROGRESS_MIN_S:
            return
        if f <= self.last_fraction and not force:
            return
        self.last_fraction, self.last_progress_t = f, now
        self._emit("progress", fraction=f)

    def done(self, result: str, sha256: str, size: int, audio_s: float, load_s: float,
             transcribe_s: float, sentences: int, mem: Optional[Dict[str, int]] = None) -> None:
        assert FILE_RE.match(result) and SHA256_RE.match(sha256)
        extra = {k: int(v) for k, v in (mem or {}).items()}
        self._emit("done", result=result, sha256=sha256, bytes=int(size),
                   audio_s=round(max(0.0, audio_s), 3), load_s=round(max(0.0, load_s), 3),
                   transcribe_s=round(max(0.0, transcribe_s), 3), sentences=int(sentences),
                   **extra)
        self.final = True

    def memory(self, d: dict) -> None:
        """The memory probe's decision (the only line of --memory-probe)."""
        assert d["mode"] in MEMORY_MODES and d["reason"] in MEMORY_REASONS
        assert d["base"] in MEMORY_BASES if d["mode"] == "memory" else d["base"] == ""
        self._emit("memory", mode=d["mode"], base=d["base"], reason=d["reason"],
                   **{k: max(0, int(d[k])) for k in ("mem_total_mb", "mem_available_mb",
                                                     "need_mb", "free_mb")})
        self.final = True

    def reading(self, typ: str, fields: dict) -> None:
        """A machine or sample event (check_reading first: ValueError, nothing written)."""
        check_reading(typ, fields)
        self._emit(typ, **fields)

    def error(self, code: str, retryable: bool, exc: Optional[BaseException] = None) -> None:
        fields = {"code": code, "retryable": bool(retryable)}
        if exc is not None:
            name = type(exc).__name__
            if EXC_RE.match(name):
                fields["exc"] = name
        self._emit("error", **fields)
        self.final = True


@contextlib.contextmanager
def isolated_stdout():
    """Yields a text stream on the original stdout for the events; meanwhile file
    descriptor 1 and sys.stdout point at stderr, so nothing else can reach stdout."""
    sys.stdout.flush()
    saved = os.dup(1)
    events_fd = os.dup(1)
    os.dup2(2, 1)
    old_stdout = sys.stdout
    sys.stdout = sys.stderr
    stream = os.fdopen(events_fd, "w", encoding="utf-8", newline="\n", buffering=1)
    try:
        yield stream
    finally:
        try:
            stream.flush()
            stream.close()
        finally:
            sys.stdout = old_stdout
            os.dup2(saved, 1)
            os.close(saved)


def log(msg: str) -> None:
    """Diagnostics to stderr (stderr.log on the runtime): no transcript text."""
    sys.stderr.write("colab_remote: %s\n" % msg)
    sys.stderr.flush()


# ---------------------------------------------------------------------------
# Job
# ---------------------------------------------------------------------------

def load_job(path: str) -> dict:
    try:
        with open(path, encoding="utf-8") as f:
            job = json.load(f)
    except (OSError, ValueError):
        raise RemoteError("bad_job", input_error=True)
    if not isinstance(job, dict):
        raise RemoteError("bad_job", input_error=True)
    for key in ("audio", "result"):
        if not isinstance(job.get(key), str) or not FILE_RE.match(job[key]):
            raise RemoteError("bad_job", input_error=True)
    if not isinstance(job.get("audio_sha256"), str) or not SHA256_RE.match(job["audio_sha256"]):
        raise RemoteError("bad_job", input_error=True)
    for key in ("model", "compute_type"):
        if not isinstance(job.get(key), str) or not job[key]:
            raise RemoteError("bad_job", input_error=True)
    for key in ("language", "initial_prompt"):
        if job.get(key) is not None and not isinstance(job[key], str):
            raise RemoteError("bad_job", input_error=True)
    for key, default in (("memory", False), ("redecode", True)):
        if not isinstance(job.get(key, default), bool):
            raise RemoteError("bad_job", input_error=True)
    split = job.get("split", 1)
    if isinstance(split, bool) or not isinstance(split, int) or not 1 <= split <= MAX_SPLIT:
        raise RemoteError("bad_job", input_error=True)
    reqs = job.get("requirements", [])
    if not isinstance(reqs, list) or not all(isinstance(r, str) and REQUIREMENT_RE.match(r)
                                             for r in reqs):
        raise RemoteError("bad_job", input_error=True)
    return job


def file_sha256(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()


# ---------------------------------------------------------------------------
# Environment
# ---------------------------------------------------------------------------

def installed_version(name: str) -> Optional[str]:
    try:
        from importlib import metadata
        return metadata.version(name)
    except Exception:
        return None


def missing_requirements(reqs: List[str]) -> List[str]:
    out = []
    for r in reqs:
        name, version = r.split("==", 1)
        if installed_version(name) != version:
            out.append(r)
    return out


def pip_command(reqs: List[str], target: Optional[str] = None,
                constraints: Optional[str] = None) -> List[str]:
    """The pip command line. Disk mode: into the runtime's site-packages (as before memory
    mode existed). Memory mode: into target on the RAM disk, without pip's cache, the
    dependencies held to constraints (the runtime's own versions)."""
    cmd = [sys.executable, "-m", "pip", "install", "--disable-pip-version-check",
           "--no-input", "--progress-bar", "off"]
    if target:
        cmd += ["--no-cache-dir", "--upgrade", "--target", target]
        if constraints:
            cmd += ["-c", constraints]
    return cmd + list(reqs)


def pip_install(reqs: List[str], log_path: str, target: Optional[str] = None,
                constraints: Optional[str] = None) -> int:
    """pip install the pins; all output goes to log_path (never stdout)."""
    with open(log_path, "ab") as logf:
        return subprocess.call(pip_command(reqs, target, constraints), stdout=logf, stderr=logf,
                               stdin=subprocess.DEVNULL)


CONSTRAINT_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*==[A-Za-z0-9][A-Za-z0-9.+_!-]*$")


def system_constraints(reqs: List[str]) -> List[str]:
    """The runtime's installed versions (pip list --format=freeze of a fresh interpreter,
    which does not see the RAM disk's directory) as constraints, the pins excluded: pip
    --target ignores installed packages, and without them it would take the newest version of
    every dependency instead of the ones disk mode uses."""
    try:
        out = subprocess.run([sys.executable, "-m", "pip", "list", "--format=freeze",
                              "--disable-pip-version-check"], stdout=subprocess.PIPE,
                             stderr=subprocess.DEVNULL, stdin=subprocess.DEVNULL, timeout=120).stdout
    except (OSError, subprocess.SubprocessError):
        return []
    pinned = {_norm(r.split("==", 1)[0]) for r in reqs}
    return [l for l in out.decode("utf-8", "replace").splitlines()
            if CONSTRAINT_RE.match(l) and _norm(l.split("==", 1)[0]) not in pinned]


# uv (colab-batch R2): Colab's image ships uv (0.13.4 on 2026-10-27), which installs the same
# packages in about 2 s where pip takes about 14 s. The fast path installs exactly the missing
# pins plus UV_EXTRAS (the one dependency of the pins the image lacks: faster-whisper's
# onnxruntime, at this repository's .venv version) with --no-deps; everything else the pins
# need (numpy, huggingface_hub, tokenizers, ...) is the runtime's own. When uv is missing,
# fails, or the result does not import or does not meet the pins' requirements, the full pip
# install below runs as before.
UV_EXTRAS = ("onnxruntime==1.31.0",)
# What must import after the fast path (in this process: the worker imports them anyway).
IMPORT_CHECK = ("faster_whisper", "ctranslate2", "av", "onnxruntime")


def find_uv() -> Optional[str]:
    import shutil
    return shutil.which("uv")


def uv_command(uv: str, reqs: List[str], target: Optional[str] = None) -> List[str]:
    """uv pip install for this interpreter: --no-deps, no cache; into target (memory mode,
    the RAM disk's package directory) or the interpreter's own environment (disk mode)."""
    cmd = [uv, "pip", "install", "--no-cache", "--no-deps", "--python", sys.executable]
    if target:
        cmd += ["--target", target]
    return cmd + list(reqs)


def uv_install(uv: str, reqs: List[str], log_path: str, target: Optional[str] = None) -> int:
    """Runs uv_command; all output goes to log_path (never stdout)."""
    with open(log_path, "ab") as logf:
        try:
            return subprocess.call(uv_command(uv, reqs, target), stdout=logf, stderr=logf,
                                   stdin=subprocess.DEVNULL)
        except OSError:
            return 127


def requirements_met(names: List[str]) -> bool:
    """Every requirement (markers evaluated, extras ignored) of the distributions names is
    installed in a matching version: the check that --no-deps left nothing out."""
    try:
        from importlib import metadata
        from packaging.requirements import Requirement
    except Exception:  # without packaging the imports below still have to work
        return True
    for name in names:
        try:
            reqs = metadata.requires(name) or []
        except Exception:
            return False
        for line in reqs:
            try:
                r = Requirement(line)
                if r.marker is not None and not r.marker.evaluate({"extra": ""}):
                    continue
                have = installed_version(r.name)
                if have is None or (r.specifier and not r.specifier.contains(have, prereleases=True)):
                    log("requirement of %s not met: %s" % (name, r.name))
                    return False
            except Exception:
                return False
    return True


def imports_ok(modules=IMPORT_CHECK) -> bool:
    """The modules import; a failed attempt is taken out of sys.modules again."""
    for m in modules:
        try:
            importlib.import_module(m)
        except Exception as e:
            log("import check failed: %s (%s)" % (m, type(e).__name__))
            for k in list(sys.modules):
                if k.split(".")[0] in modules:
                    del sys.modules[k]
            return False
    return True


def fast_install(reqs: List[str], log_dir: str, target: Optional[str],
                 uv_run: Callable[..., int]) -> bool:
    """The uv fast path: the missing pins and the missing UV_EXTRAS, --no-deps; True when
    they are all in place, meet their requirements and import."""
    missing = missing_requirements(reqs)
    extras = [e for e in UV_EXTRAS if installed_version(e.split("==", 1)[0]) is None]
    log("installing %d pinned packages and %d dependencies with uv%s"
        % (len(missing), len(extras), " into the RAM disk" if target else ""))
    rc = uv_run(missing + extras, os.path.join(log_dir, "install.log"), target)
    importlib.invalidate_caches()
    names = [r.split("==", 1)[0] for r in list(reqs) + list(UV_EXTRAS)]
    if rc != 0 or missing_requirements(reqs) or not requirements_met(names) or not imports_ok():
        log("uv install failed or incomplete (rc %d); the full pip install follows" % rc)
        return False
    return True


def ensure_requirements(reqs: List[str], log_dir: str,
                        install: Callable[..., int] = None, target: Optional[str] = None,
                        uv_run: Optional[Callable[..., int]] = None) -> None:
    """Installs the pins that are missing: with uv when the runtime has it (fast_install),
    else, or when that fails, with pip as below. uv_run(reqs, log_path, target) replaces uv
    (tests); without it find_uv() decides.

    pip: with target (memory mode, already first on
    sys.path) every pin goes there together, so that pip resolves their dependencies
    (installed there too: --target ignores the system's packages) against the pins, held to
    the runtime's own versions (system_constraints; once more without them if that fails);
    pins the runtime already has in the right version are used as they are."""
    install = install or pip_install
    missing = missing_requirements(reqs)
    if not missing:
        return
    if uv_run is None:
        uv = find_uv()
        if uv:
            uv_run = lambda r, log_path, t: uv_install(uv, r, log_path, t)  # noqa: E731
    if uv_run is not None and fast_install(reqs, log_dir, target, uv_run):
        return
    if not target:
        log("installing %d pinned packages" % len(missing))
        rc = install(missing, os.path.join(log_dir, "install.log"))
    else:
        log("installing %d pinned packages into the RAM disk" % len(reqs))
        cons = system_constraints(reqs)
        cfile = os.path.join(tempfile.gettempdir(), "constraints.txt") if cons else None
        if cfile:
            with open(cfile, "w", encoding="utf-8", newline="\n") as f:
                f.write("\n".join(cons) + "\n")
        rc = install(list(reqs), os.path.join(log_dir, "install.log"), target, cfile)
        if rc != 0 and cfile:
            log("install with the runtime's versions as constraints failed; once more without")
            rc = install(list(reqs), os.path.join(log_dir, "install.log"), target, None)
    importlib.invalidate_caches()
    if rc != 0 or missing_requirements(reqs):
        raise RemoteError("install_failed")


def cuda_device_count() -> int:
    try:
        import ctranslate2
        return int(ctranslate2.get_cuda_device_count())
    except Exception:
        return 0


def _is_oom(exc: BaseException) -> bool:
    text = str(exc).lower()
    return isinstance(exc, MemoryError) or "out of memory" in text or "cuda_error_out_of_memory" in text


# ---------------------------------------------------------------------------
# Memory mode
# ---------------------------------------------------------------------------

def read_meminfo(path: str = "/proc/meminfo") -> Dict[str, int]:
    """/proc/meminfo as {key: kB}; {} when it cannot be read (not Linux)."""
    out: Dict[str, int] = {}
    try:
        with open(path, encoding="ascii", errors="replace") as f:
            for line in f:
                key, _, rest = line.partition(":")
                parts = rest.split()
                if parts and parts[0].isdigit():
                    out[key.strip()] = int(parts[0])
    except OSError:
        return {}
    return out


def swap_count(path: str = "/proc/swaps") -> Optional[int]:
    """Enabled swap areas (lines after the header of /proc/swaps); None when unreadable."""
    try:
        with open(path, encoding="ascii", errors="replace") as f:
            lines = [l for l in f.read().splitlines() if l.strip()]
    except OSError:
        return None
    return max(0, len(lines) - 1)


def mount_of(path: str, mounts: str = "/proc/mounts"):
    """(mount point, file system type, options) of the mount holding path; ("", "", ())
    when unknown."""
    best = ("", "", ())
    try:
        with open(mounts, encoding="utf-8", errors="replace") as f:
            for line in f:
                parts = line.split()
                if len(parts) < 3:
                    continue
                mp = parts[1].replace("\\040", " ")
                prefix = mp if mp.endswith(("/", os.sep)) else mp + os.sep
                if (path == mp or path.startswith(prefix)) and len(mp) >= len(best[0]):
                    best = (mp, parts[2], tuple(parts[3].split(",")) if len(parts) > 3 else ())
    except OSError:
        pass
    return best


def ram_disk_usable(path: str, mounts: str = "/proc/mounts", own: bool = False) -> bool:
    """path is on a tmpfs that allows executing files (the packages' native extensions are
    mapped executable: Colab mounts /dev/shm noexec, where they fail to load). own: path must
    be the mount point itself (the tmpfs this probe mounts)."""
    mp, fs, opts = mount_of(path, mounts)
    return fs == "tmpfs" and "noexec" not in opts and (not own or mp == path)


def statvfs_free_mb(path: str) -> int:
    """Free MiB of the file system holding path (-1 when unknown)."""
    try:
        st = os.statvfs(path)
    except (OSError, AttributeError):
        return -1
    return int(st.f_bavail * st.f_frsize // MIB)


def mount_tmpfs(target: str, size_mb: int) -> bool:
    """Mounts a private tmpfs of at most size_mb at target (the runtime runs as root)."""
    try:
        rc = subprocess.call(["mount", "-t", "tmpfs", "-o",
                              "size=%dm,mode=0700,nosuid,nodev" % max(1, size_mb), "tmpfs", target],
                             stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                             stderr=subprocess.DEVNULL, timeout=60)
    except (OSError, subprocess.SubprocessError):
        return False
    return rc == 0


def _makedirs(path: str) -> None:
    os.makedirs(path, mode=0o700, exist_ok=True)


def model_mb(name: str) -> int:
    return MODEL_MB.get(name, DEFAULT_MODEL_MB)


def model_cached(hf_home: str, name: str) -> bool:
    """The model is in the Hugging Face cache under hf_home (an earlier lecture of this
    runtime downloaded it to the RAM disk)."""
    repo = MODEL_REPOS.get(name) or (name if "/" in name else "")
    if not repo:
        return False
    snaps = os.path.join(hf_home, "hub", "models--" + repo.replace("/", "--"), "snapshots")
    try:
        return any(os.path.isfile(os.path.join(snaps, s, "model.bin")) for s in os.listdir(snaps))
    except OSError:
        return False


def _norm(name: str) -> str:
    return re.sub(r"[-_.]+", "-", name).lower()


def env_ready(env_dir: str, reqs: List[str]) -> bool:
    """Every pin is installed in env_dir (an earlier lecture of this runtime)."""
    if not os.path.isdir(env_dir):
        return False
    try:
        from importlib import metadata
        have = {_norm(d.metadata["Name"] or ""): d.version for d in metadata.distributions(path=[env_dir])}
    except Exception:
        return False
    for r in reqs:
        name, version = r.split("==", 1)
        if have.get(_norm(name)) != version:
            return False
    return True


def memory_probe(model: str, audio_bytes: int, reqs: List[str], extra_mb: int = 0,
                 no_shm: bool = False, meminfo: str = "/proc/meminfo", swaps: str = "/proc/swaps",
                 mounts: str = "/proc/mounts", free_mb: Callable[[str], int] = statvfs_free_mb,
                 mount: Callable[[str, int], bool] = mount_tmpfs,
                 makedirs: Callable[[str], None] = _makedirs) -> dict:
    """Decides memory or disk mode (SPEC-ext "先算再开") and prepares the RAM disk.

    need = model (on the RAM disk) + model (the process's copy) + environment + recording x 2
    + 3 GB (the rest of the transcription process) + 2 GB margin (+ extra_mb, an experiment's
    way to force the fallback), without what an earlier lecture of this runtime already put on
    the RAM disk (the process's copy is always counted); it must fit MemAvailable, and no swap may be enabled (the RAM disk's pages could
    be swapped out to disk). The RAM disk is /dev/shm/lectureflow when /dev/shm is a tmpfs that
    allows executing files and has room for model + environment + recording, else a tmpfs
    mounted at /mnt/lectureflow (on Colab /dev/shm is noexec, 5.7 GB: always the latter)."""
    shm_base, tmpfs_base = MEMORY_BASES
    d = {"mode": "disk", "base": "", "reason": "", "mem_total_mb": 0, "mem_available_mb": 0,
         "need_mb": 0, "free_mb": 0}

    def disk(reason: str) -> dict:
        d["reason"] = reason
        return d

    mi = read_meminfo(meminfo)
    swap = swap_count(swaps)
    if "MemTotal" not in mi or "MemAvailable" not in mi or swap is None:
        return disk("no_proc")
    d["mem_total_mb"] = mi["MemTotal"] // 1024
    d["mem_available_mb"] = avail = mi["MemAvailable"] // 1024
    if swap > 0:
        return disk("swap_enabled")
    # A RAM disk an earlier lecture of this runtime prepared.
    shm_ok = not no_shm and ram_disk_usable(shm_base, mounts)
    existing = ""
    if shm_ok and os.path.isdir(shm_base):
        existing = shm_base
    elif ram_disk_usable(tmpfs_base, mounts, own=True):
        existing = tmpfs_base
    have_model = bool(existing) and model_cached(os.path.join(existing, "hf"), model)
    have_env = bool(existing) and env_ready(os.path.join(existing, "env"), reqs)
    audio_mb = int(math.ceil(max(0, audio_bytes) / float(MIB)))
    disk_mb = (0 if have_model else model_mb(model)) + (0 if have_env else ENV_MB) + audio_mb + RESULT_MB
    d["need_mb"] = need = (disk_mb + audio_mb + model_mb(model) + PROCESS_MB + MEMORY_MARGIN_MB
                           + max(0, extra_mb))
    if existing:
        d["free_mb"] = free_mb(existing)
    if need > avail:
        return disk("low_memory")
    if existing:
        if d["free_mb"] < disk_mb:
            return disk("low_memory")
        d["mode"], d["base"], d["reason"] = "memory", existing, "ok"
        return d
    if shm_ok:
        free = free_mb(os.path.dirname(shm_base))
        if free >= disk_mb:
            try:
                makedirs(shm_base)
                d["mode"], d["base"], d["reason"], d["free_mb"] = "memory", shm_base, "ok", free
                return d
            except OSError:
                pass
    # /dev/shm is too small, noexec or missing: a tmpfs of our own, capped at what may be used.
    try:
        makedirs(tmpfs_base)
    except OSError:
        return disk("tmpfs_failed")
    if not mount(tmpfs_base, avail - MEMORY_MARGIN_MB) or not ram_disk_usable(tmpfs_base, mounts, own=True):
        return disk("tmpfs_failed")
    d["mode"], d["base"], d["reason"], d["free_mb"] = "memory", tmpfs_base, "ok", free_mb(tmpfs_base)
    return d


def memory_base_of(job_dir: str) -> str:
    """The RAM disk of a memory-mode job (job_dir = BASE/jobs/ID); bad_job when job_dir is not
    on one of MEMORY_BASES."""
    base = os.path.dirname(os.path.dirname(os.path.abspath(job_dir)))
    for b in MEMORY_BASES:
        if os.path.normcase(os.path.abspath(b)) == os.path.normcase(base):
            return base
    raise RemoteError("bad_job", input_error=True)


# Cache variables that would point a library away from the RAM disk.
_FOREIGN_CACHE_VARS = ("HUGGINGFACE_HUB_CACHE", "TRANSFORMERS_CACHE", "HF_DATASETS_CACHE",
                       "PIP_CACHE_DIR", "PYTHONUSERBASE")


def setup_memory_env(base: str) -> str:
    """Points this process (and pip, which inherits it) at the RAM disk: HF_HOME (faster-whisper
    downloads the model through huggingface_hub into $HF_HOME/hub), the cache, temporary and
    home directories, and puts the package directory first on sys.path. Returns it."""
    dirs = {n: os.path.join(base, n) for n in ("env", "hf", "cache", "tmp", "home")}
    for p in dirs.values():
        _makedirs(p)
    for k in _FOREIGN_CACHE_VARS:
        os.environ.pop(k, None)
    os.environ.update({
        "HF_HOME": dirs["hf"], "HF_HUB_CACHE": os.path.join(dirs["hf"], "hub"),
        "HF_XET_CACHE": os.path.join(dirs["hf"], "xet"), "HF_HUB_DISABLE_TELEMETRY": "1",
        "XDG_CACHE_HOME": dirs["cache"], "TMPDIR": dirs["tmp"], "HOME": dirs["home"],
        "PYTHONNOUSERSITE": "1",
        # Colab's pip configuration logs every pip run to /var/log/pip.log (persistent disk)
        "PIP_LOG": os.path.join(dirs["tmp"], "pip.log"),
        # no __pycache__ next to the runtime's own packages (pip, imported modules)
        "PYTHONDONTWRITEBYTECODE": "1",
    })
    sys.dont_write_bytecode = True
    tempfile.tempdir = None  # re-read TMPDIR
    if dirs["env"] not in sys.path:
        sys.path.insert(0, dirs["env"])
    importlib.invalidate_caches()
    return dirs["env"]


class MemSampler:
    """Samples the runtime's used memory (MemTotal - MemAvailable; the RAM disk's files count
    as used) while the worker runs. fields() is {} when /proc/meminfo cannot be read."""

    def __init__(self, path: str = "/proc/meminfo", interval: float = 0.5):
        self.path, self.interval = path, interval
        self.peak_kb: Optional[int] = None
        self.total_kb: Optional[int] = None
        self._stop = threading.Event()
        self._thread: Optional[threading.Thread] = None

    def sample(self) -> None:
        mi = read_meminfo(self.path)
        if "MemTotal" in mi and "MemAvailable" in mi:
            used = mi["MemTotal"] - mi["MemAvailable"]
            self.total_kb = mi["MemTotal"]
            self.peak_kb = used if self.peak_kb is None else max(self.peak_kb, used)

    def _loop(self) -> None:
        while not self._stop.wait(self.interval):
            self.sample()

    def start(self) -> "MemSampler":
        self.sample()
        if self.peak_kb is not None:
            self._thread = threading.Thread(target=self._loop, name="lectureflow-mem", daemon=True)
            self._thread.start()
        return self

    def stop(self) -> None:
        self._stop.set()
        if self._thread is not None:
            self._thread.join(timeout=2)
            self._thread = None
            self.sample()

    def fields(self) -> Dict[str, int]:
        if self.peak_kb is None or self.total_kb is None:
            return {}
        return {"mem_peak_mb": self.peak_kb // 1024, "mem_total_mb": self.total_kb // 1024}


# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------

def run(job_path: str, log_dir: str, ev: Events,
        install: Callable[..., int] = None, sampler: Optional[MemSampler] = None) -> None:
    ev.stage("check")
    job = load_job(job_path)
    job_dir = os.path.dirname(os.path.abspath(job_path))
    memory_dir = memory_base_of(job_dir) if job.get("memory") else None
    audio_path = os.path.join(job_dir, job["audio"])
    if not os.path.isfile(audio_path):
        raise RemoteError("audio_missing", input_error=True)
    if file_sha256(audio_path) != job["audio_sha256"]:
        raise RemoteError("audio_hash_mismatch")

    ev.stage("install")
    target = None
    if memory_dir:
        try:
            target = setup_memory_env(memory_dir)
        except OSError as e:
            raise RemoteError("install_failed", exc=e)
        log("memory mode: packages, model cache and temporary files on the RAM disk")
    ensure_requirements(job.get("requirements", []), log_dir, install, target)
    try:
        import asr  # this directory: the local worker's transcription code
        from asr_common import make_timing, write_result
    except Exception as e:
        raise RemoteError("install_failed", exc=e)
    asr.setup_cuda_libs()  # cuBLAS / cuDNN from the nvidia-* wheels, before ctranslate2 loads
    if cuda_device_count() < 1:
        raise RemoteError("no_gpu")

    def adapter(line: str) -> None:
        # asr.py's emit lines: PROGRESS → progress events, STATUS downloading_model →
        # a stage; every other line is a diagnostic for stderr.log.
        if line.startswith("PROGRESS "):
            try:
                ev.progress(float(line.split(" ", 1)[1]))
            except ValueError:
                pass
        elif line == "STATUS downloading_model":
            ev.stage("download_model")
        else:
            log(line)

    try:
        model_path = asr.resolve_model_path(job["model"], adapter)
    except asr.InputError as e:
        raise RemoteError("bad_job", input_error=True, exc=e)
    except Exception as e:
        raise RemoteError("model_load_failed", exc=e)

    ev.stage("load_model")
    try:
        audio = asr.decode_audio(audio_path)
    except asr.InputError as e:
        raise RemoteError("audio_unreadable", input_error=True, exc=e)
    try:
        os.remove(audio_path)  # decoded into memory: the upload is not needed any more
    except OSError:
        pass
    t0 = time.monotonic()
    try:
        model = asr.load_model(model_path, DEVICE, job["compute_type"], CPU_THREADS, adapter)
    except Exception as e:
        raise RemoteError("out_of_memory" if _is_oom(e) else "model_load_failed", exc=e)
    t1 = time.monotonic()

    ev.stage("transcribe")
    rjob = asr.Job(audio_path, job["model"], job["compute_type"], job.get("language") or None,
                   job.get("initial_prompt") or "", DEVICE, CPU_THREADS,
                   redecode=job.get("redecode", True))
    post = {}  # the counters of asr.py's steps after decoding (timing.postprocess)
    try:
        result = asr.transcribe_job(model, audio, rjob, adapter, postprocess=post)
    except Exception as e:
        raise RemoteError("out_of_memory" if _is_oom(e) else "transcribe_failed", exc=e)
    t2 = time.monotonic()
    ev.progress(1.0, force=True)

    ev.stage("write")
    audio_s = len(audio) / float(asr.SAMPLE_RATE)
    result["timing"] = make_timing(t1 - t0, t2 - t1, audio_s, False, postprocess=post)
    out = os.path.join(job_dir, job["result"])
    tmp = out + ".tmp"
    try:
        write_result(tmp, result)
        os.replace(tmp, out)
        digest, size = file_sha256(out), os.path.getsize(out)
    except OSError as e:
        raise RemoteError("write_failed", exc=e)
    mem = None
    if sampler is not None:
        sampler.stop()
        mem = sampler.fields()
    ev.done(job["result"], digest, size, audio_s, t1 - t0, t2 - t1, len(result["sentences"]), mem)


# ---------------------------------------------------------------------------
# The resident worker (colab-batch)
# ---------------------------------------------------------------------------
#
# LectureFlow (internal/colab/job.go) runs one cell per lecture:
#     python colab_remote.py --attach --base BASE --job-dir BASE/jobs/ID <worker options>
# The attaching process makes sure the runtime's resident worker runs (one per runtime, started
# detached by the first attach: python colab_remote.py --serve --base BASE <worker options>) and
# copies events to its stdout: the worker's setup stages (install, download_model, load_model)
# while it is not ready yet, then the lecture's own (check, transcribe, write, then done or
# error), and exits at once after the last one.
#
# The worker prepares the environment (uv or pip) and downloads the model at the same time,
# from the moment the first lecture's cell starts — while LectureFlow is still uploading that
# lecture's audio — loads the model and keeps it on the GPU: later lectures of the runtime only
# decode their audio. It takes every BASE/jobs/*/job.json (LectureFlow writes it last, after the
# audio; a directory with audio and no job.json is a lecture uploaded ahead of its turn) in the
# order they appear, at most --workers at once: one WhisperModel with that many workers, two
# lectures at once on the larger GPUs (the same words as one at a time, colab-prof §9.2). A
# lecture whose job.json disappears (LectureFlow cancelled it and deletes its files) is stopped
# and its directory removed.
#
# Files in BASE: worker.pid, worker.events (the worker's setup events), worker.ready (the model
# is loaded), worker.log (its stderr), worker.lock (attaching processes start one worker at a
# time). Per job: events (written by the worker, copied by the attach), job.log (diagnostics).
# None of them ever holds transcript text: events are the stdout contract, logs are asr.py's
# diagnostic lines (counts and times).

WORKER_PID, WORKER_EVENTS, WORKER_READY = "worker.pid", "worker.events", "worker.ready"
WORKER_LOG, WORKER_LOCK = "worker.log", "worker.lock"
JOB_EVENTS, JOB_LOG = "events", "job.log"
POLL_S = 0.1
# How long an attach waits for its lecture (the upload of a long lecture over a slow line
# included) before it gives up; LectureFlow deletes the job directory long before.
ATTACH_WAIT_S = 6 * 3600
# How long the worker retries a job.json it cannot parse yet.
JOB_READ_S = 30.0
# faster_whisper.utils.download_model's allow_patterns (1.2.1): the files the worker downloads
# while the packages install.
FW_PATTERNS = ["config.json", "preprocessor_config.json", "model.bin", "tokenizer.json", "vocabulary.*"]


class JobCancelled(BaseException):
    """The lecture's job.json disappeared: LectureFlow cancelled it (BaseException, so that no
    `except Exception` on the way swallows it)."""


def worker_args(args) -> List[str]:
    """The options an attach passes on to the worker it starts."""
    out = ["--model", args.model, "--compute-type", args.compute_type,
           "--requirements", args.requirements or "", "--workers", str(max(1, args.workers))]
    if args.memory:
        out.append("--memory")
    return out


def proc_alive(pid: int) -> bool:
    """pid is a running (not zombie) resident worker (Linux /proc)."""
    try:
        with open("/proc/%d/stat" % pid, "rb") as f:
            stat = f.read().decode("ascii", "replace")
        if stat[stat.rfind(")") + 2:][:1] in ("Z", "X"):
            return False
        with open("/proc/%d/cmdline" % pid, "rb") as f:
            return b"--serve" in f.read()
    except (OSError, ValueError):
        return False


def spawn_worker(base: str, options: List[str]) -> int:
    """Starts the resident worker detached (its own session: deleting a kernel does not stop
    it); its stderr goes to BASE/worker.log."""
    with open(os.path.join(base, WORKER_LOG), "ab") as errf:
        p = subprocess.Popen([sys.executable, "-u", os.path.join(base, "colab_remote.py"), "--serve",
                              "--base", base] + options, stdin=subprocess.DEVNULL,
                             stdout=subprocess.DEVNULL, stderr=errf, cwd=base, close_fds=True,
                             start_new_session=True)
    return p.pid


@contextlib.contextmanager
def file_lock(path: str):
    f = open(path, "a")
    try:
        try:
            import fcntl
            fcntl.flock(f, fcntl.LOCK_EX)
        except ImportError:  # not Linux (tests)
            pass
        yield
    finally:
        f.close()


def ensure_worker(base: str, options: List[str], spawn: Callable[[str, List[str]], int],
                  alive: Callable[[int], bool]) -> int:
    """The resident worker's pid, starting one (and forgetting what a dead one left: its
    ready mark and setup events) when none runs."""
    with file_lock(os.path.join(base, WORKER_LOCK)):
        pid = 0
        try:
            with open(os.path.join(base, WORKER_PID), encoding="ascii") as f:
                pid = int(f.read().strip() or 0)
        except (OSError, ValueError):
            pid = 0
        if pid > 0 and alive(pid):
            return pid
        for name in (WORKER_READY, WORKER_EVENTS):
            try:
                os.remove(os.path.join(base, name))
            except OSError:
                pass
        pid = spawn(base, options)
        tmp = os.path.join(base, WORKER_PID + ".tmp")
        with open(tmp, "w", encoding="ascii") as f:
            f.write(str(pid))
        os.replace(tmp, os.path.join(base, WORKER_PID))
        log("resident worker started")
        return pid


class Tail:
    """New complete lines of a file that grows (a missing file has none yet)."""

    def __init__(self, path: str):
        self.path, self.pos, self.buf = path, 0, b""

    def lines(self) -> List[str]:
        try:
            with open(self.path, "rb") as f:
                f.seek(self.pos)
                data = f.read()
        except OSError:
            return []
        self.pos += len(data)
        self.buf += data
        *done, self.buf = self.buf.split(b"\n")
        return [l.decode("utf-8", "replace").rstrip("\r") for l in done if l.strip()]


def check_event_line(line: str) -> dict:
    """line is one event of the stdout contract (the worker wrote it with Events); ValueError
    otherwise. The attach checks before it copies, Go checks again."""
    ev = json.loads(line)
    if not isinstance(ev, dict) or ev.get("type") not in ("stage", "progress", "done", "error",
                                                          "machine", "sample"):
        raise ValueError("not an event")
    required, optional = EVENT_KEYS[ev["type"]]
    if ev["type"] in ("machine", "sample"):
        check_reading(ev["type"], {k: v for k, v in ev.items() if k not in ("type", "ts")})
    if not set(required) <= set(ev) <= set(required) | set(optional):
        raise ValueError("fields of a %s event" % ev["type"])
    if not all(isinstance(v, (str, int, float, bool)) for v in ev.values()):
        raise ValueError("a value that is not a scalar")
    if ev["type"] == "stage" and ev["stage"] not in STAGES:
        raise ValueError("stage")
    if ev["type"] == "error" and ev["code"] not in ERROR_CODES:
        raise ValueError("error code")
    return ev


# ---------------------------------------------------------------------------
# The runtime's own readings (colab-obs-core; .claude/research/colab-telemetry.md §3)
# ---------------------------------------------------------------------------
#
# One attaching process per runtime samples: the one that holds BASE/sample.lock (a
# non-blocking flock; the others try again every SAMPLE_S, so when the sampling attach ends
# with its lecture the next one takes over). It sends a machine event, then a sample every
# SAMPLE_S. Its thread only collects values; the attach's main loop writes them (before the
# worker's lines, never after the final event), so stdout has one writer and cancelling or the
# final event are never held up by a slow nvidia-smi. Values come from /proc and nvidia-smi,
# never from audio; an unreadable one is -1 or "".
#
# GPU readings: one resident `nvidia-smi --query-gpu=... -lms 2000` per sampling attach (a
# reader thread keeps its last line; it is ended with the attach, and on Linux also by the
# kernel when the attach dies: PR_SET_PDEATHSIG); the Whisper process's GPU memory every
# APPS_S with --query-compute-apps (pids in a container are the container's).
# Memory (§3.3, all MiB): mem_total / mem_available from /proc/meminfo; data = every file under
# BASE/jobs (uploads, also the ones ahead of their turn, job.json, results, logs); memory mode:
# ramdisk = the RAM disk in use minus data, data on the RAM disk; disk mode: no RAM disk
# (-1), data 0 in memory and data_disk_mb on the disk; whisper = RssAnon of the resident worker
# and its descendants (anonymous memory only: its file pages are reclaimable, its mapped RAM
# disk files are in the ramdisk part).

SAMPLE_LOCK = "sample.lock"
SAMPLE_S = 2.0
APPS_S = 10.0
SMI_GPU_FIELDS = ("utilization.gpu", "memory.used", "memory.reserved", "temperature.gpu",
                  "power.draw")
SMI_TIMEOUT_S = 10


def try_flock(path: str):
    """An open file holding an exclusive flock on path, or None when another process holds it
    (or flock is not available: not Linux)."""
    try:
        import fcntl
    except ImportError:
        return None
    try:
        f = open(path, "a")
    except OSError:
        return None
    try:
        fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        f.close()
        return None
    return f


def _die_with_parent() -> None:
    """preexec_fn of the nvidia-smi loop: the kernel ends it when the attach dies (Linux)."""
    try:
        import ctypes
        import signal
        ctypes.CDLL("libc.so.6", use_errno=True).prctl(1, signal.SIGTERM)  # PR_SET_PDEATHSIG
    except Exception:
        pass


def run_capture(argv: List[str]) -> Optional[str]:
    """stdout of a short command, None when it fails."""
    try:
        r = subprocess.run(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                           stderr=subprocess.DEVNULL, timeout=SMI_TIMEOUT_S)
    except (OSError, subprocess.SubprocessError):
        return None
    if r.returncode != 0:
        return None
    return r.stdout.decode("utf-8", "replace")


def popen_lines(argv: List[str]):
    """A long-running command whose stdout lines are read (the nvidia-smi loop)."""
    kw = {"preexec_fn": _die_with_parent} if sys.platform.startswith("linux") else {}
    return subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                            stderr=subprocess.DEVNULL, **kw)


def smi_number(text: str) -> float:
    """One nvidia-smi csv value ("[N/A]", "[Not Supported]", empty: -1)."""
    try:
        v = float(text.strip())
    except ValueError:
        return -1.0
    return v if math.isfinite(v) and v >= 0 else -1.0


def clean_str(key: str, value: str) -> str:
    """value when it fits the field's pattern, else "" (unknown)."""
    value = (value or "").strip()
    return value if READING_STRINGS[key].fullmatch(value) else ""


def clean_int(value, top: int = MAX_MB) -> int:
    try:
        v = int(round(float(value)))
    except (TypeError, ValueError, OverflowError):
        return -1
    return v if 0 <= v <= top else -1


def clean_number(value, top: float) -> float:
    try:
        v = float(value)
    except (TypeError, ValueError):
        return -1
    if not math.isfinite(v) or v < 0 or v > top:
        return -1
    return round(v, 1)


class Sampler:
    """Collects the machine and sample events of an attaching process (see above)."""

    def __init__(self, base: str, memory: bool, lock: Callable[[str], object] = None,
                 run: Callable[[List[str]], Optional[str]] = None,
                 popen: Callable[[List[str]], object] = None, proc: str = "/proc",
                 interval: float = SAMPLE_S, apps_interval: float = APPS_S,
                 clock: Callable[[], float] = time.monotonic,
                 statvfs: Callable[[str], object] = None, cpu_count: Callable[[], Optional[int]] = None):
        self.base, self.memory = os.path.abspath(base), memory
        self.lock, self.run, self.popen = lock or try_flock, run or run_capture, popen or popen_lines
        self.proc, self.interval, self.apps_interval, self.clock = proc, interval, apps_interval, clock
        self.statvfs = statvfs or getattr(os, "statvfs", None)
        self.cpu_count = cpu_count or os.cpu_count
        self.mu = threading.Lock()
        self.pending: List[tuple] = []
        self.stopped = threading.Event()
        self.thread: Optional[threading.Thread] = None
        self.handle = None
        self.smi = None
        self.smi_last: Optional[str] = None
        self.smi_fields = SMI_GPU_FIELDS
        self.cpu_prev = None
        self.vram_whisper = -1
        self.next_apps = 0.0

    # ---- the attach's side ----

    def start(self) -> "Sampler":
        self.thread = threading.Thread(target=self._loop, name="lectureflow-sample", daemon=True)
        self.thread.start()
        return self

    def take(self) -> List[tuple]:
        """The (type, fields) collected since the last call, oldest first."""
        with self.mu:
            out, self.pending = self.pending, []
        return out

    def stop(self) -> None:
        self.stopped.set()
        self._stop_smi()
        if self.thread is not None:
            self.thread.join(timeout=2)
        if self.handle is not None:
            try:
                self.handle.close()
            except Exception:
                pass
            self.handle = None

    # ---- the thread ----

    def _loop(self) -> None:
        while not self.stopped.is_set():
            try:
                if self.handle is None:
                    self.handle = self.lock(os.path.join(self.base, SAMPLE_LOCK))
                if self.handle is not None:
                    if self.smi is None and not self.stopped.is_set():
                        self._push("machine", self.machine())
                        self._start_smi()
                    self._push("sample", self.sample())
            except Exception as e:  # never the attach's business: the next round tries again
                log("sampling failed (%s)" % type(e).__name__)
            self.stopped.wait(self.interval)

    def _push(self, typ: str, fields: dict) -> None:
        with self.mu:
            self.pending.append((typ, fields))

    def _start_smi(self, fields=None) -> None:
        self.smi_fields = fields or SMI_GPU_FIELDS
        argv = ["nvidia-smi", "--query-gpu=" + ",".join(self.smi_fields),
                "--format=csv,noheader,nounits", "-i", "0", "-lms", str(int(self.interval * 1000))]
        try:
            self.smi = self.popen(argv)
        except (OSError, ValueError, subprocess.SubprocessError):
            self.smi = False  # no nvidia-smi: GPU values stay unknown
            return
        smi = self.smi
        threading.Thread(target=self._read_smi, args=(smi,), name="lectureflow-smi", daemon=True).start()

    def _read_smi(self, smi) -> None:
        got = False
        try:
            for raw in iter(smi.stdout.readline, b""):
                line = raw.decode("utf-8", "replace").strip() if isinstance(raw, bytes) else str(raw).strip()
                if line:
                    got = True
                    self.smi_last = line
        except (OSError, ValueError):
            pass
        if not got and "memory.reserved" in self.smi_fields and not self.stopped.is_set():
            # an older driver without memory.reserved refuses the whole query
            self._start_smi(tuple(f for f in self.smi_fields if f != "memory.reserved"))

    def _stop_smi(self) -> None:
        smi = self.smi
        if smi:
            try:
                smi.kill()
                smi.wait(timeout=2)
            except Exception:
                pass

    # ---- readings ----

    def machine(self) -> dict:
        name = arch = cap = driver = cuda = ""
        count, vram = -1, -1
        out = self.run(["nvidia-smi", "--query-gpu=name,memory.total,driver_version,compute_cap",
                        "--format=csv,noheader,nounits"])
        if out is not None:
            lines = [l for l in out.splitlines() if l.strip()]
            count = len(lines)
            if lines:
                parts = [p.strip() for p in lines[0].split(",")]
                if len(parts) >= 4:
                    name, vram, driver, cap = parts[0], clean_int(smi_number(parts[1])), parts[2], parts[3]
        q = self.run(["nvidia-smi", "-q"])
        if q is not None:
            m = re.search(r"CUDA Version\s*:\s*([0-9.]+)", q)
            cuda = m.group(1) if m else ""
            m = re.search(r"Product Architecture\s*:\s*([^\r\n]+)", q)
            arch = m.group(1) if m else ""
        mi = read_meminfo(os.path.join(self.proc, "meminfo"))
        self.cpu_prev = self._cpu_times()
        return {"gpu_name": clean_str("gpu_name", name), "gpu_arch": clean_str("gpu_arch", arch),
                "compute_cap": clean_str("compute_cap", cap), "driver": clean_str("driver", driver),
                "cuda": clean_str("cuda", cuda), "gpu_count": clean_int(count, MAX_GPUS),
                "vram_total_mb": vram, "vcpus": clean_int(self.cpu_count() or -1, MAX_VCPUS),
                "mem_total_mb": clean_int(mi["MemTotal"] // 1024) if "MemTotal" in mi else -1,
                "storage": "memory" if self.memory else "disk"}

    def sample(self) -> dict:
        mi = read_meminfo(os.path.join(self.proc, "meminfo"))
        total = clean_int(mi["MemTotal"] // 1024) if "MemTotal" in mi else -1
        avail = clean_int(mi["MemAvailable"] // 1024) if "MemAvailable" in mi else -1
        data = self._data_mb()
        if self.memory:
            used = self._used_mb(self.base)
            ramdisk = max(0, used - max(data, 0)) if used >= 0 else -1
            data_mb, data_disk = data, -1
        else:
            ramdisk, data_mb, data_disk = -1, 0, data
        pids = self._whisper_pids()
        whisper = self._rss_anon_mb(pids) if pids else -1
        now = self.clock()
        if now >= self.next_apps:
            self.next_apps = now + self.apps_interval
            self.vram_whisper = self._vram_of(pids)
        gpu = dict.fromkeys(SMI_GPU_FIELDS, -1.0)
        line = self.smi_last
        if line:
            vals = [smi_number(v) for v in line.split(",")]
            if len(vals) == len(self.smi_fields):
                gpu.update(zip(self.smi_fields, vals))
        return {"cpu_pct": self._cpu_pct(), "mem_total_mb": total, "mem_available_mb": avail,
                "ramdisk_mb": ramdisk, "data_mb": data_mb, "data_disk_mb": data_disk, "whisper_mb": whisper,
                "gpu_util_pct": clean_number(gpu["utilization.gpu"], MAX_PCT),
                "vram_used_mb": clean_int(gpu["memory.used"]),
                "vram_reserved_mb": clean_int(gpu["memory.reserved"]),
                "vram_whisper_mb": self.vram_whisper,
                "gpu_temp_c": clean_number(gpu["temperature.gpu"], MAX_TEMP_C),
                "gpu_power_w": clean_number(gpu["power.draw"], MAX_POWER_W)}

    def _cpu_times(self):
        try:
            with open(os.path.join(self.proc, "stat"), encoding="ascii", errors="replace") as f:
                parts = f.readline().split()
        except OSError:
            return None
        if not parts or parts[0] != "cpu":
            return None
        try:
            v = [int(x) for x in parts[1:9]]
        except ValueError:
            return None
        return sum(v), v[3] + (v[4] if len(v) > 4 else 0)

    def _cpu_pct(self) -> float:
        cur, prev = self._cpu_times(), self.cpu_prev
        self.cpu_prev = cur
        if cur is None or prev is None or cur[0] <= prev[0]:
            return -1
        busy = (cur[0] - prev[0]) - (cur[1] - prev[1])
        return clean_number(100.0 * max(0, busy) / (cur[0] - prev[0]), MAX_PCT)

    def _data_mb(self) -> int:
        total = 0
        try:
            for root, _dirs, files in os.walk(os.path.join(self.base, "jobs")):
                for n in files:
                    try:
                        total += os.stat(os.path.join(root, n)).st_size
                    except OSError:
                        pass
        except OSError:
            return -1
        return clean_int(total / MIB)

    def _used_mb(self, path: str) -> int:
        if self.statvfs is None:
            return -1
        try:
            st = self.statvfs(path)
        except OSError:
            return -1
        return clean_int((st.f_blocks - st.f_bfree) * st.f_frsize / MIB)

    def _whisper_pids(self) -> List[int]:
        """The resident worker (BASE/worker.pid) and its descendants (/proc/*/stat)."""
        try:
            with open(os.path.join(self.base, WORKER_PID), encoding="ascii") as f:
                root = int(f.read().strip() or 0)
        except (OSError, ValueError):
            return []
        if root <= 0:
            return []
        children: Dict[int, List[int]] = {}
        try:
            names = os.listdir(self.proc)
        except OSError:
            names = []
        for n in names:
            if not n.isdigit():
                continue
            try:
                with open(os.path.join(self.proc, n, "stat"), "rb") as f:
                    stat = f.read().decode("ascii", "replace")
                ppid = int(stat[stat.rfind(")") + 2:].split()[1])
            except (OSError, ValueError, IndexError):
                continue
            children.setdefault(ppid, []).append(int(n))
        out, todo = [], [root]
        while todo and len(out) < 256:
            p = todo.pop()
            out.append(p)
            todo.extend(children.get(p, []))
        return out

    def _rss_anon_mb(self, pids: List[int]) -> int:
        kb, seen = 0, False
        for p in pids:
            try:
                with open(os.path.join(self.proc, str(p), "status"), encoding="ascii", errors="replace") as f:
                    for line in f:
                        if line.startswith("RssAnon:"):
                            kb += int(line.split()[1])
                            seen = True
                            break
            except (OSError, ValueError, IndexError):
                continue
        return clean_int(kb / 1024.0) if seen else -1

    def _vram_of(self, pids: List[int]) -> int:
        if not pids:
            return -1
        out = self.run(["nvidia-smi", "--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits"])
        if out is None:
            return -1
        mb, want = 0.0, set(pids)
        for line in out.splitlines():
            parts = [p.strip() for p in line.split(",")]
            if len(parts) == 2 and parts[0].isdigit() and int(parts[0]) in want:
                mb += max(0.0, smi_number(parts[1]))
        return clean_int(mb)


def attach_main(args, stream, spawn: Callable[[str, List[str]], int] = None,
                alive: Callable[[int], bool] = None, poll: float = POLL_S,
                clock: Callable[[], float] = time.monotonic,
                sampler: Optional["Sampler"] = None) -> int:
    """--attach: start the resident worker when none runs, copy its setup events while it is
    not ready, then the lecture's events; return after the lecture's done (0) or error (1, or
    2 for an input error). Also returns when the job directory disappears (LectureFlow gave up
    on the lecture), and with an internal error when the worker dies or the wait is too long.
    sampler (main() passes one; None: no readings) adds the runtime's machine and sample
    events, written before the worker's lines of the same round and never after the final
    event; it is stopped (its nvidia-smi ended, the lock released) before this returns."""
    spawn, alive = spawn or spawn_worker, alive or proc_alive
    base, job_dir = os.path.abspath(args.base), os.path.abspath(args.job_dir)
    ev = Events(stream)
    if not os.path.isdir(job_dir) or os.path.dirname(os.path.dirname(job_dir)) != base:
        ev.error("bad_job", retryable=False)
        return EXIT_INPUT
    if sampler is None:
        return _attach(args, stream, ev, base, job_dir, spawn, alive, poll, clock, lambda: None)

    def readings() -> None:
        for typ, fields in sampler.take():
            try:
                ev.reading(typ, fields)
            except ValueError as e:  # never a contract violation: the reading is dropped
                log("attach: a %s reading is outside the contract (%s); dropped" % (typ, e))

    sampler.start()
    try:
        return _attach(args, stream, ev, base, job_dir, spawn, alive, poll, clock, readings)
    finally:
        sampler.stop()


def _attach(args, stream, ev, base, job_dir, spawn, alive, poll, clock, readings) -> int:
    def forward(line: str) -> Optional[dict]:
        try:
            e = check_event_line(line)
        except (ValueError, KeyError):
            log("attach: a line of the worker's events is not an event; dropped")
            return None
        stream.write(json.dumps(e, ensure_ascii=True, separators=(",", ":")) + "\n")
        stream.flush()
        return e

    def final(e: dict) -> int:
        return EXIT_OK if e["type"] == "done" else (EXIT_RETRY if e["retryable"] else EXIT_INPUT)

    spawned: List[int] = []

    def spawn_noted(b: str, options: List[str]) -> int:
        pid = spawn(b, options)
        spawned.append(pid)
        return pid

    try:
        pid = ensure_worker(base, worker_args(args), spawn_noted, alive)
    except Exception as e:
        log("attach: starting the resident worker failed (%s)" % type(e).__name__)
        ev.error("internal", retryable=True, exc=e)
        return EXIT_RETRY
    start = clock()
    next_check = start + 1.0
    # The attach that started the worker forwards its setup events even when the worker got
    # ready before this check (a quick setup on a busy machine: the stages were lost).
    if spawned or not os.path.exists(os.path.join(base, WORKER_READY)):
        setup = Tail(os.path.join(base, WORKER_EVENTS))
        while True:
            readings()
            ready = os.path.exists(os.path.join(base, WORKER_READY))
            for line in setup.lines():
                e = forward(line)
                if e is not None and e["type"] in ("error", "done"):
                    return final(e)
            if ready:
                break
            now = clock()
            if now >= next_check:
                next_check = now + 1.0
                if not alive(pid):
                    log("attach: the resident worker ended during its setup")
                    ev.error("internal", retryable=True)
                    return EXIT_RETRY
            time.sleep(poll)
    events = Tail(os.path.join(job_dir, JOB_EVENTS))
    while True:
        readings()
        for line in events.lines():
            e = forward(line)
            if e is not None and e["type"] in ("done", "error"):
                return final(e)
        if not os.path.isdir(job_dir):
            log("attach: the job directory is gone (the lecture was cancelled)")
            return EXIT_RETRY
        now = clock()
        if now >= next_check:
            next_check = now + 1.0
            if not alive(pid):
                for line in events.lines():  # what it wrote before it ended
                    e = forward(line)
                    if e is not None and e["type"] in ("done", "error"):
                        return final(e)
                log("attach: the resident worker ended")
                ev.error("internal", retryable=True)
                return EXIT_RETRY
            if now - start > ATTACH_WAIT_S:
                ev.error("internal", retryable=True)
                return EXIT_RETRY
        time.sleep(poll)


def load_whisper(path: str, compute_type: str, workers: int):
    """asr.load_model's WhisperModel with `workers` workers (num_workers: that many lectures,
    or parts of one, transcribe at once on one copy of the model)."""
    import asr
    from faster_whisper import WhisperModel
    kw = {"num_workers": workers} if workers > 1 else {}
    try:
        return WhisperModel(path, device=DEVICE, compute_type=compute_type, cpu_threads=CPU_THREADS, **kw)
    except ValueError as e:  # a compute type the GPU does not support: retrying cannot help
        raise asr.InputError("cannot load the model (compute_type=%s): %s" % (compute_type, type(e).__name__))


def hf_download(model: str) -> Optional[Callable[[], None]]:
    """The model download that runs while the packages install: the runtime's own
    huggingface_hub (imported before any package lands on the RAM disk), the files
    faster-whisper downloads, into the same cache. None for a name it cannot resolve (asr.py
    downloads it later)."""
    repo = MODEL_REPOS.get(model) or (model if "/" in model and not os.path.isdir(model) else "")
    if not repo:
        return None
    try:
        from huggingface_hub import snapshot_download
    except Exception:
        return None
    return lambda: snapshot_download(repo, allow_patterns=FW_PATTERNS)


class ResidentWorker:
    """--serve: setup() once (events to BASE/worker.events), then serve() takes the jobs."""

    def __init__(self, base: str, model: str, compute_type: str, reqs: List[str], memory: bool,
                 workers: int, install: Callable[..., int] = None,
                 uv_run: Optional[Callable[..., int]] = None,
                 load: Callable[[str, str, int], object] = None,
                 download: Callable[[str], Optional[Callable[[], None]]] = None,
                 meminfo: str = "/proc/meminfo", poll: float = POLL_S):
        self.base, self.model_name, self.compute_type = os.path.abspath(base), model, compute_type
        self.reqs, self.memory, self.workers = reqs, memory, max(1, workers)
        self.install, self.uv_run = install, uv_run
        self.load = load or load_whisper
        self.download = download or hf_download
        self.meminfo, self.poll = meminfo, poll
        self.model = None
        self.model_key = None
        self.load_s = 0.0
        self.first_done = False
        self.cond = threading.Condition()
        self.active = 0
        self.seen = set()
        self.running = set()
        self.queue: "queue.Queue[str]" = queue.Queue()
        self.stop = threading.Event()
        self.jobs_dir = os.path.join(self.base, "jobs")

    # ---- setup ----

    def setup(self) -> int:
        with open(os.path.join(self.base, WORKER_EVENTS), "a", encoding="utf-8", newline="\n",
                  buffering=1) as stream:
            ev = Events(stream)
            try:
                self._setup(ev)
            except RemoteError as e:
                log("setup failed: %s%s" % (e.code, " (%s)" % type(e.exc).__name__ if e.exc else ""))
                ev.error(e.code, retryable=not e.input_error, exc=e.exc)
                return EXIT_INPUT if e.input_error else EXIT_RETRY
            except Exception as e:
                import traceback
                traceback.print_exc(file=sys.stderr)
                ev.error("internal", retryable=True, exc=e)
                return EXIT_RETRY
        with open(os.path.join(self.base, WORKER_READY), "w", encoding="ascii") as f:
            f.write("ready\n")
        return EXIT_OK

    def _setup(self, ev: Events) -> None:
        ev.stage("install")
        target = None
        if self.memory:
            try:
                target = setup_memory_env(self.base)
            except OSError as e:
                raise RemoteError("install_failed", exc=e)
            log("memory mode: packages, model cache and temporary files on the RAM disk")
        # The model download runs while the packages install (different networks: PyPI and
        # Hugging Face; uv is quick, so the two hardly compete for the 2 vCPUs).
        box: Dict[str, object] = {}
        fetch = self.download(self.model_name)
        th = None
        if fetch is not None:
            def run_download():
                t = time.monotonic()
                try:
                    fetch()
                except Exception as e:  # asr.resolve_model_path downloads it again
                    box["err"] = type(e).__name__
                box["s"] = time.monotonic() - t
            th = threading.Thread(target=run_download, name="lectureflow-download", daemon=True)
            th.start()
        ensure_requirements(self.reqs, self.base, self.install, target, self.uv_run)
        try:
            import asr  # noqa: F401 (this directory: the local worker's transcription code)
        except Exception as e:
            raise RemoteError("install_failed", exc=e)
        asr.setup_cuda_libs()
        if cuda_device_count() < 1:
            raise RemoteError("no_gpu")
        if th is not None:
            if th.is_alive():
                ev.stage("download_model")
            th.join()
            log("model download in parallel: %.1fs%s" % (box.get("s", 0.0),
                                                         " (failed: %s)" % box["err"] if "err" in box else ""))
        self.model = self._load(ev, self.model_name, self.compute_type, log)
        self.model_key = (self.model_name, self.compute_type)

    def _load(self, ev: Events, model: str, compute_type: str, logf: Callable[[str], None]):
        import asr

        def adapter(line: str) -> None:
            if line == "STATUS downloading_model":
                ev.stage("download_model")
            else:
                logf(line)
        try:
            path = asr.resolve_model_path(model, adapter)
        except asr.InputError as e:
            raise RemoteError("bad_job", input_error=True, exc=e)
        except Exception as e:
            raise RemoteError("model_load_failed", exc=e)
        ev.stage("load_model")
        t0 = time.monotonic()
        try:
            m = self.load(path, compute_type, self.workers)
        except asr.InputError as e:
            raise RemoteError("model_load_failed", exc=e)
        except Exception as e:
            raise RemoteError("out_of_memory" if _is_oom(e) else "model_load_failed", exc=e)
        self.load_s = time.monotonic() - t0
        logf("model loaded in %.1fs (workers %d)" % (self.load_s, self.workers))
        return m

    # ---- jobs ----

    def discover(self) -> None:
        """Queues the job directories whose job.json appeared, oldest first; forgets the
        ones whose job.json is gone (a directory name may come again)."""
        try:
            names = os.listdir(self.jobs_dir)
        except OSError:
            names = []
        found = []
        for name in names:
            jp = os.path.join(self.jobs_dir, name, "job.json")
            try:
                mtime = os.stat(jp).st_mtime
            except OSError:
                continue
            if name not in self.seen:
                found.append((mtime, name))
        with self.cond:
            for name in list(self.seen):
                if name not in self.running and not os.path.exists(os.path.join(self.jobs_dir, name, "job.json")):
                    self.seen.discard(name)
            for _, name in sorted(found):
                self.seen.add(name)
                self.queue.put(os.path.join(self.jobs_dir, name))

    def serve(self) -> None:
        threads = [threading.Thread(target=self._job_loop, name="lectureflow-job-%d" % i, daemon=True)
                   for i in range(self.workers)]
        for t in threads:
            t.start()
        while not self.stop.is_set():
            self.discover()
            self.stop.wait(self.poll)
        for t in threads:
            t.join(timeout=5)

    def _job_loop(self) -> None:
        while not self.stop.is_set():
            try:
                job_dir = self.queue.get(timeout=self.poll * 5)
            except queue.Empty:
                continue
            self.run_job(job_dir)

    def run_job(self, job_dir: str) -> None:
        name = os.path.basename(job_dir)
        jp = os.path.join(job_dir, "job.json")
        try:
            stream = open(os.path.join(job_dir, JOB_EVENTS), "a", encoding="utf-8", newline="\n", buffering=1)
        except OSError:
            return  # the directory is gone already
        with self.cond:
            self.running.add(name)
        ev = Events(stream)
        joblog = JobLog(os.path.join(job_dir, JOB_LOG))
        sampler = MemSampler(self.meminfo).start()
        cancelled = False
        try:
            self._process(job_dir, jp, ev, joblog, sampler)
        except JobCancelled:
            cancelled = True
        except RemoteError as e:
            joblog("failed: %s%s" % (e.code, " (%s)" % type(e.exc).__name__ if e.exc else ""))
            ev.error(e.code, retryable=not e.input_error, exc=e.exc)
        except Exception as e:
            import traceback
            joblog(traceback.format_exc())
            ev.error("internal", retryable=True, exc=e)
        finally:
            sampler.stop()
            stream.close()
            joblog.close()
            with self.cond:
                self.running.discard(name)
        if cancelled or not os.path.exists(jp):
            log("a lecture was cancelled; its directory is removed")
            import shutil
            shutil.rmtree(job_dir, ignore_errors=True)

    def _read_job(self, jp: str, cancelled: Callable[[], bool]) -> dict:
        deadline = time.monotonic() + JOB_READ_S
        while True:
            try:
                return load_job(jp)
            except RemoteError:
                if cancelled():
                    raise JobCancelled()
                if time.monotonic() >= deadline:
                    raise
                time.sleep(self.poll)

    def _process(self, job_dir: str, jp: str, ev: Events, joblog: "JobLog", sampler: MemSampler) -> None:
        import asr
        from asr_common import make_timing, write_result

        def cancelled() -> bool:
            return not os.path.exists(jp)

        ev.stage("check")
        job = self._read_job(jp, cancelled)
        if bool(job.get("memory")) != self.memory or \
                (self.memory and memory_base_of(job_dir) != self.base):
            raise RemoteError("bad_job", input_error=True)
        split = job.get("split", 1)
        audio_path = os.path.join(job_dir, job["audio"])
        if not os.path.isfile(audio_path):
            raise RemoteError("audio_missing", input_error=True)
        if file_sha256(audio_path) != job["audio_sha256"]:
            raise RemoteError("audio_hash_mismatch")
        try:
            audio = asr.decode_audio(audio_path)
        except asr.InputError as e:
            raise RemoteError("audio_unreadable", input_error=True, exc=e)
        try:
            os.remove(audio_path)  # decoded into memory: the upload is not needed any more
        except OSError:
            pass
        if cancelled():
            raise JobCancelled()

        def adapter(line: str) -> None:
            if cancelled():
                raise JobCancelled()
            if line.startswith("PROGRESS "):
                try:
                    ev.progress(float(line.split(" ", 1)[1]))
                except ValueError:
                    pass
            elif line != "STATUS downloading_model":
                joblog(line)

        model = self._acquire(job, ev, joblog)
        try:
            load_s = 0.0
            with self.cond:
                if not self.first_done:
                    self.first_done, load_s = True, self.load_s
            ev.stage("transcribe")
            rjob = asr.Job(audio_path, job["model"], job["compute_type"], job.get("language") or None,
                           job.get("initial_prompt") or "", DEVICE, CPU_THREADS,
                           redecode=job.get("redecode", True))
            post: Dict[str, object] = {}
            t1 = time.monotonic()
            try:
                if split > 1:
                    result = transcribe_split(model, audio, rjob, adapter, post, split, joblog)
                else:
                    result = asr.transcribe_job(model, audio, rjob, adapter, postprocess=post)
            except (JobCancelled, RemoteError):
                raise
            except Exception as e:
                raise RemoteError("out_of_memory" if _is_oom(e) else "transcribe_failed", exc=e)
            t2 = time.monotonic()
        finally:
            self._release()
        ev.progress(1.0, force=True)
        ev.stage("write")
        audio_s = len(audio) / float(asr.SAMPLE_RATE)
        result["timing"] = make_timing(load_s, t2 - t1, audio_s, False, postprocess=post)
        out = os.path.join(job_dir, job["result"])
        tmp = out + ".tmp"
        try:
            write_result(tmp, result)
            if cancelled():
                os.remove(tmp)
                raise JobCancelled()
            os.replace(tmp, out)
            digest, size = file_sha256(out), os.path.getsize(out)
        except OSError as e:
            raise RemoteError("write_failed", exc=e)
        sampler.stop()
        ev.done(job["result"], digest, size, audio_s, load_s, t2 - t1, len(result["sentences"]),
                sampler.fields())

    def _acquire(self, job: dict, ev: Events, joblog: "JobLog"):
        """The model for the job; another model or precision than the loaded one (the user
        changed the settings) is loaded once no other lecture uses the current one."""
        key = (job["model"], job["compute_type"])
        with self.cond:
            while key != self.model_key and self.active:
                self.cond.wait()
            if key != self.model_key:
                self.model = None
                self.model = self._load(ev, key[0], key[1], joblog)
                self.model_key = key
            self.active += 1
            return self.model

    def _release(self) -> None:
        with self.cond:
            self.active -= 1
            self.cond.notify_all()


class JobLog:
    """A lecture's diagnostics (job.log in its directory)."""

    def __init__(self, path: str):
        try:
            self.f = open(path, "a", encoding="utf-8", newline="\n", buffering=1)
        except OSError:
            self.f = None

    def __call__(self, line: str) -> None:
        if self.f is not None:
            try:
                self.f.write("colab_remote: %s\n" % line)
            except (OSError, ValueError):
                pass

    def close(self) -> None:
        if self.f is not None:
            self.f.close()
            self.f = None


def serve_main(args, install: Callable[..., int] = None) -> int:
    reqs = [r for r in (args.requirements or "").split(",") if r]
    if not args.base or not MODEL_RE.match(args.model or "") or not args.compute_type or \
            not all(REQUIREMENT_RE.match(r) for r in reqs):
        return EXIT_INPUT
    w = ResidentWorker(args.base, args.model, args.compute_type, reqs, args.memory, args.workers,
                       install=install)
    rc = w.setup()
    if rc != EXIT_OK:
        return rc
    w.serve()
    return EXIT_OK


# ---------------------------------------------------------------------------
# "极速模式" (colab-batch; user decision 2026-10-27 05:40: an option, off by default)
# ---------------------------------------------------------------------------
#
# On the larger GPUs one lecture leaves most of the GPU idle (colab-prof §9.1: 53–74 % on a
# G4). Cut at silences into parts that transcribe at once on the same model (num_workers), the
# lecture takes 16–62 % less time; the words differ from the whole lecture's by about 3–4 %
# (case and punctuation more: the 30-second windows of every later part start at its cut,
# colab-prof §9.3), which is why the user must turn it on. Each part runs the whole of
# asr.transcribe_job (the same parameters, filters, second decode and text repair); only the
# first part gets the initial prompt, as the whole lecture's first window does.

# A lecture shorter than this many seconds per part is not cut (the parts' fixed costs).
SPLIT_MIN_PART_S = 120.0
# Each cut lies at least this share of the lecture after the previous one.
SPLIT_MIN_GAP_SHARE = 0.15


def split_points(chunks, n_samples: int, parts: int, sr: int) -> List[int]:
    """Sample offsets to cut at: the middle of a silence between VAD speech chunks
    (chunks: [{"start", "end"}] in samples), one near each equal-length target, preferring
    longer silences (gap length minus distance to the target / 4); fewer cuts when there are
    not enough silences or the lecture is short."""
    parts = min(parts, max(1, int(n_samples / sr / SPLIT_MIN_PART_S)))
    gaps = [(chunks[i]["end"], chunks[i + 1]["start"]) for i in range(len(chunks) - 1)
            if chunks[i + 1]["start"] > chunks[i]["end"]]
    cuts: List[int] = []
    prev = 0
    for k in range(1, parts):
        target = n_samples * k / parts
        cand = [g for g in gaps if (g[0] + g[1]) / 2 > prev + SPLIT_MIN_GAP_SHARE * n_samples
                and (g[0] + g[1]) / 2 < n_samples * (1 - SPLIT_MIN_GAP_SHARE / 2)]
        if not cand:
            break
        g = max(cand, key=lambda g: (g[1] - g[0]) / sr - abs((g[0] + g[1]) / 2 - target) / sr / 4)
        prev = (g[0] + g[1]) // 2
        cuts.append(int(prev))
    return cuts


def merge_counters(dst: dict, src: dict) -> None:
    """Adds one part's postprocess counters to dst: numbers summed, flags or-ed."""
    for step, counters in src.items():
        if not isinstance(counters, dict):
            continue
        acc = dst.setdefault(step, {})
        for k, v in counters.items():
            if isinstance(v, bool):
                acc[k] = bool(acc.get(k, False)) or v
            elif isinstance(v, (int, float)) and isinstance(acc.get(k, 0), (int, float)):
                total = acc.get(k, 0) + v
                acc[k] = round(total, 3) if isinstance(total, float) else total
            else:
                acc.setdefault(k, v)


def transcribe_split(model, audio, rjob, emit, post, parts, logf):
    """The lecture in at most `parts` parts at once (see above); result.json as from
    asr.transcribe_job, the parts' sentences one after the other on the lecture's time line,
    and post["split"] = {"parts", "cuts_ms"} so the result says it was cut."""
    import asr
    from asr_common import format_progress
    sr = asr.SAMPLE_RATE
    from faster_whisper.vad import VadOptions, get_speech_timestamps
    t0 = time.monotonic()
    cuts = split_points(get_speech_timestamps(audio, VadOptions()), len(audio), parts, sr)
    if not cuts:
        logf("fast mode: no place to cut; the lecture is transcribed whole")
        return asr.transcribe_job(model, audio, rjob, emit, postprocess=post)
    bounds = [0] + cuts + [len(audio)]
    n = len(bounds) - 1
    logf("fast mode: %d parts, VAD %.1fs" % (n, time.monotonic() - t0))
    weights = [(bounds[i + 1] - bounds[i]) / float(len(audio)) for i in range(n)]
    progress = [0.0] * n
    lock = threading.Lock()
    results: List[Optional[dict]] = [None] * n
    posts: List[dict] = [{} for _ in range(n)]
    errors: List[Optional[BaseException]] = [None] * n

    def part_emit(i):
        def f(line: str) -> None:
            if line.startswith("PROGRESS "):
                try:
                    v = float(line.split(" ", 1)[1])
                except ValueError:
                    return
                with lock:  # one Events object: one part at a time
                    progress[i] = max(progress[i], v)
                    emit(format_progress(sum(p * w for p, w in zip(progress, weights))))
            elif line != "STATUS downloading_model":
                with lock:
                    emit("part %d: %s" % (i + 1, line))
        return f

    def run(i):
        try:
            job = rjob if i == 0 else rjob._replace(initial_prompt="")
            results[i] = asr.transcribe_job(model, audio[bounds[i]:bounds[i + 1]], job, part_emit(i),
                                            postprocess=posts[i])
        except BaseException as e:  # JobCancelled included: re-raised below
            errors[i] = e

    threads = [threading.Thread(target=run, args=(i,), name="lectureflow-part-%d" % i, daemon=True)
               for i in range(n)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    for e in errors:
        if isinstance(e, JobCancelled):
            raise e
    for e in errors:
        if e is not None:
            raise e
    sentences = []
    for i, r in enumerate(results):
        off = int(round(bounds[i] * 1000.0 / sr))
        for sent in r["sentences"]:
            sent = dict(sent)
            sent["start_ms"] += off
            sent["end_ms"] += off
            sent["words"] = [dict(w, s=w["s"] + off, e=w["e"] + off) for w in sent.get("words", [])]
            sentences.append(sent)
    for p in posts:
        merge_counters(post, p)
    post["split"] = {"parts": n, "cuts_ms": [int(round(c * 1000.0 / sr)) for c in cuts]}
    return {"prompt_terms_used": results[0]["prompt_terms_used"], "sentences": sentences}


def probe_main(args, ev: Events, probe: Callable[..., dict] = None) -> int:
    """--memory-probe: one memory event; a probe that fails says disk (reason error)."""
    probe = probe or memory_probe
    reqs = [r for r in (args.requirements or "").split(",") if r]
    if not MODEL_RE.match(args.model or "") or args.audio_bytes < 0 or \
            not all(REQUIREMENT_RE.match(r) for r in reqs):
        ev.memory({"mode": "disk", "base": "", "reason": "error", "mem_total_mb": 0,
                   "mem_available_mb": 0, "need_mb": 0, "free_mb": 0})
        return EXIT_INPUT
    try:
        d = probe(args.model, args.audio_bytes, reqs, extra_mb=args.extra_mb, no_shm=args.no_shm)
    except Exception as e:
        log("memory probe failed (%s)" % type(e).__name__)
        d = {"mode": "disk", "base": "", "reason": "error", "mem_total_mb": 0,
             "mem_available_mb": 0, "need_mb": 0, "free_mb": 0}
    ev.memory(d)
    return EXIT_OK


def main(argv: Optional[List[str]] = None, isolate: bool = True,
         install: Callable[..., int] = None, probe: Callable[..., dict] = None,
         meminfo: str = "/proc/meminfo") -> int:
    p = argparse.ArgumentParser(prog="colab_remote.py")
    p.add_argument("--job")
    p.add_argument("--log-dir", dest="log_dir", default=None)
    p.add_argument("--memory-probe", dest="memory_probe", action="store_true")
    p.add_argument("--model", default="")
    p.add_argument("--audio-bytes", dest="audio_bytes", type=int, default=0)
    p.add_argument("--requirements", default="")
    p.add_argument("--extra-mb", dest="extra_mb", type=int, default=0)
    p.add_argument("--no-shm", dest="no_shm", action="store_true")
    # the resident worker (colab-batch)
    p.add_argument("--attach", action="store_true")
    p.add_argument("--serve", action="store_true")
    p.add_argument("--base", default="")
    p.add_argument("--job-dir", dest="job_dir", default="")
    p.add_argument("--compute-type", dest="compute_type", default="")
    p.add_argument("--workers", type=int, default=1)
    p.add_argument("--memory", action="store_true")
    try:
        args = p.parse_args(argv)
    except SystemExit:
        return EXIT_INPUT
    os.environ.update(TELEMETRY_OFF_ENV)
    if args.serve:
        return serve_main(args, install)
    if not args.memory_probe and not args.job and not args.attach:
        return EXIT_INPUT
    ctx = isolated_stdout() if isolate else contextlib.nullcontext(sys.stdout)
    if args.memory_probe:
        with ctx as stream:
            return probe_main(args, Events(stream), probe)
    if args.attach:
        if not args.base or not args.job_dir or not MODEL_RE.match(args.model or "") or                 args.workers < 1 or args.workers > 8:
            return EXIT_INPUT
        with ctx as stream:
            return attach_main(args, stream, sampler=Sampler(args.base, args.memory))
    log_dir = args.log_dir or os.path.dirname(os.path.abspath(args.job))
    sampler = MemSampler(meminfo).start()
    with ctx as stream:
        ev = Events(stream)
        try:
            run(args.job, log_dir, ev, install, sampler)
            return EXIT_OK
        except RemoteError as e:
            log("failed: %s%s" % (e.code, " (%s)" % type(e.exc).__name__ if e.exc else ""))
            ev.error(e.code, retryable=not e.input_error, exc=e.exc)
            return EXIT_INPUT if e.input_error else EXIT_RETRY
        except KeyboardInterrupt as e:
            ev.error("internal", retryable=True, exc=e)
            return EXIT_RETRY
        except Exception as e:  # anything else: an internal error, the class name only
            import traceback
            traceback.print_exc(file=sys.stderr)
            ev.error("internal", retryable=True, exc=e)
            return EXIT_RETRY
        finally:
            sampler.stop()


if __name__ == "__main__":
    if "__file__" in globals():  # not under `python -I -c <source>` (the memory probe)
        sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
    rc = main()
    if rc == EXIT_OK:
        # R3: the last event is out; skip the interpreter's teardown (CUDA in --job mode).
        sys.stderr.flush()
        os._exit(rc)
    sys.exit(rc)
