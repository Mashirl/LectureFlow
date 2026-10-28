// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Native Messaging client for the LectureFlow bridge (internal/bridge,
// ext-brief §3). Requests carry an id; the reply has the same type and id,
// `ok`, and `error { code, params }` on failure. `status` messages are pushes
// without an id. The port is opened only when needed (a Chrome service
// worker stays alive while a native port is open) and closed again after an
// idle period with no request in flight and no status subscriber.
//
// Pure: the port comes from an injected connect(), so the tests drive it
// with a fake. Course and lecture ids are strings (database ULIDs); task
// ids are integers.

export const PROTOCOL_VERSION = 1;
export const HOST_NAME = 'org.lectureflow.nmhost';

export const TYPE = Object.freeze({
  hello: 'hello',
  check: 'check',
  enqueue: 'enqueue',
  refresh: 'refresh',
  open: 'open',
  status: 'status',
});

// Errors raised by the client itself, never sent by the app. The app's
// codes (app.not_running, queue.duplicate, ...) come through unchanged.
export const CLIENT_ERRORS = Object.freeze({
  hostUnavailable: 'ext.host_unavailable', // no host registered, or it exited
  timeout: 'ext.timeout',
  tooLarge: 'ext.too_large',
  badReply: 'ext.bad_reply',
  closed: 'ext.closed',
});

const MAX_REQUEST_BYTES = 4 * 1024 * 1024; // bridge.MaxRequest

export class BridgeError extends Error {
  constructor(code, params = {}, reply = null) {
    super(`bridge: ${code}`);
    this.code = code;
    this.params = params || {};
    this.reply = reply;
  }
}

const DEFAULT_TIMEOUTS = { hello: 30000, enqueue: 30000, default: 15000 };

/** The host's answer to a noLaunch hello while LectureFlow is not running. */
export function isNotRunning(err) {
  return !!err && err.code === 'app.not_running' && !!err.params && err.params.reason === 'not_running';
}

/**
 * new NmClient({
 *   connect(): Port            // runtime.connectNative(HOST_NAME)
 *   disconnectError(port): string  // why the port closed (lastError / port.error)
 *   extension: { version, browser, locale },
 *   idleMs, timeouts, setTimer, clearTimer,
 * })
 */
export class NmClient {
  constructor(opts) {
    this.connectFn = opts.connect;
    this.disconnectError = opts.disconnectError || (() => '');
    this.extension = opts.extension;
    this.idleMs = opts.idleMs ?? 10000;
    this.timeouts = { ...DEFAULT_TIMEOUTS, ...(opts.timeouts || {}) };
    this.setTimer = opts.setTimer || ((fn, ms) => setTimeout(fn, ms));
    this.clearTimer = opts.clearTimer || ((t) => clearTimeout(t));
    this.port = null;
    this.pending = new Map(); // id → { type, resolve, reject, timer }
    this.statusListeners = new Set();
    this.stateListeners = new Set();
    // The last status push per task, while connected: the app pushes every
    // unfinished task right after hello, possibly before the popup listens.
    this.statuses = new Map();
    this.helloPromise = null;
    this.helloReply = null;
    this.passive = false; // the current connection's hello carried noLaunch
    this.idleTimer = null;
    this.seq = 0;
    this.gen = 0; // bumped for every connection, so late callbacks of an old one are ignored
    this.prefix = Math.random().toString(36).slice(2, 8);
    this.state = 'idle'; // idle | connecting | connected | unavailable
    this.lastError = null;
  }

  // ------------------------------------------------------------ public

  /**
   * hello on a fresh connection; the cached reply afterwards.
   *
   * noLaunch: a passive query (row states on a page the user merely
   * opened). A new connection then says hello with `noLaunch: true`, and
   * the native host answers app.not_running {reason: "not_running"}
   * instead of starting LectureFlow. Without it (a user action) a
   * connection still being opened passively is not trusted to answer:
   * if it finds LectureFlow not running, a new one, which may start it,
   * is opened.
   */
  hello({ noLaunch = false } = {}) {
    this.ensurePort(noLaunch);
    const p = this.helloPromise;
    if (noLaunch || !this.passive) return p;
    return p.catch((err) => {
      if (!isNotRunning(err)) throw err;
      return this.hello();
    });
  }

  /** Re-checks from scratch (the popup's "重新检测"); may start LectureFlow. */
  async recheck() {
    this.close();
    return this.hello();
  }

  /**
   * check: with titles (same index as sessionIds) and a course, LectureFlow
   * also counts a session as imported when a lecture of that course has its
   * title ("更新这门课程", dedup by title). noLaunch: see hello().
   */
  check(platform, sessionIds, { titles, courseId, noLaunch = false } = {}) {
    const body = { platform, sessionIds: [...sessionIds] };
    if (Array.isArray(titles) && titles.length === body.sessionIds.length) body.titles = titles.map((x) => String(x ?? ''));
    if (courseId) body.courseId = String(courseId);
    return this.request(TYPE.check, body, { noLaunch });
  }

  enqueue(req) {
    return this.request(TYPE.enqueue, {
      profile: req.profile,
      platform: req.platform,
      sessionId: req.sessionId,
      lecture: req.lecture,
      courseId: String(req.courseId),
      tracks: req.tracks,
      referer: req.referer,
      cookies: req.cookies || [],
      maxConnections: req.maxConnections || 0,
      overrideDisabled: !!req.overrideDisabled,
    });
  }

  refresh(req) {
    return this.request(TYPE.refresh, {
      platform: req.platform,
      sessionId: req.sessionId,
      tracks: req.tracks,
      referer: req.referer,
      cookies: req.cookies || [],
    });
  }

  /** Exactly one of lectureId (string) or taskId (integer). */
  open({ lectureId, taskId }) {
    const body = {};
    if (lectureId) body.lectureId = String(lectureId);
    else if (taskId) body.taskId = Number(taskId);
    return this.request(TYPE.open, body);
  }

  /** Status pushes; keeps the connection open while subscribed. */
  onStatus(listener) {
    this.statusListeners.add(listener);
    this.cancelIdle();
    return () => {
      this.statusListeners.delete(listener);
      this.maybeIdle();
    };
  }

  /** The last status pushed for a task on this connection, or null. */
  latestStatus(taskId) {
    return this.statuses.get(Number(taskId)) || null;
  }

  /** Connection state changes: idle | connecting | connected | unavailable. */
  onState(listener) {
    this.stateListeners.add(listener);
    return () => this.stateListeners.delete(listener);
  }

  /** Sends a request after hello; resolves with the reply, rejects with BridgeError. */
  async request(type, body, { noLaunch = false } = {}) {
    await this.hello({ noLaunch });
    return this.send(type, body);
  }

  close() {
    this.cancelIdle();
    this.gen++;
    const port = this.port;
    this.port = null;
    this.helloPromise = null;
    this.helloReply = null;
    this.statuses.clear();
    if (port) {
      try {
        port.disconnect();
      } catch {
        // already gone
      }
    }
    this.failAll(new BridgeError(CLIENT_ERRORS.closed));
    if (this.state !== 'unavailable') this.setState('idle');
  }

  // ----------------------------------------------------------- private

  ensurePort(noLaunch = false) {
    if (this.port) return;
    this.cancelIdle();
    const gen = ++this.gen;
    // Whether this connection's hello asks the host not to start LectureFlow.
    this.passive = !!noLaunch;
    this.setState('connecting');
    let port;
    try {
      port = this.connectFn();
    } catch (e) {
      this.lastError = String(e && e.message ? e.message : e);
      this.setState('unavailable');
      const err = new BridgeError(CLIENT_ERRORS.hostUnavailable, { message: this.lastError });
      this.helloPromise = Promise.reject(err);
      this.helloPromise.catch(() => {});
      return;
    }
    this.port = port;
    port.onMessage.addListener((msg) => this.onMessage(port, msg));
    port.onDisconnect.addListener(() => this.onDisconnect(port));
    const helloBody = { protocol: PROTOCOL_VERSION, extension: { ...this.extension } };
    if (noLaunch) helloBody.noLaunch = true;
    this.helloPromise = this.send(TYPE.hello, helloBody).then(
      (reply) => {
        if (gen === this.gen) {
          this.helloReply = reply;
          this.passive = false; // LectureFlow is running: an ordinary connection now
          this.lastError = null;
          this.setState('connected');
          this.maybeIdle();
        }
        return reply;
      },
      (err) => {
        // hello failed: app not running, protocol mismatch, no host.
        if (gen === this.gen) {
          this.lastError = err.code;
          if (this.port === port) {
            this.port = null;
            try {
              port.disconnect();
            } catch {
              // gone
            }
          }
          this.setState('unavailable');
        }
        throw err;
      },
    );
    this.helloPromise.catch(() => {});
  }

  send(type, body) {
    const port = this.port;
    if (!port) return Promise.reject(new BridgeError(CLIENT_ERRORS.hostUnavailable, { message: this.lastError || '' }));
    const id = `${this.prefix}-${++this.seq}`;
    const msg = { type, id, ...body };
    const size = new TextEncoder().encode(JSON.stringify(msg)).length;
    if (size > MAX_REQUEST_BYTES) return Promise.reject(new BridgeError(CLIENT_ERRORS.tooLarge, { bytes: size }));
    this.cancelIdle();
    return new Promise((resolve, reject) => {
      const ms = this.timeouts[type] ?? this.timeouts.default;
      const timer = this.setTimer(() => {
        if (!this.pending.has(id)) return;
        this.pending.delete(id);
        reject(new BridgeError(CLIENT_ERRORS.timeout, { type }));
        this.maybeIdle();
      }, ms);
      this.pending.set(id, { type, resolve, reject, timer });
      try {
        port.postMessage(msg);
      } catch (e) {
        this.clearTimer(timer);
        this.pending.delete(id);
        reject(new BridgeError(CLIENT_ERRORS.hostUnavailable, { message: String(e && e.message ? e.message : e) }));
      }
    });
  }

  onMessage(port, msg) {
    if (port !== this.port || !msg || typeof msg !== 'object') return;
    if (msg.type === TYPE.status && !msg.id) {
      if (Number.isInteger(msg.taskId)) {
        this.statuses.delete(msg.taskId);
        this.statuses.set(msg.taskId, msg);
        if (this.statuses.size > 200) this.statuses.delete(this.statuses.keys().next().value);
      }
      for (const l of this.statusListeners) {
        try {
          l(msg);
        } catch {
          // a listener's problem
        }
      }
      return;
    }
    const p = msg.id !== undefined ? this.pending.get(msg.id) : undefined;
    if (!p) {
      // The host answers a request it could not deliver with
      // app.not_running and the request's id; anything else unknown is
      // ignored.
      return;
    }
    this.pending.delete(msg.id);
    this.clearTimer(p.timer);
    if (msg.type !== p.type) {
      p.reject(new BridgeError(CLIENT_ERRORS.badReply, { expected: p.type, got: String(msg.type) }, msg));
    } else if (msg.ok === true) {
      p.resolve(msg);
    } else {
      const e = msg.error && typeof msg.error === 'object' ? msg.error : {};
      p.reject(new BridgeError(String(e.code || CLIENT_ERRORS.badReply), e.params || {}, msg));
    }
    this.maybeIdle();
  }

  onDisconnect(port) {
    if (port !== this.port) return;
    this.port = null;
    this.helloPromise = null;
    this.helloReply = null;
    this.statuses.clear();
    const why = this.disconnectError(port) || '';
    this.lastError = why || this.lastError;
    this.failAll(new BridgeError(CLIENT_ERRORS.hostUnavailable, { message: why }));
    this.setState('unavailable');
  }

  failAll(err) {
    for (const [id, p] of this.pending) {
      this.clearTimer(p.timer);
      this.pending.delete(id);
      p.reject(err);
    }
  }

  maybeIdle() {
    if (!this.port || this.pending.size > 0 || this.statusListeners.size > 0 || this.idleTimer) return;
    this.idleTimer = this.setTimer(() => {
      this.idleTimer = null;
      if (this.pending.size === 0 && this.statusListeners.size === 0) this.close();
    }, this.idleMs);
  }

  cancelIdle() {
    if (this.idleTimer) {
      this.clearTimer(this.idleTimer);
      this.idleTimer = null;
    }
  }

  setState(s) {
    if (this.state === s) return;
    this.state = s;
    for (const l of this.stateListeners) {
      try {
        l(s, this.lastError);
      } catch {
        // ignore
      }
    }
  }
}

/** Popup view of the connection: what to show and whether to retry. */
export function connectionView(state, helloReply, lastError) {
  if (state === 'connected' && helloReply) {
    return { status: 'connected', appVersion: helloReply.app?.version || '', courses: helloReply.courses || [] };
  }
  if (state === 'connecting' || state === 'idle') return { status: 'checking' };
  return { status: 'unavailable', reason: lastError || '' };
}
