# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""extract_pdf.py (SPEC §6.0, §6.0.1 material_extract; SPEC-v3 V6 pypdf)."""
import json
import os
import subprocess
import sys

import pytest
from pypdf import PdfWriter
from pypdf.generic import (ArrayObject, DecodedStreamObject, DictionaryObject, NameObject,
                           NumberObject)

import extract_pdf
from conftest import PROJECT_DIR, WORKER_DIR

# ToUnicode CMap for a simple font: bytes "ABC" / "123" are the full-width letters / digits,
# so NFKC normalisation has something to do.
_FULLWIDTH_CMAP = b"""/CIDInit /ProcSet findresource begin
12 dict begin
begincmap
/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def
/CMapName /Adobe-Identity-UCS def
/CMapType 2 def
1 begincodespacerange
<00> <FF>
endcodespacerange
2 beginbfrange
<41> <43> <FF21>
<31> <33> <FF11>
endbfrange
endcmap
CMapName currentdict /CMap defineresource pop
end
end
"""


def _stream(w, data):
    s = DecodedStreamObject()
    s.set_data(data)
    return w._add_object(s)


def _font(w, **extra):
    d = DictionaryObject({NameObject("/Type"): NameObject("/Font"),
                          NameObject("/Subtype"): NameObject("/Type1"),
                          NameObject("/BaseFont"): NameObject("/Helvetica")})
    d.update({NameObject(k): v for k, v in extra.items()})
    return w._add_object(d)


def build_pdf(path, pages, user_password=None, owner_password=None):
    """Writes a PDF with pypdf. pages: list of content streams (bytes) that may use
    /F1 (WinAnsi Helvetica), /F2 (ABC123 -> full-width via ToUnicode) and
    /F3 (codes 1 and 2 = the glyph names /uniFB01 and /uniFB02, as pdfTeX emits ligatures)."""
    w = PdfWriter()
    fonts = DictionaryObject({
        NameObject("/F1"): _font(w, **{"/Encoding": NameObject("/WinAnsiEncoding")}),
        NameObject("/F2"): _font(w, **{"/ToUnicode": _stream(w, _FULLWIDTH_CMAP)}),
        NameObject("/F3"): _font(w, **{"/Encoding": DictionaryObject({
            NameObject("/Type"): NameObject("/Encoding"),
            NameObject("/Differences"): ArrayObject(
                [NumberObject(1), NameObject("/uniFB01"), NameObject("/uniFB02")])})}),
    })
    for content in pages:
        page = w.add_blank_page(612, 792)
        page[NameObject("/Resources")] = DictionaryObject({NameObject("/Font"): fonts})
        page[NameObject("/Contents")] = _stream(w, content)
    if user_password is not None or owner_password is not None:
        w.encrypt(user_password=user_password or "", owner_password=owner_password,
                  algorithm="AES-256")
    w.write(str(path))
    return str(path)


TEXT_PAGE = (b"BT /F1 12 Tf 72 720 Td (Cache hierarchy: L1 and L2) Tj ET\n"
             b"BT /F2 12 Tf 72 700 Td (ABC123) Tj ET\n")


def make_pdf(path, encrypt=False):
    """Two pages: text (including full-width characters) and a blank page."""
    if encrypt:
        return build_pdf(path, [TEXT_PAGE, b""], user_password="user", owner_password="owner")
    return build_pdf(path, [TEXT_PAGE, b""])


def write_job(tmp_path, data):
    p = tmp_path / "job.json"
    p.write_text(json.dumps(data), encoding="utf-8")
    return str(p), str(tmp_path / "result.json.tmp")


def run_main(capsys, *argv):
    rc = extract_pdf.main(list(argv))
    out, err = capsys.readouterr()
    return rc, out.splitlines(), err


def test_two_page_pdf(tmp_path, capsys):
    pdf = make_pdf(tmp_path / "slides.pdf")
    in_path, out_path = write_job(tmp_path, {"pdf": pdf})
    rc, lines, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 0, err
    with open(out_path, encoding="utf-8") as f:
        result = json.load(f)
    # NFKC turns the full-width letters/digits into ASCII; the image-less blank page
    # keeps its slot as an empty string.
    assert result == {"pages": ["Cache hierarchy: L1 and L2\nABC123", ""], "empty_pages": 1}
    assert [l for l in lines if l.startswith("PROGRESS")] == ["PROGRESS 0.50", "PROGRESS 1.00"]
    assert sorted(os.listdir(tmp_path)) == ["job.json", "result.json.tmp", "slides.pdf"]


def test_normalize_page_text():
    assert extract_pdf.normalize_page_text("  ﬁle  system　\n") == "file  system"
    assert extract_pdf.normalize_page_text("") == ""
    assert extract_pdf.normalize_page_text(None) == ""


def test_missing_pdf_exit_2(tmp_path, capsys):
    in_path, out_path = write_job(tmp_path, {"pdf": str(tmp_path / "nope.pdf")})
    rc, _, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 2 and "not found" in err
    assert not os.path.exists(out_path)


def test_not_a_pdf_exit_2(tmp_path, capsys):
    fake = tmp_path / "notes.pdf"
    fake.write_text("hello, this is plain text", encoding="utf-8")
    in_path, out_path = write_job(tmp_path, {"pdf": str(fake)})
    rc, _, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 2 and "not a PDF" in err
    assert not os.path.exists(out_path)


def test_damaged_pdf_exit_2(tmp_path, capsys):
    bad = tmp_path / "bad.pdf"
    bad.write_bytes(b"%PDF-1.7\n" + b"\x00garbage" * 50)
    in_path, out_path = write_job(tmp_path, {"pdf": str(bad)})
    rc, _, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 2 and "cannot open PDF" in err
    assert not os.path.exists(out_path)


def test_password_protected_exit_2(tmp_path, capsys):
    pdf = make_pdf(tmp_path / "locked.pdf", encrypt=True)
    in_path, out_path = write_job(tmp_path, {"pdf": pdf})
    rc, _, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 2 and "password" in err


@pytest.mark.parametrize("content", ["{broken", "[]", '{"pdf": ""}', '{"file": "a.pdf"}'])
def test_bad_job_exit_2(tmp_path, capsys, content):
    p = tmp_path / "job.json"
    p.write_text(content, encoding="utf-8")
    rc, _, err = run_main(capsys, "--in", str(p), "--out", str(tmp_path / "r.tmp"))
    assert rc == 2
    assert err.strip().splitlines()[-1].startswith("ERROR: input error")


def test_missing_arguments_exit_2(capsys):
    assert run_main(capsys)[0] == 2


def test_cli_process(tmp_path):
    pdf = make_pdf(tmp_path / "课件.pdf")
    in_path, out_path = write_job(tmp_path, {"pdf": pdf})
    p = subprocess.run([sys.executable, os.path.join(WORKER_DIR, "extract_pdf.py"),
                        "--in", in_path, "--out", out_path],
                       capture_output=True, cwd=PROJECT_DIR, timeout=120)
    assert p.returncode == 0, p.stderr.decode("utf-8", "replace")
    assert "PROGRESS 1.00" in p.stdout.decode("utf-8").splitlines()
    with open(out_path, encoding="utf-8") as f:
        result = json.load(f)
    assert result["pages"][1] == "" and result["empty_pages"] == 1


def test_owner_password_only_opens(tmp_path, capsys):
    # Empty user password (restrictions only): readable without a password, as before V6.
    pdf = build_pdf(tmp_path / "restricted.pdf", [TEXT_PAGE], user_password="",
                    owner_password="owner")
    in_path, out_path = write_job(tmp_path, {"pdf": pdf})
    rc, _, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 0, err
    with open(out_path, encoding="utf-8") as f:
        assert json.load(f) == {"pages": ["Cache hierarchy: L1 and L2\nABC123"], "empty_pages": 0}


def test_off_page_text_and_glyph_names(tmp_path, capsys):
    # Beamer hides not-yet-uncovered overlay items by translating them ~2000pt off the
    # page; pdfTeX ligatures arrive as the glyph names /uniFB01 and /uniFB02.
    content = (b"BT /F3 12 Tf 72 720 Td (\001le \002ow) Tj ET\n"
               b"q 1 0 0 1 2000.02579 2000.02579 cm\n"
               b"BT /F1 12 Tf 72 680 Td (Hidden overlay) Tj ET Q\n"
               b"q 1 0 0 1 10 10 cm BT /F1 12 Tf 72 640 Td (Shifted but on the page) Tj ET Q\n"
               b"BT /F1 12 Tf 72 600 Td (Visible) Tj ET\n")
    pdf = build_pdf(tmp_path / "beamer.pdf", [content, b"", b""])
    in_path, out_path = write_job(tmp_path, {"pdf": pdf})
    rc, lines, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 0, err
    with open(out_path, encoding="utf-8") as f:
        result = json.load(f)
    assert result == {"pages": ["file flow\nShifted but on the page\nVisible", "", ""],
                      "empty_pages": 2}
    assert [l for l in lines if l.startswith("PROGRESS")] == [
        "PROGRESS 0.33", "PROGRESS 0.67", "PROGRESS 1.00"]
    assert "2 of 3 pages have no extractable text" in lines


def test_glyph_name_regex_leaves_other_text_alone():
    sub = extract_pdf._GLYPH_NAME.sub
    assert sub(extract_pdf._glyph_char, "/uniFB01ngerprint") == "ﬁngerprint"
    assert sub(extract_pdf._glyph_char, "a/b and /unix and /uniD800") == "a/b and /unix and /uniD800"


def test_count_empty():
    assert extract_pdf.count_empty(["a", "", "b", ""]) == 2
    assert extract_pdf.count_empty([]) == 0
