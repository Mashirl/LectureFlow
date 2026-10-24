# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
# Additional permission under AGPL-3.0 section 7: see LICENSE-EXCEPTION.md
"""LectureFlow PDF text extraction worker (SPEC §6.0, §6.0.1 material_extract; SPEC-v3 V6).

Usage:
    python worker/extract_pdf.py --in job.json --out result.json.tmp

job.json:    {"pdf": "path/to/file.pdf"}
result.json: {"pages": ["page 1 text", "", ...], "empty_pages": 1}
             pages: one entry per page, in order; empty_pages: how many entries are ""

Text comes from pypdf (BSD-3-Clause, SPEC-v3 V6) page.extract_text(), Unicode NFKC-normalised
and stripped. Pages without a text layer (e.g. scanned images) yield "" so page numbers stay
aligned (no OCR, §0). A PDF encrypted with an empty user password (owner restrictions only)
is opened like any other; one that needs a password to open is an input error.

Protocol (SPEC §6.0): exit code 0 = success, 2 = input error (missing file, not a PDF,
password protected, bad job.json), anything else = retryable. stdout carries
"PROGRESS 0.42" lines and log lines; errors go to stderr, last line "ERROR: <reason>".
Only the --out path is written; no subprocesses.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
import traceback
import unicodedata
from typing import Callable, List, Optional, Sequence

EXIT_OK = 0
EXIT_RETRY = 1
EXIT_INPUT = 2

PDF_MAGIC = b"%PDF-"
MAGIC_SEARCH_BYTES = 1024  # the PDF header may be preceded by up to 1 KB of junk


class InputError(Exception):
    """Invalid job input; mapped to exit code 2 (not retried)."""


def emit_stdout(line: str) -> None:
    sys.stdout.write(line + "\n")
    sys.stdout.flush()


def normalize_page_text(text: str) -> str:
    return unicodedata.normalize("NFKC", text or "").strip()


def load_job(path: str) -> str:
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
    return pdf


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


def _open_reader(path: str):
    """Opens path with pypdf; decrypts a PDF that only has an owner password."""
    from pypdf import PdfReader
    from pypdf.errors import DependencyError, FileNotDecryptedError, PyPdfError

    try:
        reader = PdfReader(path, strict=False)
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


def extract_pages(path: str, emit: Callable[[str], None] = emit_stdout) -> List[str]:
    check_pdf_file(path)
    reader, n = _open_reader(path)
    if n == 0:
        # Never hand back an empty result silently (SPEC §0.1 "失败响亮").
        raise InputError("PDF has no pages (damaged file?): %s" % path)
    emit("pdf %s: %d pages" % (os.path.basename(path), n))
    pages: List[str] = []
    last = None
    for i in range(n):
        try:
            text = page_text(reader.pages[i])
        except Exception as e:  # one broken page must not lose the whole document
            emit("page %d: text extraction failed (%s: %s); kept empty"
                 % (i + 1, type(e).__name__, e))
            text = ""
        pages.append(normalize_page_text(text))
        line = "PROGRESS %.2f" % ((i + 1) / n)
        if line != last:
            emit(line)
            last = line
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


def run(in_path: str, out_path: str, emit: Callable[[str], None] = emit_stdout) -> None:
    pdf = load_job(in_path)
    pages = extract_pages(pdf, emit)
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


def main(argv: Optional[Sequence[str]] = None) -> int:
    try:
        args = build_parser().parse_args(argv)
        run(args.in_path, args.out_path)
        return EXIT_OK
    except InputError as e:
        _fail("input error: ", e)
        return EXIT_INPUT
    except SystemExit:
        raise
    except KeyboardInterrupt:
        _fail("", "interrupted")
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
    sys.exit(main())
