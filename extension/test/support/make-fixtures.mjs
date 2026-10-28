// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Writes the committed fixtures from the synthetic pages in synth.mjs:
//   node test/support/make-fixtures.mjs
// Each fixture is a redacted report, exactly what the extension exports;
// test/unit/fixtures.test.mjs replays them.

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { reportFromSynthetic } from './reports.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');

for (const [dir, name, kind] of [
  ['panopto', 'viewer-mse.report.json', 'panopto'],
  ['generic', 'files-hls-dash.report.json', 'generic'],
]) {
  const { report, findings } = await reportFromSynthetic(kind);
  if (findings.length) throw new Error(`${name}: ${JSON.stringify(findings)}`);
  const out = path.join(root, 'fixtures', dir, name);
  fs.mkdirSync(path.dirname(out), { recursive: true });
  fs.writeFileSync(out, JSON.stringify(report, null, 2) + '\n');
  console.log(`wrote ${path.relative(root, out)}`);
}
