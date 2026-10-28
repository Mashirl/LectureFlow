# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""No usage reporting from the workers (privacy audit before 3.2.0, NOTES.md "无遥测").

`import openvino` sent a Google Analytics event through the openvino-telemetry package, and
huggingface_hub reports usage unless switched off. worker/asr_common.py turns both off when it
is imported (telemetry_off); every worker imports it before any third-party library. These
tests use no real OpenVINO (a fake telemetry package stands in) except the realgpu one at the
end, which runs the real --probe of the .venv-ov interpreter under an audit hook.
"""
import ast
import json
import os
import subprocess
import sys
import textwrap
import types

import pytest

from conftest import PROJECT_DIR, WORKER_DIR

import asr_common

WANT_ENV = {"HF_HUB_DISABLE_TELEMETRY": "1", "HF_HUB_DISABLE_IMPLICIT_TOKEN": "1",
            "HF_XET_TELEMETRY_ENABLED": "0", "DO_NOT_TRACK": "1"}

# A user's environment that turns everything back on: the workers must not care.
LOUD_ENV = {"HF_HUB_DISABLE_TELEMETRY": "0", "HF_HUB_DISABLE_IMPLICIT_TOKEN": "0",
            "HF_XET_TELEMETRY_ENABLED": "1", "DO_NOT_TRACK": "0"}


def child_env(**extra):
    env = {k: v for k, v in os.environ.items() if k.upper() not in WANT_ENV and k.upper() != "PYTHONPATH"}
    env.update(LOUD_ENV)
    env.update(extra)
    return env


def test_the_list():
    # the same list as internal/pyenv TelemetryOff (checked there against this file too)
    assert asr_common.TELEMETRY_OFF_ENV == WANT_ENV
    assert asr_common.TELEMETRY_MODULES == ("openvino_telemetry",)


def test_importing_asr_common_turned_it_off():
    for k, v in WANT_ENV.items():
        assert os.environ[k] == v
    assert sys.modules["openvino_telemetry"] is None
    with pytest.raises(ImportError):
        import openvino_telemetry  # noqa: F401


def test_telemetry_off_replaces_the_users_values():
    env, mods = dict(LOUD_ENV, PATH="x"), {"openvino_telemetry": types.ModuleType("openvino_telemetry")}
    asr_common.telemetry_off(env, mods)
    asr_common.telemetry_off(env, mods)  # idempotent
    assert env == dict(WANT_ENV, PATH="x")
    assert mods == {"openvino_telemetry": None}


def write_fake_telemetry(tmp_path):
    """A stand-in openvino_telemetry that leaves a marker when imported, and a module that
    imports it the way openvino.tools.ovc and nncf do (ImportError -> no-op stub)."""
    pkg = tmp_path / "openvino_telemetry"
    pkg.mkdir()
    marker = tmp_path / "telemetry_imported"
    (pkg / "__init__.py").write_text("open(%r, 'w').close()\n" % str(marker), encoding="utf-8")
    (tmp_path / "fake_ovc.py").write_text(textwrap.dedent("""\
        try:
            import openvino_telemetry as tm
        except ImportError:
            tm = None
        print("stub" if tm is None else "telemetry")
        """), encoding="utf-8")
    return marker


def run_child(tmp_path, code):
    full = "import sys; sys.path[:0] = [%r, %r]\n%s" % (str(tmp_path), WORKER_DIR, code)
    p = subprocess.run([sys.executable, "-I", "-c", full], capture_output=True, timeout=120,
                       env=child_env(), cwd=str(tmp_path))
    assert p.returncode == 0, p.stderr.decode("utf-8", "replace")
    return p.stdout.decode("utf-8").strip()


def test_libraries_fall_back_to_their_stubs(tmp_path):
    marker = write_fake_telemetry(tmp_path)
    # control: without asr_common the fake telemetry package is imported
    assert run_child(tmp_path, "import fake_ovc") == "telemetry"
    assert marker.exists()
    marker.unlink()
    assert run_child(tmp_path, "import asr_common, fake_ovc") == "stub"
    assert not marker.exists()


@pytest.mark.parametrize("script", ["asr", "asr_openvino", "asr_mlx", "textrepair"])
def test_workers_import_asr_common_before_any_other_library(script):
    """asr_common (telemetry_off) comes before every non-standard-library import at module
    level, so huggingface_hub / openvino can never be imported with the reporting on."""
    with open(os.path.join(WORKER_DIR, script + ".py"), encoding="utf-8") as f:
        tree = ast.parse(f.read())
    seen_common = False
    for node in tree.body:
        if isinstance(node, ast.Import):
            names = [a.name.split(".")[0] for a in node.names]
        elif isinstance(node, ast.ImportFrom):
            names = [(node.module or "").split(".")[0]]
        else:
            continue
        for n in names:
            if n == "asr_common":
                seen_common = True
            elif not seen_common:
                assert n in sys.stdlib_module_names or n == "__future__", \
                    "%s.py imports %s before asr_common" % (script, n)
    assert seen_common, "%s.py does not import asr_common" % script


def test_huggingface_hub_reads_the_switches(tmp_path):
    """The real huggingface_hub of this environment (faster-whisper downloads models with it)
    sees telemetry and implicit tokens off, even when the user's environment says otherwise."""
    pytest.importorskip("huggingface_hub")
    out = run_child(tmp_path, textwrap.dedent("""\
        import asr_common
        from huggingface_hub import constants as c
        from huggingface_hub.utils._headers import _http_user_agent
        print(c.HF_HUB_DISABLE_TELEMETRY, c.HF_HUB_DISABLE_IMPLICIT_TOKEN,
              "torch/" in _http_user_agent() or "agent/" in _http_user_agent())
        """))
    assert out == "True True False"


def test_hf_xet_reads_the_switches(tmp_path):
    """hf_xet (huggingface_hub's download engine, a Rust extension) reports transfers to
    its /v1/telemetry endpoint unless switched off: its effective configuration says off."""
    pytest.importorskip("hf_xet")
    out = run_child(tmp_path, textwrap.dedent("""\
        import asr_common, hf_xet
        print(hf_xet.XetConfig().get("telemetry.enabled"))
        """))
    assert out == "False"


def test_colab_remote_sets_the_switches(monkeypatch):
    import colab_remote
    assert colab_remote.TELEMETRY_OFF_ENV == WANT_ENV
    for k, v in LOUD_ENV.items():
        monkeypatch.setenv(k, v)
    assert colab_remote.main([]) == colab_remote.EXIT_INPUT  # no job: nothing else runs
    for k, v in WANT_ENV.items():
        assert os.environ[k] == v


def test_onnxruntime_events_off_before_the_vad(monkeypatch):
    """onnxruntime (faster-whisper's Silero VAD) logs usage events to Windows' diagnostic
    data unless disable_telemetry_events() runs before its first session."""
    import numpy as np
    import faster_whisper.vad as vad
    import asr
    calls = []
    fake_ort = types.ModuleType("onnxruntime")
    fake_ort.disable_telemetry_events = lambda: calls.append("telemetry_off")
    monkeypatch.setitem(sys.modules, "onnxruntime", fake_ort)
    monkeypatch.setattr(vad, "get_speech_timestamps",
                        lambda audio, opts: calls.append("vad") or [{"start": 0, "end": len(audio)}])
    monkeypatch.setattr(vad, "collect_chunks", lambda audio, chunks: ([audio], None))
    asr.speech_audio(np.zeros(16000, dtype=np.float32))
    assert calls == ["telemetry_off", "vad"]


def test_onnxruntime_off_without_onnxruntime(monkeypatch):
    import asr
    monkeypatch.setitem(sys.modules, "onnxruntime", None)  # not installed
    asr.onnxruntime_telemetry_off()


# ---------------------------------------------------------------------------
# The real OpenVINO of .venv-ov (realgpu: enumerates the real devices)
# ---------------------------------------------------------------------------

OV_PYTHON = os.environ.get("LF_REALGPU_OV_PYTHON",
                           os.path.join(PROJECT_DIR, ".venv-ov", "Scripts", "python.exe"))

NETTRACE = textwrap.dedent("""\
    import json, os, runpy, sys
    LOG = sys.argv[1]
    EVENTS = ("socket.getaddrinfo", "socket.connect", "urllib.Request", "http.client.connect")
    def hook(event, args):
        if event in EVENTS:
            with open(LOG, "a", encoding="utf-8") as f:
                f.write(json.dumps([event, repr(args)[:300]]) + "\\n")
            raise OSError("blocked by the test: " + event)
    sys.addaudithook(hook)
    script = sys.argv[2]
    sys.argv = sys.argv[2:]
    sys.path.insert(0, os.path.dirname(os.path.abspath(script)))
    runpy.run_path(script, run_name="__main__")
    """)


@pytest.mark.realgpu
def test_openvino_probe_sends_nothing(tmp_path):
    """asr_openvino.py --probe (import openvino, openvino_genai, optimum.intel) makes no
    network request and writes no telemetry files (openvino_ga_cid, stats) into the profile."""
    if not os.path.isfile(OV_PYTHON):
        pytest.skip("no OpenVINO interpreter at %s" % OV_PYTHON)
    tracer, log = tmp_path / "nettrace.py", tmp_path / "net.jsonl"
    tracer.write_text(NETTRACE, encoding="utf-8")
    profile = tmp_path / "profile"
    profile.mkdir()
    env = child_env(LOCALAPPDATA=str(profile), HOME=str(profile), CUDA_VISIBLE_DEVICES="-1")
    p = subprocess.run([OV_PYTHON, str(tracer), str(log), os.path.join(WORKER_DIR, "asr_openvino.py"),
                        "--probe"], capture_output=True, timeout=300, env=env, cwd=str(tmp_path))
    assert p.returncode == 0, p.stderr.decode("utf-8", "replace")[-2000:]
    assert any(l.startswith("PROBE ") for l in p.stdout.decode("utf-8").splitlines())
    events = log.read_text(encoding="utf-8").splitlines() if log.exists() else []
    assert events == [], events
    assert list(profile.rglob("*")) == []
