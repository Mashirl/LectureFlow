// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The shell's side of the browser extension's Native Messaging (SPEC-ext,
// .claude/ext-brief.md §4): the launcher and portable arguments, the
// --background start, the "LECTUREFLOW_OPEN <route>" line and the NSIS
// uninstall additions.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { once } = require('node:events');

const nm = require('../../src/main/lib/nmhost');
const { buildServerArgs } = require('../../src/main/lib/serverutil');
const { ServerProcess } = require('../../src/main/server');

const DESKTOP = path.join(__dirname, '..', '..');
const FAKE = path.join(__dirname, '..', 'fixtures', 'fake-server.js');

test('isBackgroundLaunch: --background in argv or the single-instance data', () => {
  assert.equal(nm.isBackgroundLaunch(['--background']), true);
  assert.equal(nm.isBackgroundLaunch(['C:\\LF\\LectureFlow.exe', '--background']), true);
  assert.equal(nm.isBackgroundLaunch(['C:\\LF\\LectureFlow.exe']), false);
  assert.equal(nm.isBackgroundLaunch(['--background=1', '-background', '--Background']), false);
  assert.equal(nm.isBackgroundLaunch([], { background: true }), true);
  assert.equal(nm.isBackgroundLaunch(['x'], { background: false }), false);
  assert.equal(nm.isBackgroundLaunch(undefined, undefined), false);
});

test('launcherFor and isPortable', () => {
  const env = { PORTABLE_EXECUTABLE_FILE: 'D:\\Apps\\LectureFlow-3.2.0-x64-portable.exe' };
  // Installed Windows build: the app's own executable.
  assert.equal(nm.launcherFor({ isPackaged: true, platform: 'win32', execPath: 'C:\\Users\\u\\AppData\\Local\\Programs\\LectureFlow\\LectureFlow.exe' }),
    'C:\\Users\\u\\AppData\\Local\\Programs\\LectureFlow\\LectureFlow.exe');
  assert.equal(nm.isPortable({ isPackaged: true, platform: 'win32', env: {} }), false);
  // Portable: the portable exe, not the temporary unpacked copy.
  assert.equal(nm.launcherFor({ isPackaged: true, platform: 'win32', execPath: 'C:\\Temp\\2x\\LectureFlow.exe', env }), env.PORTABLE_EXECUTABLE_FILE);
  assert.equal(nm.isPortable({ isPackaged: true, platform: 'win32', env }), true);
  // macOS: the executable inside the bundle.
  assert.equal(nm.launcherFor({ isPackaged: true, platform: 'darwin', execPath: '/Applications/LectureFlow.app/Contents/MacOS/LectureFlow' }),
    '/Applications/LectureFlow.app/Contents/MacOS/LectureFlow');
  assert.equal(nm.isPortable({ isPackaged: true, platform: 'darwin', env }), false);
  // Development: the bare Electron cannot start the app with --background.
  assert.equal(nm.launcherFor({ isPackaged: false, platform: 'win32', execPath: 'electron.exe', env }), null);
  assert.equal(nm.isPortable({ isPackaged: false, platform: 'win32', env }), false);
});

test('buildServerArgs passes -launcher and -portable only when set', () => {
  const base = { port: 5, dataDir: 'd', resourcesDir: 'r', appVersion: 'v', configFile: 'c', locationFile: null };
  assert.deepEqual(buildServerArgs(base), ['-desktop', '-listen', '127.0.0.1:5', '-data-dir', 'd', '-resources', 'r', '-app-version', 'v', '-config', 'c']);
  const args = buildServerArgs({ ...base, launcher: 'C:\\LF\\LectureFlow.exe', portable: true });
  assert.deepEqual(args.slice(-3), ['-launcher', 'C:\\LF\\LectureFlow.exe', '-portable']);
  // macOS from the disk image: why there is no launcher (mac-fix)
  assert.deepEqual(buildServerArgs({ ...base, launcher: null, launcherBlocked: 'dmg' }).slice(-2), ['-launcher-blocked', 'dmg']);
  // a launcher wins (they never come together)
  assert.ok(!buildServerArgs({ ...base, launcher: '/Applications/LectureFlow.app/Contents/MacOS/LectureFlow', launcherBlocked: 'dmg' }).includes('-launcher-blocked'));
});

// mac-fix: a macOS app that runs from its disk image or translocated has a
// path that disappears; launch.json must not name it.
test('launcherBlocked and launcherFor on macOS: translocated and disk image', () => {
  const mac = (execPath, readOnly = () => false) => ({ isPackaged: true, platform: 'darwin', execPath, readOnly });
  const installed = '/Applications/LectureFlow.app/Contents/MacOS/LectureFlow';
  const translocated = '/private/var/folders/ab/xyz/T/AppTranslocation/6E1F0C2A-1B2C-4D3E-8F90-0A1B2C3D4E5F/d/LectureFlow.app/Contents/MacOS/LectureFlow';
  const dmg = '/Volumes/LectureFlow 3.2.0-arm64/LectureFlow.app/Contents/MacOS/LectureFlow';
  const external = '/Volumes/Work/Apps/LectureFlow.app/Contents/MacOS/LectureFlow';
  const seen = [];
  const roDmg = (dir) => {
    seen.push(dir);
    return dir === '/Volumes/LectureFlow 3.2.0-arm64';
  };

  assert.equal(nm.launcherBlocked(mac(installed, roDmg)), null);
  assert.equal(nm.launcherFor(mac(installed, roDmg)), installed);
  assert.equal(nm.launcherBlocked(mac(translocated, roDmg)), 'translocated');
  assert.equal(nm.launcherFor(mac(translocated, roDmg)), null);
  assert.equal(nm.launcherBlocked(mac(dmg, roDmg)), 'dmg');
  assert.equal(nm.launcherFor(mac(dmg, roDmg)), null);
  // a writable external disk is the user's choice
  assert.equal(nm.launcherBlocked(mac(external, roDmg)), null);
  assert.equal(nm.launcherFor(mac(external, roDmg)), external);
  // only the volume's root is asked, only for /Volumes paths
  assert.deepEqual([...new Set(seen)].sort(), ['/Volumes/LectureFlow 3.2.0-arm64', '/Volumes/Work']);

  // Windows, development: never blocked; Windows keeps its launcher
  assert.equal(nm.launcherBlocked({ isPackaged: true, platform: 'win32', execPath: 'C:\\AppTranslocation\\LectureFlow.exe', readOnly: () => true }), null);
  assert.equal(nm.launcherBlocked({ isPackaged: false, platform: 'darwin', execPath: translocated }), null);
  assert.equal(nm.launcherFor({ isPackaged: true, platform: 'win32', execPath: 'C:\\LF\\LectureFlow.exe', readOnly: () => true }), 'C:\\LF\\LectureFlow.exe');
});

test('isReadOnlyVolume: a writable directory is not read-only', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-ro-'));
  try {
    assert.equal(nm.isReadOnlyVolume(dir), false);
    // a missing path is not a read-only volume either (ENOENT, not EROFS)
    assert.equal(nm.isReadOnlyVolume(path.join(dir, 'missing')), false);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('parseOpenLine accepts the service routes only', () => {
  assert.equal(nm.parseOpenLine('LECTUREFLOW_OPEN #/lecture/01JABCDEFGHJKMNPQRSTVWXYZ0'), '#/lecture/01JABCDEFGHJKMNPQRSTVWXYZ0');
  assert.equal(nm.parseOpenLine('LECTUREFLOW_OPEN #/course/01J0000000000000000000000Z\r'), '#/course/01J0000000000000000000000Z');
  for (const bad of [
    'LECTUREFLOW_OPEN',
    'LECTUREFLOW_OPEN ',
    'LECTUREFLOW_OPEN https://example.com/',
    'LECTUREFLOW_OPEN #/lecture/1\'; alert(1); //',
    'LECTUREFLOW_OPEN #/lecture/<img>',
    'LECTUREFLOW_OPEN #/../../etc',
    'LECTUREFLOW_OPEN javascript:alert(1)',
    `LECTUREFLOW_OPEN #/${'a'.repeat(300)}`,
    'time=2026-10-26 level=INFO msg="LECTUREFLOW_OPEN #/lecture/1"',
    ' LECTUREFLOW_OPEN #/lecture/1',
    null,
  ]) {
    assert.equal(nm.parseOpenLine(bad), null, String(bad));
  }
});

test('ServerProcess emits the service\'s open lines', async () => {
  const dataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-nm-'));
  const srv = new ServerProcess({
    command: process.execPath,
    prefixArgs: [FAKE],
    // The real flags reach the fake service like the Go one.
    argsFor: (port) => buildServerArgs({ port, dataDir, resourcesDir: dataDir, appVersion: '9.9.9', configFile: path.join(dataDir, 'config.toml'), locationFile: null, launcher: 'C:\\LF\\LectureFlow.exe', portable: true }),
    env: { ...process.env },
    cwd: dataDir,
  });
  try {
    await srv.start();
    const state = JSON.parse(fs.readFileSync(path.join(dataDir, 'fake-state.json'), 'utf8'));
    assert.deepEqual(state.args[0].slice(-3), ['-launcher', 'C:\\LF\\LectureFlow.exe', '-portable']);
    const opened = once(srv, 'open');
    await fetch(`${srv.origin}/test/open?route=${encodeURIComponent('#/lecture/01JTEST')}`);
    const [route] = await opened;
    assert.equal(route, '#/lecture/01JTEST');
    // A line that is not a valid route is ignored.
    let extra = null;
    srv.on('open', (r) => { extra = r; });
    await fetch(`${srv.origin}/test/open?route=${encodeURIComponent('javascript:alert(1)')}`);
    await new Promise((r) => setTimeout(r, 200));
    assert.equal(extra, null);
  } finally {
    await srv.stop({ timeoutMs: 5000 });
    fs.rmSync(dataDir, { recursive: true, force: true });
  }
});

test('the shell wires background starts, second instances and open routes', () => {
  const main = fs.readFileSync(path.join(DESKTOP, 'src', 'main', 'main.js'), 'utf8');
  assert.match(main, /requestSingleInstanceLock\(\{ background \}\)/);
  assert.match(main, /second-instance', \(event, argv, workingDirectory, additionalData\)/);
  assert.match(main, /!isBackgroundLaunch\(argv, additionalData\)/);
  const shell = fs.readFileSync(path.join(DESKTOP, 'src', 'main', 'shell.js'), 'utf8');
  assert.match(shell, /if \(!this\.quitting && !this\.startHidden\) win\.show\(\)/);
  assert.match(shell, /server\.on\('open'/);
  assert.match(shell, /launcher: launcherFor\(/);
  assert.match(shell, /launcherBlocked: launcherBlocked\(/);
  assert.match(shell, /portable: isPortable\(/);
});

test('NSIS uninstall removes the registration, not during updates', () => {
  const config = require(path.join(DESKTOP, 'electron-builder.config.js'));
  assert.equal(config.nsis.include, 'packaging/installer.nsh');
  const nsh = fs.readFileSync(path.join(DESKTOP, 'packaging', 'installer.nsh'), 'utf8');
  assert.match(nsh, /^# SPDX-License-Identifier: AGPL-3\.0-or-later/);
  assert.match(nsh, /!macro customUnInstall[\s\S]*\$\{ifNot\} \$\{isUpdated\}[\s\S]*\$\{endIf\}[\s\S]*!macroend/);
  for (const key of [
    'Software\\Google\\Chrome\\NativeMessagingHosts\\org.lectureflow.nmhost',
    'Software\\Microsoft\\Edge\\NativeMessagingHosts\\org.lectureflow.nmhost',
    'Software\\Mozilla\\NativeMessagingHosts\\org.lectureflow.nmhost',
  ]) {
    assert.ok(nsh.includes(`DeleteRegKey HKCU "${key}"`), key);
  }
  assert.ok(nsh.includes('RMDir /r "$APPDATA\\LectureFlow\\nmhost"'));
  assert.ok(nsh.includes('RMDir /r "$LOCALAPPDATA\\LectureFlow\\nmhost"'));
  // Never the data directory itself.
  assert.ok(!/RMDir[^\n]*"\$APPDATA\\LectureFlow"/.test(nsh));
  assert.equal(config.nsis.deleteAppDataOnUninstall, false);
});
