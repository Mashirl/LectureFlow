// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Smoke test of a packaged build (after scripts/dist.js): starts the
// unpacked app with a temporary data directory, waits until the bundled
// service answers (it must refuse a request without the token: 401), then
// closes the window like a user and expects the app and the service to exit.
// ELECTRON_RUN_AS_NODE is set on purpose: with the RunAsNode fuse off the
// executable must still start as the app.
// Playwright cannot drive the packaged app: the fuses disable --inspect.
// Debugging switches: started with --remote-debugging-port the app must
// refuse (no service, no DevTools endpoint; it shows a message and exits
// with code 2); with LECTUREFLOW_E2E_ALLOW_DEBUG=1 it must start.
// Updates: app-update.yml exists exactly when LECTUREFLOW_UPDATE_REPO is set,
// and desktop.log names the update mechanism of the build (updateMechanism
// below). A build whose service has the maintainer's minisign key (every
// release build: internal/selfupdate/keys.go) runs the in-app update and the
// shell writes "updates: in-app updates (signed update.json) are configured;
// electron-updater is not loaded"; a build without the key writes the
// electron-updater line that matches LECTUREFLOW_UPDATE_REPO. Which one is
// expected follows keys.go of this checkout (the tree dist.js built);
// --expect-updates selfupdate|electron-updater says it for a build of
// another tree.
// The browser extension's endpoint is off (LECTUREFLOW_NM_ENDPOINT=off unless
// the caller sets it), like the e2e tests: the service would otherwise
// register the browsers' native messaging host for this temporary build,
// over the registration of an installed LectureFlow.
//
// Usage: node scripts/smoke.js [--dist DIR] [--expect-updates selfupdate|electron-updater]
'use strict';

const { spawn, execFileSync } = require('node:child_process');
const fs = require('node:fs');
const http = require('node:http');
const net = require('node:net');
const os = require('node:os');
const path = require('node:path');
const { builtinUpdateKey } = require('./lib/updaterepo');

const REPO = path.resolve(__dirname, '..', '..');

/** argValue(name) → the value after a flag; '' for a flag without one; undefined when absent. */
function argValue(name) {
  const i = process.argv.indexOf(name);
  return i > 0 ? (process.argv[i + 1] ?? '') : undefined;
}

// --dist DIR: smoke test another output directory (e.g. dist-next).
const DIST = path.resolve(__dirname, '..', argValue('--dist') || 'dist');

// The lines the shell writes once it knows which update mechanism runs
// (src/main/updatepath.js; src/main/lib/updatefeed.js updateFeedNote).
const UPDATE_LINES = {
  selfupdate: /updates: in-app updates \(signed update\.json\) are configured; electron-updater is not loaded/,
  repo: /updates: checking GitHub releases of /,
  none: /updates: no update repository was configured for this build; automatic updates are off/,
  unusable: /updates: the update configuration of this build is not usable/,
};
const EXPECT_UPDATES = ['selfupdate', 'electron-updater'];

/**
 * expectedUpdates(flag, repoDir) → the mechanism the build must run: the
 * --expect-updates value, else 'selfupdate' when keys.go of repoDir has a
 * minisign key and 'electron-updater' when it is empty. A bad flag, or a
 * keys.go that cannot be read, throws (before the app starts).
 */
function expectedUpdates(flag, repoDir = REPO, readFile) {
  if (flag !== undefined) {
    if (!EXPECT_UPDATES.includes(flag)) throw new Error(`--expect-updates: ${JSON.stringify(flag)} is not one of ${EXPECT_UPDATES.join(', ')}`);
    return flag;
  }
  return builtinUpdateKey(repoDir, readFile).id ? 'selfupdate' : 'electron-updater';
}

/**
 * updateMechanism(log, { updatesOn, expect }) → null while desktop.log does
 * not say yet which update mechanism runs, else { mechanism, problem }:
 * mechanism 'selfupdate' (the service has a built-in public key; the shell
 * never loads electron-updater, whatever LECTUREFLOW_UPDATE_REPO says) or
 * 'electron-updater' (its line must match updatesOn, i.e. whether
 * LECTUREFLOW_UPDATE_REPO was set at build time). expect (expectedUpdates)
 * is the mechanism required. problem is '' when all is as expected.
 */
function updateMechanism(log, { updatesOn, expect } = {}) {
  if (!EXPECT_UPDATES.includes(expect)) throw new Error(`updateMechanism: expect must be one of ${EXPECT_UPDATES.join(', ')}`);
  const text = String(log == null ? '' : log);
  const self = UPDATE_LINES.selfupdate.test(text);
  const repo = UPDATE_LINES.repo.test(text);
  const none = UPDATE_LINES.none.test(text);
  if (!self && UPDATE_LINES.unusable.test(text)) return { mechanism: 'electron-updater', problem: 'desktop.log says the update configuration of this build is not usable (app-update.yml)' };
  if (!self && !repo && !none) return null;
  const problems = [];
  if (self && (repo || none)) problems.push('desktop.log names both the in-app update and electron-updater');
  const mechanism = self ? 'selfupdate' : 'electron-updater';
  if (!self && repo && none) problems.push('desktop.log says both that an update repository is configured and that none is');
  else if (!self && repo !== updatesOn) {
    problems.push(`desktop.log says ${repo ? 'an update repository is configured' : 'no update repository is configured'}, but LECTUREFLOW_UPDATE_REPO is ${updatesOn ? 'set' : 'not set'}`);
  }
  if (expect !== mechanism) {
    problems.push(mechanism === 'selfupdate'
      ? 'the service has a built-in update key (in-app updates), expected electron-updater'
      : 'the service has no built-in update key (electron-updater path), expected in-app updates');
  }
  return { mechanism, problem: problems.join('; ') };
}

/** smokeEnv(dataDir) → the app's environment: own data dir, no extension endpoint unless the caller set one. */
function smokeEnv(dataDir) {
  const env = { ...process.env, LECTUREFLOW_DATA_DIR: dataDir };
  for (const k of ['LECTUREFLOW_SERVER', 'LECTUREFLOW_RESOURCES', 'ELECTRON_RUN_AS_NODE', 'LECTUREFLOW_E2E_ALLOW_DEBUG']) delete env[k];
  if (!env.LECTUREFLOW_NM_ENDPOINT) env.LECTUREFLOW_NM_ENDPOINT = 'off';
  return env;
}

function appExe() {
  if (process.platform === 'win32') return path.join(DIST, 'win-unpacked', 'LectureFlow.exe');
  return path.join(DIST, 'mac-arm64', 'LectureFlow.app', 'Contents', 'MacOS', 'LectureFlow');
}

function get(port, p, headers = {}) {
  return new Promise((resolve, reject) => {
    http.get({ host: '127.0.0.1', port, path: p, headers }, (res) => {
      let body = '';
      res.on('data', (c) => { body += c; });
      res.on('end', () => resolve({ status: res.statusCode, body }));
    }).on('error', reject);
  });
}

async function waitFor(fn, ms, what) {
  const end = Date.now() + ms;
  for (;;) {
    const v = await fn().catch(() => null);
    if (v) return v;
    if (Date.now() > end) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 250));
  }
}

function alive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

function freePort() {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.on('error', reject);
    s.listen(0, '127.0.0.1', () => {
      const { port } = s.address();
      s.close(() => resolve(port));
    });
  });
}

function killTree(child) {
  if (!alive(child.pid)) return;
  if (process.platform === 'win32') {
    try {
      execFileSync(path.join(process.env.SystemRoot || 'C:\\Windows', 'System32', 'taskkill.exe'), ['/PID', String(child.pid), '/T', '/F'], { stdio: 'ignore' });
    } catch {
      // already gone
    }
  } else child.kill('SIGKILL');
}

async function debugSwitchCheck(exe) {
  for (const allow of [false, true]) {
    const dataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-smoke-debug-'));
    const env = smokeEnv(dataDir);
    if (allow) env.LECTUREFLOW_E2E_ALLOW_DEBUG = '1';
    const cdpPort = await freePort();
    const child = spawn(exe, [`--remote-debugging-port=${cdpPort}`], { env, stdio: 'ignore' });
    const exited = new Promise((resolve) => child.once('exit', (code) => resolve(code)));
    try {
      if (!allow) {
        // The message box waits for a click; give the app 5 s, then look.
        const code = await Promise.race([exited, new Promise((r) => setTimeout(() => r('running'), 5000))]);
        if (code !== 'running' && code !== 2) throw new Error(`with --remote-debugging-port the app exited with ${code}, expected 2 or a message box`);
        if (fs.existsSync(path.join(dataDir, 'electron', 'port.json'))) throw new Error('with --remote-debugging-port the app started its service');
        const cdp = await get(cdpPort, '/json/version').then((r) => r.status, () => 0);
        if (cdp) throw new Error(`with --remote-debugging-port the DevTools endpoint answered (HTTP ${cdp})`);
        console.log('--remote-debugging-port refused: no service, no DevTools endpoint');
      } else {
        await waitFor(async () => JSON.parse(fs.readFileSync(path.join(dataDir, 'electron', 'port.json'), 'utf8')).port, 60000, 'the service with LECTUREFLOW_E2E_ALLOW_DEBUG=1');
        await waitFor(async () => (await get(cdpPort, '/json/version')).status === 200, 15000, 'the DevTools endpoint with LECTUREFLOW_E2E_ALLOW_DEBUG=1');
        console.log('LECTUREFLOW_E2E_ALLOW_DEBUG=1: app started, DevTools endpoint up');
      }
    } finally {
      killTree(child);
      await Promise.race([exited, new Promise((r) => setTimeout(r, 10000))]);
      fs.rmSync(dataDir, { recursive: true, force: true, maxRetries: 10, retryDelay: 300 });
    }
  }
}

async function main() {
  const exe = appExe();
  if (!fs.existsSync(exe)) throw new Error(`no packaged app at ${exe} (run npm run dist first)`);
  // Before the app starts: a bad --expect-updates or an unreadable keys.go.
  const expect = expectedUpdates(argValue('--expect-updates'));
  console.log(`expected update mechanism: ${expect === 'selfupdate' ? 'in-app updates' : 'electron-updater'}${argValue('--expect-updates') ? ' (--expect-updates)' : ' (internal/selfupdate/keys.go)'}`);
  const dataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-smoke-'));
  const env = smokeEnv(dataDir);
  env.ELECTRON_RUN_AS_NODE = '1'; // must be ignored: the RunAsNode fuse is off
  const t0 = Date.now();
  const app = spawn(exe, [], { env, stdio: 'ignore' });
  try {
    const portFile = path.join(dataDir, 'electron', 'port.json');
    const port = await waitFor(async () => JSON.parse(fs.readFileSync(portFile, 'utf8')).port, 60000, 'the service (port.json)');
    const health = await get(port, '/api/health');
    if (health.status !== 401) throw new Error(`/api/health without token: HTTP ${health.status}, expected 401`);
    const log = fs.readFileSync(path.join(dataDir, 'logs', 'desktop.log'), 'utf8');
    if (!/service ready on port/.test(log)) throw new Error('desktop.log has no "service ready" line');
    console.log(`app and service up in ${Date.now() - t0} ms on port ${port}; unauthenticated request refused (401)`);

    // Update feed (V8): app-update.yml only with LECTUREFLOW_UPDATE_REPO at
    // build time. Which mechanism runs: updateMechanism.
    const updatesOn = !!process.env.LECTUREFLOW_UPDATE_REPO;
    const yml = path.join(path.dirname(exe), process.platform === 'win32' ? 'resources' : path.join('..', 'Resources'), 'app-update.yml');
    if (fs.existsSync(yml) !== updatesOn) throw new Error(`app-update.yml ${updatesOn ? 'missing' : 'present'} (LECTUREFLOW_UPDATE_REPO ${updatesOn ? 'set' : 'not set'})`);
    const updates = await waitFor(async () => updateMechanism(fs.readFileSync(path.join(dataDir, 'logs', 'desktop.log'), 'utf8'), { updatesOn, expect }),
      10000, 'the update line in desktop.log (in-app updates, or electron-updater on / off)');
    if (updates.problem) throw new Error(`updates: ${updates.problem}`);
    console.log(updates.mechanism === 'selfupdate'
      ? `in-app updates configured; app-update.yml ${updatesOn ? 'present' : 'absent'}, as built`
      : `electron-updater: automatic updates ${updatesOn ? 'on' : 'off'}, as built (no built-in update key)`);

    // Close the window like a user (no tasks: the app quits).
    if (process.platform === 'win32') execFileSync(path.join(process.env.SystemRoot || 'C:\\Windows', 'System32', 'taskkill.exe'), ['/PID', String(app.pid)], { stdio: 'ignore' });
    else app.kill('SIGTERM');
    const code = await new Promise((resolve) => app.once('exit', resolve));
    const serviceLog = path.join(dataDir, 'logs', 'lectureflow.log');
    const slog = fs.existsSync(serviceLog) ? fs.readFileSync(serviceLog, 'utf8') : '';
    const pids = JSON.parse(fs.readFileSync(portFile, 'utf8'));
    console.log(`app exited (code ${code}); service log ${slog ? 'present' : 'missing'}`);
    await waitFor(async () => {
      const r = await get(pids.port, '/api/health').then(() => false, () => true);
      return r;
    }, 15000, 'the service to stop');
    console.log('service stopped');
    await debugSwitchCheck(exe);
    console.log('smoke test passed');
  } finally {
    if (alive(app.pid)) app.kill();
    fs.rmSync(dataDir, { recursive: true, force: true, maxRetries: 10, retryDelay: 300 });
  }
}

module.exports = { updateMechanism, expectedUpdates, smokeEnv, UPDATE_LINES };

if (require.main === module) {
  main().catch((err) => {
    console.error(`smoke test failed: ${err.message}`);
    process.exit(1);
  });
}
