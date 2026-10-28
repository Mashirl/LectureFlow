// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Fixture replay (SPEC-ext 贡献与合并 step 4): every report under
// fixtures/<id>/ must be a valid, clean report whose inputs reproduce its
// result, and, when profiles/<id>.json exists, whose tracks that profile
// names as the report says. This is the check the profile CI runs (M3).

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { validate } from '../../src/lib/jsonschema.js';
import { scanReport } from '../../src/lib/report.js';
import { replayReport, compareReplay, compareProfile } from '../../src/lib/replay.js';
import { reportFromSynthetic } from '../support/reports.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const schema = JSON.parse(fs.readFileSync(path.join(root, 'schema', 'report.schema.json'), 'utf8'));
const dirs = fs.readdirSync(path.join(root, 'fixtures'), { withFileTypes: true }).filter((d) => d.isDirectory()).map((d) => d.name);

test('there are fixtures to replay', () => {
  assert.ok(dirs.includes('panopto') && dirs.includes('generic'));
});

for (const dir of dirs) {
  const files = fs.readdirSync(path.join(root, 'fixtures', dir)).filter((f) => f.endsWith('.json'));
  const profilePath = path.join(root, 'profiles', `${dir}.json`);
  const profile = fs.existsSync(profilePath) ? JSON.parse(fs.readFileSync(profilePath, 'utf8')) : null;
  for (const f of files) {
    test(`fixture ${dir}/${f}`, async () => {
      const report = JSON.parse(fs.readFileSync(path.join(root, 'fixtures', dir, f), 'utf8'));
      assert.deepEqual(validate(schema, report), []);
      assert.deepEqual(scanReport(report), []);
      const replay = await replayReport(report);
      assert.deepEqual(compareReplay(report, replay), []);
      if (profile) {
        assert.equal(report.profile?.id, profile.id, 'the fixture was made with this profile');
        assert.deepEqual(compareProfile(report, profile, replay), []);
      } else {
        assert.equal(report.profile, null);
      }
    });
  }
}

test('the committed fixtures are what the synthetic pages produce today', async () => {
  for (const [dir, name, kind] of [['panopto', 'viewer-mse.report.json', 'panopto'], ['generic', 'files-hls-dash.report.json', 'generic']]) {
    const committed = JSON.parse(fs.readFileSync(path.join(root, 'fixtures', dir, name), 'utf8'));
    const { report } = await reportFromSynthetic(kind);
    assert.deepEqual(report, committed, `${dir}/${name}: run node test/support/make-fixtures.mjs`);
  }
});
