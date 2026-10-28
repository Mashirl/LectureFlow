# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""worker/colab_remote.py memory mode (SPEC-ext "Colab 内存运行（实验）"): the probe's estimate
and fallbacks (memory, swap, tmpfs), its one-line output, and a memory-mode run that keeps
packages, model cache and temporary files on the RAM disk. Fake /proc files, a fake mount and
the fake faster_whisper of test_colab_remote.py; nothing is mounted or installed for real."""
import json
import os
import subprocess
import sys
import tempfile

import pytest

import colab_remote
from test_colab_remote import SECRET_WORDS, check_stdout, remote  # noqa: F401 (fixture)

PINS = ["faster-whisper==1.2.1", "av==15.1.0", "ctranslate2==4.8.3"]
MEMORY_KEYS = set(colab_remote.EVENT_KEYS["memory"][0])


def meminfo_text(total_mb, avail_mb, shmem_mb=0):
    return ("MemTotal:       %d kB\nMemFree:        %d kB\nMemAvailable:   %d kB\nShmem:          %d kB\n"
            % (total_mb * 1024, avail_mb * 1024, avail_mb * 1024, shmem_mb * 1024))


class Proc:
    """Fake /proc/meminfo, /proc/swaps and /proc/mounts, RAM disk directories, statvfs and
    mount."""

    def __init__(self, tmp_path, monkeypatch):
        self.dir = tmp_path / "proc"
        self.dir.mkdir()
        self.shm = tmp_path / "shm"
        self.shm.mkdir()
        self.shm_base = str(self.shm / "lectureflow")
        self.tmpfs_base = str(tmp_path / "mnt" / "lectureflow")
        monkeypatch.setattr(colab_remote, "MEMORY_BASES", (self.shm_base, self.tmpfs_base))
        self.meminfo = str(self.dir / "meminfo")
        self.swaps = str(self.dir / "swaps")
        self.mounts = str(self.dir / "mounts")
        self.set_memory(12985, 11800)
        self.set_swaps([])
        with open(self.mounts, "w", encoding="utf-8") as f:
            f.write("overlay / overlay rw 0 0\ntmpfs %s tmpfs rw,nosuid,nodev 0 0\n" % self.shm)
        self.free = {str(self.shm): 6000}
        self.mounted = []
        self.mount_ok = True

    def set_memory(self, total_mb, avail_mb):
        with open(self.meminfo, "w", encoding="ascii") as f:
            f.write(meminfo_text(total_mb, avail_mb))

    def set_swaps(self, lines):
        with open(self.swaps, "w", encoding="ascii") as f:
            f.write("Filename\tType\tSize\tUsed\tPriority\n")
            for l in lines:
                f.write(l + "\n")

    def free_mb(self, path):
        return self.free.get(path, -1)

    def mount(self, target, size_mb):
        self.mounted.append((target, size_mb))
        if not self.mount_ok:
            return False
        with open(self.mounts, "a", encoding="utf-8") as f:
            f.write("tmpfs %s tmpfs rw,size=%dm,mode=700 0 0\n" % (target, size_mb))
        return True

    def probe(self, model="large-v3", audio_bytes=100 * colab_remote.MIB, reqs=PINS, **kw):
        return colab_remote.memory_probe(model, audio_bytes, reqs, meminfo=self.meminfo, swaps=self.swaps,
                                         mounts=self.mounts, free_mb=self.free_mb, mount=self.mount, **kw)


@pytest.fixture
def proc(tmp_path, monkeypatch):
    return Proc(tmp_path, monkeypatch)


# model + environment + recording x 2 + 2 GB (MiB) for large-v3 and 100 MiB of audio.
# model on the RAM disk + environment + recording x 2 + results + the process's model copy
# + 3 GB (the rest of the process) + 2 GB margin (MiB), for large-v3 and 100 MiB of audio.
NEED = 2950 + 512 + 2 * 100 + 64 + 2950 + 3072 + 2048
ON_DISK = 2950 + 512 + 100 + 64


def test_estimate_formula():
    assert colab_remote.MEMORY_MARGIN_MB == 2048 and colab_remote.PROCESS_MB == 3072
    assert colab_remote.ENV_MB == 512 and colab_remote.RESULT_MB == 64
    assert colab_remote.model_mb("large-v3") == 2950 and colab_remote.model_mb("turbo") == 1550
    assert colab_remote.model_mb("someone/unknown-model") == colab_remote.DEFAULT_MODEL_MB


def test_enough_memory_uses_dev_shm(proc):
    d = proc.probe()
    assert d == {"mode": "memory", "base": proc.shm_base, "reason": "ok", "mem_total_mb": 12985,
                 "mem_available_mb": 11800, "need_mb": NEED, "free_mb": 6000}
    assert os.path.isdir(proc.shm_base)
    assert proc.mounted == []


def test_low_memory_falls_back_to_disk(proc):
    proc.set_memory(12985, NEED - 1)
    d = proc.probe()
    assert (d["mode"], d["base"], d["reason"], d["need_mb"]) == ("disk", "", "low_memory", NEED)
    assert not os.path.exists(proc.shm_base) and proc.mounted == []
    proc.set_memory(12985, NEED)
    assert proc.probe()["mode"] == "memory"


def test_process_is_counted_where_the_spec_formula_would_allow(proc):
    """Lead's ruling (2026-10-27): the transcription process is part of the estimate (the Colab
    experiment measured about 5.9 GB for large-v3 float16, and 10.2 of 13.0 GB in use at the
    peak). SPEC-ext's model + environment + recording x 2 + 2 GB would let this runtime in."""
    spec_formula = 2950 + 512 + 2 * 100 + 64 + 2048
    proc.set_memory(12985, 8000)
    assert spec_formula <= 8000 < NEED
    d = proc.probe()
    assert (d["mode"], d["reason"], d["need_mb"]) == ("disk", "low_memory", NEED)
    assert not os.path.exists(proc.shm_base) and proc.mounted == []
    # model and environment already on the RAM disk: the process still needs its room
    os.makedirs(proc.shm_base)
    make_model(proc.shm_base)
    make_env(proc.shm_base, {"faster-whisper": "1.2.1", "av": "15.1.0", "ctranslate2": "4.8.3"})
    proc.free[proc.shm_base] = 6000
    spec_formula = 100 + 64 + 100 + 2048
    d = proc.probe()
    assert spec_formula <= 8000 < d["need_mb"] == 100 + 64 + 100 + 2950 + 3072 + 2048
    assert d["mode"] == "disk" and d["reason"] == "low_memory"


def test_extra_mb_forces_the_fallback(proc):
    d = proc.probe(extra_mb=100000)
    assert d["mode"] == "disk" and d["reason"] == "low_memory" and d["need_mb"] == NEED + 100000
    assert not os.path.exists(proc.shm_base)


def test_swap_enabled_means_disk(proc):
    proc.set_swaps(["/swapfile file 4194300 0 -2"])
    d = proc.probe()
    assert d["mode"] == "disk" and d["reason"] == "swap_enabled"
    assert not os.path.exists(proc.shm_base) and proc.mounted == []


@pytest.mark.parametrize("missing", ["meminfo", "swaps"])
def test_unreadable_proc_means_disk(proc, missing):
    os.remove(getattr(proc, missing))
    d = proc.probe()
    assert d["mode"] == "disk" and d["reason"] == "no_proc"


def test_small_dev_shm_mounts_a_tmpfs(proc):
    proc.free[str(proc.shm)] = ON_DISK - 1
    proc.free[proc.tmpfs_base] = 9700
    d = proc.probe()
    assert proc.mounted == [(proc.tmpfs_base, 11800 - 2048)]
    assert d["mode"] == "memory" and d["base"] == proc.tmpfs_base and d["free_mb"] == 9700
    assert not os.path.exists(proc.shm_base)


def test_noexec_dev_shm_is_skipped(proc):
    # Colab: "shm /dev/shm tmpfs rw,nosuid,nodev,noexec,relatime,size=5890048k": the packages'
    # native extensions cannot be mapped from it, so the probe mounts its own tmpfs.
    with open(proc.mounts, "w", encoding="utf-8") as f:
        f.write("overlay / overlay rw 0 0\nshm %s tmpfs rw,nosuid,nodev,noexec,relatime,size=5890048k 0 0\n"
                % proc.shm)
    proc.free[proc.tmpfs_base] = 9700
    d = proc.probe()
    assert d["mode"] == "memory" and d["base"] == proc.tmpfs_base
    assert proc.mounted == [(proc.tmpfs_base, 11800 - 2048)] and not os.path.exists(proc.shm_base)
    # an earlier lecture's directory on the noexec /dev/shm is not reused either
    os.makedirs(proc.shm_base)
    assert proc.probe()["base"] == proc.tmpfs_base


def test_own_tmpfs_must_allow_exec(proc):
    proc.free[str(proc.shm)] = 10

    def noexec_mount(target, size_mb):
        with open(proc.mounts, "a", encoding="utf-8") as f:
            f.write("tmpfs %s tmpfs rw,noexec 0 0\n" % target)
        return True

    proc.mount = noexec_mount
    assert proc.probe()["reason"] == "tmpfs_failed"


def test_tmpfs_mount_failure_means_disk(proc):
    proc.free[str(proc.shm)] = 10
    proc.mount_ok = False
    d = proc.probe()
    assert d["mode"] == "disk" and d["reason"] == "tmpfs_failed"


def test_no_shm_knob_mounts_a_tmpfs(proc):
    proc.free[proc.tmpfs_base] = 9700
    d = proc.probe(no_shm=True)
    assert d["base"] == proc.tmpfs_base and proc.mounted and not os.path.exists(proc.shm_base)


def make_env(base, versions):
    env = os.path.join(base, "env")
    for name, version in versions.items():
        di = os.path.join(env, "%s-%s.dist-info" % (name.replace("-", "_"), version))
        os.makedirs(di)
        with open(os.path.join(di, "METADATA"), "w", encoding="utf-8") as f:
            f.write("Metadata-Version: 2.1\nName: %s\nVersion: %s\n" % (name, version))


def make_model(base, repo="Systran/faster-whisper-large-v3"):
    snap = os.path.join(base, "hf", "hub", "models--" + repo.replace("/", "--"), "snapshots", "abc123")
    os.makedirs(snap)
    open(os.path.join(snap, "model.bin"), "wb").close()


def test_second_lecture_reuses_the_ram_disk(proc):
    os.makedirs(proc.shm_base)
    make_model(proc.shm_base)
    make_env(proc.shm_base, {"faster-whisper": "1.2.1", "av": "15.1.0", "ctranslate2": "4.8.3"})
    proc.free[proc.shm_base] = 200
    d = proc.probe()
    assert d["mode"] == "memory" and d["base"] == proc.shm_base
    # no model and no environment on the RAM disk; the process's own copy of the model counts
    assert d["need_mb"] == 100 + 64 + 100 + 2950 + 3072 + 2048
    # another model or another pin version is counted again
    assert proc.probe(model="medium")["need_mb"] == 1460 + 100 + 64 + 100 + 1460 + 3072 + 2048
    assert proc.probe(reqs=["faster-whisper==1.2.2"])["need_mb"] == d["need_mb"] + 512


def test_full_ram_disk_means_disk(proc):
    os.makedirs(proc.shm_base)
    proc.free[proc.shm_base] = 10
    d = proc.probe()
    assert d["mode"] == "disk" and d["reason"] == "low_memory"


def test_existing_tmpfs_is_reused(proc):
    proc.free[str(proc.shm)] = 10
    proc.free[proc.tmpfs_base] = 9700
    assert proc.probe()["base"] == proc.tmpfs_base
    d = proc.probe()
    assert d["base"] == proc.tmpfs_base and len(proc.mounted) == 1  # mounted once


def test_unmounted_tmpfs_directory_is_not_a_ram_disk(proc):
    # a /mnt/lectureflow directory that is not a tmpfs mount is on the disk: never used as is
    os.makedirs(proc.tmpfs_base)
    proc.free[str(proc.shm)] = 10
    proc.mount_ok = False
    assert proc.probe()["reason"] == "tmpfs_failed"


# ---------------------------------------------------------------------------
# The probe's command line and output
# ---------------------------------------------------------------------------

def probe_cli(capfd, args, probe=None):
    rc = colab_remote.main(args, isolate=False, probe=probe)
    out, _ = capfd.readouterr()
    return rc, out


def check_memory_line(out):
    assert out.endswith("\n") and out.count("\n") == 1, out
    ev = json.loads(out)
    assert set(ev) == MEMORY_KEYS and ev["type"] == "memory"
    assert ev["mode"] in colab_remote.MEMORY_MODES and ev["reason"] in colab_remote.MEMORY_REASONS
    for k in ("mem_total_mb", "mem_available_mb", "need_mb", "free_mb"):
        assert isinstance(ev[k], int) and ev[k] >= 0
    return ev


def test_probe_cli_prints_one_memory_event(capfd):
    seen = {}

    def fake(model, audio_bytes, reqs, extra_mb=0, no_shm=False):
        seen.update(model=model, audio_bytes=audio_bytes, reqs=reqs, extra_mb=extra_mb, no_shm=no_shm)
        return {"mode": "memory", "base": "/dev/shm/lectureflow", "reason": "ok", "mem_total_mb": 12985,
                "mem_available_mb": 11800, "need_mb": 6286, "free_mb": 6000}

    rc, out = probe_cli(capfd, ["--memory-probe", "--model", "large-v3", "--audio-bytes", "105000000",
                                "--requirements", ",".join(PINS), "--extra-mb", "5", "--no-shm"], fake)
    assert rc == 0
    ev = check_memory_line(out)
    assert ev["base"] == "/dev/shm/lectureflow" and ev["need_mb"] == 6286
    assert seen == {"model": "large-v3", "audio_bytes": 105000000, "reqs": PINS, "extra_mb": 5, "no_shm": True}


@pytest.mark.parametrize("args", [
    ["--model", "../x"], ["--model", ""], ["--model", "large-v3", "--requirements", "faster-whisper>=1"],
])
def test_probe_cli_bad_arguments(capfd, args):
    rc, out = probe_cli(capfd, ["--memory-probe"] + args, lambda *a, **k: pytest.fail("probe ran"))
    assert rc == colab_remote.EXIT_INPUT
    ev = check_memory_line(out)
    assert ev["mode"] == "disk" and ev["reason"] == "error"


def test_probe_failure_says_disk_without_text(capfd):
    def boom(*a, **k):
        raise RuntimeError("Zebrafish could not mount quokka")

    rc, out = probe_cli(capfd, ["--memory-probe", "--model", "large-v3"], boom)
    assert rc == 0
    ev = check_memory_line(out)
    assert ev["mode"] == "disk" and ev["reason"] == "error"
    for w in SECRET_WORDS:
        assert w not in out


def test_probe_event_refuses_inconsistent_decisions():
    class Buf:
        text = ""

        def write(self, s):
            self.text += s

        def flush(self):
            pass

    ev = colab_remote.Events(Buf())
    base = {"mem_total_mb": 1, "mem_available_mb": 1, "need_mb": 1, "free_mb": 0}
    for d in ({"mode": "memory", "base": "/etc", "reason": "ok"},
              {"mode": "disk", "base": "/dev/shm/lectureflow", "reason": "low_memory"},
              {"mode": "memory", "base": "/dev/shm/lectureflow", "reason": "because"}):
        with pytest.raises(AssertionError):
            ev.memory(dict(base, **d))


def test_probe_source_runs_isolated_from_a_c_argument():
    """LectureFlow runs the probe as `python -I -c <source>` (nothing written before the
    decision). Here there is no /proc: disk, no_proc, one line on stdout."""
    src = open(colab_remote.__file__, encoding="utf-8").read()
    assert len(src.encode("utf-8")) < 100 * 1024  # one argv string (Linux: 128 KiB at most)
    code = src
    if sys.platform == "win32":
        # Windows command lines end at 32767 characters: run the same source the same way
        # (module __main__, no __file__) through a short -c.
        code = "exec(compile(open(%r, encoding='utf-8').read(), '<string>', 'exec'))" % colab_remote.__file__
    r = subprocess.run([sys.executable, "-I", "-c", code, "--memory-probe", "--model", "large-v3",
                        "--audio-bytes", "1000", "--requirements", ",".join(PINS)],
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE, stdin=subprocess.DEVNULL, timeout=120)
    assert r.returncode == 0, r.stderr
    ev = check_memory_line(r.stdout.decode("utf-8"))
    if not os.path.exists("/proc/meminfo"):
        assert ev["mode"] == "disk" and ev["reason"] == "no_proc"


# ---------------------------------------------------------------------------
# A memory-mode run
# ---------------------------------------------------------------------------

@pytest.fixture
def isolated_env(monkeypatch):
    saved = dict(os.environ)
    monkeypatch.setattr(sys, "path", list(sys.path))
    monkeypatch.setattr(sys, "dont_write_bytecode", sys.dont_write_bytecode)
    monkeypatch.setattr(tempfile, "tempdir", tempfile.tempdir)
    yield
    os.environ.clear()
    os.environ.update(saved)


def test_pip_command_disk_mode_unchanged():
    assert colab_remote.pip_command(PINS) == [
        sys.executable, "-m", "pip", "install", "--disable-pip-version-check", "--no-input",
        "--progress-bar", "off"] + PINS
    assert colab_remote.pip_command(PINS, "/mnt/lectureflow/env", "/mnt/lectureflow/tmp/c.txt") == [
        sys.executable, "-m", "pip", "install", "--disable-pip-version-check", "--no-input",
        "--progress-bar", "off", "--no-cache-dir", "--upgrade", "--target", "/mnt/lectureflow/env",
        "-c", "/mnt/lectureflow/tmp/c.txt"] + PINS


def test_system_constraints_hold_the_runtimes_versions(monkeypatch):
    freeze = ("numpy==2.1.3\nhuggingface_hub==1.33.0\nAV==14.0.0\nfaster_whisper==1.1.0\n"
              "torch==2.11.0+cu130\nsomething @ file:///tmp/x.whl\n-e git+https://x/y#egg=z\n")

    def run(cmd, **kw):
        assert cmd[1:5] == ["-m", "pip", "list", "--format=freeze"]
        return subprocess.CompletedProcess(cmd, 0, stdout=freeze.encode())

    monkeypatch.setattr(colab_remote.subprocess, "run", run)
    # the pins themselves (any spelling) are left to the pins
    assert colab_remote.system_constraints(PINS) == ["numpy==2.1.3", "huggingface_hub==1.33.0", "torch==2.11.0+cu130"]


def test_memory_run_keeps_everything_on_the_ram_disk(remote, capfd, monkeypatch, isolated_env, tmp_path):
    base = str(tmp_path)  # the fixture's job directory is BASE/jobs/j1
    monkeypatch.setattr(colab_remote, "MEMORY_BASES", (base, base + "-tmpfs"))
    installed = {"av": "15.1.0"}  # the runtime already has one pin
    monkeypatch.setattr(colab_remote, "installed_version", lambda name: installed.get(name))
    monkeypatch.setattr(colab_remote, "system_constraints", lambda reqs: ["numpy==2.1.3"])
    calls = []

    def install(reqs, log_path, target=None, constraints=None):
        cons = open(constraints, encoding="utf-8").read() if constraints else None
        calls.append((list(reqs), log_path, target, constraints, cons, os.environ.get("TMPDIR"),
                      os.environ.get("HOME")))
        if constraints:
            return 1  # the constrained install fails: once more without
        for r in reqs:
            n, v = r.split("==")
            installed[n] = v
        return 0

    seen = {}
    import asr
    orig = asr.resolve_model_path

    def resolve(name, emit, download=None):
        seen["HF_HOME"] = os.environ.get("HF_HOME")
        seen["HF_HUB_CACHE"] = os.environ.get("HF_HUB_CACHE")
        return orig(name, emit, download)

    monkeypatch.setattr(asr, "resolve_model_path", resolve)
    os.environ["HUGGINGFACE_HUB_CACHE"] = "/root/.cache/elsewhere"
    job = remote.write_job(memory=True, requirements=PINS[:2])
    meminfo = tmp_path / "meminfo"
    meminfo.write_text(meminfo_text(12985, 9000), encoding="ascii")
    rc = colab_remote.main(["--job", job, "--log-dir", base], install=install, meminfo=str(meminfo))
    out, err = capfd.readouterr()
    assert rc == 0, err
    events = check_stdout(out)
    done = events[-1]
    assert done["type"] == "done" and done["mem_total_mb"] == 12985 and done["mem_peak_mb"] == 12985 - 9000
    j = lambda *p: os.path.join(base, *p)
    # every pin into the RAM disk's package directory together, with pip's temporary files there
    assert calls == [
        (PINS[:2], j("install.log"), j("env"), j("tmp", "constraints.txt"), "numpy==2.1.3\n", j("tmp"), j("home")),
        (PINS[:2], j("install.log"), j("env"), None, None, j("tmp"), j("home")),
    ]
    assert seen == {"HF_HOME": j("hf"), "HF_HUB_CACHE": j("hf", "hub")}
    assert os.environ["XDG_CACHE_HOME"] == j("cache") and "HUGGINGFACE_HUB_CACHE" not in os.environ
    assert os.environ["PIP_LOG"] == j("tmp", "pip.log") and os.environ["PYTHONDONTWRITEBYTECODE"] == "1"
    assert sys.dont_write_bytecode
    assert sys.path[0] == j("env")
    assert tempfile.gettempdir() == j("tmp")
    for w in SECRET_WORDS:
        assert w not in out


def test_memory_run_needs_a_ram_disk_job_directory(remote, capfd, isolated_env):
    rc = colab_remote.main(["--job", remote.write_job(memory=True), "--log-dir", remote.log_dir],
                           install=remote.install)
    out, _ = capfd.readouterr()
    assert rc == colab_remote.EXIT_INPUT
    assert check_stdout(out)[-1]["code"] == "bad_job"


def test_memory_field_must_be_boolean(remote, capfd):
    rc = colab_remote.main(["--job", remote.write_job(memory="yes"), "--log-dir", remote.log_dir],
                           install=remote.install)
    out, _ = capfd.readouterr()
    assert rc == colab_remote.EXIT_INPUT and check_stdout(out)[-1]["code"] == "bad_job"


def test_mem_sampler_keeps_the_peak(tmp_path):
    p = tmp_path / "meminfo"
    p.write_text(meminfo_text(1000, 800), encoding="ascii")
    s = colab_remote.MemSampler(str(p), interval=60).start()
    p.write_text(meminfo_text(1000, 300), encoding="ascii")
    s.sample()
    p.write_text(meminfo_text(1000, 900), encoding="ascii")
    s.stop()
    assert s.fields() == {"mem_peak_mb": 700, "mem_total_mb": 1000}
    assert colab_remote.MemSampler(str(tmp_path / "absent")).start().fields() == {}
