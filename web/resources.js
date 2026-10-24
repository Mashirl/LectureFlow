// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Resource inspector, the collapsible right column (v1.1 A10): GPU (name,
// backend, utilisation and temperature with 60 s sparklines, VRAM, power,
// ASR slots, the GPU pause switch moved here from the v1 top bar) and one
// row per provider (balance, tokens today / total, cache hit rate,
// concurrency). Data: GET /api/resources and SSE "resources"; a value the
// back end does not have is shown as "--". v2 P12.4: a provider with a
// tariff shows its peak / off-peak tag next to the name (updated in place,
// so its hover text survives the 2-second refresh).
// SPEC-v2 I10 (phase 3b): "transcription devices" lists one card per turned-on
// device (name, backend · model · precision, current tasks, slots; the CUDA
// card keeps the nvidia-smi metrics, an Intel device shows its usage or
// "--"), "battery mode" on the cards while on battery, a switch per device
// and the main switch over all of them.
// Power management (power-req part 2): with the power policy the panel has
// three areas — system and power (CPU, memory, CPU temperature, the power
// source with charge and charging, the tasks the power-aware policy
// suspended), the active transcription device cards with why they are
// active, and the other devices one row each (a row opens its details and
// closes again; the active device does not change).
// SPEC-v3 V14.5: with Colab switched on (snapshot "remote" not null) a remote
// GPU area sits between the active devices and the other devices: the
// session (service and accelerator, state, usage window, how many more
// lectures fit, the current lecture's step, the plan, compute units, the
// authorization's time left) and the two shutdown buttons; without a
// session it is one line "远端 GPU：未使用".

import { h, clear, iconBtn, errorBlock, skeleton, preserveFocus, toast } from './dom.js';
import { icon } from './icons.js';
import * as U from './util.js';
import { t, has } from './i18n.js';
import { tariffTag, updateTariffTag, hasPeaks } from './tariff_ui.js';
import * as C from './settings_colab.js';

const NA = '--';
const SVG_NS = 'http://www.w3.org/2000/svg';

function svg(tag, attrs, ...children) {
  const el = document.createElementNS(SVG_NS, tag);
  for (const [k, v] of Object.entries(attrs || {})) el.setAttribute(k, String(v));
  for (const c of children) if (c) el.append(c);
  return el;
}

// 60 s sparkline (oldest first, one sample every 2 s, right-aligned). A
// missing reading (null) keeps its place on the time axis and breaks the
// line there (the time is never compressed). lo/hi fix the scale; null =
// from the data.
function sparkline(values, { lo = null, hi = null, label }) {
  const W = 120;
  const H = 30;
  const box = svg('svg', { viewBox: `0 0 ${W} ${H}`, class: 'spark', preserveAspectRatio: 'none', role: 'img', 'aria-label': label });
  const isNum = (v) => typeof v === 'number' && isFinite(v);
  const list = (values || []).slice(-30);
  const vals = list.filter(isNum);
  if (vals.length < 2) {
    box.append(svg('line', { x1: 0, y1: H - 1, x2: W, y2: H - 1, class: 'spark-empty' }));
    return box;
  }
  let min = lo != null ? lo : Math.min(...vals);
  let max = hi != null ? hi : Math.max(...vals);
  if (max - min < 6) {
    const mid = (max + min) / 2;
    min = mid - 3;
    max = mid + 3;
  }
  const step = W / 29; // 30 samples
  const x0 = W - step * (list.length - 1);
  const yOf = (v) => H - 2 - ((Math.min(max, Math.max(min, v)) - min) / (max - min)) * (H - 4);
  // the runs of consecutive readings, each drawn on its own
  const runs = [];
  let run = [];
  list.forEach((v, i) => {
    if (isNum(v)) run.push([x0 + i * step, yOf(v)]);
    else if (run.length) {
      runs.push(run);
      run = [];
    }
  });
  if (run.length) runs.push(run);
  const fmt = (pts) => pts.map(([x, y]) => `${x.toFixed(1)},${y.toFixed(1)}`).join(' ');
  for (const pts of runs) {
    if (pts.length === 1) {
      box.append(svg('circle', { cx: pts[0][0].toFixed(1), cy: pts[0][1].toFixed(1), r: 1.2, class: 'spark-dot' }));
      continue;
    }
    const line = fmt(pts);
    box.append(
      svg('polygon', { points: `${pts[0][0].toFixed(1)},${H} ${line} ${pts[pts.length - 1][0].toFixed(1)},${H}`, class: 'spark-area' }),
      svg('polyline', { points: line, class: 'spark-line' }),
    );
  }
  // the newest sample, when it was read
  if (isNum(list[list.length - 1])) box.append(svg('circle', { cx: W.toFixed(1), cy: yOf(list[list.length - 1]).toFixed(1), r: 2, class: 'spark-dot' }));
  return box;
}

function meter(frac, cls = '') {
  const f = frac == null || !isFinite(frac) ? 0 : Math.max(0, Math.min(1, frac));
  return h('div', { class: 'meter ' + cls, role: 'presentation' }, h('span', { class: 'meter-fill', style: { width: `${(f * 100).toFixed(1)}%` } }));
}

// Concurrency as cells ("3 / 8"); a plain meter above 16.
function slots(used, max, label) {
  if (!max) return h('span', { class: 'slots-text' }, NA);
  if (max > 16) return meter(used / max, 'meter-accent');
  return h(
    'span',
    { class: 'slots', role: 'img', 'aria-label': `${label} ${used} / ${max}` },
    Array.from({ length: max }, (_, i) => h('span', { class: 'slot' + (i < used ? ' is-on' : '') })),
  );
}

const num = (v) => (v == null ? NA : U.fmtInt(v)); // the tiles show the unit apart, smaller

// Hover text of a failed balance query: the coded reason when the catalog
// has it (its text is complete), else the back end's text after a prefix.
function balanceErrorText(p) {
  const info = p.balance_error_info;
  if (info && info.code && has(`errors.${info.code}`)) return U.errorInfoText(info, p.balance_error);
  return t('resources.balance_failed', { error: p.balance_error || (info && info.detail) || '' });
}

// opts: {onToggleGPU(), onToggleDevice(card), onCollapse(), onExpand()}
export function mountInspector(el, opts) {
  let res = null;
  let error = '';
  let loading = true;
  // While the full panel is shown: {sw (GPU switch, kept), gpuBody, prov,
  // foot, provKey}; while collapsed: {rail: {k: value element}}. Updates
  // every 2 s change values in place — interactive elements are never
  // replaced under the pointer (a click must not get lost, SPEC §0.1).
  let parts = null;
  // the system readings of the last 60 s for the sparklines (one per new
  // sample; the back end sends the current value only)
  const cpuHist = [];
  const tempHist = [];
  let histAt = '';

  function pushHistory(r) {
    if (!r || !r.system || r.sampled_at === histAt) return;
    histAt = r.sampled_at;
    for (const [list, v] of [[cpuHist, r.system.cpu_pct], [tempHist, r.system.cpu_temp_c]]) {
      list.push(v);
      if (list.length > 30) list.shift();
    }
  }

  function setResources(r) {
    const fresh = loading || !!error;
    C.noteRemote(r && r.remote);
    pushHistory(r);
    res = r;
    error = '';
    loading = false;
    if (fresh || !parts) preserveFocus(render);
    else update();
  }

  function update() {
    if (parts.rail) {
      updateRail();
      return;
    }
    if (devMode() !== parts.devMode) {
      preserveFocus(render);
      return;
    }
    syncSwitch(parts.sw, res.gpu);
    updateRemote();
    if (parts.devMode) updateDevices();
    else {
      const g = gpuBody(res.gpu);
      parts.gpuBody.replaceWith(g);
      parts.gpuBody = g;
    }
    const key = provKeyOf();
    if (key !== parts.provKey) {
      const list = res.providers || [];
      if (provShape(list) === parts.provShape) {
        // same providers: rows change in place (the tags keep their hover)
        for (const p of list) updateProviderRow(parts.provRows.get(p.id), p);
      } else {
        const p = providersSection(list);
        parts.prov.replaceWith(p);
        parts.prov = p;
      }
      parts.provKey = key;
    }
    parts.foot.textContent = t('resources.sampled_at', { time: timeOf(res.sampled_at) });
  }

  function budgetHit() {
    const sys = opts.system ? opts.system() : null;
    return !!(sys && sys.budget_hit);
  }

  function provKeyOf() {
    return JSON.stringify([budgetHit(), res.providers || []]);
  }

  // what decides the section's structure (rows are updated in place otherwise)
  function provShape(list) {
    return JSON.stringify([budgetHit(), list.map((p) => p.id)]);
  }

  function setError(msg) {
    if (res) return; // keep showing the last good snapshot
    error = msg || t('resources.load_error');
    loading = false;
    preserveFocus(render);
  }

  function render() {
    clear(el);
    parts = null;
    const collapsed = document.documentElement.dataset.insp === 'collapsed';
    if (collapsed) {
      el.append(buildRail());
      return;
    }
    el.append(
      h(
        'div',
        { class: 'insp-head' },
        h('h2', { class: 'insp-title' }, t('resources.title')),
        res ? h('span', { class: 'insp-live', title: t('resources.live_tip') }, h('span', { class: 'live-dot' }), t('resources.live')) : null,
        iconBtn('panel-right-close', t('resources.collapse'), { class: 'insp-toggle', fkey: 'insp:toggle', onclick: opts.onCollapse }),
      ),
    );
    if (loading) {
      el.append(h('div', { class: 'insp-body' }, skeleton(4), skeleton(3)));
      return;
    }
    if (error) {
      el.append(h('div', { class: 'insp-body' }, errorBlock(error, opts.onRetry)));
      return;
    }
    const sw = gpuSwitch();
    parts = { sw, devMode: devMode(), foot: h('div', { class: 'insp-foot' }, t('resources.sampled_at', { time: timeOf(res.sampled_at) })) };
    syncSwitch(sw, res.gpu);
    let gpuSec;
    if (parts.devMode) gpuSec = devicesSection(sw);
    else {
      parts.gpuBody = gpuBody(res.gpu);
      gpuSec = h('section', { class: 'insp-sec', 'aria-label': t('resources.gpu.title') }, h('div', { class: 'insp-sec-head' }, h('h3', null, t('resources.gpu.title')), sw), parts.gpuBody);
    }
    parts.prov = providersSection(res.providers || []);
    parts.provKey = provKeyOf();
    // without devices the remote area follows the GPU section
    const remote = parts.devMode ? null : remoteArea();
    el.append(h('div', { class: 'insp-body' }, gpuSec, remote, parts.prov), parts.foot);
  }

  // ---- transcription devices (SPEC-v2 I10) and power management ----

  // A back end with transcription devices reports the power state (and the
  // cards of the turned-on devices; none are omitted).
  function devMode() {
    return !!res && (!!res.power || (Array.isArray(res.devices) && res.devices.length > 0));
  }
  const rows = () => (res && res.devices) || [];
  // Power management (power-req part 2): a back end with the power policy
  // lists every device row and marks the active ones; the panel has three
  // areas — system and power, the active device cards, the other devices
  // one row each. Without it (phase 3b) every row is a card.
  const policy = () => (res && res.policy) || null;
  const cards = () => (policy() ? rows().filter((d) => d.active) : rows());
  const standby = () => (policy() ? rows().filter((d) => !d.active) : []);
  const suspendedCount = () => {
    const p = policy();
    return p ? (p.wait_ac || 0) + (p.wait_no_device || 0) : 0;
  };
  const devShapeOf = () => JSON.stringify([!!policy(), !!(res && res.system), cards().map((d) => d.id), standby().map((d) => d.id)]);

  function devicesSection(sw) {
    parts.devRefs = new Map();
    parts.sbRefs = new Map();
    parts.devShape = devShapeOf();
    parts.devTop = devTop();
    const list = cards().length ? h('div', { class: 'dcards' }, cards().map(deviceCard)) : null;
    const active = h(
      'section',
      { class: 'insp-sec', 'aria-label': policy() ? t('resources.active.title') : t('resources.gpu.title') },
      h('div', { class: 'insp-sec-head' }, h('h3', null, policy() ? t('resources.active.title') : t('resources.gpu.title')), sw),
      parts.devTop,
      list,
    );
    const secs = [];
    if (policy() || res.system) secs.push(systemSection());
    secs.push(active);
    secs.push(remoteArea());
    if (standby().length) secs.push(standbySection());
    parts.devSec = h('div', { class: 'insp-group' }, secs);
    return parts.devSec;
  }

  // The notes above the cards: the main switch is off; the manual list does
  // not apply on battery; why there is no active device.
  function devTop() {
    const p = policy();
    const notes = [];
    if (res.gpu && res.gpu.paused) notes.push(h('p', { class: 'insp-note tone-warn' }, icon('circle-pause', { size: 14 }), t('resources.devices.paused_note')));
    if (p && p.manual_ignored) notes.push(h('p', { class: 'insp-note' }, icon('battery-medium', { size: 14 }), t('resources.power.manual_ignored')));
    if (!cards().length) {
      const none = p && p.none_reason ? U.waitingForText(p.none_reason, opts.system ? opts.system() : null, res) : t('resources.devices.none');
      notes.push(h('p', { class: 'insp-note' + (p && p.none_reason ? ' tone-warn' : '') }, icon(p && p.none_reason ? 'battery-low' : 'info', { size: 14 }), none));
    }
    return h('div', { class: 'dcards-top' }, notes);
  }

  function updateDevices() {
    if (devShapeOf() !== parts.devShape) {
      const old = parts.devSec;
      preserveFocus(() => old.replaceWith(devicesSection(parts.sw)));
      return;
    }
    const top = devTop();
    parts.devTop.replaceWith(top);
    parts.devTop = top;
    if (parts.sysBody) {
      const b = systemBody();
      parts.sysBody.replaceWith(b);
      parts.sysBody = b;
    }
    for (const d of cards()) {
      const ref = parts.devRefs.get(d.id);
      if (!ref) continue;
      syncDeviceSwitch(ref.sw, d);
      ref.card.classList.toggle('is-paused', !!d.paused);
      ref.card.classList.toggle('is-absent', !d.present);
      ref.name.textContent = d.label;
      ref.name.title = d.label;
      const body = deviceBody(d);
      ref.body.replaceWith(body);
      ref.body = body;
    }
    for (const d of standby()) updateStandbyRow(d);
  }

  // ---- remote GPU (SPEC-v3 V14.5) ----

  // parts.remote: {wrap, shape, body, line} — the area is rebuilt when its
  // shape changes; otherwise only its text is replaced and the buttons stay
  // (a click must not get lost). The wrapper keeps the area's place while
  // Colab is off, so switching it on later fills the same slot.
  let remoteBusy = '';
  const remoteOf = () => (res && res.remote) || null;
  const remoteOpen = (r) => C.ACTIVE_STATES.has(r.state) || !!r.current;
  const remoteShapeOf = () => {
    const r = remoteOf();
    if (!r) return 'none';
    return JSON.stringify([remoteOpen(r), r.state, !!r.shutdown_after_current, remoteBusy]);
  };

  function remoteArea() {
    const wrap = h('div', { class: 'insp-remote-slot' });
    parts.remote = { wrap, shape: '', body: null, line: null };
    fillRemote();
    return wrap;
  }

  function fillRemote() {
    const ref = parts.remote;
    ref.shape = remoteShapeOf();
    ref.body = null;
    ref.line = null;
    clear(ref.wrap);
    const r = remoteOf();
    ref.wrap.hidden = !r;
    if (!r) return;
    if (!remoteOpen(r)) {
      ref.line = remoteLine(r);
      ref.wrap.append(ref.line);
      return;
    }
    ref.body = remoteBody(r);
    const stopping = r.state === 'stopping';
    const after = h(
      'button',
      {
        type: 'button',
        class: 'btn btn-sm',
        disabled: !!remoteBusy || stopping || !!r.shutdown_after_current,
        dataset: { fkey: 'insp:remote:after' },
        onclick: () => remoteShutdown('after_current'),
      },
      icon(remoteBusy === 'after_current' ? 'loader-circle' : 'circle-pause', { size: 13 }),
      h('span', { class: 'btn-label' }, t('resources.remote.after_current')),
    );
    const now = h(
      'button',
      { type: 'button', class: 'btn btn-sm btn-danger', disabled: !!remoteBusy || stopping, dataset: { fkey: 'insp:remote:now' }, onclick: () => remoteShutdown('now') },
      icon(remoteBusy === 'now' ? 'loader-circle' : 'ban', { size: 13 }),
      h('span', { class: 'btn-label' }, t('resources.remote.now')),
    );
    ref.wrap.append(
      h(
        'section',
        { class: 'insp-sec insp-remote', 'aria-label': t('resources.remote.title') },
        h('div', { class: 'insp-sec-head' }, h('h3', null, t('resources.remote.title')), settingsLink()),
        ref.body,
        stopping ? null : h('div', { class: 'rc-actions' }, after, now),
      ),
    );
  }

  const settingsLink = () => h('a', { class: 'rc-link', href: '#/settings/colab', dataset: { fkey: 'insp:remote:settings' } }, t('resources.remote.settings_link'));

  function updateRemote() {
    if (!parts.remote) return;
    if (remoteShapeOf() !== parts.remote.shape) {
      preserveFocus(fillRemote);
      return;
    }
    const r = remoteOf();
    if (!r) return;
    if (parts.remote.body) {
      const b = remoteBody(r);
      parts.remote.body.replaceWith(b);
      parts.remote.body = b;
    } else if (parts.remote.line) {
      preserveFocus(fillRemote); // the line holds the settings link only
    }
  }

  async function remoteShutdown(mode) {
    if (remoteBusy) return;
    if (mode === 'now' && !(await C.confirmShutdownNow())) return;
    remoteBusy = mode;
    updateRemote();
    try {
      const v = await C.shutdownColab(mode);
      if (v && v.status && res) res = { ...res, remote: v.status };
      toast(mode === 'now' ? t('resources.remote.now_done') : t('resources.remote.after_current_done'), 'success');
    } catch (e) {
      toast(e.message, 'error');
    }
    remoteBusy = '';
    updateRemote();
  }

  // The notes every remote state may carry: no authorization, a session
  // left over by an earlier run, the last error, the cool-down after a
  // used-up window, the automatic dispatch held after a shutdown button.
  function remoteNotes(r) {
    const out = [];
    if (!r.configured) out.push(h('p', { class: 'insp-note tone-warn' }, icon('key-round', { size: 14 }), t('resources.remote.auth_missing')));
    if (r.leftover) out.push(h('p', { class: 'insp-note tone-warn' }, icon('triangle-alert', { size: 14 }), C.infoText({ code: r.leftover.code, params: { accelerator: r.leftover.accelerator } })));
    if (r.last_error) out.push(h('p', { class: 'insp-note tone-warn' }, icon('circle-alert', { size: 14 }), C.infoText(r.last_error)));
    if (r.cooldown_until) {
      const sec = (new Date(r.cooldown_until).getTime() - Date.now()) / 1000;
      if (sec > 0) out.push(h('p', { class: 'insp-note' }, icon('clock', { size: 14 }), t('resources.remote.cooldown', { time: C.durText(sec) })));
    }
    if (C.colabHeld() && !C.ACTIVE_STATES.has(r.state)) out.push(h('p', { class: 'insp-note' }, icon('circle-pause', { size: 14 }), t('resources.remote.held')));
    return out;
  }

  // No session: one line ("远端 GPU：未使用" / "远端 GPU：已释放（原因）").
  function remoteLine(r) {
    const released = r.state === 'released' && r.last_release;
    const text = released ? t('resources.remote.released_line', { reason: C.releaseName(r.last_release.reason) }) : t('resources.remote.unused');
    return h(
      'section',
      { class: 'insp-sec insp-remote is-line', 'aria-label': t('resources.remote.title') },
      h(
        'div',
        { class: 'rc-line' },
        icon('server', { size: 15 }),
        h('span', { class: 'rc-line-t', title: released ? U.fmtDateTime(r.last_release.at) : undefined }, text),
        settingsLink(),
      ),
      remoteNotes(r),
    );
  }

  function remoteBody(r) {
    const used = r.window_used_sec || 0;
    const total = r.window_total_sec || 0;
    const frac = total ? used / total : null;
    const st = r.state;
    const kv = (ic, k, v, extra) => h('div', { class: 'rc-kv' }, h('span', { class: 'rc-k' }, icon(ic, { size: 13 }), k), h('span', { class: 'rc-v' }, v, extra || null));
    const service = t('resources.remote.service', { accelerator: r.accelerator });
    const rows = [
      h(
        'div',
        { class: 'rc-head' },
        icon('server', { size: 15 }),
        h('span', { class: 'rc-name', title: service }, service),
        h('span', { class: 'rc-tags' }, h('span', { class: 'chip ' + (st === 'running' ? 'chip-ok' : st === 'stopping' ? 'chip-quiet' : 'chip-run') }, C.stateName(st)), h('span', { class: 'chip chip-quiet' }, r.tier === 'free' ? t('resources.remote.tier.free') : t('resources.remote.tier.paid'))),
      ),
      h(
        'div',
        { class: 'metric rc-window' },
        h('div', { class: 'metric-top' }, h('span', { class: 'metric-k' }, icon('clock', { size: 14 }), t('resources.remote.window_k')), h('span', { class: 'metric-v' }, t('resources.remote.window', { used: C.durText(used) || C.durText(0), total: C.durText(total) }))),
        meter(frac, frac != null && frac > 0.9 ? 'meter-warn' : 'meter-accent'),
      ),
    ];
    if (st !== 'stopping') rows.push(h('p', { class: 'rc-left' }, r.lectures_left > 0 ? t('resources.remote.left', { n: r.lectures_left }) : t('resources.remote.left_none')));
    const cur = r.current;
    if (cur) {
      // the worker's stage only while it prepares (check, install, model…)
      const stage = cur.step === 'remote_transcribe' && cur.stage && cur.stage !== 'transcribe' ? C.stageName(cur.stage) : '';
      const step = [C.stepName(cur.step), stage, cur.step !== 'wait_accelerator' && cur.overall > 0 ? U.pct(cur.overall) : ''].filter(Boolean).join(' · ');
      rows.push(kv('audio-lines', t('resources.remote.current_k'), step, cur.eta_sec > 0 ? h('span', { class: 'rc-sub' }, t('resources.remote.eta', { time: C.durText(cur.eta_sec) })) : null));
    } else if (st === 'running') rows.push(kv('audio-lines', t('resources.remote.current_k'), t('resources.remote.idle')));
    if (r.planned > 0) rows.push(kv('list-checks', t('resources.remote.plan_k'), t('resources.remote.plan', { n: r.planned, fit: r.planned_fit })));
    if (r.tier === 'free') rows.push(kv('coins', t('resources.remote.balance_k'), t('resources.remote.balance_free')));
    else {
      const texts = [t('resources.remote.balance', { balance: r.balance_cu != null ? C.cuText(r.balance_cu) : NA })];
      if (r.plan_cost_cu > 0) texts.push(t('resources.remote.plan_cost', { cost: C.cuText(r.plan_cost_cu) }));
      rows.push(kv('coins', t('resources.remote.balance_k'), texts.join(' · '), r.session_spent_cu > 0 ? h('span', { class: 'rc-sub' }, t('resources.remote.spent', { cost: C.cuText(r.session_spent_cu) })) : null));
    }
    const auth = C.authLeftText(r.auth);
    if (auth) rows.push(kv('key-round', t('resources.remote.auth_k'), auth));
    if (r.shutdown_after_current && st !== 'stopping') rows.push(h('p', { class: 'insp-note' }, icon('circle-pause', { size: 14 }), t('resources.remote.after_current_set')));
    rows.push(...remoteNotes(r));
    return h('div', { class: 'rc-body' }, rows);
  }

  // ---- upper area: system and power ----

  function systemSection() {
    parts.sysBody = systemBody();
    return h('section', { class: 'insp-sec', 'aria-label': t('resources.system.title') }, h('div', { class: 'insp-sec-head' }, h('h3', null, t('resources.system.title'))), parts.sysBody);
  }

  // Everything of the system section below its head (no interactive elements).
  function systemBody() {
    const y = res.system || {};
    const p = policy();
    const memFrac = y.mem_used_mb != null && y.mem_total_mb ? y.mem_used_mb / y.mem_total_mb : null;
    const tile = (k, v, unit, spark) =>
      h('div', { class: 'tile' }, h('div', { class: 'tile-k' }, k), h('div', { class: 'tile-v' }, num(v), v != null ? h('small', null, unit) : null), spark || null);
    return h(
      'div',
      { class: 'sys-body' },
      powerLine(res.power),
      p && p.wait_ac > 0 ? h('p', { class: 'insp-note pw-wait tone-warn' }, icon('plug', { size: 14 }), t('resources.power.wait_ac', { n: p.wait_ac })) : null,
      p && p.wait_no_device > 0 ? h('p', { class: 'insp-note pw-wait tone-warn' }, icon('battery-low', { size: 14 }), t('resources.power.wait_no_device', { n: p.wait_no_device })) : null,
      h(
        'div',
        { class: 'tiles' },
        tile(t('resources.system.cpu'), y.cpu_pct, '%', sparkline(cpuHist, { lo: 0, hi: 100, label: t('resources.system.cpu') })),
        tile(t('resources.system.cpu_temp'), y.cpu_temp_c, '°C', sparkline(tempHist, { label: t('resources.system.cpu_temp') })),
      ),
      h(
        'div',
        { class: 'metric' },
        h('div', { class: 'metric-top' }, h('span', { class: 'metric-k' }, icon('memory-stick', { size: 14 }), t('resources.system.mem')), h('span', { class: 'metric-v' }, `${y.mem_used_mb == null ? NA : U.fmtMB(y.mem_used_mb)} / ${y.mem_total_mb == null ? NA : U.fmtMB(y.mem_total_mb)}`)),
        meter(memFrac, memFrac != null && memFrac > 0.92 ? 'meter-warn' : 'meter-accent'),
      ),
    );
  }

  // The power state the power-aware choice follows: source (plugged in /
  // battery; USB-C PD and a power adapter cannot be told apart, power-brief
  // ruling 16), charge and whether it charges.
  function powerLine(pw) {
    if (!pw) return null;
    const known = !!(pw.known && pw.source);
    const onBattery = pw.source === 'battery';
    const level = pw.battery_pct;
    let ic = 'circle-alert';
    if (known && onBattery) ic = level == null ? 'battery-medium' : level < 25 ? 'battery-low' : level < 75 ? 'battery-medium' : 'battery-full';
    else if (known) ic = pw.charging ? 'battery-charging' : 'plug';
    const tags = [];
    if (known && pw.has_battery && level != null) tags.push(h('span', { class: 'pw-level' }, t('resources.power.level', { pct: U.fmtPercent(level / 100, 0) })));
    if (known && pw.has_battery && !onBattery) tags.push(h('span', { class: 'chip ' + (pw.charging ? 'chip-ok' : 'chip-quiet') }, pw.charging ? t('resources.power.charging') : t('resources.power.not_charging')));
    if (known && !pw.has_battery) tags.push(h('span', { class: 'chip chip-quiet' }, t('resources.power.no_battery')));
    return h(
      'div',
      { class: 'pw-line' + (onBattery ? ' is-battery' : ''), title: [t('resources.power.basis_tip'), onBatteryPolicy() ? t('resources.devices.battery_mode_tip') : ''].filter(Boolean).join('\n') },
      icon(ic, { size: 16 }),
      h('span', { class: 'pw-src' }, !known ? t('system.power.unknown') : onBattery ? t('resources.power.battery') : t('resources.power.ac')),
      tags.length ? h('span', { class: 'pw-tags' }, tags) : null,
    );
  }

  // ---- middle area: the active device cards ----

  function deviceSwitch(d) {
    return h(
      'button',
      {
        type: 'button',
        class: 'switch switch-sm',
        role: 'switch',
        'aria-label': t('resources.devices.device_switch', { device: d.label }),
        dataset: { fkey: `insp:dev:${d.id}` },
        onclick: () => opts.onToggleDevice && opts.onToggleDevice(rows().find((x) => x.id === d.id) || d),
      },
      h('span', { class: 'switch-track' }, h('span', { class: 'switch-thumb' })),
      h('span', { class: 'switch-label' }),
    );
  }

  function syncDeviceSwitch(sw, d) {
    const on = !d.paused;
    sw.classList.toggle('is-on', on);
    sw.setAttribute('aria-checked', String(on));
    sw.title = on ? t('devices.pause_tip') : t('devices.resume_tip');
    sw.querySelector('.switch-label').textContent = on ? t('common.status.running') : t('devices.paused');
  }

  function deviceCard(d) {
    const sw = deviceSwitch(d);
    syncDeviceSwitch(sw, d);
    const name = h('span', { class: 'dcard-name', title: d.label }, d.label);
    const body = deviceBody(d);
    const card = h('div', { class: 'dcard' + (d.paused ? ' is-paused' : '') + (d.present ? '' : ' is-absent'), dataset: { dev: d.id } }, h('div', { class: 'dcard-head' }, icon('cpu', { size: 15 }), name, sw), body);
    parts.devRefs.set(d.id, { card, sw, name, body });
    return card;
  }

  // A machine with a battery runs on battery: the power-aware battery rules
  // apply (the NPU, else the Intel GPU; never the graphics card or the CPU).
  const onBatteryPolicy = () => {
    const p = policy();
    return !!(p && p.battery_machine && res.power && res.power.source === 'battery');
  };

  // The hover text of the battery rules, with the charge.
  function batteryTip() {
    const p = res.power;
    const rules = policy() ? onBatteryPolicy() : !!(p && p.battery_aware); // 3b: battery awareness
    return [rules ? t('resources.devices.battery_mode_tip') : '', p && p.battery_pct != null ? t('system.power.battery_pct', { pct: U.fmtPercent(p.battery_pct / 100, 0) }) : ''].filter(Boolean).join('\n');
  }

  // "为什么是它" (power-req part 2): why the active devices are active. A
  // running device that is no longer the preferred one (plugged in while
  // the NPU runs a lecture) says that it hands over after the lecture
  // instead of the plugged-in reason.
  const REASON_ICONS = { ac_auto: 'plug', battery: 'battery-medium' };
  function reasonChip(p, d) {
    if (!p) return null;
    if (p.next && d.id !== p.next) {
      return h('span', { class: 'chip chip-run dcard-why', title: t('resources.power.next', { device: U.deviceLabel(p.next, null, res) }) }, icon('arrow-right', { size: 12 }), t('resources.power.reason.switch_pending'));
    }
    if (!p.reason) return null;
    const tip = p.reason === 'battery' ? batteryTip() : '';
    return h(
      'span',
      { class: 'chip chip-run dcard-why', title: tip || undefined },
      REASON_ICONS[p.reason] ? icon(REASON_ICONS[p.reason], { size: 12 }) : null,
      has(`resources.power.reason.${p.reason}`) ? t(`resources.power.reason.${p.reason}`) : p.reason,
    );
  }

  // Everything of a device card below its head (no interactive elements).
  function deviceBody(d) {
    const pol = policy();
    const backend = U.backendName(d); // CUDA on an NVIDIA card, like the standby rows
    const tasks = d.tasks || [];
    const combo = [backend, U.comboText(d.model, d.precision)].filter(Boolean).join(' · ');
    // the switch of power management: the running device finishes its
    // lecture, the next one starts on pol.next
    const next = pol && pol.next && pol.next !== d.id && d.active && tasks.some((tk) => !tk.selftest);
    return h(
      'div',
      { class: 'dcard-body' },
      h('div', { class: 'dcard-combo mono', title: combo }, combo),
      h(
        'div',
        { class: 'gpu-meta' },
        h(
          'span',
          { class: 'dcard-tags' },
          h('span', { class: 'dcard-kind' }, U.deviceKindName(d.kind)),
          pol && d.active ? reasonChip(pol, d) : null,
          d.paused ? h('span', { class: 'chip chip-warn' }, t('devices.paused')) : null,
          !pol && d.battery_mode ? h('span', { class: 'chip chip-warn', title: batteryTip() || undefined }, icon('battery-medium', { size: 12 }), t('resources.devices.battery_mode')) : null,
        ),
        h('span', { class: 'gpu-slots', title: t('resources.gpu.slots_tip', { used: d.slots_used, max: d.slots_max }) }, h('span', { class: 'num-strong' }, `${d.slots_used}/${d.slots_max}`), slots(d.slots_used, d.slots_max, t('resources.gpu.slots_label'))),
      ),
      next ? h('p', { class: 'insp-note dcard-next' }, icon('arrow-right', { size: 14 }), t('resources.power.next', { device: U.deviceLabel(pol.next, null, res) })) : null,
      !d.present ? h('p', { class: 'insp-note tone-warn', title: t('devices.not_present_tip') }, icon('triangle-alert', { size: 14 }), t('devices.not_present')) : null,
      d.battery_hold ? h('p', { class: 'insp-note tone-warn' }, icon('battery-medium', { size: 14 }), t('devices.battery_hold')) : null,
      d.holds_suspended ? h('p', { class: 'insp-note tone-warn' }, icon('plug', { size: 14 }), t('waiting.power_wait_ac')) : null,
      d.kind === 'cuda' ? cudaMetrics(res.gpu) : usageTile(d),
      d.kind !== 'cuda' && (d.mem_used_mb != null || d.mem_total_mb != null) ? sharedMem(d) : null,
      h(
        'ul',
        { class: 'dcard-tasks' },
        tasks.length
          ? tasks.map((tk) => {
              const title = tk.selftest ? t('selftest.title') : tk.lecture_title || tk.job_id;
              return h('li', { class: 'dcard-task' }, icon(tk.selftest ? 'gauge' : 'audio-lines', { size: 13 }), h('span', { class: 'dcard-task-t', title }, title), h('span', { class: 'dcard-task-p' }, U.pct(tk.progress)));
            })
          : h('li', { class: 'dcard-task is-idle' }, t('resources.devices.idle')),
      ),
    );
  }

  // An Intel device's shared GPU memory (power management: "显存或内存").
  function sharedMem(d) {
    const frac = d.mem_used_mb != null && d.mem_total_mb ? d.mem_used_mb / d.mem_total_mb : null;
    return h(
      'div',
      { class: 'metric' },
      h('div', { class: 'metric-top' }, h('span', { class: 'metric-k' }, icon('memory-stick', { size: 14 }), t('resources.system.mem')), h('span', { class: 'metric-v' }, `${d.mem_used_mb == null ? NA : U.fmtMB(d.mem_used_mb)} / ${d.mem_total_mb == null ? NA : U.fmtMB(d.mem_total_mb)}`)),
      meter(frac, 'meter-accent'),
    );
  }

  // ---- bottom area: the other devices, one row each ----

  // Rows opened by the user (kept across updates; opening one never
  // changes the active device).
  const opened = new Set();

  function standbySection() {
    return h(
      'section',
      { class: 'insp-sec insp-standby', 'aria-label': t('resources.standby.title') },
      h('div', { class: 'insp-sec-head' }, h('h3', null, t('resources.standby.title'))),
      h('ul', { class: 'sb-list' }, standby().map(standbyRow)),
    );
  }

  // "名称 · 后端 · 待命 / 已停用 / 未测试 / 有任务挂起" (power-req part 2):
  // the state on the right of the name; the kind and back end below it,
  // followed by the small "not tested" and "suspended task" tags.
  function standbyState(d) {
    if (!d.enabled) return h('span', { class: 'chip chip-quiet sb-state' }, t('devices.disabled'));
    if (!d.present) return h('span', { class: 'chip chip-warn sb-state', title: t('devices.not_present_tip') }, t('devices.not_present'));
    if (d.paused) return h('span', { class: 'chip chip-warn sb-state' }, t('devices.paused'));
    return h('span', { class: 'chip chip-quiet sb-state' }, t('resources.standby.standby'));
  }

  function standbyMeta(d) {
    return h(
      'span',
      { class: 'sb-meta' },
      h('span', { class: 'sb-meta-t' }, [U.deviceKindName(d.kind), U.backendName(d)].filter(Boolean).join(' · ')),
      d.holds_suspended ? h('span', { class: 'chip chip-warn', title: t('waiting.power_wait_ac') }, t('resources.standby.holds_suspended')) : null,
      d.untested ? h('span', { class: 'chip chip-draft' }, t('resources.standby.untested')) : null,
    );
  }

  function standbyRow(d) {
    const open = opened.has(d.id);
    const name = h('span', { class: 'sb-name', title: d.label }, d.label);
    const meta = standbyMeta(d);
    const tags = standbyState(d);
    const btnEl = h(
      'button',
      {
        type: 'button',
        class: 'sb-row',
        'aria-expanded': String(open),
        dataset: { fkey: `insp:sb:${d.id}` },
        onclick: () => toggleStandby(d.id),
      },
      icon('chevron-right', { size: 14, class: 'sb-chev' }),
      h('span', { class: 'sb-main' }, name, meta),
      tags,
    );
    const ref = { name, meta, tags, btn: btnEl, li: null, sw: null, body: null };
    const li = h('li', { class: 'sb-item' + (open ? ' is-open' : '') + (d.enabled ? '' : ' is-off'), dataset: { dev: d.id } }, btnEl);
    if (open) {
      ref.body = deviceBody(d);
      const detail = h('div', { class: 'sb-detail' });
      if (d.enabled) {
        ref.sw = deviceSwitch(d);
        syncDeviceSwitch(ref.sw, d);
        detail.append(h('div', { class: 'sb-detail-head' }, ref.sw));
      }
      detail.append(ref.body);
      li.append(detail);
    }
    ref.li = li;
    parts.sbRefs.set(d.id, ref);
    return li;
  }

  function updateStandbyRow(d) {
    const ref = parts.sbRefs.get(d.id);
    if (!ref) return;
    if (!!ref.sw !== (opened.has(d.id) && d.enabled)) {
      preserveFocus(() => ref.li.replaceWith(standbyRow(d)));
      return;
    }
    ref.li.classList.toggle('is-off', !d.enabled);
    ref.name.textContent = d.label;
    ref.name.title = d.label;
    const meta = standbyMeta(d);
    ref.meta.replaceWith(meta);
    ref.meta = meta;
    const tags = standbyState(d);
    ref.tags.replaceWith(tags);
    ref.tags = tags;
    if (ref.sw) syncDeviceSwitch(ref.sw, d);
    if (ref.body) {
      const body = deviceBody(d);
      ref.body.replaceWith(body);
      ref.body = body;
    }
  }

  function toggleStandby(id) {
    if (opened.has(id)) opened.delete(id);
    else opened.add(id);
    const ref = parts && parts.sbRefs && parts.sbRefs.get(id);
    const d = standby().find((x) => x.id === id);
    if (!ref || !d) return;
    preserveFocus(() => ref.li.replaceWith(standbyRow(d)));
  }

  // An Intel device's usage — the system counter's reading, or "--" (I10) —
  // and its temperature (power-req part 2 "占用率与温度的小折线图"): no
  // counter reports it without administrator rights, so "--" unless the
  // back end sends temp_c / temp_history.
  function usageTile(d) {
    const na = d.util_pct == null;
    const temp = typeof d.temp_c === 'number' ? d.temp_c : null;
    return h(
      'div',
      { class: 'tiles' },
      h(
        'div',
        { class: 'tile', title: na ? t('resources.devices.util_na') : undefined },
        h('div', { class: 'tile-k' }, t('resources.gpu.util')),
        h('div', { class: 'tile-v' }, num(d.util_pct), !na ? h('small', null, '%') : null),
        sparkline(d.util_history, { lo: 0, hi: 100, label: t('resources.gpu.util_history') }),
      ),
      h(
        'div',
        { class: 'tile', title: temp == null ? t('resources.devices.temp_na') : undefined },
        h('div', { class: 'tile-k' }, t('resources.gpu.temp')),
        h('div', { class: 'tile-v' }, num(temp), temp != null ? h('small', null, '°C') : null),
        sparkline(Array.isArray(d.temp_history) ? d.temp_history : [], { label: t('resources.gpu.temp_history') }),
      ),
    );
  }

  // The CUDA card keeps the nvidia-smi metrics of before 3b.
  function cudaMetrics(g) {
    if (!g || !g.available) return h('p', { class: 'insp-note', title: (g && g.error) || '' }, icon('circle-alert', { size: 14 }), g && g.error ? U.excerpt(g.error, 90) : t('resources.gpu.unavailable'));
    return [...gpuBody(g).querySelectorAll(':scope > .tiles, :scope > .metric')];
  }

  // the sampling time, 24-hour with seconds (util.js)
  function timeOf(iso) {
    return U.fmtHMS(iso) || NA;
  }

  function gpuSwitch() {
    return h(
      'button',
      { type: 'button', class: 'switch', role: 'switch', 'aria-label': t('resources.gpu.switch'), dataset: { fkey: 'insp:gpu' }, onclick: opts.onToggleGPU },
      h('span', { class: 'switch-track' }, h('span', { class: 'switch-thumb' })),
      h('span', { class: 'switch-label' }),
    );
  }

  function syncSwitch(sw, g) {
    const on = !g.paused;
    sw.classList.toggle('is-on', on);
    sw.setAttribute('aria-checked', String(on));
    if (devMode()) {
      // SPEC-v2 I8: the main switch pauses every transcription device
      sw.setAttribute('aria-label', t('resources.devices.switch'));
      sw.title = on ? t('resources.devices.pause_tip') : t('resources.devices.resume_tip');
      sw.querySelector('.switch-label').textContent = on ? t('common.status.running') : t('devices.paused');
      return;
    }
    sw.setAttribute('aria-label', t('resources.gpu.switch'));
    sw.title = on ? t('resources.gpu.pause_tip') : t('resources.gpu.resume_tip');
    sw.querySelector('.switch-label').textContent = on ? t('resources.gpu.running') : t('resources.gpu.paused');
  }

  // Everything of the GPU section below its head (no interactive elements).
  function gpuBody(g) {
    const warn = !!g.temp_warn;
    const memFrac = g.mem_used_mb != null && g.mem_total_mb ? g.mem_used_mb / g.mem_total_mb : null;
    const powFrac = g.power_w != null && g.power_limit_w ? g.power_w / g.power_limit_w : null;
    return h(
      'div',
      { class: 'gpu-body' },
      h('div', { class: 'gpu-name', title: g.name || g.error || '' }, icon('cpu', { size: 15 }), h('span', null, g.available && g.name ? g.name : t('resources.gpu.unavailable'))),
      h(
        'div',
        { class: 'gpu-meta' },
        h('span', { class: 'chip chip-quiet mono' }, g.backend || NA),
        h('span', { class: 'gpu-slots', title: t('resources.gpu.slots_tip', { used: g.asr_slots_used, max: g.asr_slots_max }) }, h('span', { class: 'dim' }, 'ASR'), h('span', { class: 'num-strong' }, `${g.asr_slots_used}/${g.asr_slots_max}`), slots(g.asr_slots_used, g.asr_slots_max, t('resources.gpu.slots_label'))),
      ),
      g.paused ? h('p', { class: 'insp-note tone-warn' }, icon('circle-pause', { size: 14 }), t('resources.gpu.paused_note')) : null,
      !g.available && g.error ? h('p', { class: 'insp-note', title: g.error }, icon('circle-alert', { size: 14 }), U.excerpt(g.error, 90)) : null,
      h(
        'div',
        { class: 'tiles' },
        h(
          'div',
          { class: 'tile' },
          h('div', { class: 'tile-k' }, t('resources.gpu.util')),
          h('div', { class: 'tile-v' }, num(g.util_pct), g.util_pct != null ? h('small', null, '%') : null),
          sparkline(g.util_history, { lo: 0, hi: 100, label: t('resources.gpu.util_history') }),
        ),
        h(
          'div',
          { class: 'tile' + (warn ? ' is-warn' : '') },
          h('div', { class: 'tile-k' }, t('resources.gpu.temp'), warn ? h('span', { class: 'chip chip-warn' }, t('resources.gpu.hot')) : null),
          h('div', { class: 'tile-v' }, num(g.temp_c), g.temp_c != null ? h('small', null, '°C') : null),
          sparkline(g.temp_history, { label: t('resources.gpu.temp_history') }),
        ),
      ),
      h(
        'div',
        { class: 'metric' },
        h('div', { class: 'metric-top' }, h('span', { class: 'metric-k' }, icon('memory-stick', { size: 14 }), t('resources.gpu.vram')), h('span', { class: 'metric-v' }, `${g.mem_used_mb == null ? NA : U.fmtMB(g.mem_used_mb)} / ${g.mem_total_mb == null ? NA : U.fmtMB(g.mem_total_mb)}`)),
        meter(memFrac, memFrac != null && memFrac > 0.92 ? 'meter-warn' : 'meter-accent'),
      ),
      h(
        'div',
        { class: 'metric' },
        h('div', { class: 'metric-top' }, h('span', { class: 'metric-k' }, icon('zap', { size: 14 }), t('resources.gpu.power')), h('span', { class: 'metric-v' }, `${g.power_w == null ? NA : Math.round(g.power_w)} / ${g.power_limit_w == null ? NA : Math.round(g.power_limit_w)}`, h('small', null, ' W'))),
        meter(powFrac, 'meter-quiet'),
      ),
    );
  }

  function providersSection(list) {
    if (parts) {
      parts.provRows = new Map();
      parts.provShape = provShape(list);
    }
    return h(
      'section',
      { class: 'insp-sec', 'aria-label': t('resources.providers') },
      h('div', { class: 'insp-sec-head' }, h('h3', null, t('resources.providers'))),
      budgetHit() ? h('p', { class: 'insp-note insp-alert' }, icon('circle-pause', { size: 14 }), t('resources.budget_hit')) : null,
      list.length ? list.map(providerRow) : h('p', { class: 'insp-note' }, t('resources.no_providers')),
    );
  }

  function balanceEl(p) {
    const failed = !!(p.balance_error || p.balance_error_info);
    const bal = p.balance != null ? U.fmtMoney(Number(p.balance), p.balance_currency) || p.balance : NA;
    const balTitle = failed
      ? balanceErrorText(p)
      : p.balance != null
        ? p.balance_at
          ? t('resources.balance_tip_at', { when: U.relTime(p.balance_at) })
          : t('resources.balance_tip')
        : t('resources.no_balance_api');
    return h('span', { class: 'prov-bal' + (failed ? ' is-error' : ''), title: balTitle, tabindex: failed ? '0' : undefined }, failed ? icon('triangle-alert', { size: 13 }) : icon('wallet', { size: 13 }), bal);
  }

  // Everything of a provider row below its head (no interactive elements).
  function providerBody(p) {
    const row = (k, tk, rate) => [
      h('span', { class: 'ptab-k' }, k),
      h('span', { class: 'ptab-v' }, U.fmtTokens(tk.prompt_tokens)),
      h('span', { class: 'ptab-v' }, U.fmtTokens(tk.completion_tokens)),
      h('span', { class: 'ptab-v' }, rate == null ? NA : U.fmtPercent(rate, 0)),
    ];
    const ts = p.tariff_state;
    return h(
      'div',
      { class: 'prov-body' },
      ts && ts.missing_holidays ? h('p', { class: 'insp-note prov-gap' }, icon('info', { size: 13 }), t('resources.missing_holidays')) : null,
      h('div', { class: 'prov-conc', title: t('resources.conc_tip', { used: p.inflight, max: p.max }) }, h('span', { class: 'dim' }, t('resources.conc')), slots(p.inflight, p.max, t('resources.conc')), h('span', { class: 'num-strong' }, `${p.inflight} / ${p.max}`)),
      h(
        'div',
        { class: 'ptab', role: 'table', 'aria-label': t('resources.usage_label', { name: p.name }) },
        h('span', { class: 'ptab-h' }),
        h('span', { class: 'ptab-h' }, t('resources.col.input')),
        h('span', { class: 'ptab-h' }, t('resources.col.output')),
        h('span', { class: 'ptab-h' }, t('resources.col.cache_hit')),
        row(t('resources.row.today'), p.today, p.cache_hit_rate_today),
        row(t('resources.row.total'), p.total, p.cache_hit_rate_total),
      ),
      // D3: part of today's requests had no price (not in the estimate);
      // without any estimate (cost_today null) only tokens are shown
      p.cost_today != null && p.cost_unpriced_requests > 0
        ? h('p', { class: 'insp-note', title: t('resources.unpriced_tip', { n: p.cost_unpriced_requests }) }, icon('info', { size: 13 }), t('resources.unpriced'))
        : null,
    );
  }

  const balKeyOf = (p) => JSON.stringify([p.balance, p.balance_currency, p.balance_error, p.balance_at]);

  function providerRow(p) {
    const ref = { tag: null, tagSlot: h('span', { class: 'prov-tag' }), bal: balanceEl(p), balKey: balKeyOf(p), body: providerBody(p) };
    setTag(ref, p.tariff_state);
    if (parts && parts.provRows) parts.provRows.set(p.id, ref);
    return h('div', { class: 'prov' }, h('div', { class: 'prov-head' }, h('span', { class: 'prov-name' }, p.name), ref.tagSlot, ref.bal), ref.body);
  }

  // The peak / off-peak tag (P12.4): the element stays, its text and hover change.
  function setTag(ref, ts) {
    if (!ts || !hasPeaks(ts)) {
      if (ref.tag) ref.tag.remove();
      ref.tag = null;
      return;
    }
    if (ref.tag) updateTariffTag(ref.tag, ts);
    else ref.tagSlot.append((ref.tag = tariffTag(ts)));
  }

  function updateProviderRow(ref, p) {
    if (!ref) return;
    setTag(ref, p.tariff_state);
    const k = balKeyOf(p);
    if (k !== ref.balKey) {
      const b = balanceEl(p);
      ref.bal.replaceWith(b);
      ref.bal = b;
      ref.balKey = k;
    }
    const body = providerBody(p);
    ref.body.replaceWith(body);
    ref.body = body;
  }

  // Collapsed: the toggle plus four live numbers (updated in place).
  function buildRail() {
    const refs = {};
    const item = (k, ic, label) => {
      const v = h('span', { class: 'rail-v' }, NA);
      const box = h('div', { class: 'rail-stat' }, icon(ic, { size: 15 }), h('span', { class: 'rail-k' }, label), v);
      refs[k] = { box, v };
      return box;
    };
    parts = { rail: refs };
    const wrap = h(
      'div',
      { class: 'insp-rail' },
      iconBtn('panel-right-open', t('resources.expand'), { class: 'insp-toggle', fkey: 'insp:toggle', onclick: opts.onExpand }),
      item('gpu', 'gauge', 'GPU'),
      item('temp', 'thermometer', t('resources.gpu.temp')),
      item('asr', 'audio-lines', 'ASR'),
      item('api', 'activity', 'API'),
    );
    updateRail();
    return wrap;
  }

  function updateRail() {
    const r = parts.rail;
    const g = res && res.gpu;
    const prov = (res && res.providers) || [];
    if (!g) {
      for (const k of Object.keys(r)) r[k].box.hidden = true;
      return;
    }
    const set = (k, value, title, warn = false) => {
      r[k].v.textContent = value;
      r[k].box.title = title;
      r[k].box.classList.toggle('is-warn', warn);
      r[k].box.hidden = false;
    };
    const inflight = prov.reduce((n, p) => n + p.inflight, 0);
    const max = prov.reduce((n, p) => n + p.max, 0);
    const util = g.util_pct == null ? NA : U.fmtPercent(g.util_pct / 100, 0);
    set('gpu', util, t('resources.rail.gpu', { value: util }));
    set('temp', g.temp_c == null ? NA : U.fmtCelsius(g.temp_c, true), t('resources.rail.temp', { value: g.temp_c == null ? NA : U.fmtCelsius(g.temp_c) }), !!g.temp_warn);
    if (devMode()) {
      // the slots of every turned-on device (SPEC-v2 I10); with the power
      // policy the tasks it suspended are named too
      const on = rows().filter((d) => d.enabled !== false);
      const used = on.reduce((n, d) => n + d.slots_used, 0);
      const max = on.reduce((n, d) => n + d.slots_max, 0);
      const n = suspendedCount();
      const title = g.paused ? t('resources.rail.asr_all_paused', { used, max }) : n ? t('resources.rail.asr_power', { used, max, n }) : t('resources.rail.asr', { used, max });
      set('asr', `${used}/${max}`, title, !!g.paused || n > 0 || cards().some((d) => d.paused || (!policy() && d.battery_hold)));
    } else set('asr', `${g.asr_slots_used}/${g.asr_slots_max}`, t(g.paused ? 'resources.rail.asr_paused' : 'resources.rail.asr', { used: g.asr_slots_used, max: g.asr_slots_max }), !!g.paused);
    set('api', `${inflight}/${max}`, t('resources.rail.api', { used: inflight, max }));
  }

  render();
  return {
    setResources,
    setError,
    render,
    // the system state changed (budget_hit is shown with the providers)
    systemChanged() {
      if (parts && !parts.rail && res && provKeyOf() !== parts.provKey) update();
    },
    get: () => res,
  };
}
