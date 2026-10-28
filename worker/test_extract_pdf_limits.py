# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""extract_pdf.py resource limits (security review 2, M2): pages released as they are done,
the decode budget, the memory limit. The "bomb" PDFs are generated here, small: a few KB that
decode to tens of MB, never more than ~50 MB in a test."""
import json
import os
import subprocess
import sys
import threading
import time
import tracemalloc
import types

import pytest
from pypdf import PdfReader, PdfWriter
from pypdf.generic import DecodedStreamObject, DictionaryObject, NameObject, StreamObject

import extract_pdf
from conftest import PROJECT_DIR, WORKER_DIR
from test_extract_pdf import TEXT_PAGE, build_pdf

MiB = 1 << 20


def build_bomb(path, pages, mib_per_page, shared=False, filler_bytes=None):
    """Pages whose Flate-compressed content stream decodes to mib_per_page MiB (or filler_bytes):
    one line of text ("Page n") and a comment of "A"s (zlib squeezes 1 MiB of it into ~1 KB).
    shared: every page draws the same stream object (its text is then "Page 1")."""
    w = PdfWriter()
    font = w._add_object(DictionaryObject({
        NameObject("/Type"): NameObject("/Font"), NameObject("/Subtype"): NameObject("/Type1"),
        NameObject("/BaseFont"): NameObject("/Helvetica")}))
    resources = DictionaryObject({NameObject("/Font"): DictionaryObject({NameObject("/F1"): font})})
    n = filler_bytes if filler_bytes is not None else mib_per_page * MiB
    filler = b"%" + b"A" * n + b"\n"
    ref = None
    for i in range(pages):
        page = w.add_blank_page(612, 792)
        page[NameObject("/Resources")] = resources
        if ref is None or not shared:
            s = DecodedStreamObject()
            s.set_data(b"BT /F1 12 Tf 72 720 Td (Page %d) Tj ET\n" % (i + 1) + filler)
            ref = w._add_object(s.flate_encode())
        page[NameObject("/Contents")] = ref
    w.write(str(path))
    return str(path)


def write_job(tmp_path, data):
    p = tmp_path / "job.json"
    p.write_text(json.dumps(data), encoding="utf-8")
    return str(p), str(tmp_path / "result.json.tmp")


def run_main(capsys, *argv):
    rc = extract_pdf.main(list(argv))
    out, err = capsys.readouterr()
    return rc, out.splitlines(), err


def test_decode_budget_default():
    assert extract_pdf.decode_budget(0) == extract_pdf.DECODE_BUDGET_MIN == 512 * MiB
    assert extract_pdf.decode_budget(10 * MiB) == 512 * MiB  # 32 x 10 MiB is below the floor
    assert extract_pdf.decode_budget(40 * MiB) == 1280 * MiB
    assert extract_pdf.decode_budget(1 << 30) == extract_pdf.DECODE_BUDGET_MAX == 2 << 30


def test_pages_are_released_as_they_are_done(tmp_path):
    # 16 pages x 2 MiB decoded from a ~40 KB file. Before the fix pypdf kept every decoded page
    # in its object cache (measured: Python allocations peaked at ~38 MiB here, the process
    # grew ~8.7 MB per 8 MiB page); now one page at a time is held.
    pdf = build_bomb(tmp_path / "bomb.pdf", 16, 2)
    assert os.path.getsize(pdf) < 100 * 1024
    lines = []
    tracemalloc.start()
    try:
        pages = extract_pdf.extract_pages(pdf, lines.append, budget=64 * MiB)
        _, peak = tracemalloc.get_traced_memory()
    finally:
        tracemalloc.stop()
    assert pages == ["Page %d" % (i + 1) for i in range(16)]
    assert peak < 16 * MiB, "peak %.1f MiB: decoded pages are kept" % (peak / MiB)
    assert "STATUS pages 16" in lines
    decoded = [int(l.split()[1]) for l in lines if l.startswith("decoded ")]
    assert decoded and decoded[0] >= 16 * 2 * MiB  # every page was decoded, and counted


def test_evictor_drops_stream_objects(tmp_path):
    pdf = build_bomb(tmp_path / "bomb.pdf", 3, 1)
    with open(pdf, "rb") as f:
        reader = PdfReader(f, strict=False)
        evictor = extract_pdf._StreamEvictor(reader)
        evictor.release()
        extract_pdf.page_text(reader.pages[0])
        streams = [o for o in reader.resolved_objects.values() if isinstance(o, StreamObject)]
        assert len(streams) == 1 and streams[0].decoded_self is not None  # decoded and cached
        assert evictor.release() == 1
        assert not [o for o in reader.resolved_objects.values() if isinstance(o, StreamObject)]
        # an evicted stream is read again when needed
        assert extract_pdf.page_text(reader.pages[0]).startswith("Page 1")


def test_small_shared_streams_stay_cached(tmp_path):
    # A 64 KiB stream drawn by 40 pages (as a font CMap is used by every page) is read and decoded
    # once: small streams are not evicted, so the count is not inflated by the page count.
    pdf = build_bomb(tmp_path / "shared-small.pdf", 40, 0, shared=True, filler_bytes=64 * 1024)
    lines = []
    assert extract_pdf.extract_pages(pdf, lines.append) == ["Page 1"] * 40
    decoded = [int(l.split()[1]) for l in lines if l.startswith("decoded ")]
    assert decoded and decoded[0] < 2 * 64 * 1024, decoded
    with open(pdf, "rb") as f:
        reader = PdfReader(f, strict=False)
        evictor = extract_pdf._StreamEvictor(reader)
        extract_pdf.page_text(reader.pages[0])
        assert evictor.release() == 0  # 64 KiB < EVICT_MIN_BYTES
        assert [o for o in reader.resolved_objects.values() if isinstance(o, StreamObject)]
        # a stream that was small when it was read but decodes large is looked at again
        big = extract_pdf._StreamEvictor(reader, keep_below=1)
        stream = next(o for o in reader.resolved_objects.values() if isinstance(o, StreamObject))
        big.note(stream)
        assert big.release() == 1


def test_bomb_stops_at_the_decode_budget(tmp_path, capsys):
    # 24 MiB decoded from a ~30 KB file; budget 4 MiB: stops at the page that goes over it.
    pdf = build_bomb(tmp_path / "bomb.pdf", 24, 1)
    in_path, out_path = write_job(tmp_path, {"pdf": pdf, "decode_budget_bytes": 4 * MiB})
    rc, lines, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 2, err
    assert 'ERROR_CODE decode_budget {"limit_bytes": 4194304}' in lines
    last = err.strip().splitlines()[-1]
    assert last.startswith("ERROR: input error: ") and "decode budget" in last
    assert len([l for l in lines if l.startswith("PROGRESS")]) <= 4
    assert not os.path.exists(out_path)


def test_shared_stream_is_counted_for_every_page(tmp_path):
    # One 1 MiB content stream drawn by 40 pages: pypdf parses it again for every page, so after
    # each page releases it, it is decoded (and counted) again: the budget bounds that work too.
    pdf = build_bomb(tmp_path / "shared.pdf", 40, 1, shared=True)
    with pytest.raises(extract_pdf.DecodeBudgetExceeded) as ei:
        extract_pdf.extract_pages(pdf, lambda l: None, budget=8 * MiB)
    assert ei.value.code == "decode_budget" and ei.value.params == {"limit_bytes": 8 * MiB}
    # within a generous budget it is extracted normally
    assert extract_pdf.extract_pages(pdf, lambda l: None, budget=64 * MiB) == ["Page 1"] * 40


def test_cli_bomb_exit_2(tmp_path):
    # The command line, with a memory limit (Windows: the caller's job object enforces it;
    # Linux: RLIMIT_AS; macOS: the watchdog) and a low decode budget.
    pdf = build_bomb(tmp_path / "bomb.pdf", 24, 1)
    in_path, out_path = write_job(tmp_path, {"pdf": pdf, "decode_budget_bytes": 4 * MiB,
                                             "memory_limit_bytes": 1 << 30})
    p = subprocess.run([sys.executable, os.path.join(WORKER_DIR, "extract_pdf.py"),
                        "--in", in_path, "--out", out_path],
                       capture_output=True, cwd=PROJECT_DIR, timeout=120)
    out = p.stdout.decode("utf-8").splitlines()
    err = p.stderr.decode("utf-8", "replace")
    assert p.returncode == 2, err
    assert "STATUS pages 24" in out
    assert any(l.startswith("memory limit 1073741824 bytes (") for l in out)
    assert 'ERROR_CODE decode_budget {"limit_bytes": 4194304}' in out
    assert err.strip().splitlines()[-1].startswith("ERROR: input error: ")
    assert not os.path.exists(out_path)


def test_encrypted_pages_read_again_after_release(tmp_path, capsys):
    # Owner password only (AES): streams evicted after a page are read and decrypted again.
    pdf = build_pdf(tmp_path / "restricted.pdf", [TEXT_PAGE] * 3, user_password="",
                    owner_password="owner")
    in_path, out_path = write_job(tmp_path, {"pdf": pdf})
    rc, _, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 0, err
    with open(out_path, encoding="utf-8") as f:
        assert json.load(f) == {"pages": ["Cache hierarchy: L1 and L2\nABC123"] * 3,
                                "empty_pages": 0}


@pytest.mark.parametrize("field,value", [
    ("decode_budget_bytes", 0), ("decode_budget_bytes", -5), ("decode_budget_bytes", "64"),
    ("memory_limit_bytes", True), ("memory_limit_bytes", 1.5)])
def test_bad_limit_fields_exit_2(tmp_path, capsys, field, value):
    pdf = build_bomb(tmp_path / "ok.pdf", 1, 0)
    in_path, out_path = write_job(tmp_path, {"pdf": pdf, field: value})
    rc, _, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 2
    assert err.strip().splitlines()[-1] == "ERROR: input error: job.%s must be a positive integer" % field


def test_memory_error_is_an_input_error_under_a_limit(tmp_path, capsys, monkeypatch):
    def out_of_memory(*a, **k):
        raise MemoryError()

    monkeypatch.setattr(extract_pdf, "extract_pages", out_of_memory)
    pdf = build_bomb(tmp_path / "ok.pdf", 1, 0)
    in_path, out_path = write_job(tmp_path, {"pdf": pdf, "memory_limit_bytes": 256 * MiB})
    rc, lines, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 2
    assert 'ERROR_CODE memory_limit {"limit_bytes": 268435456}' in lines
    assert "memory limit" in err.strip().splitlines()[-1]
    # without a limit of the caller's, running out of memory stays retryable
    in_path, out_path = write_job(tmp_path, {"pdf": pdf})
    rc, lines, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 1 and not [l for l in lines if l.startswith("ERROR_CODE")]


def test_memory_error_on_a_page_is_not_kept_empty(tmp_path, monkeypatch):
    pdf = build_bomb(tmp_path / "three.pdf", 3, 0)
    real = extract_pdf.page_text
    calls = []

    def page_text(page):
        calls.append(1)
        if len(calls) == 1:
            raise ValueError("broken page")  # an ordinary failure: the page is kept empty
        if len(calls) == 2:
            raise MemoryError()
        return real(page)

    monkeypatch.setattr(extract_pdf, "page_text", page_text)
    lines = []
    with pytest.raises(MemoryError):
        extract_pdf.extract_pages(pdf, lines.append)
    assert any("page 1: text extraction failed (ValueError: broken page)" in l for l in lines)


def test_apply_memory_limit_windows_is_the_job_object(monkeypatch):
    started = []
    monkeypatch.setattr(extract_pdf, "_start_rss_watchdog", lambda *a, **k: started.append(a))
    assert extract_pdf.apply_memory_limit(1 << 30, platform="win32") == "job object"
    assert started == []


def test_apply_memory_limit_macos_starts_the_watchdog(monkeypatch):
    started = []
    monkeypatch.setattr(extract_pdf, "_start_rss_watchdog", lambda limit: started.append(limit))
    assert extract_pdf.apply_memory_limit(123, platform="darwin") == "rss watchdog"
    assert started == [123]


def _fake_resource(hard, maxrss=0):
    calls = []
    return types.SimpleNamespace(
        RLIMIT_AS=9, RLIM_INFINITY=-1, RUSAGE_SELF=0, calls=calls,
        getrlimit=lambda which: (-1, hard),
        setrlimit=lambda which, limits: calls.append((which, limits)),
        getrusage=lambda who: types.SimpleNamespace(ru_maxrss=maxrss)), calls


def test_apply_memory_limit_linux_sets_rlimit_as(monkeypatch):
    fake, calls = _fake_resource(hard=-1)
    monkeypatch.setitem(sys.modules, "resource", fake)
    assert extract_pdf.apply_memory_limit(1 << 30, platform="linux") == "RLIMIT_AS"
    assert calls == [(9, (1 << 30, -1))]
    # an existing lower hard limit is kept (setrlimit cannot raise it)
    fake, calls = _fake_resource(hard=512 * MiB)
    monkeypatch.setitem(sys.modules, "resource", fake)
    extract_pdf.apply_memory_limit(1 << 30, platform="linux")
    assert calls == [(9, (512 * MiB, 512 * MiB))]


def test_peak_rss_units(monkeypatch):
    fake, _ = _fake_resource(hard=-1, maxrss=1000)
    monkeypatch.setitem(sys.modules, "resource", fake)
    monkeypatch.setattr(sys, "platform", "darwin")
    assert extract_pdf.peak_rss_bytes() == 1000  # bytes on macOS
    monkeypatch.setattr(sys, "platform", "linux")
    assert extract_pdf.peak_rss_bytes() == 1000 * 1024  # KiB on Linux


def test_rss_watchdog():
    fired, stop = threading.Event(), threading.Event()
    t = extract_pdf._start_rss_watchdog(100, peak=lambda: 101, interval=0.01,
                                        on_exceeded=fired.set, stop=stop)
    assert fired.wait(5)
    t.join(5)
    assert not t.is_alive()
    quiet, stop = threading.Event(), threading.Event()
    t = extract_pdf._start_rss_watchdog(100, peak=lambda: 100, interval=0.01,
                                        on_exceeded=quiet.set, stop=stop)
    time.sleep(0.1)
    assert not quiet.is_set()
    stop.set()
    t.join(5)
    assert not t.is_alive()
