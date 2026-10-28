// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// The diagnostics row "浏览器扩展连接" (check browser_ext; SPEC-ext "3. Native
// Messaging 宿主", .claude/ext-brief.md §4; ext web3, web-builder). The row's
// status, sentence and hint are the server's (internal/api/system_ext.go:
// the endpoint, an isolated instance, another protocol, a missing host
// program, browsers not registered, or registered and the last
// connection). Under it, from GET /api/nmhost, what the sentence does not
// spell out:
//   - each browser's registration (registered / not registered / host
//     program missing / not installed, and on macOS blocked by the system /
//     not opened yet; the manifest path on hover);
//   - the latest extension connection since LectureFlow started (browser,
//     extension version, when), unless the row's sentence already says it
//     (check.browser_ext_connected / _registered);
//   - that the unpacked development extension may connect too
//     (st.dev_extension: "build" in a development build, "env" with
//     LECTUREFLOW_DEV_EXTENSION=1; nm-harden);
//   - "重新登记" (POST /api/nmhost/register; only an instance that manages
//     the registration) with the latest registration's time, or its failure
//     with the English reason under "查看详情"; on macOS what it could not
//     do (last_register.blocked: allow Full Disk Access; not_ready: open
//     the browser once) in its own lines (mac-fix). After registering, the
//     diagnostics run again (the row is the server's) and the details are
//     read again. Nothing changes optimistically.
// While the service is not running (503) the row says so and nothing is
// added.

import { h, clear, toast } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import * as U from './util.js';
import { nmhostStatus, nmhostRegister } from './api_ext.js';

// nmhost.State* → [chip class, text key]
const STATES = {
  registered: ['chip-ok', 'system.ext.state.registered'],
  not_registered: ['chip-warn', 'system.ext.state.not_registered'],
  host_missing: ['chip-warn', 'system.ext.state.host_missing'], // warn like the row (registering again fixes it)
  not_installed: ['chip-quiet', 'system.ext.state.not_installed'],
  blocked: ['chip-warn', 'system.ext.state.blocked'], // macOS refused the directory: Full Disk Access
  not_ready: ['chip-warn', 'system.ext.state.not_ready'], // macOS: the browser has not been opened yet
};
// nmhost.Browser* in the order the server reports them
const BROWSERS = {
  chrome: 'system.ext.browser.chrome',
  edge: 'system.ext.browser.edge',
  firefox: 'system.ext.browser.firefox',
};
// why the development extension may connect (nmhost.DevExtension*) → text key
const DEV_EXTENSION = {
  build: 'system.ext.dev_extension_build',
  env: 'system.ext.dev_extension_env',
};
// row sentences that already name the latest connection (or its absence)
const SAYS_CONNECTION = new Set(['check.browser_ext_connected', 'check.browser_ext_registered']);

const browserName = (b) => (BROWSERS[b] ? t(BROWSERS[b]) : b);
const browserList = (list) => (list || []).map(browserName).join(', ');
// what a registration could not do on macOS (nmhost.RegisterResult)
const macProblems = (r) => !!r && ((r.blocked || []).length > 0 || (r.not_ready || []).length > 0);

// opts: {recheck() → Promise (the page's "重新检查")}
export function createBrowserExt(opts) {
  let status = null;
  let loadError = '';
  let unavailable = false; // 503: the service has not started (the row says so)
  let loading = false;
  let registering = false;
  let requestError = ''; // POST failed as a request (not the registration itself)
  let rowCode = '';
  let seq = 0;
  let alive = true;
  const el = h('div', { class: 'sys-ext', 'aria-live': 'polite' });

  async function refresh() {
    const my = ++seq;
    loading = true;
    if (!status) render();
    try {
      const v = await nmhostStatus();
      if (!alive || my !== seq) return;
      status = v;
      loadError = '';
      unavailable = false;
    } catch (e) {
      if (!alive || my !== seq) return;
      unavailable = e.status === 503;
      if (unavailable) status = null;
      loadError = unavailable ? '' : e.message || t('common.load_failed');
    }
    loading = false;
    render();
  }

  async function register() {
    if (registering) return;
    registering = true;
    requestError = '';
    render();
    try {
      const v = await nmhostRegister();
      if (!alive) return;
      status = v;
      loadError = '';
      const r = v && v.last_register;
      if (!(r && r.error) && !macProblems(r)) toast(t('system.ext.registered_toast'), 'success');
    } catch (e) {
      if (!alive) return;
      requestError = e.message || t('common.load_failed');
    }
    registering = false;
    render();
    // the row's status is the server's: check again, then read the details
    try {
      await opts.recheck();
    } finally {
      if (alive) refresh();
    }
  }

  function chip(state) {
    const [cls, key] = STATES[state] || ['chip-quiet', ''];
    return h('span', { class: `chip ${cls}` }, key ? t(key) : state);
  }

  function line(tone, ic, ...content) {
    return h('p', { class: `sys-ext-line${tone ? ' tone-' + tone : ''}` }, icon(ic, { size: 13 }), h('span', null, ...content));
  }

  function connectionLine(c) {
    if (SAYS_CONNECTION.has(rowCode)) return null;
    if (!c) return line('', 'plug', t('system.ext.no_conn'));
    const text = t('system.ext.last_conn', { browser: browserName(c.browser), version: c.extension_version || '?', when: c.at ? U.relTime(c.at) : '' });
    return line('', 'plug', h('span', { title: c.at ? U.fmtDateTime(c.at) : undefined }, text));
  }

  // the registration admits the unpacked development extension as well
  function devLine(why) {
    if (!why) return null;
    return line('', 'braces', DEV_EXTENSION[why] ? t(DEV_EXTENSION[why]) : why);
  }

  // macOS: the browsers the system refused (Full Disk Access) and those
  // not opened yet, each with what to do
  function macLines(r) {
    return [
      (r.blocked || []).length ? line('warn', 'triangle-alert', t('system.ext.register_blocked', { browsers: browserList(r.blocked) })) : null,
      (r.not_ready || []).length ? line('warn', 'triangle-alert', t('system.ext.register_not_ready', { browsers: browserList(r.not_ready) })) : null,
    ].filter(Boolean);
  }

  function registerResult(r) {
    if (!r) return null;
    const when = r.at ? U.relTime(r.at) : '';
    const notes = (r.notes || []).filter(Boolean);
    if (r.error) {
      return h(
        'div',
        { class: 'sys-ext-reg is-err' },
        line('err', 'circle-alert', t('system.ext.register_failed', { when })),
        ...macLines(r),
        h('details', { class: 'sys-ext-details' }, h('summary', null, t('system.ext.details')), h('pre', { class: 'mono' }, [r.error, ...notes].join('\n'))),
      );
    }
    return h(
      'div',
      { class: 'sys-ext-reg' },
      h('p', { class: 'sys-ext-line is-quiet' }, h('span', { title: r.at ? U.fmtDateTime(r.at) : undefined }, t('system.ext.last_register', { when }))),
      ...macLines(r),
      notes.length ? h('details', { class: 'sys-ext-details' }, h('summary', null, t('system.ext.notes', { n: notes.length })), h('pre', { class: 'mono' }, notes.join('\n'))) : null,
    );
  }

  function render() {
    clear(el);
    if (!status) {
      if (loadError) el.append(line('warn', 'triangle-alert', t('system.ext.status_failed', { error: loadError })));
      else if (loading && !unavailable) el.append(h('p', { class: 'sys-ext-line is-quiet' }, icon('loader-circle', { size: 13, class: 'spin' }), h('span', null, t('system.ext.loading'))));
      return;
    }
    const st = status;
    const browsers = (st.browsers || []).map((b) =>
      h('li', { class: 'sys-ext-browser', title: b.manifest || undefined, dataset: { browser: b.browser, state: b.state } }, h('span', { class: 'sys-ext-bname' }, browserName(b.browser)), chip(b.state)),
    );
    // (Element.append would print a null: the optional parts are filtered)
    el.append(
      ...[
        st.managed && browsers.length ? h('ul', { class: 'sys-ext-browsers', 'aria-label': t('system.ext.browsers') }, browsers) : null,
        st.managed ? devLine(st.dev_extension) : null,
        connectionLine(st.last_connection),
        loadError ? line('warn', 'triangle-alert', t('system.ext.status_failed', { error: loadError })) : null,
      ].filter(Boolean),
    );
    if (!st.managed) return;
    el.append(
      h(
        'div',
        { class: 'sys-ext-actions' },
        h(
          'button',
          { type: 'button', class: 'btn btn-sm', disabled: registering, dataset: { fkey: 'sys:ext:register' }, onclick: register },
          icon(registering ? 'loader-circle' : 'refresh-cw', { size: 14, class: registering ? 'spin' : '' }),
          h('span', { class: 'btn-label' }, t(registering ? 'system.ext.registering' : 'system.ext.register')),
        ),
        registerResult(st.last_register),
      ),
    );
    if (requestError) el.append(line('err', 'circle-alert', t('system.ext.register_request_failed', { error: requestError })));
  }

  refresh();

  return {
    el,
    refresh,
    // setRow(check): the server's browser_ext row (its sentence decides
    // whether the connection line is needed)
    setRow(c) {
      const code = (c && c.detail_info && c.detail_info.code) || '';
      if (code === rowCode) return;
      rowCode = code;
      render();
    },
    destroy() {
      alive = false;
    },
  };
}
