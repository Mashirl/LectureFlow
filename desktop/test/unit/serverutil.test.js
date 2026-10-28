// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');

const u = require('../../src/main/lib/serverutil');

test('token: 32 random bytes as hex, different every time', () => {
  const a = u.generateToken();
  const b = u.generateToken();
  assert.match(a, /^[0-9a-f]{64}$/);
  assert.notEqual(a, b);
});

test('command line follows v3-brief §1 and never carries the token', () => {
  const args = u.buildServerArgs({
    port: 50001, dataDir: 'C:\\d', resourcesDir: 'C:\\r', appVersion: '3.0.0', configFile: 'C:\\d\\config.toml', locationFile: 'C:\\a\\location.json',
  });
  assert.deepEqual(args, [
    '-desktop', '-listen', '127.0.0.1:50001', '-data-dir', 'C:\\d', '-resources', 'C:\\r',
    '-app-version', '3.0.0', '-config', 'C:\\d\\config.toml', '-location-file', 'C:\\a\\location.json',
  ]);
  const noLoc = u.buildServerArgs({ port: 1, dataDir: 'd', resourcesDir: 'r', appVersion: 'v', configFile: 'c', locationFile: null });
  assert.equal(noLoc.includes('-location-file'), false, 'not passed when the data dir comes from LECTUREFLOW_DATA_DIR');
  const token = u.generateToken();
  assert.equal(args.concat(noLoc).some((a) => a.includes(token)), false);
});

test('ready line', () => {
  assert.deepEqual(u.parseReadyLine('LECTUREFLOW_READY http://127.0.0.1:50001'), { url: 'http://127.0.0.1:50001', port: 50001 });
  assert.deepEqual(u.parseReadyLine('LECTUREFLOW_READY http://127.0.0.1:50001/ \r'), { url: 'http://127.0.0.1:50001', port: 50001 });
  for (const bad of ['LECTUREFLOW_READY http://0.0.0.0:1', 'LECTUREFLOW_READY http://localhost:1', 'lectureflow started', 'LECTUREFLOW_READY http://127.0.0.1:99999', ' LECTUREFLOW_READY http://127.0.0.1:1', null]) {
    assert.equal(u.parseReadyLine(bad), null, String(bad));
  }
});

test('error reason: the last ERROR: line', () => {
  assert.equal(u.extractErrorReason(['time=… level=INFO msg=start', 'ERROR: first', 'noise', 'ERROR: open database: locked  ']), 'open database: locked');
  assert.equal(u.extractErrorReason(['panic: x']), null);
  assert.equal(u.extractErrorReason([]), null);
  assert.equal(u.extractErrorReason([`ERROR: ${'x'.repeat(1000)}`]).length, 601);
});

test('port-in-use detection (Windows and POSIX wording)', () => {
  assert.equal(u.isPortInUseError('listen tcp 127.0.0.1:50001: bind: Only one usage of each socket address (protocol/network address/port) is normally permitted.'), true);
  assert.equal(u.isPortInUseError('listen tcp 127.0.0.1:50001: bind: An attempt was made to access a socket in a way forbidden by its access permissions.'), true);
  assert.equal(u.isPortInUseError('listen tcp 127.0.0.1:50001: bind: address already in use'), true);
  assert.equal(u.isPortInUseError('open database: database is locked'), false);
  assert.equal(u.isPortInUseError(null), false);
});

test('redact', () => {
  assert.equal(u.redact('a SECRET b SECRET', 'SECRET'), 'a [token] b [token]');
  assert.equal(u.redact('abc', ''), 'abc');
});

test('server command: packaged, development, Node script', () => {
  const p = path.win32;
  assert.deepEqual(
    u.resolveServerCommand({ isPackaged: true, resourcesPath: 'C:\\App\\resources', env: { LECTUREFLOW_SERVER: 'C:\\evil.exe' }, repoRoot: 'C:\\repo', platform: 'win32', execPath: 'x', pathMod: p }),
    { command: 'C:\\App\\resources\\bin\\lectureflow.exe', prefixArgs: [], resourcesDir: 'C:\\App\\resources', extraEnv: {} },
    'packaged builds ignore the development variables',
  );
  assert.deepEqual(
    u.resolveServerCommand({ isPackaged: false, resourcesPath: 'x', env: {}, repoRoot: 'C:\\repo', platform: 'win32', execPath: 'x', pathMod: p }),
    { command: 'C:\\repo\\lectureflow.exe', prefixArgs: [], resourcesDir: 'C:\\repo', extraEnv: {} },
  );
  assert.deepEqual(
    u.resolveServerCommand({ isPackaged: false, resourcesPath: 'x', env: { LECTUREFLOW_SERVER: 'C:\\tmp\\lf.exe', LECTUREFLOW_RESOURCES: 'C:\\res' }, repoRoot: 'C:\\repo', platform: 'win32', execPath: 'x', pathMod: p }),
    { command: 'C:\\tmp\\lf.exe', prefixArgs: [], resourcesDir: 'C:\\res', extraEnv: {} },
  );
  assert.deepEqual(
    u.resolveServerCommand({ isPackaged: false, resourcesPath: 'x', env: { LECTUREFLOW_SERVER: 'C:\\t\\fake-server.js' }, repoRoot: 'C:\\repo', platform: 'win32', execPath: 'C:\\e\\electron.exe', pathMod: p }),
    { command: 'C:\\e\\electron.exe', prefixArgs: ['C:\\t\\fake-server.js'], resourcesDir: 'C:\\repo', extraEnv: { ELECTRON_RUN_AS_NODE: '1' } },
  );
  assert.deepEqual(
    u.resolveServerCommand({ isPackaged: true, resourcesPath: '/Applications/LectureFlow.app/Contents/Resources', env: {}, repoRoot: '/r', platform: 'darwin', execPath: 'x', pathMod: path.posix }),
    {
      command: '/Applications/LectureFlow.app/Contents/Resources/bin/lectureflow',
      prefixArgs: [],
      resourcesDir: '/Applications/LectureFlow.app/Contents/Resources',
      // no worker/__pycache__ inside the signed bundle (mac-accept 3.2.0)
      extraEnv: { PYTHONDONTWRITEBYTECODE: '1' },
    },
  );
  assert.deepEqual(
    u.resolveServerCommand({ isPackaged: false, resourcesPath: 'x', env: {}, repoRoot: '/r', platform: 'darwin', execPath: 'x', pathMod: path.posix }).extraEnv,
    {},
    'development runs (the repository’s worker/) keep writing bytecode',
  );
});

test('the service starts without usage reporting (NOTES.md "无遥测")', () => {
  assert.deepEqual(u.NO_TELEMETRY_ENV, {
    HF_HUB_DISABLE_TELEMETRY: '1', HF_HUB_DISABLE_IMPLICIT_TOKEN: '1', HF_XET_TELEMETRY_ENABLED: '0', DO_NOT_TRACK: '1',
  });
  assert.ok(Object.isFrozen(u.NO_TELEMETRY_ENV));
  const env = u.serviceEnv(
    { PATH: 'C:\\bin', hf_hub_disable_telemetry: '0', DO_NOT_TRACK: '0', HF_HUB_DISABLE_IMPLICIT_TOKEN: '', hf_xet_telemetry_enabled: '1' },
    { ELECTRON_RUN_AS_NODE: '1' },
  );
  assert.deepEqual(env, {
    PATH: 'C:\\bin',
    HF_HUB_DISABLE_TELEMETRY: '1',
    HF_HUB_DISABLE_IMPLICIT_TOKEN: '1',
    HF_XET_TELEMETRY_ENABLED: '0',
    DO_NOT_TRACK: '1',
    ELECTRON_RUN_AS_NODE: '1',
  }, 'the user’s values of these keys (any case) are replaced; extraEnv is kept');
  assert.deepEqual(u.serviceEnv(undefined), { ...u.NO_TELEMETRY_ENV });
});

test('the shell starts the service with serviceEnv', () => {
  const fs = require('node:fs');
  const src = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'main', 'shell.js'), 'utf8');
  assert.match(src, /env: serviceEnv\(process\.env, cmd\.extraEnv\)/);
  assert.doesNotMatch(src, /env: \{ \.\.\.process\.env, \.\.\.cmd\.extraEnv \}/);
});
