// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');

const v = require('../../src/main/lib/validate');

const rejects = (fn, re) => assert.throws(fn, (err) => err instanceof v.ValidationError && (!re || re.test(err.message)));

test('no-argument handlers reject arguments', () => {
  assert.equal(v.validateNoArgs([]), undefined);
  assert.equal(v.validateNoArgs([undefined]), undefined);
  rejects(() => v.validateNoArgs(['x']));
  rejects(() => v.validateNoArgs([{}]));
  rejects(() => v.validateNoArgs('x'));
});

test('pickFiles: defaults and accepted options', () => {
  assert.deepEqual(v.validatePickFiles([]), { title: undefined, multiple: false, filters: [] });
  assert.deepEqual(v.validatePickFiles([null]), { title: undefined, multiple: false, filters: [] });
  assert.deepEqual(
    v.validatePickFiles([{ title: '选择录像', multiple: true, filters: [{ name: 'Video', extensions: ['MP4', 'mkv'] }, { name: 'All', extensions: ['*'] }] }]),
    { title: '选择录像', multiple: true, filters: [{ name: 'Video', extensions: ['mp4', 'mkv'] }, { name: 'All', extensions: ['*'] }] },
  );
});

test('pickFiles: rejects wrong types, unknown keys and over-long values', () => {
  rejects(() => v.validatePickFiles(['x']), /object/);
  rejects(() => v.validatePickFiles([[]]), /object/);
  rejects(() => v.validatePickFiles([{ defaultPath: 'C:\\' }]), /unknown key/);
  rejects(() => v.validatePickFiles([{ properties: ['openDirectory'] }]), /unknown key/);
  rejects(() => v.validatePickFiles([{ title: 1 }]), /string/);
  rejects(() => v.validatePickFiles([{ title: '' }]), /short/);
  rejects(() => v.validatePickFiles([{ title: 'x'.repeat(201) }]), /long/);
  rejects(() => v.validatePickFiles([{ multiple: 'yes' }]), /boolean/);
  rejects(() => v.validatePickFiles([{ filters: 'mp4' }]), /list/);
  rejects(() => v.validatePickFiles([{ filters: new Array(11).fill({ name: 'a', extensions: ['a'] }) }]), /at most/);
  rejects(() => v.validatePickFiles([{ filters: [{ name: 'a', extensions: [] }] }]), /non-empty/);
  rejects(() => v.validatePickFiles([{ filters: [{ name: 'a', extensions: ['../x'] }] }]), /letters/);
  rejects(() => v.validatePickFiles([{ filters: [{ name: 'a', extensions: ['m p4'] }] }]), /letters/);
  rejects(() => v.validatePickFiles([{ filters: [{ name: 'a', extensions: ['x'.repeat(17)] }] }]), /long/);
  rejects(() => v.validatePickFiles([{ filters: [{ name: 'a', extensions: ['a'], x: 1 }] }]), /unknown key/);
  rejects(() => v.validatePickFiles([{}, {}]), /at most 1/);
  rejects(() => v.validatePickFiles([Object.create({ title: 'inherited' })]), /object/);
});

test('notify: title and body strings with limits', () => {
  assert.deepEqual(v.validateNotify([{ title: '转写完成', body: 'CSC1003 第 3 讲' }]), { title: '转写完成', body: 'CSC1003 第 3 讲' });
  assert.deepEqual(v.validateNotify([{ title: 'Done' }]), { title: 'Done', body: '' });
  rejects(() => v.validateNotify([]), /title, body/);
  rejects(() => v.validateNotify(['Done']), /title, body/);
  rejects(() => v.validateNotify([{ title: 1 }]), /string/);
  rejects(() => v.validateNotify([{ title: '' }]), /short/);
  rejects(() => v.validateNotify([{ title: 'x'.repeat(201) }]), /long/);
  rejects(() => v.validateNotify([{ title: 'a', body: 'x'.repeat(1001) }]), /long/);
  rejects(() => v.validateNotify([{ title: 'a', body: 5 }]), /string/);
  rejects(() => v.validateNotify([{ title: 'a', icon: 'C:\\x.png' }]), /unknown key/);
  rejects(() => v.validateNotify([{ title: 'a\0b' }]), /NUL/);
});

test('openExternal: http, https and mailto only', () => {
  assert.equal(v.validateOpenExternal(['https://example.com/a b']), 'https://example.com/a%20b');
  assert.equal(v.validateOpenExternal(['http://example.com']), 'http://example.com/');
  assert.equal(v.validateOpenExternal(['mailto:lecturer@uni.example?subject=CSC1003']), 'mailto:lecturer@uni.example?subject=CSC1003');
  for (const bad of ['javascript:alert(1)', 'file:///C:/Windows/System32/calc.exe', 'mailto:', 'ms-settings:privacy', 'tel:123', 'sms:123',
    'smb://server/share', 'https://user:pw@example.com/', 'https://', '', 'example.com']) {
    rejects(() => v.validateOpenExternal([bad]), undefined);
  }
  rejects(() => v.validateOpenExternal([42]), /string/);
  rejects(() => v.validateOpenExternal([`https://example.com/${'a'.repeat(2048)}`]), /long/);
  rejects(() => v.validateOpenExternal(['https://example.com', 'extra']), /at most 1/);
});
