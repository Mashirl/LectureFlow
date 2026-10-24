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
//
// Usage: node scripts/smoke.js [--dist DIR]
'use strict';

const { spawn, execFileSync } = require('node:child_process');
const fs = require('node:fs');
const http = require('node:http');
const net = require('node:net');
const os = require('node:os');
const path = require('node:path');

// --dist DIR: smoke test another output directory (e.g. dist-next).
const DIST_ARG = process.argv.indexOf('--dist') > 0 ? process.argv[process.argv.indexOf('--dist') + 1] : 'dist';
const DIST = path.resolve(__dirname, '..', DIST_ARG);

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
      execFileSync(path.join(process.env.SystemRoot || 'C:\Windows', 'System32', 'taskkill.exe'), ['/PID', String(child.pid), '/T', '/F'], { stdio: 'ignore' });
    } catch {
      // already gone
    }
  } else child.kill('SIGKILL');
}

async function debugSwitchCheck(exe) {
  for (const allow of [false, true]) {
    const dataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-smoke-debug-'));
    const env = { ...process.env, LECTUREFLOW_DATA_DIR: dataDir };
    for (const k of ['LECTUREFLOW_SERVER', 'LECTUREFLOW_RESOURCES', 'ELECTRON_RUN_AS_NODE', 'LECTUREFLOW_E2E_ALLOW_DEBUG']) delete env[k];
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
  const dataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-smoke-'));
  const env = { ...process.env, LECTUREFLOW_DATA_DIR: dataDir };
  for (const k of ['LECTUREFLOW_SERVER', 'LECTUREFLOW_RESOURCES', 'ELECTRON_RUN_AS_NODE']) delete env[k];
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

    // Update feed (V8): only with LECTUREFLOW_UPDATE_REPO at build time.
    const updatesOn = !!process.env.LECTUREFLOW_UPDATE_REPO;
    const yml = path.join(path.dirname(exe), process.platform === 'win32' ? 'resources' : path.join('..', 'Resources'), 'app-update.yml');
    if (fs.existsSync(yml) !== updatesOn) throw new Error(`app-update.yml ${updatesOn ? 'missing' : 'present'} (LECTUREFLOW_UPDATE_REPO ${updatesOn ? 'set' : 'not set'})`);
    const want = updatesOn ? /updates: checking GitHub releases of / : /no update repository was configured for this build; automatic updates are off/;
    await waitFor(async () => want.test(fs.readFileSync(path.join(dataDir, 'logs', 'desktop.log'), 'utf8')), 10000, `the update line in desktop.log (${want})`);
    console.log(`automatic updates ${updatesOn ? 'on' : 'off'}, as built`);

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

main().catch((err) => {
  console.error(`smoke test failed: ${err.message}`);
  process.exit(1);
});
