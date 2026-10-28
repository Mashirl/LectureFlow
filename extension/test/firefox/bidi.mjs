// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// A small WebDriver BiDi client (https://w3c.github.io/webdriver-bidi/) over
// Node's own WebSocket, for driving the Firefox that web-ext started: tabs,
// navigation, script evaluation, screenshots. No dependency beyond Node.

export class Bidi {
  static async connect(url, { timeoutMs = 30000 } = {}) {
    const until = Date.now() + timeoutMs;
    let last = null;
    while (Date.now() < until) {
      try {
        const b = new Bidi();
        await b.open(url);
        return b;
      } catch (e) {
        last = e;
        await new Promise((r) => setTimeout(r, 300));
      }
    }
    throw new Error(`no WebDriver BiDi at ${url}: ${last && last.message}`);
  }

  open(url) {
    return new Promise((resolve, reject) => {
      const ws = new WebSocket(url);
      this.ws = ws;
      this.seq = 0;
      this.pending = new Map();
      ws.onopen = () => resolve();
      ws.onerror = (ev) => reject(new Error(ev && ev.message ? ev.message : 'websocket error'));
      ws.onclose = () => {
        for (const p of this.pending.values()) p.reject(new Error('BiDi connection closed'));
        this.pending.clear();
      };
      ws.onmessage = (ev) => {
        let msg;
        try {
          msg = JSON.parse(typeof ev.data === 'string' ? ev.data : Buffer.from(ev.data).toString('utf8'));
        } catch {
          return;
        }
        if (msg.id === undefined) return; // events: not subscribed to any
        const p = this.pending.get(msg.id);
        if (!p) return;
        this.pending.delete(msg.id);
        if (msg.type === 'error') p.reject(new Error(`${msg.error}: ${msg.message}`));
        else p.resolve(msg.result);
      };
    });
  }

  send(method, params = {}, { timeoutMs = 30000 } = {}) {
    const id = ++this.seq;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new Error(`BiDi ${method} timed out`));
      }, timeoutMs);
      this.pending.set(id, {
        resolve: (v) => {
          clearTimeout(timer);
          resolve(v);
        },
        reject: (e) => {
          clearTimeout(timer);
          reject(e);
        },
      });
      this.ws.send(JSON.stringify({ id, method, params }));
    });
  }

  close() {
    try {
      this.ws.close();
    } catch {
      // gone
    }
  }

  async newTab() {
    return (await this.send('browsingContext.create', { type: 'tab' })).context;
  }

  navigate(context, url) {
    return this.send('browsingContext.navigate', { context, url, wait: 'complete' }, { timeoutMs: 30000 });
  }

  /**
   * Runs `body` (the body of an async function) in the page's own realm and
   * returns its JSON-serializable result. `args` are passed as `args`.
   */
  async eval(context, body, args = null) {
    const expression = `(async () => { const args = ${JSON.stringify(args)}; const __r = await (async () => { ${body} })(); return JSON.stringify(__r === undefined ? null : __r); })()`;
    const r = await this.send('script.evaluate', { expression, target: { context }, awaitPromise: true, resultOwnership: 'none' });
    if (r.type === 'exception') throw new Error(`in page: ${r.exceptionDetails && r.exceptionDetails.text}`);
    return r.result && r.result.type === 'string' ? JSON.parse(r.result.value) : null;
  }

  /** Polls `body` until it returns something truthy. */
  async waitFor(context, body, args = null, { timeoutMs = 15000, what = body } = {}) {
    const until = Date.now() + timeoutMs;
    let last = null;
    while (Date.now() < until) {
      try {
        last = await this.eval(context, body, args);
        if (last) return last;
      } catch (e) {
        last = e.message; // the page may be between documents
      }
      await new Promise((r) => setTimeout(r, 200));
    }
    throw new Error(`timed out waiting for: ${what} (last: ${JSON.stringify(last)})`);
  }

  async screenshot(context) {
    return Buffer.from((await this.send('browsingContext.captureScreenshot', { context })).data, 'base64');
  }
}
