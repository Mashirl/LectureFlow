// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// "续传" starts with the choice the lecture was sent with (src/lib/sentpreset.js):
// what is kept (a hash and a choice's name), which selections count as one
// of the three choices, the newest entries only.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { PRESET_KEY, MAX_ENTRIES, presetKey, presetOf, presetMap, presetFor, withPreset } from '../../src/lib/sentpreset.js';
import { presetSelection } from '../../src/lib/send.js';

const PRESETS = { full: ['podcast'], audio: ['primary-audio'] };
const POD = { id: 'pod', url: 'https://media.example.net/s/whole.mp4', kind: 'video', format: 'file', role: 'podcast', protected: false };
const AUD = { id: 'aud', url: 'https://media.example.net/s/audio.m3u8', kind: 'audio', format: 'hls', role: 'primary-audio', protected: false };
const SC1 = { id: 'sc1', url: 'https://media.example.net/s/screen1.m3u8', kind: 'video', format: 'hls', role: 'screen', protected: false };
const SC2 = { id: 'sc2', url: 'https://media.example.net/s/screen2.m3u8', kind: 'video', format: 'hls', role: 'screen', protected: false };
const TRACKS = [POD, AUD, SC1, SC2];

test('the key: 32 hex digits of a hash of platform and session id, nothing readable', async () => {
  assert.equal(PRESET_KEY, 'sentPresets');
  const k = await presetKey('panopto', '0d7e1d3c-0000-4000-8000-000000000001');
  assert.match(k, /^[0-9a-f]{32}$/);
  assert.equal(k, await presetKey('panopto', '0d7e1d3c-0000-4000-8000-000000000001'), 'stable');
  assert.notEqual(k, await presetKey('generic', '0d7e1d3c-0000-4000-8000-000000000001'), 'per platform');
  assert.equal(await presetKey('', 'x'), '');
  assert.equal(await presetKey('panopto', ''), '');
});

test('presetOf: one of the three choices, or "" for a hand-picked selection', () => {
  assert.equal(presetOf(TRACKS, ['pod'], PRESETS), 'main');
  assert.equal(presetOf(TRACKS, ['sc2', 'aud', 'sc1'], PRESETS), 'dual', 'order does not matter');
  assert.equal(presetOf(TRACKS, ['aud'], PRESETS), 'audio');
  assert.equal(presetOf(TRACKS, ['aud', 'sc2'], PRESETS), '', 'audio + the second screen: by hand');
  assert.equal(presetOf(TRACKS, [], PRESETS), '');
  // without a full track, main is audio + the first picture
  assert.equal(presetOf([AUD, SC1, SC2], ['aud', 'sc1'], null), 'main');
});

test('remembered per lecture; "续传" gets the same tracks back', async () => {
  const k = await presetKey('panopto', 'session-a');
  const map = withPreset(undefined, k, presetOf(TRACKS, ['aud', 'sc1', 'sc2'], PRESETS));
  assert.deepEqual(map, { [k]: 'dual' });
  assert.equal(presetFor(map, k), 'dual');
  assert.deepEqual(presetSelection(TRACKS, presetFor(map, k), PRESETS), ['aud', 'sc1', 'sc2']);
  assert.equal(presetFor(map, await presetKey('panopto', 'session-b')), '', 'another lecture: nothing');
  // sent again by hand: what an earlier send remembered is dropped
  assert.deepEqual(withPreset(map, k, ''), {});
});

test('only hashes and choice names are kept, the newest MAX_ENTRIES', () => {
  assert.deepEqual(presetMap({ ['a'.repeat(32)]: 'dual', short: 'main', ['b'.repeat(32)]: 'everything', ['c'.repeat(32)]: { title: 'x' } }), { ['a'.repeat(32)]: 'dual' });
  assert.deepEqual(presetMap(['dual']), {});
  let map = {};
  const key = (i) => i.toString(16).padStart(32, 'f');
  for (let i = 0; i < MAX_ENTRIES + 5; i++) map = withPreset(map, key(i), 'main');
  assert.equal(Object.keys(map).length, MAX_ENTRIES);
  assert.equal(presetFor(map, key(0)), '', 'the oldest went first');
  assert.equal(presetFor(map, key(MAX_ENTRIES + 4)), 'main');
  // sending an old one again makes it the newest
  map = withPreset(map, key(5), 'audio');
  map = withPreset(map, key(MAX_ENTRIES + 5), 'main');
  assert.equal(presetFor(map, key(5)), 'audio');
  assert.equal(presetFor(map, key(6)), '');
});
