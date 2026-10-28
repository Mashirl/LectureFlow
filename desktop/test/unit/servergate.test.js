// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The token waits for the ready line, and the free-port check sees other
// listeners (sec2-app M1). The services here are small scripts written for
// each case; only loopback addresses are listened on (a listener on 0.0.0.0
// or :: would make the Windows firewall ask about the test's Node).
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const net = require('node:net');
const os = require('node:os');
const path = require('node:path');

const { ServerProcess, StartError, isPortFree, pickFreePort } = require('../../src/main/server');
const { TOKEN_HEADER } = require('../../src/main/lib/security');

// a test may put setTimeout on a fake clock
const realSetTimeout = setTimeout;
const realClearTimeout = clearTimeout;

// A service following the desktop contract, steered by GATE_MODE:
//   fail_bind     no listener; after GATE_DELAY_MS it fails as the real one
//                 does when the port is taken
//   slow          listens at once, prints the ready line after GATE_DELAY_MS
//                 (the real service listens, then opens the database etc.)
//   wrong_port    the ready line names another port
//   bad_line      the ready line names 0.0.0.0
//   stderr_ready  the ready line goes to stderr only
// GATE_STATE: a JSON file with the requests it received before / after its
// ready line.
const SERVICE = String.raw`'use strict';
const fs = require('node:fs');
const http = require('node:http');
const args = process.argv.slice(2);
const port = Number(args[args.indexOf('-listen') + 1].split(':')[1]);
const mode = process.env.GATE_MODE;
const delay = Number(process.env.GATE_DELAY_MS || 0);
const state = { requests: 0, beforeReady: 0, ready: false, pid: process.pid };
const save = () => fs.writeFileSync(process.env.GATE_STATE, JSON.stringify(state));
let token = null;
let buf = '';
process.stdin.setEncoding('utf8');
process.stdin.on('data', (c) => {
  if (token !== null) return;
  buf += c;
  const nl = buf.indexOf('\n');
  if (nl >= 0) {
    token = buf.slice(0, nl).trim();
    begin();
  }
});
process.stdin.on('end', () => process.exit(0));
function ready(line, stream) {
  state.ready = true;
  save();
  stream.write(line + '\n');
}
function begin() {
  save();
  if (mode === 'fail_bind') {
    setTimeout(() => {
      process.stderr.write('ERROR: listen 127.0.0.1:' + port + ': listen tcp 127.0.0.1:' + port + ': bind: Only one usage of each socket address (protocol/network address/port) is normally permitted.\n');
      process.exit(1);
    }, delay);
    return;
  }
  const srv = http.createServer((req, res) => {
    state.requests++;
    if (!state.ready) state.beforeReady++;
    save();
    const ok = req.headers['x-lf-token'] === token;
    res.writeHead(ok ? 200 : 401, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify(ok ? { ok: true, version: '9.9.9', mode: 'desktop' } : { error: { code: 'auth.token_required' } }));
  });
  srv.listen(port, '127.0.0.1', () => {
    process.stdout.write('service: listening\n');
    setTimeout(() => {
      const url = 'http://127.0.0.1:' + port;
      if (mode === 'slow') ready('LECTUREFLOW_READY ' + url, process.stdout);
      else if (mode === 'wrong_port') ready('LECTUREFLOW_READY http://127.0.0.1:' + (port === 65535 ? port - 1 : port + 1), process.stdout);
      else if (mode === 'bad_line') ready('LECTUREFLOW_READY http://0.0.0.0:' + port, process.stdout);
      else if (mode === 'stderr_ready') ready('LECTUREFLOW_READY ' + url, process.stderr);
    }, delay);
  });
}
`;

let dir;
let script;

test.before(() => {
  dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-gate-'));
  script = path.join(dir, 'service.js');
  fs.writeFileSync(script, SERVICE);
});

test.after(() => {
  fs.rmSync(dir, { recursive: true, force: true });
});

// Every service is ended after its test, also when an assertion failed
// (a child left running would keep the test process alive).
const servers = [];
test.afterEach(async () => {
  for (const s of servers.splice(0)) await s.stop({ timeoutMs: 1000 }).catch(() => {});
});

let seq = 0;

/** gateServer(mode, opts) → { srv, stateFile, calls } with the fetches the shell made. */
function gateServer(mode, { delayMs = 0, port = null, ...opts } = {}) {
  const stateFile = path.join(dir, `state-${++seq}.json`);
  const calls = [];
  const srv = new ServerProcess({
    command: process.execPath,
    prefixArgs: [script],
    argsFor: (p) => ['-desktop', '-listen', `127.0.0.1:${p}`, '-data-dir', dir],
    env: { ...process.env, GATE_MODE: mode, GATE_DELAY_MS: String(delayMs), GATE_STATE: stateFile },
    cwd: dir,
    pollIntervalMs: 40,
    pickPort: port === null ? pickFreePort : async () => port,
    fetchImpl: (url, init) => {
      calls.push({ url: String(url), at: Date.now(), token: !!(init && init.headers && init.headers[TOKEN_HEADER]) });
      return fetch(url, init);
    },
    ...opts,
  });
  servers.push(srv);
  return { srv, stateFile, calls };
}

const readState = (f) => JSON.parse(fs.readFileSync(f, 'utf8'));

function alive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

async function gone(pid, ms = 5000) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    if (!alive(pid)) return true;
    await new Promise((r) => setTimeout(r, 50));
  }
  return false;
}

function listen(server, port, host) {
  return new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(port, host, () => resolve(server.address().port));
  });
}

const close = (server) => new Promise((r) => server.close(() => r()));

test('a listener already on the port gets no token while the service starts (M1)', async () => {
  // Another process holds the port; ours is slow and then fails to bind.
  // Before the fix the shell polled /api/health with the token after 250 ms.
  const seen = [];
  const squatter = http.createServer((req, res) => {
    seen.push({ path: req.url, token: req.headers['x-lf-token'] || null });
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ ok: true, mode: 'desktop' }));
  });
  const port = await listen(squatter, 0, '127.0.0.1');
  try {
    const { srv, calls } = gateServer('fail_bind', { delayMs: 600, port, maxPortRetries: 0 });
    await assert.rejects(srv.start(), (err) => err instanceof StartError && err.kind === 'port_in_use');
    assert.deepEqual(seen, [], 'the squatter received no request');
    assert.deepEqual(calls, [], 'no request was made at all');
  } finally {
    await close(squatter);
  }
});

test('the health check with the token starts only after the ready line', async () => {
  const { srv, stateFile, calls } = gateServer('slow', { delayMs: 700 });
  const t0 = Date.now();
  const { port } = await srv.start();
  try {
    const st = readState(stateFile);
    assert.equal(st.ready, true);
    assert.equal(st.beforeReady, 0, 'no request reached the service before its ready line');
    assert.ok(st.requests >= 1, 'the health check ran');
    assert.ok(calls.length >= 1 && calls.every((c) => c.token && c.url === `http://127.0.0.1:${port}/api/health`), JSON.stringify(calls));
    assert.ok(calls[0].at - t0 >= 600, `first request ${calls[0].at - t0} ms after the start`);
  } finally {
    await srv.stop();
  }
});

test('a ready line naming another port fails the start; no token is sent', async () => {
  for (const mode of ['wrong_port', 'bad_line']) {
    const { srv, stateFile, calls } = gateServer(mode);
    await assert.rejects(srv.start(), (err) => {
      assert.ok(err instanceof StartError, mode);
      assert.equal(err.kind, 'bad_mode', mode);
      assert.match(err.reason, /^the ready line names .+, not port \d+$/, mode);
      return true;
    });
    assert.deepEqual(calls, [], `${mode}: no request`);
    const st = readState(stateFile);
    assert.equal(st.requests, 0, `${mode}: the service got no request`);
    assert.equal(await gone(st.pid), true, `${mode}: the service was ended`);
  }
});

test('a ready line on stderr does not count', async (t) => {
  // The start timeout runs on a fake clock (node:test's mock timers), from
  // the moment the shell has read the line: the service's own start, slow
  // on a loaded machine (flake-hunt #11), takes no fake time.
  t.mock.timers.enable({ apis: ['setTimeout', 'Date'] });
  t.after(() => t.mock.timers.reset());
  const { srv, stateFile, calls } = gateServer('stderr_ready', { startTimeoutMs: 1200 });
  const started = srv.start();
  started.catch(() => {}); // awaited below
  const printed = () => srv.stderrTail().some((l) => l.startsWith('LECTUREFLOW_READY '));
  for (const end = performance.now() + 60_000; !printed() && performance.now() < end;) await new Promise((r) => realSetTimeout(r, 20));
  assert.equal(printed(), true, 'the service printed its ready line on stderr');
  t.mock.timers.tick(1199);
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(calls, [], 'no health check: the line on stderr is not the ready line');
  t.mock.timers.tick(1);
  // a minute of real time at most for the timeout to end the start
  let limit;
  const pending = new Promise((resolve, reject) => {
    limit = realSetTimeout(() => reject(new Error('the start timeout did not end the start')), 60_000);
  });
  await assert.rejects(Promise.race([started, pending]), (err) => err instanceof StartError && err.kind === 'timeout');
  realClearTimeout(limit);
  t.mock.timers.reset();
  assert.deepEqual(calls, []);
  const st = readState(stateFile);
  assert.equal(st.ready, true, 'the service did print it');
  assert.equal(st.requests, 0);
  assert.equal(await gone(st.pid), true);
});

test('isPortFree: a listener on another loopback address makes the port not free', async () => {
  const v6 = net.createServer();
  const port = await listen(v6, 0, '::1');
  try {
    assert.equal(await isPortFree(port), false, '[::1] answers');
    assert.equal(await isPortFree(port, { answers: async () => false }), true, 'binding 127.0.0.1 alone would not notice');
  } finally {
    await close(v6);
  }
});

test('isPortFree: a listener on a wildcard address (any account) makes the port not free', async () => {
  // Such a listener answers on 127.0.0.1 or [::1] although our own bind of
  // 127.0.0.1 may succeed; simulated, as a real one would make the Windows
  // firewall ask about the test's Node.
  const asked = [];
  let binds = 0;
  const bind = async () => {
    binds++;
    return true;
  };
  const wildcardV4 = async (host) => {
    asked.push(host);
    return host === '127.0.0.1';
  };
  assert.equal(await isPortFree(50123, { answers: wildcardV4, bind }), false);
  assert.deepEqual(asked.sort(), ['127.0.0.1', '::1']);
  assert.equal(binds, 0, 'no bind when something answers');
  assert.equal(await isPortFree(50123, { answers: async (host) => host === '::1', bind }), false, 'a listener on ::');

  // Nothing answers (no IPv6 counts as nothing): free when the bind works.
  assert.equal(await isPortFree(50123, { answers: async () => false, bind }), true);
  assert.equal(binds, 1);
  assert.equal(await isPortFree(50123, { answers: async () => false, bind: async () => false }), false);
});
