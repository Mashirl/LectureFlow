# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""Generates wer_golden.json: the WER of self-composed cases measured exactly
like the 3a benchmark (spikes/openvino/common.py normalize + jiwer 4.0.0),
for internal/devices TestWERGolden (SPEC-v2 I9, phase 3b task D).

The texts are written for this test (no lecture text). Run from the
repository root with the .venv-ov interpreter (jiwer is installed there):

    .venv-ov\\Scripts\\python.exe internal\\devices\\testdata\\wer_golden.py

It rewrites wer_golden.json next to this file (LF line endings).
"""
from __future__ import annotations

import json
import os
import sys

import jiwer

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", "..", ".."))
sys.path.insert(0, os.path.join(ROOT, "spikes", "openvino"))
from common import normalize, word_errors  # noqa: E402

PARAGRAPH = (
    "Okay, let's get started. Today we look at how a set-associative cache "
    "finds a line: the address is split into a tag, an index and an offset. "
    "The index selects one set; the tag tells us whether it's a hit. On a miss "
    "we fetch the whole 64-byte line from memory, and with a write-back policy "
    "a dirty line is only written out when it is evicted."
)
PARAGRAPH_HYP = (
    "okay let's get started today we look at how a set associative cash "
    "finds the line the address is split into a tag an index and offset "
    "the index selects one set the tag tells us whether its a hit on a miss "
    "we fetch the whole sixty four byte line from memory and with a right back policy "
    "a dirty line is only written out when it is evicted evicted"
)

CASES = [
    ("identical", "Today we talk about caches.", "Today we talk about caches."),
    ("case_and_punctuation", "Today, we talk about CACHES!", "today we talk about caches"),
    ("one_substitution", "Today we talk about the cache hierarchy.", "today we talk about the cash hierarchy"),
    ("apostrophes", "It's a write-back cache, isn’t it?", "its a write back cache isnt it"),
    ("apostrophe_kept_word", "the student's notes", "the students notes"),
    ("deletion", "a two way set associative cache halves the number of sets",
     "a two way set associative cache halves the sets"),
    ("insertion_repeat", "so the first question is how we find a line",
     "so the the first question is how we find a line"),
    ("empty_hypothesis", "nothing was recognised here", ""),
    ("numbers", "Each line is 64 bytes on x86 machines.", "each line is sixty four bytes on x 86 machines"),
    ("digits_glued", "Each line is 64 bytes on x86 machines.", "each line is 64bytes on x86 machines"),
    ("non_ascii_letters", "The Vigenère cipher uses a keyword.", "the vigenere cipher uses a key word"),
    ("non_ascii_same", "The Vigenère cipher.", "the Vigenère cipher"),
    ("dashes_quotes", "“Write-through” — or write‑back?", "write through or write back"),
    ("slash_and_symbols", "I/O-bound code: 3.5% of time (roughly).", "i o bound code 3 5 of time roughly"),
    ("whitespace", "  tag\tindex \n offset  ", "tag index offset"),
    ("hypothesis_longer", "hit", "hit or miss or maybe a hit again"),
    ("all_different", "the cache line is valid", "a page table entry is dirty"),
    ("reordered", "tag index offset", "offset index tag"),
    ("paragraph", PARAGRAPH, PARAGRAPH_HYP),
    ("paragraph_identical", PARAGRAPH, PARAGRAPH.upper()),
]


def main() -> None:
    out = []
    for name, ref, hyp in CASES:
        r, h = normalize(ref), normalize(hyp)
        if not r:
            raise SystemExit("case %s: empty reference" % name)
        w = jiwer.wer(r, h)
        ours = word_errors(ref, hyp)
        if round(w, 4) != ours["wer"]:
            raise SystemExit("case %s: jiwer %.6f != common.word_errors %.4f" % (name, w, ours["wer"]))
        out.append({"name": name, "ref": ref, "hyp": hyp, "wer": w, "ref_words": len(r.split())})
    path = os.path.join(HERE, "wer_golden.json")
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        json.dump({"generator": "internal/devices/testdata/wer_golden.py",
                   "jiwer": getattr(jiwer, "__version__", "4.0.0"), "cases": out},
                  f, ensure_ascii=False, indent=1)
        f.write("\n")
    print("wrote %d cases to %s" % (len(out), path))


if __name__ == "__main__":
    main()
