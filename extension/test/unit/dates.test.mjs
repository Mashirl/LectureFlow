// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// A recording time does not depend on the computer's time zone (tz-fix,
// Mac acceptance of 3.2.0): the same page sent from computers in different
// zones gives the same lecture.recordedAt, the wall time the page states,
// which is also what LectureFlow's file-name rules read from a downloaded
// file's name. The zone is switched with process.env.TZ (Node re-reads it
// on assignment); node --test runs this file in its own process.

import { test, after } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { normalizeRecordedAt } from '../../src/lib/dates.js';
import { lectureFor, readResults } from '../../src/lib/profile.js';
import { buildEnqueue } from '../../src/lib/send.js';
import * as S from '../support/synth.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const panopto = JSON.parse(fs.readFileSync(path.join(root, 'profiles', 'panopto.json'), 'utf8'));

// Zones on both sides of the date line and of the universities' zones:
// a student abroad (Los Angeles for a UK or Australian course), the
// university's own zone, and the extremes (UTC−11, UTC+14).
const ZONES = ['America/Los_Angeles', 'Europe/London', 'UTC', 'Asia/Shanghai', 'Australia/Sydney', 'Pacific/Kiritimati', 'Pacific/Pago_Pago'];

const savedTZ = process.env.TZ;
after(() => {
  if (savedTZ === undefined) delete process.env.TZ;
  else process.env.TZ = savedTZ;
});

/** Runs fn in each zone; returns {zone: result}. */
function inEachZone(fn) {
  const out = {};
  for (const zone of ZONES) {
    process.env.TZ = zone;
    out[zone] = fn(zone);
  }
  return out;
}

test('the zone switch takes effect (otherwise the tests below would prove nothing)', () => {
  const instant = Date.UTC(2026, 9, 19, 21, 0); // Mon 19 Oct 2026 21:00 UTC
  const seen = inEachZone(() => {
    const d = new Date(instant);
    return `${d.getDate()} ${d.getHours()}`;
  });
  assert.equal(seen['America/Los_Angeles'], '19 14');
  assert.equal(seen['Europe/London'], '19 22');
  assert.equal(seen['Australia/Sydney'], '20 8');
  assert.equal(seen['Pacific/Kiritimati'], '20 11');
  assert.equal(seen['Pacific/Pago_Pago'], '19 10');
});

// page text → the wall time it states, in every zone
const CASES = [
  // Panopto's automatic session names: the lecture theatre's clock
  ['CSC1003/L01/01 Wed Oct 21 09:35:00 BST 2026', '2026-10-21T09:35'],
  ['Wed Oct 21 09:35:00 BST 2026', '2026-10-21T09:35'],
  // a Monday 08:00 lecture in Sydney: 14:00 on Sunday in Los Angeles, the
  // previous teaching week if it were converted
  ['Mon Oct 19 08:00:00 AEDT 2026', '2026-10-19T08:00'],
  // late evening in the UK: the next day in Asia and Australia
  ['Fri Oct 23 23:30:00 BST 2026', '2026-10-23T23:30'],
  // just after midnight in the UK: the day before in the Americas
  ['Mon Oct 26 00:15:00 GMT 2026', '2026-10-26T00:15'],
  ['Oct 21 23:35:00 GMT-5 2026', '2026-10-21T23:35'],
  ['Wed Oct 21 2026 09:35:00 GMT+0100 (British Summer Time)', '2026-10-21T09:35'],
  ['Oct 21, 2026 9:35 PM', '2026-10-21T21:35'],
  ['2026-10-21T09:35:00+01:00', '2026-10-21T09:35'],
  ['2026-10-19T08:00:00+11:00', '2026-10-19T08:00'],
  ['2026-10-23T23:30:00Z', '2026-10-23T23:30'],
  ['2026-10-21 09:35', '2026-10-21T09:35'],
  ['2026-10-21', '2026-10-21'],
];

test('normalizeRecordedAt: the same page time gives the same result in every zone', () => {
  for (const [raw, want] of CASES) {
    const got = inEachZone(() => normalizeRecordedAt(raw));
    for (const zone of ZONES) assert.equal(got[zone], want, `${raw} in ${zone}`);
  }
});

test('Panopto profile → enqueue: the recording time sent is the title’s, in every zone', () => {
  const m = { profile: panopto, url: S.panoptoPageUrl };
  const fromTitle = (title) => ({ 'lecture.title.0': { count: 1, value: title }, 'lecture.recordedAt.0': { count: 1, value: title } });
  const sent = inEachZone(() => {
    const viewer = lectureFor(m, readResults(S.panoptoFrames()), 3300);
    const sydney = lectureFor(m, fromTitle('CSC1003/L01/04 Mon Oct 19 08:00:00 AEDT 2026'), 3300);
    const result = {
      status: 'ok', pageUrl: S.panoptoPageUrl, profile: { id: 'panopto', version: panopto.version, reviewed: true },
      request: { sendReferer: true, cookieUrls: [], maxConnectionsPerFile: 4 }, sessionId: 'abc', lecture: sydney,
      downloadPolicy: 'enabled', tracks: [], named: { tracks: [], others: [] },
    };
    const e = buildEnqueue({ result, ids: [], courseId: '01JCOURSE', cookies: [], overrideDisabled: false, msg: (k) => k });
    return [viewer.recordedAt, sydney.recordedAt, e.lecture.recordedAt];
  });
  for (const zone of ZONES) {
    assert.deepEqual(sent[zone], ['2026-10-21T09:35', '2026-10-19T08:00', '2026-10-19T08:00'], zone);
  }
});

test('dates.js reads no local-time clock', () => {
  const src = fs.readFileSync(path.join(root, 'src', 'lib', 'dates.js'), 'utf8');
  assert.doesNotMatch(src, /\.get(FullYear|Month|Date|Day|Hours|Minutes|Seconds|TimezoneOffset)\(|toLocale|Intl\.DateTimeFormat|new Date\(\)/);
});
