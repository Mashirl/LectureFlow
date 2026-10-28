// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The "nothing landed on disk" check (SPEC-ext 凭据处理, M2 验收): every file
// under the given roots (the database and its WAL included) is searched for
// each needle as UTF-8 and as UTF-16LE. Hits name the file and the needle's
// label, never the value.

import fs from 'node:fs';
import path from 'node:path';

function* files(root) {
  let st;
  try {
    st = fs.lstatSync(root);
  } catch {
    return;
  }
  if (st.isFile()) {
    yield root;
    return;
  }
  if (!st.isDirectory()) return;
  for (const e of fs.readdirSync(root, { withFileTypes: true })) yield* files(path.join(root, e.name));
}

/** needles: { label: value }. Returns { files, bytes, hits: [{ file, label, encoding }] }. */
export function scan(roots, needles) {
  const pats = [];
  for (const [label, value] of Object.entries(needles)) {
    if (!value || value.length < 8) throw new Error(`needle ${label} is too short to search for`);
    pats.push({ label, encoding: 'utf8', bytes: Buffer.from(value, 'utf8') });
    pats.push({ label, encoding: 'utf16le', bytes: Buffer.from(value, 'utf16le') });
  }
  const out = { files: 0, bytes: 0, hits: [], unreadable: [] };
  for (const root of roots) {
    for (const f of files(root)) {
      let b;
      try {
        b = fs.readFileSync(f);
      } catch (e) {
        out.unreadable.push(`${f}: ${e.code || e.message}`);
        continue;
      }
      out.files++;
      out.bytes += b.length;
      for (const p of pats) if (b.includes(p.bytes)) out.hits.push({ file: f, label: p.label, encoding: p.encoding });
    }
  }
  return out;
}
