# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
# Additional permission under AGPL-3.0 section 7: see LICENSE-EXCEPTION.md
"""LectureFlow PDF text extraction worker (SPEC §6.0, §6.0.1 material_extract; SPEC-v3 V6).

Usage:
    python worker/extract_pdf.py --in job.json --out result.json.tmp

job.json:    {"pdf": "path/to/file.pdf",
              "decode_budget_bytes": 536870912,    (optional, see decode_budget())
              "memory_limit_bytes": 1073741824}    (optional, see apply_memory_limit())
result.json: {"pages": ["page 1 text", "", ...], "empty_pages": 1}
             pages: one entry per page, in order; empty_pages: how many entries are ""

Text comes from pypdf (BSD-3-Clause, SPEC-v3 V6) page.extract_text(), Unicode NFKC-normalised
and stripped. Pages without a text layer (e.g. scanned images) yield "" so page numbers stay
aligned (no OCR, §0). A PDF encrypted with an empty user password (owner restrictions only)
is opened like any other; one that needs a password to open is an input error.

Resource limits (security review 2, M2): pypdf keeps every object it reads, including the
decoded data of every stream, for the lifetime of the reader, so memory grew with the page
count and a few-MB PDF whose streams inflate ~1000x could take gigabytes. Now every large
stream object read or decoded while extracting a page is dropped from pypdf's object cache once
the page is done (_StreamEvictor), the bytes decoded from streams in one document are counted
against a budget
(DecodeBudget), and the caller's memory limit is enforced (Windows: the Job Object Go starts
the process in; Linux: RLIMIT_AS; macOS: a peak-RSS watchdog). Over the budget or the memory
limit is an input error (exit 2) with an ERROR_CODE line.

Protocol (SPEC §6.0): exit code 0 = success, 2 = input error (missing file, not a PDF,
password protected, bad job.json, decode budget or memory limit exceeded), anything else =
retryable. stdout carries "PROGRESS 0.42" lines, one "STATUS pages <n>" line once the page
count is known (Go sizes its timeout with it), at most one "ERROR_CODE <name> <json>" line
before a failed exit (decode_budget / memory_limit, params {"limit_bytes": n}) and log lines;
errors go to stderr, last line "ERROR: <reason>". Only the --out path is written; no
subprocesses.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
import threading
import traceback
import unicodedata
from typing import Any, Callable, List, NamedTuple, Optional, Sequence

EXIT_OK = 0
EXIT_RETRY = 1
EXIT_INPUT = 2

PDF_MAGIC = b"%PDF-"
MAGIC_SEARCH_BYTES = 1024  # the PDF header may be preceded by up to 1 KB of junk

# Decode budget: the bytes pypdf may decode from streams (content streams, form XObjects, font
# CMaps, object and xref streams; images are not decoded for text) in one document, counting a
# large stream again each time it is decoded again after a page released it (a huge content
# stream drawn by every page is parsed again for every page: the budget bounds that work too).
# Measured on the CSC3631 decks (65-104 pages, 0.4-1 MB files): 0.7-1.2 MB decoded per deck.
# Content streams compress 4-10x, so a real PDF decodes about 1-10x its file size; a
# decompression bomb decodes 100-1000x (zlib's ceiling is ~1000x). Hence 32x the file size, at
# least 512 MiB (dense vector slides, long books: still far more than they need) and at most
# 2 GiB (beyond that the work alone would run into Go's timeout).
DECODE_BUDGET_MIN = 512 * 2**20
DECODE_BUDGET_MAX = 2 * 2**30
DECODE_BUDGET_PER_FILE_BYTE = 32

RSS_POLL_SECONDS = 0.1  # macOS memory watchdog


class InputError(Exception):
    """Invalid job input; mapped to exit code 2 (not retried). code / params, when set, are
    reported in an "ERROR_CODE <code> <params JSON>" stdout line before the exit."""

    def __init__(self, message: str, code: Optional[str] = None,
                 params: Optional[dict] = None) -> None:
        super().__init__(message)
        self.code = code
        self.params = params or {}


class DecodeBudgetExceeded(InputError):
    def __init__(self, limit: int) -> None:
        super().__init__("the PDF's streams decode to more than %d bytes (decode budget); "
                         "the file is damaged or crafted to exhaust memory" % limit,
                         code="decode_budget", params={"limit_bytes": limit})


class MemoryLimitExceeded(InputError):
    def __init__(self, limit: int) -> None:
        super().__init__("extracting the text needs more than %d bytes of memory (memory limit); "
                         "the file is damaged or crafted to exhaust memory" % limit,
                         code="memory_limit", params={"limit_bytes": limit})


class Job(NamedTuple):
    pdf: str
    decode_budget: Optional[int] = None  # bytes; None = decode_budget(file size)
    memory_limit: Optional[int] = None  # bytes; None = no limit of its own


def emit_stdout(line: str) -> None:
    sys.stdout.write(line + "\n")
    sys.stdout.flush()


def normalize_page_text(text: str) -> str:
    return unicodedata.normalize("NFKC", text or "").strip()


def _positive_int(data: dict, key: str) -> Optional[int]:
    v = data.get(key)
    if v is None:
        return None
    if isinstance(v, bool) or not isinstance(v, int) or v <= 0:
        raise InputError("job.%s must be a positive integer" % key)
    return v


def load_job(path: str) -> Job:
    try:
        with open(path, encoding="utf-8-sig") as f:
            data = json.load(f)
    except FileNotFoundError:
        raise InputError("job file not found: %s" % path)
    except (OSError, ValueError) as e:
        raise InputError("cannot read job file %s: %s" % (path, e))
    if not isinstance(data, dict):
        raise InputError("job.json must contain a JSON object")
    pdf = data.get("pdf")
    if not isinstance(pdf, str) or not pdf:
        raise InputError("job.pdf must be a non-empty string")
    return Job(pdf, _positive_int(data, "decode_budget_bytes"),
               _positive_int(data, "memory_limit_bytes"))


def decode_budget(file_size: int) -> int:
    """The default decode budget of a PDF of file_size bytes (see DECODE_BUDGET_MIN)."""
    return min(DECODE_BUDGET_MAX, max(DECODE_BUDGET_MIN, DECODE_BUDGET_PER_FILE_BYTE * file_size))


class DecodeBudget:
    """Counts the bytes pypdf decodes from streams while it is installed (counting()).
    observer, when set, is called with every stream that was decoded."""

    def __init__(self, limit: int) -> None:
        self.limit = limit
        self.used = 0
        self.exceeded = False
        self.observer: Optional[Callable[[Any], None]] = None

    def counting(self):
        """A context manager that routes pypdf's stream decoding through the budget.

        Every decode goes through pypdf.filters.decode_stream_data (EncodedStreamObject.get_data
        imports it at call time). Over the budget the decode raises DecodeBudgetExceeded and so
        does every later one: pypdf swallows some exceptions (a broken form XObject is skipped
        with a warning), so the caller also calls check() after each step."""
        import pypdf.filters as filters

        budget = self
        original = filters.decode_stream_data

        def decode_stream_data(stream):
            if budget.exceeded:
                raise DecodeBudgetExceeded(budget.limit)
            data = original(stream)
            budget.used += len(data)
            if budget.observer is not None:
                budget.observer(stream)
            if budget.used > budget.limit:
                budget.exceeded = True
                raise DecodeBudgetExceeded(budget.limit)
            return data

        class _Installed:
            def __enter__(self):
                filters.decode_stream_data = decode_stream_data
                return budget

            def __exit__(self, *exc):
                filters.decode_stream_data = original
                return False

        return _Installed()

    def check(self) -> None:
        if self.exceeded:
            raise DecodeBudgetExceeded(self.limit)


# Streams holding at least this many bytes (raw + decoded) are dropped from pypdf's cache after
# each page; smaller ones (font CMaps and programs, shared by every page) stay cached, so they are
# neither read nor decoded (nor counted against the budget) again for every page. What stays is
# bounded: decoded bytes by the budget (each stream is counted when it is decoded), raw bytes by
# the file size.
EVICT_MIN_BYTES = 256 * 1024


def _stream_bytes(obj) -> int:
    """Raw plus cached decoded bytes of a pypdf stream object (pypdf 6.21.2's attributes)."""
    n = len(getattr(obj, "_data", None) or b"")
    decoded = getattr(obj, "decoded_self", None)
    if decoded is not None:
        n += len(getattr(decoded, "_data", None) or b"")
    return n


class _StreamEvictor:
    """Drops the large stream objects pypdf read or decoded since the last release() from its
    object cache.

    PdfReader.resolved_objects keeps every object it resolves; a stream object holds its raw
    data and, once decoded, the decoded data (EncodedStreamObject.decoded_self). Pages (copies
    in reader.flattened_pages) and resources refer to streams through indirect references, so an
    evicted stream is simply read again from the file when a later page needs it. Non-stream
    objects (dictionaries, arrays: the document structure) and streams under keep_below bytes
    stay cached."""

    def __init__(self, reader, keep_below: int = EVICT_MIN_BYTES) -> None:
        from pypdf.generic import StreamObject

        self._reader = reader
        self._keep_below = keep_below
        self._keys: List[Any] = []
        # streams cached while the reader was being opened (an xref stream)
        for key, obj in reader.resolved_objects.items():
            if isinstance(obj, StreamObject):
                self._keys.append(key)
        cache = reader.cache_indirect_object
        keys = self._keys

        def cache_indirect_object(generation, idnum, obj):
            result = cache(generation, idnum, obj)
            if isinstance(obj, StreamObject):
                keys.append((generation, idnum))
            return result

        reader.cache_indirect_object = cache_indirect_object  # shadows the method on this reader

    def note(self, stream) -> None:
        """A cached stream was decoded (DecodeBudget.observer): look at it again at release()."""
        ref = getattr(stream, "indirect_reference", None)
        if ref is not None and getattr(ref, "pdf", None) is self._reader:
            self._keys.append((ref.generation, ref.idnum))

    def release(self) -> int:
        """Evicts the large streams cached or decoded since the last call; returns how many."""
        cache = self._reader.resolved_objects
        n = 0
        for key in self._keys:
            obj = cache.get(key)
            if obj is not None and _stream_bytes(obj) >= self._keep_below:
                del cache[key]
                n += 1
        self._keys.clear()
        return n


def apply_memory_limit(limit: int, platform: str = sys.platform) -> str:
    """Enforces a memory limit of limit bytes on this process; returns how.

    - Windows: the Job Object Go starts this process in (suspended until it is in the job)
      caps its committed memory; an allocation over the cap fails (MemoryError), nothing to do
      here.
    - macOS: xnu does not enforce RLIMIT_AS (nor RLIMIT_RSS), so a daemon thread polls the peak
      resident size (ru_maxrss) every RSS_POLL_SECONDS and ends the process with the
      memory_limit error once it is over the limit: a soft limit (it can overshoot by what is
      allocated within one poll; a single decode is at most pypdf's 75 MB per stream).
    - Other POSIX systems (Linux): RLIMIT_AS (address space), so an allocation over it fails
      with MemoryError; the limit is never raised above an existing hard limit."""
    if platform == "win32":
        return "job object"
    if platform == "darwin":
        _start_rss_watchdog(limit)
        return "rss watchdog"
    import resource

    _soft, hard = resource.getrlimit(resource.RLIMIT_AS)
    if hard != resource.RLIM_INFINITY and hard < limit:
        limit = hard
    resource.setrlimit(resource.RLIMIT_AS, (limit, hard))
    return "RLIMIT_AS"


def peak_rss_bytes() -> int:
    import resource

    rss = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
    return rss if sys.platform == "darwin" else rss * 1024  # bytes on macOS, KiB elsewhere


def _start_rss_watchdog(limit: int, peak: Callable[[], int] = peak_rss_bytes,
                        interval: float = RSS_POLL_SECONDS,
                        on_exceeded: Optional[Callable[[], None]] = None,
                        stop: Optional[threading.Event] = None) -> threading.Thread:
    """Polls peak() every interval seconds until stop is set; over limit it calls on_exceeded
    (default: report the memory_limit error and end the process with exit code 2; os._exit,
    since the main thread may be deep inside pypdf)."""

    def exceeded() -> None:
        _report_input_error(MemoryLimitExceeded(limit))
        os._exit(EXIT_INPUT)

    on_exceeded = on_exceeded or exceeded
    stop = stop or threading.Event()

    def watch() -> None:
        while not stop.is_set():
            if peak() > limit:
                on_exceeded()
                return
            stop.wait(interval)

    t = threading.Thread(target=watch, name="memory-watchdog", daemon=True)
    t.start()
    return t


def check_pdf_file(path: str) -> None:
    if not os.path.isfile(path):
        raise InputError("PDF file not found: %s" % path)
    try:
        with open(path, "rb") as f:
            head = f.read(MAGIC_SEARCH_BYTES)
    except OSError as e:
        raise InputError("cannot read %s: %s" % (path, e))
    if PDF_MAGIC not in head:
        raise InputError("not a PDF file (no %%PDF- header): %s" % path)


def _open_reader(path: str, stream=None):
    """Opens path (read from stream when given) with pypdf; decrypts a PDF that only has an
    owner password."""
    from pypdf import PdfReader
    from pypdf.errors import DependencyError, FileNotDecryptedError, PyPdfError

    try:
        reader = PdfReader(stream if stream is not None else path, strict=False)
    except (PyPdfError, ValueError, KeyError, TypeError, IndexError, AttributeError,
            RecursionError, AssertionError) as e:
        raise InputError("cannot open PDF %s: %s" % (path, e))
    if reader.is_encrypted:
        try:
            ok = reader.decrypt("")
        except DependencyError as e:  # AES without the cryptography package
            raise RuntimeError("cannot decrypt PDF %s: %s" % (path, e))
        except (PyPdfError, ValueError, KeyError, TypeError, NotImplementedError) as e:
            raise InputError("cannot open encrypted PDF %s: %s" % (path, e))
        if not ok:
            raise InputError("PDF is password protected: %s" % path)
    try:
        n = len(reader.pages)
    except FileNotDecryptedError:
        raise InputError("PDF is password protected: %s" % path)
    except (PyPdfError, ValueError, KeyError, TypeError, IndexError, AttributeError,
            RecursionError, AssertionError) as e:
        raise InputError("cannot open PDF %s: %s" % (path, e))
    return reader, n


# Text drawn this far outside the page box is not visible. Beamer, for example, hides the
# not-yet-uncovered overlay items of a slide by translating them ~2000pt off the page; the previous
# extractor clipped them, pypdf's extract_text() returns them (SPEC-v3 V6 comparison).
OFF_PAGE_MARGIN = 0.10  # fraction of the page width / height
_IDENTITY = (1.0, 0.0, 0.0, 1.0, 0.0, 0.0)

# Glyph names pypdf could not map to Unicode come out literally, e.g. "/uniFB01" for the
# "fi" ligature in pdfTeX Type1 fonts (directly followed by the rest of the word). An Adobe
# glyph-list "uniXXXX" name is exactly four upper-case hex digits naming a BMP code point.
_GLYPH_NAME = re.compile(r"/uni([0-9A-F]{4})")
_BLANK_LINES = re.compile(r"\n[ \t\r\f\v]*(?=\n)")


def _glyph_char(m: "re.Match[str]") -> str:
    cp = int(m.group(1), 16)
    if 0xD800 <= cp <= 0xDFFF:  # a lone surrogate is not text
        return m.group(0)
    return chr(cp)


def page_text(page) -> str:
    """Raw text of one pypdf page: on-page fragments only, glyph names resolved, no blank lines."""
    box = page.cropbox
    x0, x1 = sorted((float(box.left), float(box.right)))
    y0, y1 = sorted((float(box.bottom), float(box.top)))
    mx, my = (x1 - x0) * OFF_PAGE_MARGIN, (y1 - y0) * OFF_PAGE_MARGIN
    parts: List[str] = []

    def visit(text, cm, tm, font_dict, font_size):
        if not text:
            return
        if not text.strip():
            parts.append(text)  # line breaks pypdf inserts between text objects
            return
        if tuple(cm) == _IDENTITY:
            # Inside a form XObject pypdf reports the identity matrix instead of the
            # transform in effect, so the position is unknown: keep the text.
            parts.append(text)
            return
        x = tm[4] * cm[0] + tm[5] * cm[2] + cm[4]
        y = tm[4] * cm[1] + tm[5] * cm[3] + cm[5]
        if x0 - mx <= x <= x1 + mx and y0 - my <= y <= y1 + my:
            parts.append(text)

    page.extract_text(visitor_text=visit)
    text = _GLYPH_NAME.sub(_glyph_char, "".join(parts))
    return _BLANK_LINES.sub("", text)


def extract_pages(path: str, emit: Callable[[str], None] = emit_stdout,
                  budget: Optional[int] = None) -> List[str]:
    """The text of every page of path. budget: the decode budget in bytes (None = the default
    for the file's size, decode_budget())."""
    check_pdf_file(path)
    if budget is None:
        budget = decode_budget(os.path.getsize(path))
    counter = DecodeBudget(budget)
    # The file is read through a handle, not loaded whole into memory (PdfReader(path) does).
    with open(path, "rb") as stream, counter.counting():
        try:
            reader, n = _open_reader(path, stream)
        except InputError:
            counter.check()  # the budget ran out while opening: report that instead
            raise
        counter.check()
        if n == 0:
            # Never hand back an empty result silently (SPEC §0.1 "失败响亮").
            raise InputError("PDF has no pages (damaged file?): %s" % path)
        evictor = _StreamEvictor(reader)
        counter.observer = evictor.note
        evictor.release()
        emit("pdf %s: %d pages" % (os.path.basename(path), n))
        emit("STATUS pages %d" % n)
        pages: List[str] = []
        last = None
        for i in range(n):
            try:
                text = page_text(reader.pages[i])
            except (InputError, MemoryError):
                raise
            except Exception as e:  # one broken page must not lose the whole document
                emit("page %d: text extraction failed (%s: %s); kept empty"
                     % (i + 1, type(e).__name__, e))
                text = ""
            counter.check()  # a refused decode that pypdf swallowed still stops the run
            evictor.release()  # this page's large streams: raw and decoded data
            pages.append(normalize_page_text(text))
            line = "PROGRESS %.2f" % ((i + 1) / n)
            if line != last:
                emit(line)
                last = line
    emit("decoded %d bytes of stream data (budget %d)" % (counter.used, budget))
    empty = count_empty(pages)
    if empty:
        emit("%d of %d pages have no extractable text" % (empty, n))
    return pages


def count_empty(pages: Sequence[str]) -> int:
    return sum(1 for p in pages if not p)


def write_result(path: str, result: dict) -> None:
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        json.dump(result, f, ensure_ascii=False)
        f.flush()
        os.fsync(f.fileno())


# The running job's memory limit, for reporting a MemoryError (main).
_MEMORY_LIMIT: List[Optional[int]] = [None]


def run(in_path: str, out_path: str, emit: Callable[[str], None] = emit_stdout,
        apply_limits: bool = False) -> None:
    """Runs the job in in_path. apply_limits: enforce job.memory_limit_bytes on this process
    (the command line only: in-process callers such as tests keep their own limits)."""
    job = load_job(in_path)
    _MEMORY_LIMIT[0] = job.memory_limit
    if apply_limits and job.memory_limit:
        try:
            how = apply_memory_limit(job.memory_limit)
        except (OSError, ValueError, RuntimeError) as e:  # best effort: the budget still applies
            how = "not applied: %s" % e
        emit("memory limit %d bytes (%s)" % (job.memory_limit, how))
    pages = extract_pages(job.pdf, emit, job.decode_budget)
    write_result(out_path, {"pages": pages, "empty_pages": count_empty(pages)})


class _ArgParser(argparse.ArgumentParser):
    def error(self, message):
        self.print_usage(sys.stderr)
        raise InputError(message)


def build_parser() -> argparse.ArgumentParser:
    p = _ArgParser(prog="extract_pdf.py", description="LectureFlow PDF text extraction worker")
    p.add_argument("--in", dest="in_path", required=True, help="job.json")
    p.add_argument("--out", dest="out_path", required=True, help="result file to write")
    return p


def _fail(prefix: str, err) -> None:
    sys.stderr.write("ERROR: %s%s\n" % (prefix, err))
    sys.stderr.flush()


def _report_input_error(e: InputError) -> None:
    if e.code:
        emit_stdout("ERROR_CODE %s %s" % (e.code, json.dumps(e.params, sort_keys=True)))
    _fail("input error: ", e)


def main(argv: Optional[Sequence[str]] = None, apply_limits: bool = False) -> int:
    _MEMORY_LIMIT[0] = None
    try:
        args = build_parser().parse_args(argv)
        run(args.in_path, args.out_path, apply_limits=apply_limits)
        return EXIT_OK
    except InputError as e:
        _report_input_error(e)
        return EXIT_INPUT
    except SystemExit:
        raise
    except KeyboardInterrupt:
        _fail("", "interrupted")
        return EXIT_RETRY
    except MemoryError:
        limit = _MEMORY_LIMIT[0]
        if limit:  # over the caller's limit (Job Object, RLIMIT_AS): the input's fault
            _report_input_error(MemoryLimitExceeded(limit))
            return EXIT_INPUT
        _fail("", "MemoryError: out of memory")
        return EXIT_RETRY
    except Exception as e:  # every other failure is retryable (SPEC §6.0)
        traceback.print_exc(file=sys.stderr)
        _fail("", "%s: %s" % (type(e).__name__, e))
        return EXIT_RETRY


def _setup_stdio() -> None:
    for stream in (sys.stdout, sys.stderr):
        try:
            stream.reconfigure(encoding="utf-8", errors="replace", line_buffering=True)
        except (AttributeError, ValueError):
            pass


if __name__ == "__main__":
    _setup_stdio()
    sys.exit(main(apply_limits=True))
