// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Starting, watching and stopping the Go service (SPEC-v3 V1.1, v3-brief §1).
// No Electron imports: unit tests drive it with a fake service in Node.
//
// One ServerProcess = one run of the service. start() picks a free port,
// passes a fresh token on stdin (stdin then stays open: when this process
// dies the service reads EOF and stops), waits for the ready line and a
// successful /api/health with the token. A port taken between our check and
// the service's bind is retried with another port (at most 3 times).
'use strict';

const { spawn, execFile } = require('node:child_process');
const { EventEmitter } = require('node:events');
const net = require('node:net');
const path = require('node:path');
const readline = require('node:readline');

const {
  EXIT_RESTART, generateToken, parseReadyLine, extractErrorReason, isPortInUseError, redact,
} = require('./lib/serverutil');
const { TOKEN_HEADER } = require('./lib/security');

const STDERR_KEEP = 200;

class StartError extends Error {
  /** kind: spawn_failed | exited | port_in_use | restart_requested | timeout | token_rejected | bad_mode */
  constructor({ kind, reason = null, code = null }) {
    super(`service did not start: ${kind}${reason ? `: ${reason}` : ''}`);
    this.name = 'StartError';
    this.kind = kind;
    this.reason = reason;
    this.code = code;
  }
}

/** pickFreePort asks the system for a free port on 127.0.0.1. */
function pickFreePort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.unref();
    srv.once('error', reject);
    srv.listen(0, '127.0.0.1', () => {
      const { port } = srv.address();
      srv.close(() => resolve(port));
    });
  });
}

/** isPortFree: nothing listens on 127.0.0.1:port and we may bind it. */
function isPortFree(port) {
  return new Promise((resolve) => {
    const srv = net.createServer();
    srv.unref();
    srv.once('error', () => resolve(false));
    srv.listen(port, '127.0.0.1', () => srv.close(() => resolve(true)));
  });
}

const sleep = (ms) => new Promise((r) => setTimeout(r, Math.max(0, ms)));

class ServerProcess extends EventEmitter {
  /**
   * opts:
   *   command, prefixArgs     executable and leading arguments
   *   argsFor(port)           the service arguments for a port (buildServerArgs)
   *   env, cwd                child environment and working directory
   *   startTimeoutMs          per attempt (default 30 s)
   *   pollIntervalMs          /api/health polling (default 250 ms)
   *   maxPortRetries          default 3
   *   pickPort, fetchImpl     injectable for tests
   *   log                     { info, warn, error }
   */
  constructor(opts) {
    super();
    this.command = opts.command;
    this.prefixArgs = opts.prefixArgs || [];
    this.argsFor = opts.argsFor;
    this.env = opts.env || process.env;
    this.cwd = opts.cwd;
    this.startTimeoutMs = opts.startTimeoutMs || 30000;
    this.pollIntervalMs = opts.pollIntervalMs || 250;
    this.maxPortRetries = opts.maxPortRetries === undefined ? 3 : opts.maxPortRetries;
    this.pickPort = opts.pickPort || pickFreePort;
    this.fetchImpl = opts.fetchImpl || fetch;
    this.log = opts.log || { info() {}, warn() {}, error() {} };

    this.port = null;
    this.token = null;
    this.ready = false;
    this.stopping = false;
    this.attempts = 0;
    this._child = null;
    this._stderr = [];
    this._started = false;
  }

  get origin() {
    return this.port ? `http://127.0.0.1:${this.port}` : null;
  }

  get pid() {
    return this._child ? this._child.pid : null;
  }

  get alive() {
    const c = this._child;
    return !!c && c.exitCode === null && c.signalCode === null;
  }

  stderrTail() {
    return this._stderr.slice();
  }

  async start() {
    if (this._started) throw new Error('ServerProcess.start called twice');
    this._started = true;
    let portRetries = 0;
    for (;;) {
      if (this.stopping) throw new StartError({ kind: 'exited', reason: 'stopped' });
      const port = await this.pickPort();
      if (this.stopping) throw new StartError({ kind: 'exited', reason: 'stopped' });
      const token = generateToken();
      this.attempts++;
      const r = await this._attempt(port, token);
      if (r.ok) {
        this.port = port;
        this.token = token;
        this.ready = true;
        this._watch(this._child);
        this.log.info(`service ready on port ${port} (attempt ${this.attempts})`);
        return { port, origin: this.origin };
      }
      await this._killChild(this._child);
      if (r.kind === 'port_in_use' && portRetries < this.maxPortRetries) {
        portRetries++;
        this.log.warn(`port ${port} not usable, trying another (${portRetries}/${this.maxPortRetries})`);
        continue;
      }
      throw new StartError(r);
    }
  }

  _attempt(port, token) {
    const args = [...this.prefixArgs, ...this.argsFor(port)];
    this._stderr = [];
    this.log.info(`starting service: ${path.basename(this.command)} (port ${port})`);
    let child;
    try {
      child = spawn(this.command, args, { cwd: this.cwd, env: this.env, stdio: ['pipe', 'pipe', 'pipe'], windowsHide: true });
    } catch (err) {
      return Promise.resolve({ ok: false, kind: 'spawn_failed', reason: err.code || err.message });
    }
    this._child = child;
    const stderr = this._stderr;
    const origin = `http://127.0.0.1:${port}`;

    return new Promise((resolve) => {
      let done = false;
      let pollTimer = null;
      let timer = null;
      let polling = false;
      const finish = (r) => {
        if (done) return;
        done = true;
        clearTimeout(timer);
        clearTimeout(pollTimer);
        resolve(r);
      };

      // stdout carries the service's log lines and the ready line; it must
      // be read to the end, or a full pipe would block the service.
      readline.createInterface({ input: child.stdout, crlfDelay: Infinity }).on('line', (line) => {
        const ready = parseReadyLine(line);
        if (ready) {
          if (ready.port !== port) this.log.warn(`ready line names port ${ready.port}, expected ${port}`);
          kick();
        }
      });
      readline.createInterface({ input: child.stderr, crlfDelay: Infinity }).on('line', (line) => {
        const clean = redact(line, token);
        stderr.push(clean);
        if (stderr.length > STDERR_KEEP) stderr.shift();
        this.log.warn(`service: ${clean}`);
      });

      child.stdin.on('error', () => {}); // EPIPE when the service has already gone
      child.stdin.write(`${token}\n`);

      child.once('error', (err) => finish({ ok: false, kind: 'spawn_failed', reason: err.code || err.message }));
      child.once('exit', (code, signal) => {
        // Give stderr a moment to deliver its last line (the ERROR: reason).
        waitClose(child, 500).then(() => {
          const reason = extractErrorReason(stderr);
          let kind = 'exited';
          if (code === EXIT_RESTART) kind = 'restart_requested';
          else if (isPortInUseError(reason)) kind = 'port_in_use';
          finish({ ok: false, kind, reason, code: code === null ? signal : code });
        });
      });

      timer = setTimeout(() => finish({ ok: false, kind: 'timeout', reason: null }), this.startTimeoutMs);

      const poll = async () => {
        if (done || polling) return;
        polling = true;
        let r;
        try {
          r = await this._health(origin, token);
        } finally {
          polling = false;
        }
        if (done) return;
        if (r.status === 200 && r.json && r.json.ok === true) {
          if (r.json.mode !== 'desktop') return finish({ ok: false, kind: 'bad_mode', reason: `mode ${String(r.json.mode).slice(0, 20)}` });
          return finish({ ok: true });
        }
        if (r.status === 401 || r.status === 403) return finish({ ok: false, kind: 'token_rejected', reason: `HTTP ${r.status}` });
        pollTimer = setTimeout(poll, this.pollIntervalMs);
      };
      const kick = () => {
        if (done) return;
        clearTimeout(pollTimer);
        poll();
      };
      pollTimer = setTimeout(poll, this.pollIntervalMs);
    });
  }

  async _health(origin, token) {
    try {
      const res = await this.fetchImpl(`${origin}/api/health`, {
        headers: { [TOKEN_HEADER]: token },
        signal: AbortSignal.timeout(2000),
      });
      let json = null;
      try {
        json = await res.json();
      } catch {
        json = null;
      }
      return { status: res.status, json };
    } catch {
      return { status: 0, json: null }; // not listening yet
    }
  }

  _watch(child) {
    child.once('exit', (code, signal) => {
      this.ready = false;
      waitClose(child, 500).then(() => {
        const info = { code, signal, reason: extractErrorReason(this._stderr), expected: this.stopping };
        this.log[info.expected ? 'info' : 'warn'](`service exited: code ${code} signal ${signal}${info.reason ? ` (${info.reason})` : ''}`);
        this.emit('exit', info);
      });
    });
  }

  /**
   * stop: ask the service to shut down (POST /api/app/shutdown and stdin
   * EOF), wait up to timeoutMs, then end the process tree by force.
   */
  async stop({ timeoutMs = 10000 } = {}) {
    this.stopping = true;
    const child = this._child;
    if (!child || child.exitCode !== null || child.signalCode !== null) return;
    const deadline = Date.now() + timeoutMs;
    const exited = waitExit(child);
    if (this.ready && this.port) {
      try {
        await this.fetchImpl(`${this.origin}/api/app/shutdown`, {
          method: 'POST',
          headers: { [TOKEN_HEADER]: this.token },
          signal: AbortSignal.timeout(Math.max(100, Math.min(2000, deadline - Date.now()))),
        });
      } catch {
        // stdin EOF below asks for the same graceful stop
      }
    }
    try {
      child.stdin.end();
    } catch {
      // already closed
    }
    const inTime = await Promise.race([exited.then(() => true), sleep(deadline - Date.now()).then(() => false)]);
    if (!inTime) {
      this.log.warn(`service did not stop within ${timeoutMs} ms, ending it`);
      await this._killChild(child);
    }
  }

  async _killChild(child) {
    if (!child || child.exitCode !== null || child.signalCode !== null) return;
    const exited = waitExit(child);
    await forceKill(child);
    await Promise.race([exited, sleep(3000)]);
  }
}

function waitExit(child) {
  if (child.exitCode !== null || child.signalCode !== null) return Promise.resolve();
  return new Promise((resolve) => child.once('exit', () => resolve()));
}

function waitClose(child, ms) {
  return new Promise((resolve) => {
    const t = setTimeout(resolve, ms);
    if (child.stdout && child.stdout.readableEnded && child.stderr && child.stderr.readableEnded) {
      clearTimeout(t);
      resolve();
      return;
    }
    child.once('close', () => {
      clearTimeout(t);
      resolve();
    });
  });
}

/** forceKill ends the process and, on Windows, its children (Python workers, ffmpeg). */
function forceKill(child) {
  if (process.platform === 'win32' && child.pid) {
    const taskkill = path.join(process.env.SystemRoot || 'C:\\Windows', 'System32', 'taskkill.exe');
    return new Promise((resolve) => {
      execFile(taskkill, ['/PID', String(child.pid), '/T', '/F'], { windowsHide: true, timeout: 5000 }, () => {
        try {
          child.kill();
        } catch {
          // gone
        }
        resolve();
      });
    });
  }
  try {
    child.kill('SIGKILL');
  } catch {
    // gone
  }
  return Promise.resolve();
}

module.exports = { ServerProcess, StartError, pickFreePort, isPortFree };
