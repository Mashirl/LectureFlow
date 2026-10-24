# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
# Additional permission under AGPL-3.0 section 7: see LICENSE-EXCEPTION.md
"""LectureFlow remote transcription worker for Google Colab (SPEC-v3 V14.3).

LectureFlow uploads this file, asr.py and asr_common.py to the Colab runtime and runs
    python colab_remote.py --job job.json [--log-dir DIR]
in the kernel (internal/colab/job.go cellCode). The transcription is the local
faster-whisper worker's own code (asr.transcribe_job: the same parameters,
hallucination and repeat filters, sentence splitting and word times), so result.json
has the structure the local worker writes (prompt_terms_used, sentences with words,
timing).

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
           "requirements": ["faster-whisper==1.2.1", ...]}
Exit code 0 = done, 2 = input error, 1 = other error (as asr.py).
"""
from __future__ import annotations

import argparse
import contextlib
import hashlib
import importlib
import json
import os
import re
import subprocess
import sys
import time
from typing import Callable, List, Optional

EXIT_OK, EXIT_RETRY, EXIT_INPUT = 0, 1, 2

STAGES = ("check", "install", "download_model", "load_model", "transcribe", "write")
ERROR_CODES = ("bad_job", "audio_missing", "audio_hash_mismatch", "audio_unreadable",
               "install_failed", "no_gpu", "model_load_failed", "out_of_memory",
               "transcribe_failed", "write_failed", "internal")

# Same keys as internal/colab/events.go eventKeys: (required, optional).
EVENT_KEYS = {
    "progress": (("type", "ts", "fraction"), ()),
    "stage": (("type", "ts", "stage"), ()),
    "done": (("type", "ts", "result", "sha256", "bytes", "audio_s", "load_s",
              "transcribe_s", "sentences"), ()),
    "error": (("type", "ts", "code", "retryable"), ("exc",)),
}

FILE_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
REQUIREMENT_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*==[A-Za-z0-9][A-Za-z0-9.+_-]*$")
EXC_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_.]{0,79}$")

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
        assert name in STAGES
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
             transcribe_s: float, sentences: int) -> None:
        assert FILE_RE.match(result) and SHA256_RE.match(sha256)
        self._emit("done", result=result, sha256=sha256, bytes=int(size),
                   audio_s=round(max(0.0, audio_s), 3), load_s=round(max(0.0, load_s), 3),
                   transcribe_s=round(max(0.0, transcribe_s), 3), sentences=int(sentences))
        self.final = True

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


def pip_install(reqs: List[str], log_path: str) -> int:
    """pip install the pins; all output goes to log_path (never stdout)."""
    with open(log_path, "ab") as logf:
        return subprocess.call(
            [sys.executable, "-m", "pip", "install", "--disable-pip-version-check",
             "--no-input", "--progress-bar", "off"] + list(reqs),
            stdout=logf, stderr=logf, stdin=subprocess.DEVNULL)


def ensure_requirements(reqs: List[str], log_dir: str,
                        install: Callable[[List[str], str], int] = None) -> None:
    install = install or pip_install
    missing = missing_requirements(reqs)
    if not missing:
        return
    log("installing %d pinned packages" % len(missing))
    rc = install(missing, os.path.join(log_dir, "install.log"))
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
# Run
# ---------------------------------------------------------------------------

def run(job_path: str, log_dir: str, ev: Events,
        install: Callable[[List[str], str], int] = None) -> None:
    ev.stage("check")
    job = load_job(job_path)
    job_dir = os.path.dirname(os.path.abspath(job_path))
    audio_path = os.path.join(job_dir, job["audio"])
    if not os.path.isfile(audio_path):
        raise RemoteError("audio_missing", input_error=True)
    if file_sha256(audio_path) != job["audio_sha256"]:
        raise RemoteError("audio_hash_mismatch")

    ev.stage("install")
    ensure_requirements(job.get("requirements", []), log_dir, install)
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
                   job.get("initial_prompt") or "", DEVICE, CPU_THREADS)
    try:
        result = asr.transcribe_job(model, audio, rjob, adapter)
    except Exception as e:
        raise RemoteError("out_of_memory" if _is_oom(e) else "transcribe_failed", exc=e)
    t2 = time.monotonic()
    ev.progress(1.0, force=True)

    ev.stage("write")
    audio_s = len(audio) / float(asr.SAMPLE_RATE)
    result["timing"] = make_timing(t1 - t0, t2 - t1, audio_s, False)
    out = os.path.join(job_dir, job["result"])
    tmp = out + ".tmp"
    try:
        write_result(tmp, result)
        os.replace(tmp, out)
        digest, size = file_sha256(out), os.path.getsize(out)
    except OSError as e:
        raise RemoteError("write_failed", exc=e)
    ev.done(job["result"], digest, size, audio_s, t1 - t0, t2 - t1, len(result["sentences"]))


def main(argv: Optional[List[str]] = None, isolate: bool = True,
         install: Callable[[List[str], str], int] = None) -> int:
    p = argparse.ArgumentParser(prog="colab_remote.py")
    p.add_argument("--job", required=True)
    p.add_argument("--log-dir", dest="log_dir", default=None)
    try:
        args = p.parse_args(argv)
    except SystemExit:
        return EXIT_INPUT
    log_dir = args.log_dir or os.path.dirname(os.path.abspath(args.job))
    ctx = isolated_stdout() if isolate else contextlib.nullcontext(sys.stdout)
    with ctx as stream:
        ev = Events(stream)
        try:
            run(args.job, log_dir, ev, install)
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


if __name__ == "__main__":
    sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
    sys.exit(main())
