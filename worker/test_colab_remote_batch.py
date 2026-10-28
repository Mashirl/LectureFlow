# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""worker/colab_remote.py, colab-batch: the uv install (R2), the resident worker (--serve: the
environment and the model prepared while the audio uploads, the model kept for later lectures,
several lectures at once, a cancelled lecture stopped and removed) and the attaching process
(--attach: copies the worker's setup events and the lecture's events, exits after the last
one). The fake faster_whisper of test_colab_remote.py; nothing is installed or started for real."""
import hashlib
import json
import os
import sys
import threading
import time
from types import SimpleNamespace

import pytest

import asr
import colab_remote
from test_colab_remote import SECRET_WORDS, check_stdout, fake_segments, remote  # noqa: F401 (fixture)

PINS = ["faster-whisper==1.2.1", "av==15.1.0", "ctranslate2==4.8.3"]


# ---------------------------------------------------------------------------
# R2: uv
# ---------------------------------------------------------------------------

@pytest.fixture
def pkgs(monkeypatch, tmp_path):
    """installed versions (name -> version) and the import check, both under the test's
    control; uv runs and pip runs are recorded."""
    st = SimpleNamespace(installed={}, uv_calls=[], pip_calls=[], uv_rc=0, uv_installs=True,
                         imports=True, met=True, log_dir=str(tmp_path))
    monkeypatch.setattr(colab_remote, "installed_version", lambda name: st.installed.get(name))
    monkeypatch.setattr(colab_remote, "imports_ok", lambda modules=None: st.imports)
    monkeypatch.setattr(colab_remote, "requirements_met", lambda names: st.met)

    def uv(reqs, log_path, target):
        st.uv_calls.append((list(reqs), log_path, target))
        if st.uv_installs and st.uv_rc == 0:
            for r in reqs:
                n, v = r.split("==")
                st.installed[n] = v
        return st.uv_rc

    def pip(reqs, log_path, target=None, constraints=None):
        st.pip_calls.append((list(reqs), target))
        for r in reqs:
            n, v = r.split("==")
            st.installed[n] = v
        return 0

    st.uv, st.pip = uv, pip
    return st


def test_uv_installs_only_the_missing_pins_and_extras_without_deps(pkgs):
    pkgs.installed = {"av": "15.1.0"}  # the runtime has one pin already
    colab_remote.ensure_requirements(PINS, pkgs.log_dir, pkgs.pip, None, pkgs.uv)
    assert pkgs.uv_calls == [(["faster-whisper==1.2.1", "ctranslate2==4.8.3", "onnxruntime==1.31.0"],
                              os.path.join(pkgs.log_dir, "install.log"), None)]
    assert pkgs.pip_calls == []


def test_uv_into_the_ram_disk(pkgs):
    pkgs.installed = {"onnxruntime": "1.30.0"}  # present: not one of the extras to install
    colab_remote.ensure_requirements(PINS, pkgs.log_dir, pkgs.pip, "/mnt/lectureflow/env", pkgs.uv)
    assert pkgs.uv_calls == [(PINS, os.path.join(pkgs.log_dir, "install.log"), "/mnt/lectureflow/env")]
    assert pkgs.pip_calls == []


@pytest.mark.parametrize("failure", ["rc", "incomplete", "requirements", "imports"])
def test_uv_failures_fall_back_to_the_full_pip_install(pkgs, failure):
    if failure == "rc":
        pkgs.uv_rc = 2
    elif failure == "incomplete":
        pkgs.uv_installs = False
    elif failure == "requirements":
        pkgs.met = False
    else:
        pkgs.imports = False
    colab_remote.ensure_requirements(PINS, pkgs.log_dir, pkgs.pip, None, pkgs.uv)
    assert len(pkgs.uv_calls) == 1
    assert pkgs.pip_calls == [(PINS, None)]


def test_nothing_missing_runs_nothing(pkgs):
    pkgs.installed = {"faster-whisper": "1.2.1", "av": "15.1.0", "ctranslate2": "4.8.3"}
    colab_remote.ensure_requirements(PINS, pkgs.log_dir, pkgs.pip, None, pkgs.uv)
    assert pkgs.uv_calls == [] and pkgs.pip_calls == []


def test_without_uv_pip_as_before(pkgs, monkeypatch):
    monkeypatch.setattr(colab_remote, "find_uv", lambda: None)
    colab_remote.ensure_requirements(PINS, pkgs.log_dir, pkgs.pip, None)
    assert pkgs.pip_calls == [(PINS, None)]


def test_uv_found_on_path_is_used(pkgs, monkeypatch):
    monkeypatch.setattr(colab_remote, "find_uv", lambda: "/usr/local/bin/uv")
    calls = []

    def fake_uv_install(uv, reqs, log_path, target=None):
        calls.append((uv, list(reqs), target))
        for r in reqs:
            n, v = r.split("==")
            pkgs.installed[n] = v
        return 0

    monkeypatch.setattr(colab_remote, "uv_install", fake_uv_install)
    colab_remote.ensure_requirements(PINS, pkgs.log_dir, pkgs.pip, None)
    assert calls == [("/usr/local/bin/uv", PINS + ["onnxruntime==1.31.0"], None)]
    assert pkgs.pip_calls == []


def test_uv_command_line():
    assert colab_remote.uv_command("/usr/local/bin/uv", ["av==15.1.0"]) == [
        "/usr/local/bin/uv", "pip", "install", "--no-cache", "--no-deps", "--python", sys.executable,
        "av==15.1.0"]
    assert colab_remote.uv_command("uv", ["av==15.1.0"], "/mnt/lectureflow/env")[-3:] == [
        "--target", "/mnt/lectureflow/env", "av==15.1.0"]


def test_requirements_met_reads_the_metadata(monkeypatch):
    reqs = {"faster-whisper": ["ctranslate2<5,>=4.0", "tokenizers<1,>=0.13", "onnxruntime<2,>=1.14",
                               "av>=11", "tqdm", "pytest; extra == \"dev\""]}
    from importlib import metadata
    monkeypatch.setattr(metadata, "requires", lambda name: reqs.get(name, []))
    have = {"ctranslate2": "4.8.3", "tokenizers": "0.23.3", "onnxruntime": "1.31.0", "av": "15.1.0",
            "tqdm": "4.67.1"}
    monkeypatch.setattr(colab_remote, "installed_version", lambda name: have.get(name))
    assert colab_remote.requirements_met(["faster-whisper"])
    have["tokenizers"] = "1.0.0"
    assert not colab_remote.requirements_met(["faster-whisper"])
    del have["tokenizers"]
    assert not colab_remote.requirements_met(["faster-whisper"])


# ---------------------------------------------------------------------------
# The resident worker
# ---------------------------------------------------------------------------

def read_events(path):
    with open(path, encoding="utf-8") as f:
        return check_stdout(f.read())


@pytest.fixture
def base(remote, tmp_path, monkeypatch):
    """BASE (tmp_path) with jobs/ (the remote fixture's j1 among them), a ResidentWorker with
    fake loading and no download, uv off. The install (nothing to install here) ends only once
    the parallel download has: see ensure_requirements below."""
    st = SimpleNamespace(remote=remote, base=str(tmp_path), loads=[], workers=[], downloads=0,
                         download_calls=0, download_threads=[])
    remote.cached = True  # the download that ran during the install filled faster-whisper's cache
    monkeypatch.setattr(colab_remote, "installed_version", lambda name: {
        "faster-whisper": "1.2.1", "av": "15.1.0", "ctranslate2": "4.8.3"}.get(name))
    fw = sys.modules["faster_whisper"]

    def load(path, compute_type, workers):
        st.loads.append((os.path.basename(path), compute_type, workers))
        return fw.WhisperModel(path, "cuda", compute_type, 4)

    def download(model):
        st.download_calls += 1

        def fetch():
            st.download_threads.append(threading.current_thread())
            st.downloads += 1
        return fetch

    real_ensure = colab_remote.ensure_requirements

    def ensure_requirements(*args, **kwargs):
        """The fake install returns only after the fake parallel download has finished, as a real
        install (pip or uv, tens of seconds) normally does. Both fakes are instant: on a loaded
        machine the download thread was sometimes not yet scheduled when the install returned, and
        the worker then rightly said download_model (the model still downloading), a stage these
        tests do not expect (flake-hunt #10; test_setup_downloads_through_asr_... covers it)."""
        real_ensure(*args, **kwargs)
        wait_for(lambda: len(st.download_threads) >= st.download_calls, what="the parallel download")
        for th in list(st.download_threads):
            th.join(10)
            assert not th.is_alive(), "the parallel download did not finish"

    monkeypatch.setattr(colab_remote, "ensure_requirements", ensure_requirements)

    def worker(workers=1, **kw):
        w = colab_remote.ResidentWorker(st.base, "large-v3", "float16", PINS, False, workers,
                                        install=remote.install, load=load, download=download,
                                        meminfo=str(tmp_path / "absent-meminfo"), poll=0.01, **kw)
        st.workers.append(w)
        return w

    def add_job(name, audio=b"fLaC fake audio bytes", job_json=True, **changes):
        d = os.path.join(st.base, "jobs", name)
        os.makedirs(d, exist_ok=True)
        with open(os.path.join(d, "audio.flac"), "wb") as f:
            f.write(audio)
        j = dict(remote.job, audio_sha256=hashlib.sha256(audio).hexdigest())
        j.update(changes)
        if job_json:
            with open(os.path.join(d, "job.json"), "w", encoding="utf-8") as f:
                json.dump(j, f)
        return d

    st.worker, st.add_job = worker, add_job
    yield st
    for w in st.workers:
        w.stop.set()


def wait_for(cond, timeout=10.0, what="condition"):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if cond():
            return
        time.sleep(0.01)
    raise AssertionError("timed out waiting for " + what)


def final_event(job_dir):
    p = os.path.join(job_dir, colab_remote.JOB_EVENTS)
    if not os.path.exists(p):
        return None
    evs = read_events(p)
    return evs[-1] if evs and evs[-1]["type"] in ("done", "error") else None


def serve_in_thread(w):
    t = threading.Thread(target=w.serve, daemon=True)
    t.start()
    return t


def test_setup_events_then_ready(base):
    w = base.worker()
    assert w.setup() == colab_remote.EXIT_OK
    evs = read_events(os.path.join(base.base, colab_remote.WORKER_EVENTS))
    assert [e["stage"] for e in evs] == ["install", "load_model"]
    assert os.path.exists(os.path.join(base.base, colab_remote.WORKER_READY))
    assert base.loads == [("large-v3", "float16", 1)] and base.downloads == 1


def test_setup_downloads_through_asr_when_the_parallel_download_did_not(base):
    base.remote.cached = False
    w = base.worker()
    assert w.setup() == colab_remote.EXIT_OK
    evs = read_events(os.path.join(base.base, colab_remote.WORKER_EVENTS))
    assert [e["stage"] for e in evs] == ["install", "download_model", "load_model"]


def test_setup_failure_is_an_error_event_and_no_ready_mark(base):
    base.remote.cuda = 0
    w = base.worker()
    assert w.setup() == colab_remote.EXIT_RETRY
    evs = read_events(os.path.join(base.base, colab_remote.WORKER_EVENTS))
    assert evs[-1]["type"] == "error" and evs[-1]["code"] == "no_gpu"
    assert not os.path.exists(os.path.join(base.base, colab_remote.WORKER_READY))


def test_lectures_run_on_the_loaded_model_one_after_the_other(base):
    w = base.worker()
    assert w.setup() == 0
    a = base.add_job("a")
    serve_in_thread(w)
    wait_for(lambda: final_event(a), what="lecture a")
    b = base.add_job("b", audio=b"another fLaC")
    wait_for(lambda: final_event(b), what="lecture b")
    for d, first in ((a, True), (b, False)):
        evs = read_events(os.path.join(d, colab_remote.JOB_EVENTS))
        assert [e["stage"] for e in evs if e["type"] == "stage"] == ["check", "transcribe", "write"]
        done = evs[-1]
        assert done["type"] == "done"
        data = open(os.path.join(d, "result.json"), "rb").read()
        assert done["sha256"] == hashlib.sha256(data).hexdigest() and done["bytes"] == len(data)
        assert (done["load_s"] >= 0) and (first or done["load_s"] == 0)
        assert not os.path.exists(os.path.join(d, "audio.flac"))  # decoded, then removed
        result = json.loads(data)
        assert set(result) == {"prompt_terms_used", "sentences", "timing"}
        log = open(os.path.join(d, colab_remote.JOB_LOG), encoding="utf-8").read()
        for word in SECRET_WORDS:
            assert word not in log
    assert base.loads == [("large-v3", "float16", 1)]  # loaded once


def test_two_lectures_at_once_with_two_workers(base, monkeypatch):
    barrier = threading.Barrier(2, timeout=5)
    real = asr.transcribe_job

    def together(*a, **k):
        barrier.wait()  # both lectures are being transcribed at the same time
        return real(*a, **k)

    monkeypatch.setattr(asr, "transcribe_job", together)
    w = base.worker(workers=2)
    assert w.setup() == 0
    a, b = base.add_job("a"), base.add_job("b", audio=b"second")
    serve_in_thread(w)
    wait_for(lambda: final_event(a) and final_event(b), what="both lectures")
    assert final_event(a)["type"] == final_event(b)["type"] == "done"
    assert base.loads == [("large-v3", "float16", 2)]
    # the same result but for the measured seconds (a loaded machine makes one of the two fake
    # transcriptions take 0.01 s; colab-obs-core: the byte comparison was flaky for that alone)
    ra, rb = (json.load(open(os.path.join(d, "result.json"), encoding="utf-8")) for d in (a, b))
    for r in (ra, rb):
        assert r["timing"]["transcribe_s"] < 1 and r["timing"]["load_s"] < 1
        r["timing"]["transcribe_s"] = r["timing"]["load_s"] = 0
    assert ra == rb


def test_uploaded_ahead_waits_for_its_job_json(base):
    w = base.worker()
    assert w.setup() == 0
    d = base.add_job("ahead", job_json=False)
    serve_in_thread(w)
    time.sleep(0.2)
    assert not os.path.exists(os.path.join(d, colab_remote.JOB_EVENTS))
    assert os.path.exists(os.path.join(d, "audio.flac"))
    with open(os.path.join(d, "job.json"), "w", encoding="utf-8") as f:
        json.dump(dict(base.remote.job, audio_sha256=hashlib.sha256(b"fLaC fake audio bytes").hexdigest()), f)
    wait_for(lambda: final_event(d), what="the lecture")
    assert final_event(d)["type"] == "done"


def test_cancelled_lecture_stops_and_its_directory_goes(base, monkeypatch):
    started = threading.Event()
    real = asr.transcribe_job

    def slow(model, audio, job, emit, **kw):
        started.set()
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            emit("PROGRESS 0.1000")  # the adapter notices the cancel here
            time.sleep(0.01)
        return real(model, audio, job, emit, **kw)

    monkeypatch.setattr(asr, "transcribe_job", slow)
    w = base.worker()
    assert w.setup() == 0
    d = base.add_job("c")
    serve_in_thread(w)
    assert started.wait(5)
    os.remove(os.path.join(d, "job.json"))  # LectureFlow cancels: it deletes the job's files
    wait_for(lambda: not os.path.exists(d), what="the directory removed")
    # the worker goes on with the next lecture
    monkeypatch.setattr(asr, "transcribe_job", real)
    e = base.add_job("e")
    wait_for(lambda: final_event(e), what="the next lecture")
    assert final_event(e)["type"] == "done"


def test_job_errors_are_the_lectures_own(base):
    w = base.worker()
    assert w.setup() == 0
    bad = base.add_job("bad", audio_sha256="0" * 64)
    serve_in_thread(w)
    wait_for(lambda: final_event(bad), what="the failed lecture")
    assert final_event(bad)["code"] == "audio_hash_mismatch"
    good = base.add_job("good")
    wait_for(lambda: final_event(good), what="the next lecture")
    assert final_event(good)["type"] == "done"


def test_another_model_is_loaded_for_the_lecture_that_asks(base):
    w = base.worker()
    assert w.setup() == 0
    d = base.add_job("m", compute_type="int8_float16")
    serve_in_thread(w)
    wait_for(lambda: final_event(d), what="the lecture")
    evs = read_events(os.path.join(d, colab_remote.JOB_EVENTS))
    assert [e["stage"] for e in evs if e["type"] == "stage"] == ["check", "load_model", "transcribe", "write"]
    assert base.loads == [("large-v3", "float16", 1), ("large-v3", "int8_float16", 1)]


def test_job_directory_name_may_come_again(base):
    w = base.worker()
    assert w.setup() == 0
    d = base.add_job("again")
    serve_in_thread(w)
    wait_for(lambda: final_event(d), what="first run")
    for name in os.listdir(d):  # LectureFlow's cleanup
        os.remove(os.path.join(d, name))
    os.rmdir(d)
    time.sleep(0.1)
    d = base.add_job("again")
    wait_for(lambda: final_event(d), what="second run")
    assert final_event(d)["type"] == "done"


def test_job_split_field_is_checked(remote, capfd):
    for bad in (0, 5, True, "2"):
        rc = colab_remote.main(["--job", remote.write_job(split=bad), "--log-dir", remote.log_dir],
                               install=remote.install)
        out, _ = capfd.readouterr()
        assert rc == colab_remote.EXIT_INPUT and check_stdout(out)[-1]["code"] == "bad_job"


# ---------------------------------------------------------------------------
# The attaching process
# ---------------------------------------------------------------------------

class Out:
    def __init__(self):
        self.text = ""

    def write(self, s):
        self.text += s

    def flush(self):
        pass


def attach_args(base, job_dir, **kw):
    a = dict(base=base, job_dir=job_dir, model="large-v3", compute_type="float16",
             requirements=",".join(PINS), workers=1, memory=False)
    a.update(kw)
    return SimpleNamespace(**a)


def test_attach_starts_the_worker_and_copies_setup_then_lecture_events(base):
    d = base.add_job("a", job_json=False)
    spawned = []

    def spawn(b, options):
        spawned.append(options)
        w = base.worker()

        def run():
            if w.setup() == 0:
                w.serve()
        threading.Thread(target=run, daemon=True).start()
        return 4242

    out = Out()
    done = {}

    def attach():
        done["rc"] = colab_remote.attach_main(attach_args(base.base, d), out, spawn=spawn,
                                              alive=lambda pid: True, poll=0.01)
    t = threading.Thread(target=attach)
    t.start()
    wait_for(lambda: os.path.exists(os.path.join(base.base, colab_remote.WORKER_READY)), what="ready")
    with open(os.path.join(d, "job.json"), "w", encoding="utf-8") as f:  # after the "upload"
        json.dump(dict(base.remote.job, audio_sha256=hashlib.sha256(b"fLaC fake audio bytes").hexdigest()), f)
    t.join(10)
    assert done["rc"] == 0
    evs = check_stdout(out.text)
    stages = [e["stage"] for e in evs if e["type"] == "stage"]
    assert stages == ["install", "load_model", "check", "transcribe", "write"]
    assert evs[-1]["type"] == "done"
    assert spawned == [["--model", "large-v3", "--compute-type", "float16", "--requirements", ",".join(PINS),
                        "--workers", "1"]]
    assert open(os.path.join(base.base, colab_remote.WORKER_PID)).read() == "4242"
    # a second lecture: the worker runs and is ready; no setup events, no second worker
    d2 = base.add_job("b")
    out2 = Out()
    rc = colab_remote.attach_main(attach_args(base.base, d2), out2, spawn=spawn, alive=lambda pid: pid == 4242,
                                  poll=0.01)
    assert rc == 0 and len(spawned) == 1
    assert [e["stage"] for e in check_stdout(out2.text) if e["type"] == "stage"] == ["check", "transcribe", "write"]


def write_lines(path, events):
    with open(path, "a", encoding="utf-8") as f:
        for e in events:
            f.write(json.dumps(e) + "\n")


def test_attach_exit_codes_and_contract(tmp_path):
    b = str(tmp_path)
    d = os.path.join(b, "jobs", "x")
    os.makedirs(d)
    open(os.path.join(b, colab_remote.WORKER_READY), "w").close()
    with open(os.path.join(b, colab_remote.WORKER_PID), "w") as f:
        f.write("7")
    ts = time.time()
    write_lines(os.path.join(d, colab_remote.JOB_EVENTS), [
        {"type": "stage", "ts": ts, "stage": "check"},
        {"type": "progress", "ts": ts, "fraction": 0.5, "text": "Zebrafish"},  # not the contract: dropped
        {"type": "error", "ts": ts, "code": "audio_unreadable", "retryable": False}])
    out = Out()
    rc = colab_remote.attach_main(attach_args(b, d), out, spawn=None, alive=lambda pid: pid == 7, poll=0.01)
    assert rc == colab_remote.EXIT_INPUT
    evs = check_stdout(out.text)
    assert [e["type"] for e in evs] == ["stage", "error"] and "Zebrafish" not in out.text


def test_attach_returns_when_the_job_directory_goes(tmp_path):
    b = str(tmp_path)
    d = os.path.join(b, "jobs", "x")
    os.makedirs(d)
    open(os.path.join(b, colab_remote.WORKER_READY), "w").close()
    with open(os.path.join(b, colab_remote.WORKER_PID), "w") as f:
        f.write("7")
    threading.Timer(0.1, lambda: os.rmdir(d)).start()
    out = Out()
    rc = colab_remote.attach_main(attach_args(b, d), out, spawn=None, alive=lambda pid: True, poll=0.01)
    assert rc == colab_remote.EXIT_RETRY and out.text == ""


def test_attach_reports_a_dead_worker(tmp_path):
    b = str(tmp_path)
    d = os.path.join(b, "jobs", "x")
    os.makedirs(d)
    with open(os.path.join(b, colab_remote.WORKER_PID), "w") as f:
        f.write("7")
    state = {"alive": True}
    clock = [0.0]

    def tick():
        clock[0] += 0.5
        return clock[0]

    out = Out()
    threading.Timer(0.05, lambda: state.update(alive=False)).start()
    rc = colab_remote.attach_main(attach_args(b, d), out, spawn=lambda b, o: 7, alive=lambda pid: state["alive"],
                                  poll=0.01, clock=tick)
    assert rc == colab_remote.EXIT_RETRY
    assert check_stdout(out.text)[-1]["code"] == "internal"


def test_ensure_worker_restarts_a_dead_one_and_forgets_its_state(tmp_path):
    b = str(tmp_path)
    for name in (colab_remote.WORKER_READY, colab_remote.WORKER_EVENTS):
        open(os.path.join(b, name), "w").close()
    with open(os.path.join(b, colab_remote.WORKER_PID), "w") as f:
        f.write("11")
    spawned = []
    pid = colab_remote.ensure_worker(b, ["--model", "x"], lambda base, o: spawned.append(o) or 12,
                                     alive=lambda p: False)
    assert pid == 12 and spawned == [["--model", "x"]]
    assert not os.path.exists(os.path.join(b, colab_remote.WORKER_READY))
    assert not os.path.exists(os.path.join(b, colab_remote.WORKER_EVENTS))
    assert colab_remote.ensure_worker(b, [], lambda base, o: 99, alive=lambda p: p == 12) == 12


def test_attach_refuses_a_job_directory_outside_the_base(tmp_path):
    out = Out()
    rc = colab_remote.attach_main(attach_args(str(tmp_path / "base"), str(tmp_path)), out, spawn=None,
                                  alive=lambda p: True)
    assert rc == colab_remote.EXIT_INPUT and check_stdout(out.text)[-1]["code"] == "bad_job"


def test_attach_command_line_is_checked(capfd):
    assert colab_remote.main(["--attach", "--base", "/b", "--job-dir", "/b/jobs/x", "--model", "large-v3",
                              "--workers", "0"]) == colab_remote.EXIT_INPUT
    assert colab_remote.main(["--attach", "--base", "/b"]) == colab_remote.EXIT_INPUT
    out, _ = capfd.readouterr()
    assert out == ""


# ---------------------------------------------------------------------------
# "极速模式": a lecture cut at silences, the parts at once
# ---------------------------------------------------------------------------

SR = 16000


def test_split_points_at_silences_near_the_targets():
    # speech with silences every 60 s (1 s long) and a long one (3 s) near the middle
    n = 600 * SR
    chunks, t = [], 0
    while t < 600:
        end = t + 59
        chunks.append({"start": int(t * SR), "end": int(min(end, 600) * SR)})
        t = end + (3 if 290 <= end <= 300 else 1)
    cuts = colab_remote.split_points(chunks, n, 2, SR)
    assert len(cuts) == 1 and abs(cuts[0] / SR - 300) < 20
    # every cut is in a silence
    for c in cuts:
        assert not any(ch["start"] <= c < ch["end"] for ch in chunks)
    cuts4 = colab_remote.split_points(chunks, n, 4, SR)
    assert len(cuts4) == 3 and cuts4 == sorted(cuts4)
    # short lectures are not cut into parts under SPLIT_MIN_PART_S
    assert colab_remote.split_points(chunks[:3], 200 * SR, 4, SR) == []
    assert len(colab_remote.split_points(chunks, n, 8, SR)) <= 4  # at most 5 parts of 2 minutes in 10 min
    # no silence: no cut
    assert colab_remote.split_points([{"start": 0, "end": n}], n, 4, SR) == []


def test_merge_counters():
    dst = {}
    colab_remote.merge_counters(dst, {"redecode": {"windows": 10, "accepted": 2, "redecode_s": 1.5, "skipped": False}})
    colab_remote.merge_counters(dst, {"redecode": {"windows": 7, "accepted": 1, "redecode_s": 0.25, "skipped": True},
                                      "textrepair": {"segments": 3}})
    assert dst == {"redecode": {"windows": 17, "accepted": 3, "redecode_s": 1.75, "skipped": True},
                   "textrepair": {"segments": 3}}


@pytest.fixture
def fake_vad(monkeypatch):
    import types
    vad = types.ModuleType("faster_whisper.vad")

    class VadOptions:
        pass

    def get_speech_timestamps(audio, opts):
        n = len(audio)
        # speech everywhere but 2 s of silence at 1/2, 1/4, 3/4
        cuts = [n // 4, n // 2, 3 * n // 4]
        out, prev = [], 0
        for c in cuts:
            out.append({"start": prev, "end": c - SR})
            prev = c + SR
        out.append({"start": prev, "end": n})
        return out

    vad.VadOptions, vad.get_speech_timestamps = VadOptions, get_speech_timestamps
    monkeypatch.setitem(sys.modules, "faster_whisper.vad", vad)
    return vad


def test_transcribe_split_offsets_the_parts(remote, fake_vad, monkeypatch):
    import numpy as np
    calls = []

    def fake_job(model, audio, job, emit, postprocess=None, **kw):
        calls.append((len(audio), job.initial_prompt))
        emit("PROGRESS 1.00")
        postprocess["redecode"] = {"windows": 2, "accepted": 1}
        return {"prompt_terms_used": 3 if job.initial_prompt else 0,
                "sentences": [{"start_ms": 1000, "end_ms": 2000, "text": "Zebrafish.", "low_conf_words": [],
                               "words": [{"w": "Zebrafish.", "s": 1000, "e": 2000}]}]}

    monkeypatch.setattr(asr, "transcribe_job", fake_job)
    audio = np.zeros(600 * SR, dtype=np.float32)
    rjob = asr.Job("a.flac", "large-v3", "float16", "en", "Vigenère", "cuda", 4)
    lines, post = [], {}
    res = colab_remote.transcribe_split(None, audio, rjob, lines.append, post, 2, lambda m: None)
    assert [c[1] for c in calls] == ["Vigenère", ""]  # the prompt for the first part only
    assert sum(c[0] for c in calls) == len(audio)
    cut_ms = post["split"]["cuts_ms"][0]
    assert post["split"]["parts"] == 2 and abs(cut_ms - 300000) < 1000
    assert [s["start_ms"] for s in res["sentences"]] == [1000, 1000 + cut_ms]
    assert res["sentences"][1]["words"][0] == {"w": "Zebrafish.", "s": 1000 + cut_ms, "e": 2000 + cut_ms}
    assert res["prompt_terms_used"] == 3
    assert post["redecode"] == {"windows": 4, "accepted": 2}
    progress = [float(l.split()[1]) for l in lines if l.startswith("PROGRESS ")]
    assert progress == sorted(progress) and progress[-1] == 1.0


def test_split_job_in_the_resident_worker(base, fake_vad, monkeypatch):
    import numpy as np
    seen = []
    real = colab_remote.transcribe_split

    def spy(model, audio, rjob, emit, post, parts, logf):
        seen.append(parts)
        return real(model, audio, rjob, emit, post, parts, logf)

    monkeypatch.setattr(colab_remote, "transcribe_split", spy)
    sys.modules["faster_whisper"].decode_audio = lambda path, sampling_rate=16000: np.zeros(600 * SR, dtype=np.float32)
    w = base.worker(workers=4)
    assert w.setup() == 0
    d = base.add_job("fast", split=4)
    serve_in_thread(w)
    wait_for(lambda: final_event(d), what="the lecture")
    assert final_event(d)["type"] == "done" and seen == [4]
    timing = json.load(open(os.path.join(d, "result.json"), encoding="utf-8"))["timing"]
    assert timing["postprocess"]["split"]["parts"] == 4


def test_attach_that_started_the_worker_forwards_its_setup_even_when_already_ready(base):
    """3.2.0: a quick setup on a busy machine was ready before the attach looked, and its
    stages were lost; the attach that started the worker forwards them all the same."""
    d = base.add_job("a")

    def spawn(b, options):
        w = base.worker()
        assert w.setup() == 0  # ready before attach_main looks
        threading.Thread(target=w.serve, daemon=True).start()
        return 4243

    out = Out()
    rc = colab_remote.attach_main(attach_args(base.base, d), out, spawn=spawn, alive=lambda pid: True, poll=0.01)
    assert rc == 0
    stages = [e["stage"] for e in check_stdout(out.text) if e["type"] == "stage"]
    assert stages == ["install", "load_model", "check", "transcribe", "write"]


def test_the_same_stage_twice_in_a_row_is_said_once():
    out = Out()
    ev = colab_remote.Events(out)
    for s in ("install", "download_model", "download_model", "load_model", "download_model"):
        ev.stage(s)
    assert [e["stage"] for e in check_stdout(out.text)] == ["install", "download_model", "load_model", "download_model"]
