// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// "对这个平台不再提醒" (user decision 2026-10-27): what is stored (a platform
// id and a time, nothing else), generic detection never remembered, undo.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { RESTRICTED_KEY, rememberable, rememberedMap, isRemembered, withRemembered, withoutRemembered, rememberedList } from '../../src/lib/remember.js';

const NOW = new Date('2026-10-27T03:50:00Z');

test('only a profile’s id can be remembered, never generic detection', () => {
  assert.equal(RESTRICTED_KEY, 'restrictedRemembered');
  assert.equal(rememberable('panopto'), true);
  assert.equal(rememberable('echo-360'), true);
  for (const bad of ['generic', '', 'p', 'Panopto', 'pan opto', '9panopto', 'x'.repeat(41), null, undefined, 42, {}]) assert.equal(rememberable(bad), false, String(bad));
});

test('remembering stores the platform id and the time only', () => {
  const map = withRemembered(undefined, 'panopto', NOW);
  assert.deepEqual(map, { panopto: '2026-10-27T03:50:00.000Z' });
  assert.equal(isRemembered(map, 'panopto'), true);
  assert.equal(isRemembered(map, 'echo360'), false);
  // generic detection or a bad id: unchanged
  assert.deepEqual(withRemembered(map, 'generic', NOW), map);
  assert.deepEqual(withRemembered(map, 'Not An Id', NOW), map);
  assert.equal(isRemembered(map, 'generic'), false);
  // again: the time moves, nothing else is added
  const later = withRemembered(map, 'panopto', new Date('2026-11-01T00:00:00Z'));
  assert.deepEqual(later, { panopto: '2026-11-01T00:00:00.000Z' });
  assert.deepEqual(withRemembered(later, 'echo360', NOW), { panopto: '2026-11-01T00:00:00.000Z', echo360: '2026-10-27T03:50:00.000Z' });
});

test('a damaged stored value is cleaned, never trusted', () => {
  assert.deepEqual(rememberedMap(null), {});
  assert.deepEqual(rememberedMap(['panopto']), {});
  assert.deepEqual(rememberedMap('panopto'), {});
  assert.deepEqual(rememberedMap({ panopto: 'yesterday', generic: NOW.toISOString(), echo360: NOW.toISOString(), x: 1, course: { name: 'CSC1003' } }), { echo360: NOW.toISOString() });
  assert.equal(isRemembered({ panopto: true }, 'panopto'), false);
  assert.equal(isRemembered(Object.create({ panopto: NOW.toISOString() }), 'panopto'), false, 'own entries only');
});

test('undo: the platform is reminded every time again', () => {
  const map = withRemembered(withRemembered({}, 'panopto', NOW), 'echo360', NOW);
  assert.deepEqual(withoutRemembered(map, 'panopto'), { echo360: NOW.toISOString() });
  assert.deepEqual(withoutRemembered(withoutRemembered(map, 'panopto'), 'echo360'), {});
  assert.deepEqual(withoutRemembered(map, 'nothing-here'), map);
  assert.equal(isRemembered(withoutRemembered(map, 'panopto'), 'panopto'), false);
});

test('the settings list: profile names, newest first, the id when the profile is gone', () => {
  const map = { panopto: '2026-10-27T03:50:00.000Z', echo360: '2026-10-28T09:00:00.000Z', gone: '2026-10-01T00:00:00.000Z' };
  assert.deepEqual(rememberedList(map, { panopto: 'Panopto', echo360: 'Echo360' }), [
    { id: 'echo360', name: 'Echo360', at: '2026-10-28T09:00:00.000Z' },
    { id: 'panopto', name: 'Panopto', at: '2026-10-27T03:50:00.000Z' },
    { id: 'gone', name: 'gone', at: '2026-10-01T00:00:00.000Z' },
  ]);
  assert.deepEqual(rememberedList(undefined), []);
});
