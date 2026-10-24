// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// A fake LectureFlow service for the shell's tests. It follows the command
// line and HTTP contract of v3-brief §1–§2 (desktop mode only): token on the
// first stdin line, stdin EOF stops it, "LECTUREFLOW_READY <url>" on stdout,
// "ERROR: <reason>" on stderr before exit code 1, token and Host checks on
// /api/. Its state (attempt counter, activity, settings, calls received) is
// kept in {data dir}/fake-state.json so tests can steer and inspect it
// across restarts. The /test/ endpoints exist only here.
//
// Behaviour switches (environment):
//   FAKE_PORT_FAIL_TIMES=n   the first n attempts fail with "port in use"
//   FAKE_START_FAIL_TIMES=n  the first n attempts fail with a database error
//   FAKE_HANG=1              never become ready
//   FAKE_IGNORE_STOP=1       ignore /api/app/shutdown and stdin EOF
//   FAKE_BAD_MODE=1          /api/health reports mode "web"
'use strict';

const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');

const VALUE_FLAGS = new Set(['listen', 'data-dir', 'resources', 'app-version', 'config', 'location-file']);

function parseArgs(argv) {
  const out = { desktop: false };
  for (let i = 0; i < argv.length; i++) {
    const m = /^--?([a-z-]+)$/.exec(argv[i]);
    if (!m) fatal(`unexpected argument ${argv[i]}`);
    if (m[1] === 'desktop') out.desktop = true;
    else if (VALUE_FLAGS.has(m[1])) out[m[1]] = argv[++i];
    else fatal(`flag provided but not defined: -${m[1]}`);
  }
  return out;
}

function fatal(reason, code = 1) {
  process.stderr.write(`ERROR: ${reason}\n`);
  process.exit(code);
}

const opts = parseArgs(process.argv.slice(2));
if (!opts.desktop) fatal('the fake service only knows desktop mode');
if (!opts.listen || !opts['data-dir']) fatal('-listen and -data-dir are required');
const port = Number(opts.listen.split(':')[1]);
const dataDir = opts['data-dir'];
const stateFile = path.join(dataDir, 'fake-state.json');
const env = process.env;

function loadState() {
  try {
    return JSON.parse(fs.readFileSync(stateFile, 'utf8'));
  } catch {
    return {
      attempts: 0,
      pids: [],
      args: [],
      shutdownCalls: 0,
      stdinEof: 0,
      patches: [],
      unauthorized: 0,
      activity: { running: 0, queued: 0, transcribing: false, api_running: false },
      settings: { close_behavior: 'ask', prevent_sleep: true, update_channel: 'stable' },
    };
  }
}
const state = loadState();
function save() {
  fs.mkdirSync(dataDir, { recursive: true });
  fs.writeFileSync(stateFile, JSON.stringify(state, null, 2));
}
state.attempts++;
state.pids.push(process.pid);
state.args.push(process.argv.slice(2));
save();

let token = null;
let stopping = false;

function stop(why) {
  if (stopping) return;
  if (env.FAKE_IGNORE_STOP === '1') return;
  stopping = true;
  process.stdout.write(`fake: stopping (${why})\n`);
  setTimeout(() => process.exit(0), 50);
}

// stdin: first line = token; EOF = the shell has gone.
let buf = '';
process.stdin.setEncoding('utf8');
process.stdin.on('data', (chunk) => {
  if (token !== null) return;
  buf += chunk;
  const nl = buf.indexOf('\n');
  if (nl < 0) return;
  token = buf.slice(0, nl).trim();
  if (token.length < 32) fatal(`desktop mode: access token too short (${token.length} characters, at least 32)`);
  begin();
});
process.stdin.on('end', () => {
  state.stdinEof++;
  save();
  if (token === null) fatal('desktop mode: no access token on stdin');
  stop('stdin EOF');
});

const PAGE = `<!doctype html>
<html><head><meta charset="utf-8"><title>Fake LectureFlow</title><script src="/app.js"></script></head>
<body><h1>Fake LectureFlow</h1><a id="ext" href="https://example.com/">external</a></body></html>`;
const SCRIPT = `window.__fakeLoaded = true;
fetch('/api/health').then((r) => { document.documentElement.dataset.health = String(r.status); });`;

function send(res, status, body, type = 'application/json') {
  const data = typeof body === 'string' ? body : JSON.stringify(body);
  res.writeHead(status, { 'Content-Type': type, 'Cache-Control': 'no-store' });
  res.end(data);
}

function readBody(req) {
  return new Promise((resolve) => {
    let s = '';
    req.on('data', (c) => { s += c; });
    req.on('end', () => {
      try {
        resolve(s ? JSON.parse(s) : {});
      } catch {
        resolve(null);
      }
    });
  });
}

async function handle(req, res) {
  const host = String(req.headers.host || '').toLowerCase();
  if (host !== `127.0.0.1:${port}` && host !== `localhost:${port}`) return send(res, 403, { error: { code: 'auth.bad_host' } });
  const url = new URL(req.url, `http://127.0.0.1:${port}`);
  const p = url.pathname;

  if (p === '/api' || p.startsWith('/api/')) {
    if (req.headers['x-lf-token'] !== token) {
      state.unauthorized++;
      save();
      return send(res, 401, { error: { code: 'auth.token_required' } });
    }
    if (p === '/api/health') return send(res, 200, { ok: true, version: opts['app-version'] || 'dev', mode: env.FAKE_BAD_MODE === '1' ? 'web' : 'desktop' });
    if (p === '/api/app/activity') return send(res, 200, state.activity);
    if (p === '/api/app/settings' && req.method === 'GET') return send(res, 200, state.settings);
    if (p === '/api/app/settings' && req.method === 'PATCH') {
      const patch = await readBody(req);
      if (!patch || (patch.close_behavior !== undefined && !['ask', 'background', 'quit'].includes(patch.close_behavior))) {
        return send(res, 400, { error: { code: 'api.bad_request' } });
      }
      state.patches.push(patch);
      Object.assign(state.settings, patch);
      save();
      return send(res, 200, state.settings);
    }
    if (p === '/api/app/shutdown' && req.method === 'POST') {
      state.shutdownCalls++;
      save();
      send(res, 202, { ok: true });
      stop('POST /api/app/shutdown');
      return undefined;
    }
    return send(res, 404, { error: { code: 'api.not_found' } });
  }

  // Test-only controls (no token: the test runner calls them directly).
  if (p === '/test/state') return send(res, 200, { ...state, pid: process.pid });
  if (p === '/test/set' && req.method === 'POST') {
    const body = await readBody(req);
    if (body && body.activity) Object.assign(state.activity, body.activity);
    if (body && body.settings) Object.assign(state.settings, body.settings);
    save();
    return send(res, 200, state);
  }
  if (p === '/test/exit') {
    const code = Number(url.searchParams.get('code') || '1');
    send(res, 200, { exiting: code });
    setTimeout(() => {
      if (code !== 0 && code !== 3) process.stderr.write('ERROR: simulated failure for the test\n');
      process.exit(code);
    }, 50);
    return undefined;
  }

  if (p === '/' || p === '/index.html') return send(res, 200, PAGE, 'text/html; charset=utf-8');
  if (p === '/app.js') return send(res, 200, SCRIPT, 'text/javascript; charset=utf-8');
  if (p === '/other.html') return send(res, 200, '<!doctype html><title>Other</title><p>other</p>', 'text/html; charset=utf-8');
  return send(res, 404, 'not found', 'text/plain');
}

function begin() {
  if (state.attempts <= Number(env.FAKE_PORT_FAIL_TIMES || 0)) {
    fatal(`listen tcp 127.0.0.1:${port}: bind: Only one usage of each socket address (protocol/network address/port) is normally permitted.`);
  }
  if (state.attempts <= Number(env.FAKE_START_FAIL_TIMES || 0)) fatal('open database: database is locked');
  if (env.FAKE_HANG === '1') return;
  const server = http.createServer((req, res) => {
    handle(req, res).catch((err) => send(res, 500, { error: { code: 'internal', message: String(err) } }));
  });
  server.on('error', (err) => fatal(`listen tcp 127.0.0.1:${port}: ${err.code}`));
  server.listen(port, '127.0.0.1', () => {
    process.stdout.write(`fake: listening\nLECTUREFLOW_READY http://127.0.0.1:${port}\n`);
  });
}
