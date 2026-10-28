// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The isolated LectureFlow of the native end-to-end test and its native
// messaging host:
//
// - lectureflow.exe and lectureflow-nmhost.exe built from this checkout
//   (CGO_ENABLED=0, as desktop/scripts/stage.js does), or taken from
//   LF_NATIVE_BIN;
// - its own data directory, temporary directory (aria2c's conf file goes
//   there) and logs, all under the work directory; fake ASR (test mode: no
//   GPU, no Python, no power requests), no LLM provider, no Colab, no
//   DEEPSEEK_API_KEY, no component manifest, no update key (dev build), so
//   it reaches nothing but 127.0.0.1;
// - its own named pipe (LECTUREFLOW_NM_ENDPOINT): an instance with an
//   explicit endpoint does not manage the browsers' registration
//   (cmd/lectureflow/nmhost.go), so the user's own LectureFlow and its
//   registration are left alone;
// - the host: a .bat that sets the pipe and LECTUREFLOW_NM_DIR (where the
//   test's launch.json is) before running lectureflow-nmhost.exe, and its
//   manifest (allowed_origins: the development extension id); launch.json
//   starts this instance through lib/supervisor.mjs, as the desktop app's
//   launch.json starts the desktop app.

import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { spawn, execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { MARKER } from './registry.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
export const SUPERVISOR = path.join(here, 'supervisor.mjs');
export const DEV_EXTENSION_ID = 'lbfjnfhpnaafingpjkoefobbjnmheehl';

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/** The work directory's layout. */
export function workPaths(root) {
  const host = path.join(root, `${MARKER}-host`);
  const p = {
    root,
    bin: path.join(root, 'bin'),
    data: path.join(root, 'data'),
    logs: path.join(root, 'logs'),
    tmp: path.join(root, 'tmp'),
    state: path.join(root, 'state'),
    media: path.join(root, 'media'),
    browser: path.join(root, 'browser'),
    ext: path.join(root, 'extension'),
    host,
    nmdir: path.join(host, 'nmdir'),
    manifest: path.join(host, 'org.lectureflow.nmhost.json'),
    bat: path.join(host, 'lectureflow-nmhost.bat'),
    hostLaunches: path.join(host, 'host-launches.log'),
    // one file per host process: two hosts may run at once (Chrome keeps
    // a redirect file open for the whole life of the process)
    hostStderr: path.join(host, 'host-stderr'),
  };
  for (const d of [p.bin, p.data, p.logs, p.tmp, p.state, p.media, p.browser, p.nmdir, p.hostStderr]) fs.mkdirSync(d, { recursive: true });
  return p;
}

/** { app, host } executables: LF_NATIVE_BIN's, or built from the checkout into dir. */
export function buildBinaries(repo, dir) {
  const given = process.env.LF_NATIVE_BIN;
  const from = given || dir;
  const bins = { app: path.join(from, 'lectureflow.exe'), host: path.join(from, 'lectureflow-nmhost.exe') };
  if (!given) {
    const env = { ...process.env, CGO_ENABLED: '0' };
    for (const [out, pkg] of [[bins.app, './cmd/lectureflow'], [bins.host, './cmd/lectureflow-nmhost']]) {
      execFileSync('go', ['build', '-trimpath', '-o', out, pkg], { cwd: repo, env, stdio: ['ignore', 'ignore', 'pipe'], windowsHide: true });
    }
  }
  for (const f of Object.values(bins)) if (!fs.existsSync(f)) throw new Error(`missing ${f}`);
  return bins;
}

const tomlPath = (p) => `'${p.replace(/\\/g, '/')}'`;

/** Writes config.toml and instance.json; returns { pipe, token }. */
export function writeInstance(p, { app, ffmpeg, aria2c, fakeAsr }) {
  const pipe = `\\\\.\\pipe\\${MARKER}-${crypto.randomBytes(6).toString('hex')}`;
  const token = crypto.randomBytes(32).toString('hex');
  const config = path.join(p.state, 'config.toml');
  fs.writeFileSync(
    config,
    [
      '# native end-to-end test only: an isolated instance (fake ASR, no LLM, no Colab)',
      'listen = "127.0.0.1:0"',
      `data_dir = ${tomlPath(p.data)}`,
      'log_level = "debug"',
      `ffmpeg = ${tomlPath(ffmpeg)}`,
      `aria2c = ${tomlPath(aria2c)}`,
      `python = ${tomlPath(path.join(p.root, 'no-python', 'python.exe'))}`,
      `nvidia_smi = ${tomlPath(path.join(p.root, 'no-nvidia-smi.exe'))}`,
      '',
      '[asr]',
      `fake_result = ${tomlPath(fakeAsr)}`,
      'language = "en"',
      '',
    ].join('\n'),
  );
  const instance = {
    exe: app,
    args: ['-desktop', '-config', config, '-data-dir', p.data, '-listen', '127.0.0.1:0'],
    // The fake platform and its media are on 127.0.0.1, which a release
    // refuses as a track address (security review L1); this switch admits
    // loopback for development and tests only.
    env: {
      LECTUREFLOW_NM_ENDPOINT: pipe, LECTUREFLOW_NM_DIR: p.nmdir, TEMP: p.tmp, TMP: p.tmp, HF_HUB_OFFLINE: '1',
      LECTUREFLOW_DEV_DOWNLOAD_LOOPBACK: '1',
    },
    unset: ['DEEPSEEK_API_KEY', 'LECTUREFLOW_DATA_DIR', 'GOOGLE_APPLICATION_CREDENTIALS', 'CLOUDSDK_CONFIG', 'LECTUREFLOW_DEV_POWER'],
    token,
    logDir: p.logs,
  };
  fs.writeFileSync(path.join(p.state, 'instance.json'), JSON.stringify(instance, null, 2));
  return { pipe, token };
}

/** The host wrapper, its manifest and launch.json; returns the manifest's path. */
export function writeHost(p, { host, pipe, extensionId = DEV_EXTENSION_ID }) {
  const q = (s) => `"${s}"`;
  fs.writeFileSync(
    p.bat,
    [
      '@echo off',
      `set "LECTUREFLOW_NM_ENDPOINT=${pipe}"`,
      `set "LECTUREFLOW_NM_DIR=${p.nmdir}"`,
      `echo %DATE% %TIME% %* >> ${q(p.hostLaunches)}`,
      `${q(host)} %* 2> ${q(path.join(p.hostStderr, '%RANDOM%%RANDOM%%RANDOM%.log'))}`,
      '',
    ].join('\r\n'),
  );
  fs.writeFileSync(
    p.manifest,
    JSON.stringify({ name: 'org.lectureflow.nmhost', description: 'LectureFlow native e2e test host', path: p.bat, type: 'stdio', allowed_origins: [`chrome-extension://${extensionId}/`] }, null, 2),
  );
  writeLaunch(p);
  return p.manifest;
}

const launchFile = (p) => path.join(p.nmdir, 'launch.json');
export function writeLaunch(p) {
  fs.writeFileSync(launchFile(p), JSON.stringify({ exe: process.execPath, args: [SUPERVISOR, p.state, 'launch.json'] }, null, 2));
}
export function removeLaunch(p) {
  fs.rmSync(launchFile(p), { force: true });
}

export const instanceUrl = (p) => {
  try {
    return fs.readFileSync(path.join(p.state, 'url.txt'), 'utf8').trim();
  } catch {
    return '';
  }
};
/** How many lines the hosts wrote to stderr that contain text. */
export function hostStderrCount(p, text) {
  let n = 0;
  for (const f of fs.readdirSync(p.hostStderr)) {
    for (const line of fs.readFileSync(path.join(p.hostStderr, f), 'utf8').split(/\r?\n/)) if (line.includes(text)) n++;
  }
  return n;
}

export const launches = (p) => {
  try {
    return fs.readFileSync(path.join(p.state, 'launches.log'), 'utf8').split('\n').filter((l) => / start \d+ /.test(l));
  } catch {
    return [];
  }
};

/** Waits for url.txt (the app printed its ready line); returns the URL. */
export async function waitReady(p, timeout = 40000) {
  const end = Date.now() + timeout;
  while (Date.now() < end) {
    const u = instanceUrl(p);
    if (u) return u;
    if (fs.existsSync(path.join(p.state, 'exit.txt'))) break;
    await sleep(100);
  }
  const logs = fs.readdirSync(p.logs).filter((f) => f.endsWith('.stderr.log')).map((f) => fs.readFileSync(path.join(p.logs, f), 'utf8')).join('\n');
  throw new Error(`LectureFlow did not become ready: ${logs.slice(-2000)}`);
}

/** Starts the instance through the supervisor (detached, like the host does). */
export async function startInstance(p, who = 'test') {
  fs.rmSync(path.join(p.state, 'url.txt'), { force: true });
  const child = spawn(process.execPath, [SUPERVISOR, p.state, who], { detached: true, stdio: 'ignore', windowsHide: true });
  child.unref();
  return waitReady(p);
}

const alive = (pid) => {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
};
const appPid = (p) => {
  try {
    return Number(fs.readFileSync(path.join(p.state, 'app.pid'), 'utf8'));
  } catch {
    return 0;
  }
};

/** Graceful stop (stdin closed, as when the desktop shell quits); killed after 45 s. */
export async function stopInstance(p) {
  const pid = appPid(p);
  if (!pid || !alive(pid)) return 'not running';
  fs.rmSync(path.join(p.state, 'exit.txt'), { force: true });
  fs.writeFileSync(path.join(p.state, 'stop.flag'), '');
  const end = Date.now() + 45000;
  while (Date.now() < end) {
    if (!alive(pid) && fs.existsSync(path.join(p.state, 'exit.txt'))) return fs.readFileSync(path.join(p.state, 'exit.txt'), 'utf8').trim();
    await sleep(100);
  }
  killInstance(p);
  return 'killed';
}

/** Synchronous last resort (process exit): ends the app and its children. */
export function killInstance(p) {
  const pid = appPid(p);
  if (pid && alive(pid)) {
    try {
      execFileSync('taskkill.exe', ['/PID', String(pid), '/T', '/F'], { stdio: 'ignore', windowsHide: true });
    } catch {
      // gone
    }
  }
}

/** The instance's HTTP API (desktop mode: the token in X-LF-Token). */
export function apiClient(p, token) {
  return async function api(method, route, body) {
    const base = instanceUrl(p);
    if (!base) throw new Error('LectureFlow is not running');
    const res = await fetch(base + route, {
      method,
      headers: { 'X-LF-Token': token, ...(body ? { 'content-type': 'application/json' } : {}) },
      body: body ? JSON.stringify(body) : undefined,
    });
    const text = await res.text();
    let json = null;
    try {
      json = text ? JSON.parse(text) : null;
    } catch {
      json = null;
    }
    if (!res.ok) throw Object.assign(new Error(`${method} ${route} → ${res.status} ${text.slice(0, 300)}`), { status: res.status, json });
    return json;
  };
}
