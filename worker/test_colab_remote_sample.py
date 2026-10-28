# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""worker/colab_remote.py, colab-obs-core: the runtime's own readings (machine and sample
events) of the attaching process that holds BASE/sample.lock. A fake /proc, fake nvidia-smi
runs and a fake nvidia-smi loop; nothing reads this machine's GPU or memory."""
import json
import os
import threading
import time
from types import SimpleNamespace

import pytest

import colab_remote
from test_colab_remote_batch import Out, attach_args, write_lines


class FakeLoop:
    """The resident nvidia-smi -lms loop: yields lines, then blocks until killed."""

    def __init__(self, lines, block=True):
        self.lines, self.block = list(lines), block
        self.killed = threading.Event()
        self.stdout = self

    def readline(self):
        if self.lines:
            return (self.lines.pop(0) + "\n").encode()
        if self.block:
            self.killed.wait(5)
        return b""

    def kill(self):
        self.killed.set()

    def wait(self, timeout=None):
        return 0


SMI_QUERY = "Tesla T4, 15360, 580.82.07, 7.5\n"
SMI_Q = "Driver Version : 580.82.07\nCUDA Version : 13.0\n    Product Architecture : Turing\n"


@pytest.fixture
def rt(tmp_path):
    """A fake runtime: BASE with jobs, /proc with meminfo, stat and the worker's process tree."""
    base = tmp_path / "base"
    (base / "jobs" / "a").mkdir(parents=True)
    (base / "jobs" / "a" / "audio.flac").write_bytes(b"x" * (3 * colab_remote.MIB))
    (base / "jobs" / "b").mkdir()
    (base / "jobs" / "b" / "job.json").write_bytes(b"y" * colab_remote.MIB)
    (base / colab_remote.WORKER_PID).write_text("100")
    proc = tmp_path / "proc"
    proc.mkdir()
    (proc / "meminfo").write_text("MemTotal:       13286400 kB\nMemFree:  1 kB\nMemAvailable:    6246400 kB\n")
    st = SimpleNamespace(cpu=[1000, 0, 500, 8000, 500, 0, 0, 0], runs=[], loops=[], compute="100, 3650\n999, 10\n",
                         smi_lines=["88, 4100, 448, 61, 65.24"])

    def write_stat():
        (proc / "stat").write_text("cpu  " + " ".join(str(v) for v in st.cpu) + "\ncpu0 1 2 3\n")
    write_stat()
    st.write_stat = write_stat
    for pid, ppid, rss in ((100, 1, 3000000), (101, 100, 72000), (102, 101, 1024), (200, 1, 999999)):
        (proc / str(pid)).mkdir()
        (proc / str(pid) / "stat").write_text("%d (python3) S %d 1 1 0" % (pid, ppid))
        (proc / str(pid) / "status").write_text("Name: python3\nRssAnon:\t%d kB\nRssFile: 5 kB\n" % rss)

    def run(argv):
        st.runs.append(argv)
        if argv[1].startswith("--query-gpu=name"):
            return SMI_QUERY
        if argv[1] == "-q":
            return SMI_Q
        if argv[1].startswith("--query-compute-apps"):
            return st.compute
        return None

    def popen(argv):
        st.loops.append(argv)
        loop = FakeLoop(st.smi_lines)
        st.loop = loop
        return loop

    def statvfs(path):
        assert path == str(base)
        return SimpleNamespace(f_blocks=5000, f_bfree=1000, f_frsize=colab_remote.MIB)

    st.base, st.proc, st.run, st.popen, st.statvfs = str(base), str(proc), run, popen, statvfs

    def sampler(memory=True, lock=lambda path: open(path, "a"), **kw):
        return colab_remote.Sampler(st.base, memory, lock=lock, run=st.run, popen=st.popen, proc=st.proc,
                                    statvfs=st.statvfs, cpu_count=lambda: 2, **kw)
    st.sampler = sampler
    return st


def test_machine_and_sample_readings(rt):
    s = rt.sampler()
    m = s.machine()
    assert m == {"gpu_name": "Tesla T4", "gpu_arch": "Turing", "compute_cap": "7.5", "driver": "580.82.07",
                 "cuda": "13.0", "gpu_count": 1, "vram_total_mb": 15360, "vcpus": 2, "mem_total_mb": 12975,
                 "storage": "memory"}
    colab_remote.check_reading("machine", m)
    s.smi_last = "88, 4100, 448, 61, 65.24"
    rt.cpu = [1100, 0, 550, 8200, 550, 0, 0, 0]  # +150 busy of +400
    rt.write_stat()
    got = s.sample()
    colab_remote.check_reading("sample", got)
    assert got["cpu_pct"] == 37.5
    assert got["mem_total_mb"] == 12975 and got["mem_available_mb"] == 6100
    # the RAM disk in use (4000 MiB) minus the job files (4 MiB: uploads, job.json)
    assert got["data_mb"] == 4 and got["ramdisk_mb"] == 3996 and got["data_disk_mb"] == -1
    # RssAnon of the worker (pid 100) and its descendants, not of other processes
    assert got["whisper_mb"] == round((3000000 + 72000 + 1024) / 1024)
    assert got["vram_whisper_mb"] == 3650  # compute-apps: only the worker's tree
    assert (got["gpu_util_pct"], got["vram_used_mb"], got["vram_reserved_mb"], got["gpu_temp_c"],
            got["gpu_power_w"]) == (88, 4100, 448, 61, 65.2)
    # disk mode: no RAM disk, the job files on the disk
    d = rt.sampler(memory=False).sample()
    assert (d["ramdisk_mb"], d["data_mb"], d["data_disk_mb"]) == (-1, 0, 4)
    assert rt.sampler(memory=False).machine()["storage"] == "disk"


def test_unreadable_values_are_unknown(rt, tmp_path):
    # statvfs=None would mean os.statvfs (present on macOS and Linux, absent
    # on Windows): an unreadable file system is one whose statvfs fails.
    def statvfs_fails(path):
        raise OSError("statvfs: not readable")

    s = colab_remote.Sampler(rt.base, True, lock=lambda p: open(p, "a"), run=lambda argv: None,
                             popen=rt.popen, proc=str(tmp_path / "noproc"), statvfs=statvfs_fails,
                             cpu_count=lambda: None)
    m = s.machine()
    colab_remote.check_reading("machine", m)
    assert m["gpu_name"] == "" and m["gpu_count"] == -1 and m["vcpus"] == -1 and m["mem_total_mb"] == -1
    got = s.sample()
    colab_remote.check_reading("sample", got)
    assert got["cpu_pct"] == -1 and got["mem_total_mb"] == -1 and got["ramdisk_mb"] == -1 and got["whisper_mb"] == -1
    assert got["gpu_util_pct"] == -1 and got["vram_whisper_mb"] == -1
    # nvidia-smi's [N/A] and odd names are unknowns too, never a violation
    s.smi_last = "[N/A], [N/A], [Not Supported], 61, [N/A]"
    got = s.sample()
    colab_remote.check_reading("sample", got)
    assert got["gpu_util_pct"] == -1 and got["vram_reserved_mb"] == -1 and got["gpu_temp_c"] == 61
    assert colab_remote.clean_str("gpu_name", "Tesla T4\nWelcome to the lecture") == ""
    assert colab_remote.clean_str("driver", "580") == ""


def test_events_and_attach_contract_check_the_readings(rt):
    out = Out()
    ev = colab_remote.Events(out)
    s = rt.sampler()
    ev.reading("machine", s.machine())
    s.smi_last = "88, 4100, 448, 61, 65.24"
    ev.reading("sample", s.sample())
    for line in out.text.splitlines():
        e = colab_remote.check_event_line(line)
        assert set(e) == set(colab_remote.EVENT_KEYS[e["type"]][0])
    bad = dict(s.machine(), gpu_name="Welcome, students\n")
    with pytest.raises(ValueError):
        ev.reading("machine", bad)
    with pytest.raises(ValueError):
        ev.reading("sample", dict(s.sample(), cpu_pct=101))
    with pytest.raises(ValueError):
        ev.reading("sample", dict(s.sample(), text="Zebrafish"))
    m = json.loads(out.text.splitlines()[0])
    m["gpu_name"] = "Zebrafish <b>"
    with pytest.raises(ValueError):
        colab_remote.check_event_line(json.dumps(m))
    assert "Zebrafish" not in out.text


def test_sampling_thread_machine_first_then_samples(rt):
    s = rt.sampler(interval=0.01, apps_interval=10).start()
    try:
        deadline = time.time() + 5
        got = []
        while time.time() < deadline and sum(1 for t, _ in got if t == "sample") < 3:
            got += s.take()
            time.sleep(0.01)
    finally:
        s.stop()
    assert got[0][0] == "machine" and [t for t, _ in got].count("machine") == 1
    assert sum(1 for t, _ in got if t == "sample") >= 3
    # one resident loop (GPU 0, the interval), ended with the sampler; compute-apps at most every 10 s
    assert len(rt.loops) == 1 and rt.loops[0][-4:] == ["-i", "0", "-lms", "10"] and rt.loop.killed.is_set()
    assert sum(1 for a in rt.runs if a[1].startswith("--query-compute-apps")) == 1
    assert any(f["gpu_util_pct"] == 88 for t, f in got if t == "sample")


def test_without_the_lock_no_readings_until_it_is_free(rt):
    free = threading.Event()
    s = rt.sampler(interval=0.01, lock=lambda path: open(path, "a") if free.is_set() else None).start()
    try:
        time.sleep(0.1)
        assert s.take() == [] and rt.loops == []
        free.set()  # the sampling attach ended: this one takes over
        deadline = time.time() + 5
        got = []
        while time.time() < deadline and len(got) < 2:
            got += s.take()
            time.sleep(0.01)
    finally:
        s.stop()
    assert got[0][0] == "machine" and got[1][0] == "sample"


def test_real_lock_is_exclusive(tmp_path):
    pytest.importorskip("fcntl")
    a = colab_remote.try_flock(str(tmp_path / "sample.lock"))
    assert a is not None
    import subprocess
    import sys
    code = "import sys; sys.path.insert(0, %r); import colab_remote; print(colab_remote.try_flock(%r) is None)" % (
        os.path.dirname(colab_remote.__file__), str(tmp_path / "sample.lock"))
    assert subprocess.run([sys.executable, "-c", code], capture_output=True, text=True).stdout.strip() == "True"
    a.close()


def test_older_driver_without_memory_reserved(rt):
    s = rt.sampler(interval=0.01)
    queries = []

    def popen(argv):
        queries.append(argv[1])
        # the first query names memory.reserved: an older nvidia-smi refuses it at once
        if "memory.reserved" in argv[1]:
            return FakeLoop([], block=False)
        return FakeLoop(["50, 2000, 60, 30.5"])
    s.popen = popen
    s._start_smi()
    deadline = time.time() + 5
    while time.time() < deadline and not s.smi_last:
        time.sleep(0.01)
    got = s.sample()
    s.stop()
    assert "memory.reserved" in queries[0] and "memory.reserved" not in queries[1]
    assert got["gpu_util_pct"] == 50 and got["vram_used_mb"] == 2000 and got["vram_reserved_mb"] == -1
    assert got["gpu_temp_c"] == 60 and got["gpu_power_w"] == 30.5


class FakeSampler:
    """What attach_main sees of a Sampler: readings to take, start and stop."""

    def __init__(self, batches):
        self.batches, self.started, self.stopped = list(batches), False, False

    def start(self):
        self.started = True
        return self

    def take(self):
        return self.batches.pop(0) if self.batches else []

    def stop(self):
        self.stopped = True


def test_attach_writes_readings_before_the_worker_lines_and_never_after_the_final(rt, tmp_path):
    b = str(tmp_path / "b2")
    d = os.path.join(b, "jobs", "x")
    os.makedirs(d)
    open(os.path.join(b, colab_remote.WORKER_READY), "w").close()
    with open(os.path.join(b, colab_remote.WORKER_PID), "w") as f:
        f.write("7")
    ts = time.time()
    write_lines(os.path.join(d, colab_remote.JOB_EVENTS), [
        {"type": "stage", "ts": ts, "stage": "check"},
        {"type": "error", "ts": ts, "code": "audio_unreadable", "retryable": False}])
    src = rt.sampler()
    machine = src.machine()
    sample = src.sample()
    fake = FakeSampler([[("machine", machine), ("sample", sample),
                         ("sample", dict(sample, gpu_name="x"))],  # outside the contract: dropped
                        [("sample", sample)]])  # would come after the final event
    out = Out()
    rc = colab_remote.attach_main(attach_args(b, d), out, spawn=None, alive=lambda pid: pid == 7, poll=0.01,
                                  sampler=fake)
    assert rc == colab_remote.EXIT_INPUT and fake.started and fake.stopped
    types = [json.loads(l)["type"] for l in out.text.splitlines()]
    assert types == ["machine", "sample", "stage", "error"]
    for line in out.text.splitlines():
        colab_remote.check_event_line(line)


def test_attach_without_a_sampler_writes_no_readings(tmp_path):
    b = str(tmp_path)
    d = os.path.join(b, "jobs", "x")
    os.makedirs(d)
    open(os.path.join(b, colab_remote.WORKER_READY), "w").close()
    with open(os.path.join(b, colab_remote.WORKER_PID), "w") as f:
        f.write("7")
    write_lines(os.path.join(d, colab_remote.JOB_EVENTS),
                [{"type": "error", "ts": time.time(), "code": "internal", "retryable": True}])
    out = Out()
    colab_remote.attach_main(attach_args(b, d), out, spawn=None, alive=lambda pid: True, poll=0.01)
    assert [json.loads(l)["type"] for l in out.text.splitlines()] == ["error"]
