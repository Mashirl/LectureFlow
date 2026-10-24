# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""SPEC-v3 V6: the previous PDF library must not come back through any dependency file.

Scans worker/requirements*.txt, every lock / manifest file under packaging/components/ (when
that directory exists) and the worker's Python sources. Any line naming the library or its
legacy import name fails the test.
"""
import os
import re

import pytest

from conftest import PROJECT_DIR, WORKER_DIR

# The names are assembled so that this file itself does not match the pattern.
_LIB = "py" + "mu" + "pdf"
_LEGACY = "fi" + "tz"
FORBIDDEN = re.compile(r"(?i)%s|(?<![a-z])%s(?![a-z])" % (_LIB, _LEGACY))

# Text files that can pin Python packages for a component (pip / uv / pylock / manifests).
LOCK_SUFFIXES = (".txt", ".lock", ".toml", ".in", ".cfg", ".json", ".yaml", ".yml")
SELF = os.path.basename(__file__)


def dependency_files(project_dir):
    worker = os.path.join(project_dir, "worker")
    files = sorted(os.path.join(worker, n) for n in os.listdir(worker)
                   if n.startswith("requirements") and n.endswith(".txt"))
    comp = os.path.join(project_dir, "packaging", "components")
    if os.path.isdir(comp):
        for root, dirs, names in os.walk(comp):
            dirs.sort()
            for n in sorted(names):
                if n.lower().endswith(LOCK_SUFFIXES):
                    files.append(os.path.join(root, n))
    return files


def worker_sources(project_dir):
    worker = os.path.join(project_dir, "worker")
    return sorted(os.path.join(worker, n) for n in os.listdir(worker)
                  if n.endswith(".py") and n != SELF)


def violations(paths):
    """[(path, line number, line)] for every line that names the forbidden library."""
    found = []
    for path in paths:
        with open(path, encoding="utf-8", errors="replace") as f:
            for i, line in enumerate(f, 1):
                if FORBIDDEN.search(line):
                    found.append((path, i, line.rstrip("\n")))
    return found


def test_requirements_exist():
    names = [os.path.basename(p) for p in dependency_files(PROJECT_DIR)]
    assert "requirements.txt" in names


def test_no_forbidden_library_in_dependency_files():
    assert violations(dependency_files(PROJECT_DIR)) == []


def test_no_forbidden_library_in_worker_sources():
    assert violations(worker_sources(PROJECT_DIR)) == []


def test_requirements_pin_pypdf_exactly():
    with open(os.path.join(WORKER_DIR, "requirements.txt"), encoding="utf-8") as f:
        lines = [l.strip() for l in f]
    assert any(re.fullmatch(r"pypdf==\d+\.\d+\.\d+", l) for l in lines)


def _make_tree(root, comp_files, req_lines):
    os.makedirs(os.path.join(root, "worker"))
    with open(os.path.join(root, "worker", "requirements.txt"), "w", encoding="utf-8") as f:
        f.write("\n".join(req_lines) + "\n")
    for rel, text in comp_files.items():
        p = os.path.join(root, "packaging", "components", *rel.split("/"))
        os.makedirs(os.path.dirname(p), exist_ok=True)
        with open(p, "w", encoding="utf-8") as f:
            f.write(text)


def test_check_catches_violation_in_component_lock(tmp_path):
    root = str(tmp_path)
    _make_tree(root, {
        "python/uv.lock": '[[package]]\nname = "pypdf"\nversion = "6.21.2"\n',
        "asr-cuda/requirements.lock.txt": "faster-whisper==1.2.1 \\\n"
                                          "    --hash=sha256:" + "ab" * 32 + "\n"
                                          + _LIB.replace("mu", "Mu").replace("pdf", "PDF")
                                          + "==1.26.0\n",
        "asr-openvino/pylock.toml": 'name = "%s"\n' % _LEGACY,
        "notes.md": _LIB + " is not a lock file\n",
    }, ["pypdf==6.21.2"])
    found = violations(dependency_files(root))
    got = sorted((os.path.relpath(p, root).replace(os.sep, "/"), n) for p, n, _ in found)
    assert got == [("packaging/components/asr-cuda/requirements.lock.txt", 3),
                   ("packaging/components/asr-openvino/pylock.toml", 1)]


def test_check_catches_violation_in_worker_requirements(tmp_path):
    root = str(tmp_path)
    _make_tree(root, {}, ["pypdf==6.21.2", _LIB + "==2.*"])
    assert not os.path.isdir(os.path.join(root, "packaging"))
    found = violations(dependency_files(root))
    assert [(os.path.basename(p), n) for p, n, _ in found] == [("requirements.txt", 2)]


@pytest.mark.parametrize("line,bad", [
    ("Fitzgerald==1.0", False),  # a word that merely contains the legacy name
    ("profitz", False),
    ("import %s" % _LEGACY, True),
    ("%s-binary==2.0" % _LIB.upper(), True),
    ("pypdf==6.21.2", False),
])
def test_pattern(line, bad):
    assert bool(FORBIDDEN.search(line)) is bad
