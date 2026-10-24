# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""Merge web/i18n/_parts/<task>.<locale>.json into web/i18n/<locale>.json (lead tool, v3).

Each new key goes after the last existing key with the longest common dotted
prefix, so related keys stay together. An existing key with a different value
is overwritten and reported. Merged part files are deleted unless --keep.

Usage: python scripts/i18n/merge_parts.py [--keep] TASK [TASK ...]
       (only the parts of the named tasks are merged; "--all" merges every part)
"""
import glob
import json
import os
import sys

ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "web", "i18n")


def prefix_len(a, b):
    n = 0
    for x, y in zip(a.split("."), b.split(".")):
        if x != y:
            break
        n += 1
    return n


def merge(target, new):
    items = list(target.items())
    for k, v in new.items():
        if k in target:
            if target[k] != v:
                print("  overwrite %s: %r -> %r" % (k, target[k], v))
                items = [(kk, v if kk == k else vv) for kk, vv in items]
                target[k] = v
            continue
        best, at = 0, len(items)
        for i, (kk, _) in enumerate(items):
            n = prefix_len(k, kk)
            if n >= best and n > 0:
                best, at = n, i + 1
        items.insert(at, (k, v))
        target[k] = v
    return dict(items)


def dump(d):
    """The catalog layout: one key per line, plural objects inline, _meta indented."""
    lines = []
    for k, v in d.items():
        if k == "_meta":
            body = json.dumps(v, ensure_ascii=False, indent=2).replace("\n", "\n  ")
        else:
            body = json.dumps(v, ensure_ascii=False, separators=(", ", ": "))
        lines.append("  %s: %s" % (json.dumps(k, ensure_ascii=False), body))
    return "{\n" + ",\n".join(lines) + "\n}\n"


def main():
    sys.stdout.reconfigure(encoding="utf-8")
    keep = "--keep" in sys.argv
    tasks = [a for a in sys.argv[1:] if not a.startswith("--")]
    if not tasks and "--all" not in sys.argv:
        sys.exit("name the tasks to merge (or --all): parts of tasks still in progress must stay")
    by_locale = {}
    for p in sorted(glob.glob(os.path.join(ROOT, "_parts", "*.json"))):
        name = os.path.basename(p)[:-5]
        task, locale = name.rsplit(".", 1)
        if tasks and task not in tasks:
            continue
        by_locale.setdefault(locale, []).append(p)
    for locale, files in sorted(by_locale.items()):
        tp = os.path.join(ROOT, locale + ".json")
        if not os.path.exists(tp):
            print("skip %s: no %s" % (files, tp))
            continue
        with open(tp, encoding="utf-8") as f:
            target = json.load(f)
        for p in files:
            with open(p, encoding="utf-8") as f:
                new = json.load(f)
            print("%s <- %s (%d keys)" % (locale, os.path.basename(p), len(new)))
            target = merge(target, new)
        with open(tp, "w", encoding="utf-8", newline="\n") as f:
            f.write(dump(target))
        if not keep:
            for p in files:
                os.remove(p)


if __name__ == "__main__":
    main()
