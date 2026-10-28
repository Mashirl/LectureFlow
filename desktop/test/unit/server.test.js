// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// ServerProcess against the fake service (test/fixtures/fake-server.js),
// which follows the Go service's command-line and HTTP contract.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { once } = require('node:events');

const { ServerProcess, StartError } = require('../../src/main/server');
const { buildServerArgs } = require('../../src/main/lib/serverutil');

const FAKE = path.join(__dirname, '..', 'fixtures', 'fake-server.js');
// the long-start tests put setTimeout on a fake clock
const realSetTimeout = setTimeout;
const realClearTimeout = clearTimeout;
const dirs = [];

function tmpDir() {
  const d = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-srv-'));
  dirs.push(d);
  return d;
}

test.after(() => {
  for (const d of dirs) fs.rmSync(d, { recursive: true, force: true });
});

function makeServer(dataDir, env = {}, opts = {}) {
  return new ServerProcess({
    command: process.execPath,
    prefixArgs: [FAKE],
    argsFor: (port) => buildServerArgs({ port, dataDir, resourcesDir: dataDir, appVersion: '9.9.9', configFile: path.join(dataDir, 'config.toml'), locationFile: null }),
    env: { ...process.env, ...env },
    cwd: dataDir,
    ...opts,
  });
}

const fakeState = (dataDir) => JSON.parse(fs.readFileSync(path.join(dataDir, 'fake-state.json'), 'utf8'));

function alive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

async function waitFor(fn, ms = 5000) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    if (await fn()) return true;
    await new Promise((r) => setTimeout(r, 50));
  }
  return false;
}

test('starts, passes the token on stdin only, stops gracefully', async () => {
  const dir = tmpDir();
  const srv = makeServer(dir);
  const { port, origin } = await srv.start();
  assert.equal(origin, `http://127.0.0.1:${port}`);
  assert.equal(srv.ready, true);
  assert.match(srv.token, /^[0-9a-f]{64}$/);

  const st = fakeState(dir);
  assert.deepEqual(st.args[0].slice(0, 5), ['-desktop', '-listen', `127.0.0.1:${port}`, '-data-dir', dir]);
  assert.equal(JSON.stringify(st.args).includes(srv.token), false, 'token not on the command line');

  const noToken = await fetch(`${origin}/api/health`);
  assert.equal(noToken.status, 401);
  const wrong = await fetch(`${origin}/api/health`, { headers: { 'X-LF-Token': 'a'.repeat(64) } });
  assert.equal(wrong.status, 401);
  const ok = await fetch(`${origin}/api/health`, { headers: { 'X-LF-Token': srv.token } });
  assert.deepEqual(await ok.json(), { ok: true, version: '9.9.9', mode: 'desktop' });

  const pid = srv.pid;
  const exited = once(srv, 'exit');
  const t0 = Date.now();
  await srv.stop();
  const [info] = await exited;
  assert.equal(info.expected, true);
  assert.ok(Date.now() - t0 < 5000);
  assert.equal(fakeState(dir).shutdownCalls, 1, 'POST /api/app/shutdown was sent');
  assert.equal(await waitFor(() => !alive(pid)), true);
});

test('a taken port is retried with another one, at most 3 times', async () => {
  const dir = tmpDir();
  const srv = makeServer(dir, { FAKE_PORT_FAIL_TIMES: '2' });
  const { port } = await srv.start();
  const st = fakeState(dir);
  assert.equal(st.attempts, 3);
  assert.equal(srv.attempts, 3);
  assert.equal(st.args[2][2], `127.0.0.1:${port}`);
  await srv.stop();

  const dir2 = tmpDir();
  const srv2 = makeServer(dir2, { FAKE_PORT_FAIL_TIMES: '10' });
  await assert.rejects(srv2.start(), (err) => err instanceof StartError && err.kind === 'port_in_use' && /Only one usage/.test(err.reason));
  assert.equal(fakeState(dir2).attempts, 4, 'first try + 3 retries');
});

test('a start failure carries the ERROR: reason and exit code', async () => {
  const dir = tmpDir();
  const srv = makeServer(dir, { FAKE_START_FAIL_TIMES: '1' });
  await assert.rejects(srv.start(), (err) => err instanceof StartError && err.kind === 'exited' && err.reason === 'open database: database is locked' && err.code === 1);
  assert.equal(fakeState(dir).attempts, 1, 'not retried: not a port problem');
});

test('no answer within the start timeout: the process is ended', async () => {
  const dir = tmpDir();
  const srv = makeServer(dir, { FAKE_HANG: '1' }, { startTimeoutMs: 1200 });
  const t0 = Date.now();
  await assert.rejects(srv.start(), (err) => err instanceof StartError && err.kind === 'timeout');
  assert.ok(Date.now() - t0 < 8000);
  const pid = fakeState(dir).pids[0];
  assert.equal(await waitFor(() => !alive(pid)), true);
});

// The long-start tests run the start timeouts on a fake clock (node:test's
// mock timers: setTimeout and Date of this process). The fake service is a
// real process; how long it takes to start (seconds on a loaded machine,
// flake-hunt #11) takes no fake time. Its "LECTUREFLOW_STARTING" lines and
// its ready line come when the test asks for them (FAKE_STEPS=stdin), and
// only the test's tick() moves the clock between them.

/**
 * within(promise, what): the promise, or a failure if it has not settled
 * after a minute of real time (the fake clock would never end the wait).
 */
function within(promise, what, ms = 60_000) {
  let timer;
  const limit = new Promise((resolve, reject) => {
    timer = realSetTimeout(() => reject(new Error(`${what}: still pending after ${ms / 1000} s`)), ms);
  });
  return Promise.race([promise, limit]).finally(() => realClearTimeout(timer));
}

/** stepped(t, opts): a fake service steered line by line, on a fake clock; start() is running. */
async function stepped(t, opts) {
  t.mock.timers.enable({ apis: ['setTimeout', 'Date'] });
  const dir = tmpDir();
  const srv = makeServer(dir, { FAKE_STEPS: 'stdin' }, opts);
  t.after(async () => {
    t.mock.timers.reset();
    await srv.stop({ timeoutMs: 5000 }).catch(() => {}); // also when an assertion failed
  });
  const steps = [];
  srv.on('starting', (s) => steps.push(s));
  const started = srv.start();
  started.catch(() => {}); // awaited by the test
  await within((async () => {
    while (!srv._child) await new Promise((r) => setImmediate(r)); // spawned after the port is picked
  })(), 'spawning the fake service');
  const child = srv._child;
  const exited = once(child, 'exit').then(([code]) => {
    throw new Error(`the fake service exited (${code}) before the test was done with it`);
  });
  exited.catch(() => {});
  return {
    srv,
    dir,
    steps,
    t0: Date.now(),
    /** the service prints one LECTUREFLOW_STARTING line; resolves once the shell has read it */
    async starting() {
      const seen = once(srv, 'starting');
      child.stdin.write('starting\n');
      await within(Promise.race([seen, exited]), 'the LECTUREFLOW_STARTING line');
    },
    /** the service listens and prints its ready line; resolves when start() has */
    ready() {
      child.stdin.write('ready\n');
      return within(started, 'the start after the ready line');
    },
    /** start() ends with a timeout */
    timedOut() {
      return assert.rejects(within(started, 'the start timeout'), isTimeout);
    },
    tick: (ms) => t.mock.timers.tick(ms),
  };
}

const isTimeout = (err) => err instanceof StartError && err.kind === 'timeout';

test('a long start that keeps saying LECTUREFLOW_STARTING is waited for past the start timeout (test-upgrade F3)', async (t) => {
  const s = await stepped(t, { startTimeoutMs: 800 });
  // a line every 799 ms (just under the start timeout), five times
  for (let i = 0; i < 5; i++) {
    await s.starting();
    s.tick(799);
  }
  assert.equal(Date.now() - s.t0, 5 * 799, 'well past the start timeout');
  await s.ready();
  assert.equal(s.srv.ready, true);
  assert.deepEqual(s.steps, Array(5).fill('upgrade-backup'));
  assert.equal(fakeState(s.dir).attempts, 1);
});

test('the starting lines extend the wait only up to startMaxMs; silence ends it after the start timeout', async (t) => {
  // startMaxMs 2000: lines at 0, 700 and 1400 ms; the start timeout from the
  // last one would end at 2200, startMaxMs ends it at 2000.
  const capped = async (t2, endAt) => {
    const s = await stepped(t2, { startTimeoutMs: 800, startMaxMs: 2000 });
    for (const at of [0, 700, 1400]) {
      s.tick(at - (Date.now() - s.t0));
      await s.starting();
    }
    s.tick(endAt - (Date.now() - s.t0));
    return s;
  };
  await t.test('still waiting at 1999 ms', async (t2) => {
    const s = await capped(t2, 1999);
    await s.ready(); // no timeout yet: the service's ready line still counts
  });
  await t.test('ended at 2000 ms', async (t2) => {
    const s = await capped(t2, 2000);
    await s.timedOut();
  });

  // No startMaxMs: each line gives the start timeout (800 ms) again, from
  // that line; silence for that long ends the start.
  const quiet = async (t2, endAt) => {
    const s = await stepped(t2, { startTimeoutMs: 800 });
    await s.starting();
    s.tick(500);
    await s.starting();
    s.tick(endAt - (Date.now() - s.t0));
    return s;
  };
  await t.test('a line at 500 ms: still waiting at 1299 ms', async (t2) => {
    const s = await quiet(t2, 1299);
    await s.ready();
  });
  await t.test('a line at 500 ms: ended at 1300 ms, 800 ms after it', async (t2) => {
    const s = await quiet(t2, 1300);
    await s.timedOut();
  });
});

test('a coded failure carries its code and parameters besides the ERROR reason', async () => {
  const dir = tmpDir();
  const failure = { code: 'startup.data_dir_in_use', params: { data_dir: dir, pid: 4242 } };
  const srv = makeServer(dir, { FAKE_FAILURE: JSON.stringify(failure) });
  await assert.rejects(srv.start(), (err) => err instanceof StartError && err.kind === 'exited' && err.code === 1 &&
    err.reason === 'a coded failure for the test' && JSON.stringify(err.failure) === JSON.stringify(failure));
  const plain = makeServer(tmpDir(), { FAKE_START_FAIL_TIMES: '1' });
  await assert.rejects(plain.start(), (err) => err instanceof StartError && err.failure === null);
});

test('a service not in desktop mode is refused', async () => {
  const dir = tmpDir();
  const srv = makeServer(dir, { FAKE_BAD_MODE: '1' });
  await assert.rejects(srv.start(), (err) => err instanceof StartError && err.kind === 'bad_mode');
});

test('a missing program is reported', async () => {
  const dir = tmpDir();
  const srv = new ServerProcess({ command: path.join(dir, 'nope.exe'), argsFor: () => [], cwd: dir });
  await assert.rejects(srv.start(), (err) => err instanceof StartError && err.kind === 'spawn_failed' && err.reason === 'ENOENT');
});

test('exit code 3 and crashes after start are reported as exit events', async () => {
  const dir = tmpDir();
  const srv = makeServer(dir);
  const { origin } = await srv.start();
  let ev = once(srv, 'exit');
  await fetch(`${origin}/test/exit?code=3`);
  let [info] = await ev;
  assert.equal(info.code, 3);
  assert.equal(info.expected, false);
  assert.equal(srv.ready, false);

  const dir2 = tmpDir();
  const srv2 = makeServer(dir2);
  const s2 = await srv2.start();
  ev = once(srv2, 'exit');
  await fetch(`${s2.origin}/test/exit?code=1`);
  [info] = await ev;
  assert.equal(info.code, 1);
  assert.equal(info.reason, 'simulated failure for the test');
});

test('stdin EOF alone stops the service (the shell crashed)', async () => {
  const dir = tmpDir();
  const srv = makeServer(dir);
  await srv.start();
  const ev = once(srv, 'exit');
  srv._child.stdin.end();
  const [info] = await ev;
  assert.equal(info.code, 0);
  assert.equal(fakeState(dir).stdinEof, 1);
  assert.equal(fakeState(dir).shutdownCalls, 0);
});

test('a service that ignores the stop request is ended after the timeout', async () => {
  const dir = tmpDir();
  const srv = makeServer(dir, { FAKE_IGNORE_STOP: '1' });
  await srv.start();
  const pid = srv.pid;
  const t0 = Date.now();
  await srv.stop({ timeoutMs: 800 });
  const took = Date.now() - t0;
  assert.ok(took >= 700 && took < 8000, `took ${took} ms`);
  assert.equal(await waitFor(() => !alive(pid)), true);
  assert.equal(fakeState(dir).shutdownCalls, 1);
});
