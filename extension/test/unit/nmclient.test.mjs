// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { NmClient, BridgeError, CLIENT_ERRORS, PROTOCOL_VERSION, HOST_NAME, connectionView, isNotRunning } from '../../src/lib/nmclient.js';

// A fake native port with WebExtension Port's shape; `app` answers posted
// messages (or not) the way the bridge would.
function fakePort(app) {
  const msgL = [];
  const discL = [];
  const port = {
    sent: [],
    disconnected: false,
    onMessage: { addListener: (f) => msgL.push(f) },
    onDisconnect: { addListener: (f) => discL.push(f) },
    postMessage(m) {
      port.sent.push(JSON.parse(JSON.stringify(m)));
      queueMicrotask(() => app(m, port));
    },
    disconnect() {
      port.disconnected = true;
    },
    reply: (m) => msgL.forEach((f) => f(m)),
    drop: () => discL.forEach((f) => f()),
  };
  return port;
}

const HELLO_OK = (m) => ({ type: 'hello', id: m.id, ok: true, protocol: 1, app: { version: '3.2.0', running: true, mode: 'desktop' }, courses: [{ id: '01JABCDEFGHJKMNPQRSTVWXYZ0', name: 'Cryptography' }] });

// Timers under the test's control.
function clock() {
  const timers = new Map();
  let n = 0;
  return {
    setTimer: (fn, ms) => {
      timers.set(++n, { fn, ms });
      return n;
    },
    clearTimer: (id) => timers.delete(id),
    fire(predicate = () => true) {
      for (const [id, t] of [...timers]) {
        if (predicate(t)) {
          timers.delete(id);
          t.fn();
        }
      }
    },
    timers,
  };
}

function client(app, extra = {}) {
  const ports = [];
  const c = clock();
  const nm = new NmClient({
    connect: () => {
      const p = fakePort(app);
      ports.push(p);
      return p;
    },
    disconnectError: () => 'Native host has exited.',
    extension: { version: '0.1.0', browser: 'chrome', locale: 'zh-CN' },
    setTimer: c.setTimer,
    clearTimer: c.clearTimer,
    ...extra,
  });
  return { nm, ports, clock: c };
}

test('connects only when asked, says hello first with the protocol version', async () => {
  const { nm, ports } = client((m, p) => {
    if (m.type === 'hello') p.reply(HELLO_OK(m));
    if (m.type === 'check') p.reply({ type: 'check', id: m.id, ok: true, items: [{ sessionId: 's1', state: 'imported', lectureId: '01JLECTURE0000000000000000' }] });
  });
  assert.equal(ports.length, 0);
  assert.equal(nm.state, 'idle');
  const reply = await nm.check('panopto', ['s1', 's2']);
  assert.equal(ports.length, 1);
  const [hello, check] = ports[0].sent;
  assert.deepEqual(hello, { type: 'hello', id: hello.id, protocol: PROTOCOL_VERSION, extension: { version: '0.1.0', browser: 'chrome', locale: 'zh-CN' } });
  assert.deepEqual(check, { type: 'check', id: check.id, platform: 'panopto', sessionIds: ['s1', 's2'] });
  assert.notEqual(hello.id, check.id);
  assert.equal(typeof reply.items[0].lectureId, 'string');
  assert.equal(nm.state, 'connected');
  assert.equal(HOST_NAME, 'org.lectureflow.nmhost');
});

test('replies match by id and type, whatever the order', async () => {
  let held = [];
  const { nm, ports } = client((m, p) => {
    if (m.type === 'hello') p.reply(HELLO_OK(m));
    else held.push(m);
  });
  await nm.hello();
  const a = nm.open({ lectureId: '01JLECTURE0000000000000000' });
  const b = nm.open({ taskId: 7 });
  await new Promise((r) => setTimeout(r, 0));
  assert.equal(held.length, 2);
  assert.deepEqual(held.map((m) => [m.lectureId, m.taskId]), [['01JLECTURE0000000000000000', undefined], [undefined, 7]]);
  // answer the second first; an unknown id is ignored
  ports[0].reply({ type: 'open', id: 'nope', ok: true });
  ports[0].reply({ type: 'open', id: held[1].id, ok: true, which: 'b' });
  ports[0].reply({ type: 'open', id: held[0].id, ok: true, which: 'a' });
  assert.equal((await a).which, 'a');
  assert.equal((await b).which, 'b');
  held = [];
});

test('enqueue sends string course ids and the frozen field names', async () => {
  const { nm, ports } = client((m, p) => {
    if (m.type === 'hello') p.reply(HELLO_OK(m));
    if (m.type === 'enqueue') p.reply({ type: 'enqueue', id: m.id, ok: true, taskId: 12, position: 2 });
  });
  const r = await nm.enqueue({
    profile: { id: 'panopto', version: '1.0.0', reviewed: true },
    platform: 'panopto',
    sessionId: 's1',
    lecture: { title: 'T', recordedAt: '2026-10-21T09:35', durationSec: 3300 },
    courseId: '01JCOURSE00000000000000000',
    tracks: [{ url: 'https://cdn.example.org/a.m3u8', kind: 'audio', format: 'hls', role: 'primary-audio', label: 'Lecture audio', sizeBytes: -1 }],
    referer: 'https://x.cloud.panopto.eu/Panopto/Pages/Viewer.aspx?id=s1',
    cookies: [],
    maxConnections: 4,
  });
  assert.equal(r.taskId, 12);
  const sent = ports[0].sent[1];
  assert.deepEqual(Object.keys(sent).sort(), ['cookies', 'courseId', 'id', 'lecture', 'maxConnections', 'overrideDisabled', 'platform', 'profile', 'referer', 'sessionId', 'tracks', 'type']);
  assert.equal(sent.courseId, '01JCOURSE00000000000000000');
  assert.equal(sent.overrideDisabled, false);
});

test('ok:false becomes a BridgeError with the app\'s code and params', async () => {
  const { nm } = client((m, p) => {
    if (m.type === 'hello') p.reply(HELLO_OK(m));
    if (m.type === 'enqueue') p.reply({ type: 'enqueue', id: m.id, ok: false, error: { code: 'queue.duplicate', params: { state: 'imported', lectureId: '01JX' } } });
  });
  await assert.rejects(nm.enqueue({ courseId: 'c' }), (e) => e instanceof BridgeError && e.code === 'queue.duplicate' && e.params.lectureId === '01JX');
});

test('a reply of the wrong type is rejected', async () => {
  const { nm } = client((m, p) => {
    if (m.type === 'hello') p.reply(HELLO_OK(m));
    if (m.type === 'check') p.reply({ type: 'open', id: m.id, ok: true });
  });
  await assert.rejects(nm.check('generic', []), (e) => e.code === CLIENT_ERRORS.badReply);
});

test('status pushes reach subscribers; the port stays open while subscribed', async () => {
  const { nm, ports, clock: c } = client((m, p) => {
    if (m.type === 'hello') p.reply(HELLO_OK(m));
  });
  await nm.hello();
  const got = [];
  const off = nm.onStatus((s) => got.push(s.state));
  ports[0].reply({ type: 'status', taskId: 3, platform: 'panopto', sessionId: 's', courseId: 'c', state: 'downloading', position: 0, bytesDone: 1, bytesTotal: 10 });
  ports[0].reply({ type: 'status', taskId: 3, state: 'done' });
  assert.deepEqual(got, ['downloading', 'done']);
  c.fire();
  assert.equal(ports[0].disconnected, false);
  off();
  c.fire((t) => t.ms === 10000); // idle timeout
  assert.equal(ports[0].disconnected, true);
  assert.equal(nm.state, 'idle');
});

test('no host: connectNative throws or the port drops at once → unavailable', async () => {
  const thrower = new NmClient({ connect: () => { throw new Error('Access to the specified native messaging host is forbidden.'); }, extension: {} });
  await assert.rejects(thrower.hello(), (e) => e.code === CLIENT_ERRORS.hostUnavailable);
  assert.equal(thrower.state, 'unavailable');

  const { nm, ports } = client((m, p) => p.drop());
  const states = [];
  nm.onState((s) => states.push(s));
  await assert.rejects(nm.check('generic', ['x']), (e) => e.code === CLIENT_ERRORS.hostUnavailable && e.params.message === 'Native host has exited.');
  assert.equal(nm.state, 'unavailable');
  assert.deepEqual(states, ['connecting', 'unavailable']);
  // "重新检测" connects again
  await assert.rejects(nm.recheck());
  assert.equal(ports.length, 2);
});

test('the host answering app.not_running to hello leaves the client unavailable', async () => {
  // the host answers, then exits
  const { nm } = client((m, p) => {
    p.reply({ type: m.type, id: m.id, ok: false, error: { code: 'app.not_running', params: { reason: 'timeout' } } });
    p.drop();
  });
  await assert.rejects(nm.hello(), (e) => e.code === 'app.not_running' && e.params.reason === 'timeout');
  assert.equal(nm.state, 'unavailable');
  assert.equal(nm.lastError, 'app.not_running');
  // a host that answers and stays: the client closes the port itself
  const second = client((m, p) => p.reply({ type: m.type, id: m.id, ok: false, error: { code: 'protocol.unsupported', params: { app: 2, extension: 1 } } }));
  await assert.rejects(second.nm.hello(), (e) => e.code === 'protocol.unsupported');
  assert.equal(second.ports[0].disconnected, true);
  assert.equal(second.nm.state, 'unavailable');
  assert.deepEqual(connectionView(nm.state, null, nm.lastError), { status: 'unavailable', reason: 'app.not_running' });
});

test('timeouts reject the request, not the connection', async () => {
  const { nm, clock: c } = client((m, p) => {
    if (m.type === 'hello') p.reply(HELLO_OK(m));
  });
  await nm.hello();
  const pending = nm.check('generic', ['x']);
  await new Promise((r) => setTimeout(r, 0));
  c.fire((t) => t.ms === 15000);
  await assert.rejects(pending, (e) => e.code === CLIENT_ERRORS.timeout);
  assert.equal(nm.state, 'connected');
});

test('a request over 4 MiB is refused before it is sent', async () => {
  const { nm, ports } = client((m, p) => {
    if (m.type === 'hello') p.reply(HELLO_OK(m));
  });
  await nm.hello();
  await assert.rejects(nm.refresh({ platform: 'p', sessionId: 's', tracks: [], referer: 'x'.repeat(4 * 1024 * 1024), cookies: [] }), (e) => e.code === CLIENT_ERRORS.tooLarge);
  assert.equal(ports[0].sent.length, 1);
});

test('disconnect while requests are in flight rejects them all', async () => {
  const { nm, ports } = client((m, p) => {
    if (m.type === 'hello') p.reply(HELLO_OK(m));
  });
  await nm.hello();
  const a = nm.check('generic', ['1']);
  const b = nm.check('generic', ['2']);
  await new Promise((r) => setTimeout(r, 0));
  ports[0].drop();
  await assert.rejects(a, (e) => e.code === CLIENT_ERRORS.hostUnavailable);
  await assert.rejects(b, (e) => e.code === CLIENT_ERRORS.hostUnavailable);
  assert.deepEqual(connectionView('connected', { app: { version: '3.2.0' }, courses: [] }), { status: 'connected', appVersion: '3.2.0', courses: [] });
  assert.deepEqual(connectionView('connecting'), { status: 'checking' });
});

test('the last status of each task is kept while connected (pushed right after hello, before anyone listens)', async () => {
  const { nm, ports } = client((m, p) => {
    if (m.type === 'hello') {
      p.reply(HELLO_OK(m));
      p.reply({ type: 'status', taskId: 7, state: 'queued', position: 2 });
    }
  });
  await nm.hello();
  await new Promise((r) => setTimeout(r, 0));
  assert.equal(nm.latestStatus(7).position, 2);
  ports[0].reply({ type: 'status', taskId: 7, state: 'downloading', bytesDone: 5, bytesTotal: 10 });
  assert.equal(nm.latestStatus(7).state, 'downloading');
  assert.equal(nm.latestStatus(8), null);
  ports[0].drop();
  assert.equal(nm.latestStatus(7), null, 'gone with the connection');
});

test('check sends titles and the course only when given, titles aligned with the ids', async () => {
  const { nm, ports } = client((m, p) => {
    if (m.type === 'hello') p.reply(HELLO_OK(m));
    if (m.type === 'check') p.reply({ type: 'check', id: m.id, ok: true, items: [] });
  });
  await nm.check('panopto', ['a', 'b']);
  await nm.check('panopto', ['a', 'b'], { titles: ['A title', ''], courseId: '01JCOURSE' });
  await nm.check('panopto', ['a', 'b'], { titles: ['only one'] });
  const checks = ports[0].sent.filter((m) => m.type === 'check');
  assert.deepEqual(Object.keys(checks[0]).sort(), ['id', 'platform', 'sessionIds', 'type']);
  assert.deepEqual(checks[1].titles, ['A title', '']);
  assert.equal(checks[1].courseId, '01JCOURSE');
  assert.equal(checks[2].titles, undefined, 'misaligned titles are not sent');
});

// The native host's noLaunch rule (internal/nmhost, NOTES "ext core3" 12):
// when LectureFlow is not running and the first frame is a hello with
// noLaunch, it answers app.not_running {reason: not_running} and exits;
// any other first frame starts LectureFlow.
function hostApp(world) {
  return (m, p) => {
    if (m.type === 'hello') {
      if (!world.running && m.noLaunch === true) {
        p.reply({ type: 'hello', id: m.id, ok: false, error: { code: 'app.not_running', params: { reason: 'not_running' } } });
        p.drop();
        return;
      }
      if (!world.running) {
        world.running = true;
        world.launches++;
      }
      p.reply(HELLO_OK(m));
      return;
    }
    if (m.type === 'check') p.reply({ type: 'check', id: m.id, ok: true, items: [] });
  };
}

test('noLaunch: a passive check says hello with noLaunch and never starts LectureFlow', async () => {
  const world = { running: false, launches: 0 };
  const { nm, ports } = client(hostApp(world));
  await assert.rejects(nm.check('panopto', ['s1'], { noLaunch: true }), (e) => isNotRunning(e) && e.code === 'app.not_running');
  assert.equal(ports.length, 1);
  assert.deepEqual(ports[0].sent.map((m) => [m.type, m.noLaunch]), [['hello', true]], 'the check itself is never sent');
  assert.equal(world.launches, 0);
  assert.equal(nm.state, 'unavailable');
  // the next passive query asks again, still without starting it
  await assert.rejects(nm.hello({ noLaunch: true }), isNotRunning);
  assert.equal(world.launches, 0);
  // a user action (no noLaunch) opens a connection that may start it
  await nm.check('panopto', ['s1']);
  assert.equal(world.launches, 1);
  assert.equal(ports.length, 3);
  assert.equal(ports[2].sent[0].type, 'hello');
  assert.equal('noLaunch' in ports[2].sent[0], false, 'an active hello carries no noLaunch at all');
  assert.equal(nm.state, 'connected');
});

test('noLaunch: when LectureFlow runs, the passive connection is an ordinary one', async () => {
  const world = { running: true, launches: 0 };
  const { nm, ports } = client(hostApp(world));
  await nm.check('panopto', ['s1'], { noLaunch: true });
  assert.equal(ports[0].sent[0].noLaunch, true);
  assert.equal(nm.state, 'connected');
  // later user actions reuse it: no second hello, no new port
  await nm.check('panopto', ['s2']);
  await nm.hello();
  assert.equal(ports.length, 1);
  assert.deepEqual(ports[0].sent.map((m) => m.type), ['hello', 'check', 'check']);
});

test('noLaunch: a user action while a passive hello is in flight is not lost to "not running"', async () => {
  const world = { running: false, launches: 0 };
  const { nm, ports } = client(hostApp(world));
  const passive = nm.check('panopto', ['row'], { noLaunch: true });
  const active = nm.check('panopto', ['clicked']); // the user clicks before the host answers
  await assert.rejects(passive, isNotRunning);
  const r = await active;
  assert.deepEqual(r.items, []);
  assert.equal(world.launches, 1);
  assert.equal(ports.length, 2);
  assert.equal(ports[0].sent[0].noLaunch, true);
  assert.equal('noLaunch' in ports[1].sent[0], false);
  assert.deepEqual(ports[1].sent.map((m) => m.type), ['hello', 'check']);
  assert.equal(nm.state, 'connected');
});

test('noLaunch: a passive query during a launching hello waits for it instead of asking again', async () => {
  const world = { running: false, launches: 0 };
  const { nm, ports } = client(hostApp(world));
  const active = nm.hello();
  const passive = nm.check('panopto', ['row'], { noLaunch: true });
  await active;
  await passive;
  assert.equal(world.launches, 1);
  assert.equal(ports.length, 1);
  assert.equal('noLaunch' in ports[0].sent[0], false);
});

test('noLaunch: other refusals are not retried, and "重新检测" always may start LectureFlow', async () => {
  // no launch.json: the host says not_installed whatever the hello says
  let hellos = 0;
  const { nm, ports } = client((m, p) => {
    hellos++;
    p.reply({ type: m.type, id: m.id, ok: false, error: { code: 'app.not_running', params: { reason: 'not_installed' } } });
    p.drop();
  });
  const passive = nm.hello({ noLaunch: true });
  const active = nm.hello();
  await assert.rejects(passive, (e) => e.params.reason === 'not_installed');
  await assert.rejects(active, (e) => e.params.reason === 'not_installed');
  assert.equal(hellos, 1, 'not_installed is not a reason to try again');
  await assert.rejects(nm.recheck());
  assert.equal(ports.length, 2);
  assert.equal('noLaunch' in ports[1].sent[0], false);
});
