// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// 3.2.0 UI fixes (the UI sweep before the release, .claude/research/test-ui.md):
//   P2   a double click on a row's "启动" / "重试" / "更新" sends the request
//        once and opens one model-choice dialog (actions.js once(),
//        withModelChoice's key; pipeline.js runAction for the cards);
//   L12  the "18 秒后自动重试" countdown goes on: the seconds become a
//        deadline per stage (util.js backoffLeft), the summary and the hover
//        card count down, the card's timer runs only while it is open
//        (dom.js everySecond);
//   L13  Esc closes a hover card under the pointer too, and when the focus
//        is in it the focus stays on its segment (not on the page);
//   L18  the provider dialog's hint names the address as typed
//        (provider_rules.js endpointParts).
// The modules run on a small DOM stand-in with events that bubble up to
// the document, focus, isConnected and dialogs; timers and the clock are
// fakes. web/ui320_test.go starts it with the web directory as a file: URL.

import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

// ---- DOM stand-in ----

class Node {
  constructor() {
    this.parentNode = null;
    this.listeners = {};
  }
  get isConnected() {
    let n = this;
    while (n.parentNode) n = n.parentNode;
    return n === root;
  }
  remove() {
    if (!this.parentNode) return;
    const c = this.parentNode.children;
    c.splice(c.indexOf(this), 1);
    this.parentNode = null;
  }
  addEventListener(type, fn) {
    (this.listeners[type] ||= []).push(fn);
  }
  removeEventListener(type, fn) {
    const l = this.listeners[type] || [];
    if (l.includes(fn)) l.splice(l.indexOf(fn), 1);
  }
  // events bubble up to the document (the html element's parent here)
  dispatchEvent(ev) {
    ev.target ??= this;
    for (let n = this; n && !ev.stopped; n = n === root ? doc : n.parentNode) {
      ev.currentTarget = n;
      for (const fn of [...(n.listeners[ev.type] || [])]) fn(ev);
      if (!ev.bubbles) break;
    }
    return !ev.defaultPrevented;
  }
}
class Text extends Node {
  constructor(s) {
    super();
    this.data = String(s);
    this.children = [];
  }
  get textContent() {
    return this.data;
  }
}
class El extends Node {
  constructor(tag) {
    super();
    this.tagName = String(tag).toUpperCase();
    this.children = [];
    this.attrs = {};
    this.dataset = {};
    this.style = {};
    this.className = '';
    const self = this;
    this.classList = {
      toggle(c, on) {
        const set = new Set(self.className.split(/\s+/).filter(Boolean));
        if (on === undefined ? !set.has(c) : on) set.add(c);
        else set.delete(c);
        self.className = [...set].join(' ');
      },
      add(c) {
        this.toggle(c, true);
      },
      remove(c) {
        this.toggle(c, false);
      },
      contains(c) {
        return self.className.split(/\s+/).includes(c);
      },
    };
  }
  set textContent(v) {
    this.replaceChildren(new Text(v));
  }
  get textContent() {
    return this.children.map((c) => c.textContent).join('');
  }
  setAttribute(k, v) {
    if (k === 'class') this.className = String(v);
    else this.attrs[k] = String(v);
  }
  getAttribute(k) {
    return k in this.attrs ? this.attrs[k] : null;
  }
  removeAttribute(k) {
    delete this.attrs[k];
  }
  append(...cs) {
    for (const c0 of cs) {
      const c = c0 instanceof Node ? c0 : new Text(c0);
      c.remove();
      c.parentNode = this;
      this.children.push(c);
    }
  }
  replaceChildren(...cs) {
    for (const c of [...this.children]) c.remove();
    this.append(...cs);
  }
  contains(n) {
    for (; n; n = n.parentNode) if (n === this) return true;
    return false;
  }
  focus() {
    const prev = doc.activeElement;
    if (prev === this) return;
    doc.activeElement = this;
    if (prev) prev.dispatchEvent(event('focusout', { relatedTarget: this }));
    this.dispatchEvent(event('focusin', { relatedTarget: prev }));
  }
  click() {
    this.dispatchEvent(event('click'));
  }
  showModal() {
    this.open = true;
    dialogs.push(this);
  }
  close() {
    this.open = false;
  }
  getBoundingClientRect() {
    return { top: 0, bottom: 0, left: 0, right: 0 };
  }
  matches(sel) {
    const m = /^([a-z]*)((?:\.[\w-]+)*)(?:\[([\w-]+)(?:="([^"]*)")?\])?$/.exec(sel);
    if (!m) throw new Error('selector not supported here: ' + sel);
    const [, tag, classes, attr, val] = m;
    if (tag && this.tagName !== tag.toUpperCase()) return false;
    const own = this.className.split(/\s+/);
    for (const c of classes.split('.').filter(Boolean)) if (!own.includes(c)) return false;
    if (attr) {
      const v = attr.startsWith('data-') ? this.dataset[attr.slice(5).replace(/-(\w)/g, (_, c) => c.toUpperCase())] : this.getAttribute(attr);
      if (v == null || (val !== undefined && v !== val)) return false;
    }
    return true;
  }
  querySelectorAll(sel) {
    const out = [];
    const walk = (n) => {
      for (const c of n.children) {
        if (c instanceof El) {
          if (c.matches(sel)) out.push(c);
          walk(c);
        }
      }
    };
    walk(this);
    return out;
  }
  querySelector(sel) {
    return this.querySelectorAll(sel)[0] || null;
  }
}

function event(type, o = {}) {
  return {
    type,
    bubbles: type !== 'pointerenter' && type !== 'pointerleave',
    defaultPrevented: false,
    stopped: false,
    preventDefault() {
      this.defaultPrevented = true;
    },
    stopPropagation() {
      this.stopped = true;
    },
    ...o,
  };
}

const dialogs = [];
const store = { getItem: () => null, setItem() {}, removeItem() {} };
const root = new El('html');
const body = new El('body');
root.append(body);
const doc = {
  documentElement: root,
  body,
  activeElement: body,
  listeners: {},
  addEventListener(type, fn) {
    (this.listeners[type] ||= []).push(fn);
  },
  removeEventListener() {},
  querySelector: (sel) => root.querySelector(sel),
  querySelectorAll: (sel) => root.querySelectorAll(sel),
  getElementById: () => null,
  createElement: (t) => new El(t),
  createElementNS: (_, t) => new El(t),
  createTextNode: (s) => new Text(s),
  visibilityState: 'visible',
};

// fake timers (setInterval) and clock (Date.now); setTimeout stays real
const intervals = new Map();
let nextTimer = 1;
let clock = Date.UTC(2026, 9, 27, 12, 0, 0);
const realNow = Date.now;
Date.now = () => clock;
const second = () => {
  clock += 1000;
  for (const fn of [...intervals.values()]) fn();
};

Object.assign(globalThis, {
  Node,
  location: { search: '', hash: '', pathname: '/', origin: 'http://127.0.0.1' },
  localStorage: store,
  sessionStorage: store,
  document: doc,
  addEventListener() {},
  removeEventListener() {},
  dispatchEvent: () => true,
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  innerHeight: 900,
  CSS: { escape: (s) => String(s) },
  setInterval: (fn) => {
    const id = nextTimer++;
    intervals.set(id, fn);
    return id;
  },
  clearInterval: (id) => intervals.delete(id),
});
globalThis.window = globalThis;

const base = process.argv[2];

// fetch: the UI catalogs from the web directory, with the not yet merged
// part web/i18n/_parts/ui-320 laid over them (absent once merged); the
// requests of the actions, answered by `answer` after a short delay and
// recorded in `posts`.
const posts = [];
let answer = () => ({ status: 200, body: {} });
globalThis.fetch = async (url, init = {}) => {
  const u = String(url);
  if (u.startsWith('i18n/')) {
    const cat = JSON.parse(await readFile(new URL(u, base), 'utf8'));
    const loc = u.slice('i18n/'.length).replace(/\.json$/, '');
    try {
      Object.assign(cat, JSON.parse(await readFile(new URL(`i18n/_parts/ui-320.${loc}.json`, base), 'utf8')));
    } catch {
      // merged already
    }
    const text = JSON.stringify(cat);
    return { ok: true, status: 200, json: async () => cat, text: async () => text };
  }
  const method = init.method || 'GET';
  if (method !== 'GET') posts.push(`${method} ${u}`);
  await new Promise((r) => setTimeout(r, 20));
  const a = answer(method, u);
  const text = JSON.stringify(a.body);
  return { ok: a.status < 300, status: a.status, text: async () => text };
};
const settle = (ms = 60) => new Promise((r) => setTimeout(r, ms));

const D = await import(new URL('dom.js', base).href);
const P = await import(new URL('pipeline.js', base).href);
const U = await import(new URL('util.js', base).href);
const A = await import(new URL('actions.js', base).href);
const I = await import(new URL('i18n.js', base).href);
const R = await import(new URL('provider_rules.js', base).href);
const { ApiError } = await import(new URL('api.js', base).href);
await I.setLocale('zh-CN');

const opts = { key: 'lec:L1', lecture: '', sys: () => null, res: () => null, onAction: () => Promise.resolve() };

// ---- L12: the retry countdown ----

function retryStage(w = 'retry_backoff:18') {
  return {
    stage: 'summary', status: 'queued', display_status: 'queued', progress: 0, attempt: 1, error: 'HTTP 503',
    allowed_actions: ['cancel'], started_at: null, finished_at: null, duration_ms: null, waiting_for: w,
  };
}
const asr = { stage: 'asr', status: 'done', display_status: 'done', progress: 1, attempt: 0, error: null, allowed_actions: ['rerun'], duration_ms: 60000 };

{
  // the seconds are a deadline, kept per stage object
  const st = retryStage();
  assert.equal(U.backoffLeft(st), 18);
  clock += 5000;
  assert.equal(U.backoffLeft(st), 13);
  assert.equal(U.stageWaitingText(st), I.t('waiting.retry_backoff', { n: 13 }));
  // a lecture read again starts from the server's number
  const again = retryStage('retry_backoff:13');
  assert.equal(U.backoffLeft(again), 13);
  // the same object with a new reason (the card read the lecture again)
  st.waiting_for = 'retry_backoff:30';
  assert.equal(U.backoffLeft(st), 30);
  st.waiting_for = 'api_slot';
  assert.equal(U.backoffLeft(st), null);
  assert.equal(U.stageWaitingText(st), U.waitingForText('api_slot'));
  // due: "about to retry", not "0 seconds" (nor a number that stays)
  const due = retryStage('retry_backoff:2');
  const stages = [asr, due];
  const text0 = U.lectureSummary(stages, {}).text;
  assert.ok(text0.includes(I.t('waiting.retry_backoff', { n: 2 })), text0);
  assert.equal(U.backoffCounting(stages), true);
  clock += 1000;
  assert.ok(U.lectureSummary(stages, {}).text.includes(I.t('waiting.retry_backoff', { n: 1 })));
  clock += 1500;
  assert.equal(U.backoffLeft(due), 0);
  assert.equal(U.backoffCounting(stages), false);
  const text1 = U.lectureSummary(stages, {}).text;
  assert.ok(text1.includes(I.t('waiting.retry_backoff_due')), text1);
  assert.ok(!I.t('waiting.retry_backoff_due').includes('waiting.'), 'the text of waiting.retry_backoff_due exists (web/i18n or _parts/ui-320)');
}

{
  // dom.js everySecond: one timer, only while something counts; a node out
  // of the document is dropped, fn returning false stops it
  const a = new El('span');
  body.append(a);
  let n = 0;
  D.everySecond(a, () => ++n < 3);
  assert.deepEqual(D.ticking(), { nodes: 1, timer: true });
  second();
  second();
  assert.equal(n, 2);
  second();
  assert.equal(n, 3);
  assert.deepEqual(D.ticking(), { nodes: 0, timer: false }, 'false stops it and the timer');
  const b = new El('span');
  body.append(b);
  D.everySecond(b, () => true);
  b.remove();
  second();
  assert.equal(D.ticking().nodes, 1, 'out of the document once: kept (a redraw may put it back)');
  second();
  assert.deepEqual(D.ticking(), { nodes: 0, timer: false }, 'out for two ticks: dropped');
  const c = new El('span');
  body.append(c);
  D.everySecond(c, () => true);
  D.stopEverySecond(c);
  assert.deepEqual(D.ticking(), { nodes: 0, timer: false });
  c.remove();
  a.remove();
}

const cardWait = (bar) => bar.querySelector('.card-wait').querySelector('.card-v').textContent;

{
  // the hover card counts down while it is open, and runs no timer closed
  const st = retryStage('retry_backoff:18');
  const bar = P.pipelineBar([asr, st], opts);
  body.append(bar);
  const wrap = bar.querySelector('.seg-wrap[data-stage="summary"]');
  assert.equal(cardWait(bar), I.t('waiting.retry_backoff', { n: 18 }));
  assert.deepEqual(D.ticking(), { nodes: 0, timer: false }, 'no timer before the card opens');
  wrap.dispatchEvent(event('pointerenter'));
  assert.deepEqual(D.ticking(), { nodes: 1, timer: true });
  for (let i = 0; i < 5; i++) second();
  assert.equal(cardWait(bar), I.t('waiting.retry_backoff', { n: 13 }));
  wrap.dispatchEvent(event('pointerleave'));
  assert.deepEqual(D.ticking(), { nodes: 0, timer: false }, 'closed: no timer');
  for (let i = 0; i < 20; i++) second();
  wrap.dispatchEvent(event('pointerenter'));
  assert.equal(cardWait(bar), I.t('waiting.retry_backoff_due'), 'opened again after the deadline');
  wrap.dispatchEvent(event('pointerleave'));
  // a finished stage's card has nothing that changes: no timer
  const done = bar.querySelector('.seg-wrap[data-stage="asr"]');
  done.dispatchEvent(event('pointerenter'));
  assert.deepEqual(D.ticking(), { nodes: 0, timer: false });
  done.dispatchEvent(event('pointerleave'));
  bar.remove();
}

// ---- L13: Esc closes the card and keeps the focus on the segment ----

{
  const st = { ...retryStage(), allowed_actions: ['cancel'] };
  const bar = P.pipelineBar([asr, st], opts);
  body.append(bar);
  const wrap = bar.querySelector('.seg-wrap[data-stage="summary"]');
  const seg = wrap.querySelector('.seg');
  const cardBtn = wrap.querySelector('.card-actions').querySelector('button');

  // keyboard: the focus on a button in the card
  seg.focus();
  cardBtn.focus();
  assert.equal(D.ticking().nodes, 1);
  const esc = event('keydown', { key: 'Escape' });
  cardBtn.dispatchEvent(esc);
  assert.ok(wrap.classList.contains('is-dismissed'), 'Esc closes the card');
  assert.equal(doc.activeElement, seg, 'the focus stays on the segment, not the page');
  assert.ok(esc.stopped, 'the Esc is not passed on (a dialog or the inspector would close too)');
  assert.deepEqual(D.ticking(), { nodes: 0, timer: false }, 'a closed card runs no timer');
  // a second Esc has no card to close: passed on
  const esc2 = event('keydown', { key: 'Escape' });
  seg.dispatchEvent(esc2);
  assert.ok(!esc2.stopped);
  // the focus leaves and comes back: the card opens again
  body.focus();
  assert.ok(!wrap.classList.contains('is-dismissed'));
  seg.focus();
  assert.ok(!wrap.classList.contains('is-dismissed'));
  body.focus();

  // the pointer: Esc with the focus elsewhere (on the page)
  wrap.dispatchEvent(event('pointerenter'));
  const esc3 = event('keydown', { key: 'Escape' });
  body.dispatchEvent(esc3);
  assert.ok(wrap.classList.contains('is-dismissed'), 'Esc closes a card under the pointer (WCAG 1.4.13)');
  assert.equal(doc.activeElement, body, 'and moves no focus');
  wrap.dispatchEvent(event('pointerleave'));
  assert.ok(!wrap.classList.contains('is-dismissed'), 'the next hover opens it again');
  // Esc after the pointer left does nothing to it
  body.dispatchEvent(event('keydown', { key: 'Escape' }));
  assert.ok(!wrap.classList.contains('is-dismissed'));
  bar.remove();
}

// ---- P2: one request, one dialog ----

const lec = { id: 'L9', title: 'Deadlocks', course_id: 'C1', lecture_actions: ['launch', 'update'], stages: [] };

{
  // launch: a double click sends it once
  answer = () => ({ status: 200, body: { ...lec, lecture_actions: [] } });
  posts.length = 0;
  const p1 = A.launchLectures([lec]);
  const p2 = A.launchLectures([lec]);
  assert.equal(p1, p2, 'the second click gets the first one\'s promise');
  const [r1, r2] = await Promise.all([p1, p2]);
  assert.deepEqual(posts, ['POST /api/lectures/L9/launch']);
  assert.equal(r1, r2);
  // once answered, a new click is a new request
  await A.launchLectures([lec]);
  assert.equal(posts.length, 2);
}

const needsChoice = {
  status: 409,
  body: { error: { code: 'needs_model_choice' }, diff: [{ stage: 'translate', snapshot: { label: 'deepseek-chat' }, current: { label: 'deepseek-v4' }, snapshot_usable: true }] },
};

async function cancelDialogs(n) {
  await settle();
  const open = dialogs.filter((d) => d.open);
  assert.equal(open.length, n, `${n} model-choice dialog(s) open`);
  for (const d of open) d.dispatchEvent(event('cancel'));
  await settle();
}

{
  // retry of a failed stage (the failure notice and the row): one request,
  // one model-choice dialog
  answer = () => needsChoice;
  posts.length = 0;
  dialogs.length = 0;
  const p1 = A.stageAction(lec, 'translate', 'retry', true);
  const p2 = A.stageAction(lec, 'translate', 'retry', true);
  assert.equal(p1, p2);
  await cancelDialogs(1);
  assert.equal(await p1, null, 'cancelled');
  assert.deepEqual(posts, ['POST /api/lectures/L9/stages/translate/retry']);

  // update of an outdated lecture: the same
  posts.length = 0;
  dialogs.length = 0;
  const u1 = A.updateLectures([lec]);
  const u2 = A.updateLectures([lec]);
  await cancelDialogs(1);
  assert.deepEqual(await Promise.all([u1, u2]), [[], []]);
  assert.deepEqual(posts, ['POST /api/lectures/L9/update']);

  // withModelChoice itself: the same operation's question is not asked twice
  dialogs.length = 0;
  const ask = () => Promise.reject(new ApiError(409, 'x', needsChoice.body));
  const w1 = A.withModelChoice(ask, 'retry', 'stage:L9:translate:retry');
  const w2 = A.withModelChoice(ask, 'retry', 'stage:L9:translate:retry');
  await settle();
  assert.equal(dialogs.filter((d) => d.open).length, 1, 'the second call opens no dialog');
  assert.equal(await w2, null, 'and resolves as if cancelled');
  await cancelDialogs(1);
  assert.equal(await w1, null);
  // without a key (other callers): asked as before
  dialogs.length = 0;
  const w3 = A.withModelChoice(ask, 'retry');
  await cancelDialogs(1);
  assert.equal(await w3, null);
}

{
  // a card's button: a double click runs its action once while it is on its way
  let calls = 0;
  let release;
  const st = { stage: 'translate', status: 'failed', display_status: 'failed', progress: 0, attempt: 3, error: 'x', allowed_actions: ['retry'] };
  const bar = P.pipelineBar([st], { ...opts, onAction: () => (calls++, new Promise((r) => (release = r))) });
  body.append(bar);
  const retry = bar.querySelector('.card-actions').querySelector('button');
  retry.click();
  retry.click();
  await settle(10);
  assert.equal(calls, 1, 'one action for a double click');
  release();
  await settle(10);
  retry.click();
  await settle(10);
  assert.equal(calls, 2, 'clickable again once answered');
  release();
  bar.remove();
}

// ---- L18: the provider dialog's hint names the address ----

{
  const M = '';
  assert.deepEqual(R.endpointParts(`请求发往 ${M}/chat/completions。`, M, ' https://api.deepseek.com/ '), ['请求发往 ', 'https://api.deepseek.com/chat/completions', '。']);
  assert.deepEqual(R.endpointParts(`Requests go to ${M}/chat/completions.`, M, 'http://127.0.0.1:8080/v1'), ['Requests go to ', 'http://127.0.0.1:8080/v1/chat/completions', '.']);
  assert.deepEqual(R.endpointParts(`… Requests go to ${M}/v1/messages.`, M, ''), ['… Requests go to ', 'https://…/v1/messages', '.']);
  assert.deepEqual(R.endpointParts('no mark', M, 'x'), ['no mark']);
  // every language writes the path right after {base_url}
  for (const loc of ['zh-CN', 'zh-TW', 'yue-Hant-HK', 'en', 'ru', 'hi']) {
    await I.setLocale(loc);
    for (const [key, path] of [['settings.provider.endpoint_openai', '/chat/completions'], ['settings.provider.endpoint_anthropic', '/v1/messages']]) {
      const parts = R.endpointParts(I.t(key, { base_url: M }), M, 'https://example.org/');
      assert.equal(parts[1], 'https://example.org' + path, `${loc} ${key}`);
      assert.ok(!parts.join('').includes('{base_url}'), `${loc} ${key}`);
    }
  }
}

Date.now = realNow;
console.log('ui 3.2.0 checks: ok');
