// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Google Colab remote transcription in the UI (SPEC-v3 V14.2, V14.4, V14.5;
// .claude/v3-brief.md §8).
//
// - The settings page's "Colab 远端转写" section: what is uploaded where
//   (V14.1: the recordings go to Google's servers); 3.1.0: the in-app
//   sign-in of colab_login.js (install the Google Cloud CLI component, then
//   "sign in to Google" — the same as the onboarding's remote step), with
//   the earlier manual way folded under "advanced: use your own gcloud":
//   the gcloud guide with
//   the login command the back end gives (adc.login_command), 检测授权,
//   importing with a lifecycle (N days / this run only / until I
//   disconnect); the authorization's state, time left and source
//   (auth.source: "app" = the in-app sign-in, "gcloud" = imported from the
//   user's own gcloud, also every 3.0.0 import), 断开连接 (which also signs
//   gcloud's application-default login out — only for the source gcloud,
//   whose refresh token gcloud shares), and the settings:
//   enabled, accelerator, model and precision, paid / free tier, the
//   longest wait for a GPU, "电池模式下优先使用 Colab". Every change is
//   sent at once; what is shown is the last GET /api/colab or the answer
//   of the change (nothing optimistic).
// - The lecture menu's "改用 Colab 处理" (POST /api/lectures/{id}/use-colab).
// - Shared helpers of the resource panel, the pipeline card and the
//   workbench line: the last resource snapshot's "remote" (Colab status;
//   null = switched off), durations, the state names.

import { h, btn, skeleton, errorBlock, toast, confirmDialog, copyText } from './dom.js';
import { icon } from './icons.js';
import { t, has } from './i18n.js';
import * as U from './util.js';
import { appRequest } from './settings_app.js';
import { createColabLogin } from './colab_login.js';

// ---- shared state: the resource snapshot's "remote" ----

let lastRemote = null; // colab.Status | null
let heldShown = false; // GET /api/colab held (after a shutdown button)
const listeners = new Set();

/** Called with every resource snapshot (resources.js): r.remote or null. */
export function noteRemote(remote) {
  const prev = JSON.stringify(lastRemote);
  lastRemote = remote || null;
  if (JSON.stringify(lastRemote) !== prev) for (const fn of listeners) fn(lastRemote);
}

export function remoteStatus() {
  return lastRemote;
}

export function onRemoteChange(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

export function colabHeld() {
  return heldShown;
}

function noteView(v) {
  if (v && typeof v.held === 'boolean') heldShown = v.held;
}

/** Colab is switched on and holds a usable authorization. */
export function colabConfigured() {
  return !!(lastRemote && lastRemote.configured);
}

// A session that exists (the workbench line, the resource card).
export const ACTIVE_STATES = new Set(['queued', 'booting', 'running', 'stopping']);

export function stateName(s) {
  return has(`colab.state.${s}`) ? t(`colab.state.${s}`) : s || '';
}

export function stepName(s) {
  return has(`colab.step.${s}`) ? t(`colab.step.${s}`) : s || '';
}

export function stageName(s) {
  return has(`colab.stage.${s}`) ? t(`colab.stage.${s}`) : s || '';
}

export function releaseName(r) {
  return r && has(`colab.release.${r}`) ? t(`colab.release.${r}`) : (r || '');
}

/** "1 小时 47 分", "4 小时", "12 分 3 秒", "6 天" (whole days from one day on). */
export function durText(sec) {
  if (sec == null || !isFinite(sec) || sec < 0) return '';
  const s = Math.round(sec);
  if (s >= 86400) {
    const days = Math.floor(s / 86400);
    return t('resources.remote.dur_days', { n: days, days });
  }
  if (s >= 3600 && s % 3600 < 60) {
    const hours = Math.floor(s / 3600);
    return t('resources.remote.dur_hours', { n: hours, h: hours });
  }
  return U.fmtDuration(s * 1000);
}

/** Compute units with up to two decimals. */
export function cuText(v) {
  return v == null || !isFinite(v) ? '--' : U.fmtDecimal(v, 2);
}

/** The authorization's time left: "剩余 6 天" / "仅本次运行" / "直到手动断开". */
export function authLeftText(auth) {
  if (!auth || !auth.present) return '';
  if (auth.lifecycle === 'session') return t('resources.remote.auth_session');
  if (auth.lifecycle === 'manual') return t('resources.remote.auth_manual');
  if (auth.remaining_sec >= 0) return t('resources.remote.auth_left', { time: durText(auth.remaining_sec) || durText(0) });
  return '';
}

/** A Colab error / waiting reason with its params ({code, params}). */
export function infoText(info) {
  return info && info.code ? U.errorInfoText(info, info.code) : '';
}

/** Whether a stage view's asr runs (or ran) on Colab. */
export function stageOnColab(st) {
  if (!st || st.stage !== 'asr') return false;
  if (st.stats && st.stats.remote) return true;
  const a = st.model && st.model.asr;
  return !!(a && a.device_id === 'colab:remote');
}

// ---- requests ----

const colabGet = () => appRequest('GET', '/api/colab');
const colabPut = (patch) => appRequest('PUT', '/api/colab/settings', patch);

/** POST /api/colab/shutdown {mode}; resolves to the ColabView or throws. */
export async function shutdownColab(mode) {
  const v = await appRequest('POST', '/api/colab/shutdown', { mode });
  noteView(v);
  return v;
}

/** "立即关机" asks first (the running lecture goes back to the queue). */
export async function confirmShutdownNow() {
  return confirmDialog({
    title: t('resources.remote.now_title'),
    message: t('resources.remote.now_message'),
    okText: t('resources.remote.now_ok'),
    danger: true,
  });
}

// ---- "改用 Colab 处理" (V14.4) ----

const USE_STATES = new Set(['queued', 'running', 'failed', 'cancelled']);

// The 409 reasons: remote.disabled, remote.unavailable {waiting_for}
// (the waiting text follows as its own sentence), state.conflict.
function useColabError(e) {
  const b = e && e.body && e.body.error;
  if (b && b.code === 'remote.unavailable' && b.params && b.params.waiting_for) {
    const w = U.waitingForText(b.params.waiting_for);
    return U.clauseJoin([U.errorInfoText({ code: b.code }), w].filter(Boolean));
  }
  return (b && b.code && U.errorInfoText(b, e.message)) || (e && e.message) || t('common.unknown_error');
}

async function useColab(lec) {
  const ok = await confirmDialog({
    title: t('lecture.use_colab.title', { title: lec.title || '' }),
    message: t('lecture.use_colab.message'),
    okText: t('lecture.use_colab.ok'),
  });
  if (!ok) return null;
  try {
    const l = await appRequest('POST', `/api/lectures/${encodeURIComponent(lec.id)}/use-colab`);
    heldShown = false;
    toast(t('lecture.use_colab.done'), 'success');
    return l;
  } catch (e) {
    toast(useColabError(e), 'error');
    throw e;
  }
}

/**
 * The lecture menu item (null when not offered): Colab is on and
 * authorized, and the lecture's transcription is queued, running (on this
 * computer), failed or cancelled — the back end refuses done, stale and
 * pending ones. done(promise of the lecture) renders the answer.
 */
export function useColabMenuItem(lec, done) {
  if (!colabConfigured()) return null;
  const asr = (lec.stages || []).find((s) => s.stage === 'asr');
  if (!asr || !USE_STATES.has(asr.status) || stageOnColab(asr)) return null;
  return { id: 'use_colab', label: t('menu.lecture.use_colab'), icon: 'upload', onSelect: () => done(useColab(lec)) };
}

// ---- the settings section ----

const ACCELERATORS = ['T4', 'L4', 'G4', 'A100', 'H100'];
const PRECISIONS = ['float16', 'int8_float16', 'int8', 'bfloat16', 'float32', 'int8_bfloat16', 'int8_float32'];
const MODELS = ['large-v3', 'large-v3-turbo', 'large-v2', 'medium', 'small'];
const LIFECYCLES = ['days', 'session', 'manual'];

// createColabSection({head, formRow, switchBtn, segmented, rerender}): the
// helpers of view_settings.js (like settings_app.js).
export function createColabSection(ui) {
  let alive = true;
  let view = null; // GET /api/colab
  let error = '';
  let unavailable = false; // 503: this server has no Colab back end
  let saving = ''; // the field being saved
  let saveError = '';
  let checking = false;
  let checked = null; // the last 检测授权 answer {adc, auth}
  let importing = false;
  let disconnecting = false;
  let lifecycle = 'days';
  let days = 7;
  let daysError = '';
  let waitDraft = null; // the typed 等卡上限 while it is not saved
  let waitError = '';
  let modelDraft = null;
  let advancedOpen = null; // the "use your own gcloud" fold (null: not toggled yet)
  let loginStarted = false;
  const redraw = () => alive && ui.rerender();
  // 3.1.0: the in-app sign-in (the gcloud component, then the Google sign-in)
  const appLogin = createColabLogin({
    rerender: redraw,
    onAuthorized: async () => {
      await load();
      if (alive) toast(t('settings.colab.login_done'), 'success');
    },
    fkey: 'set:colab:login',
  });

  function setView(v) {
    view = v;
    noteView(v);
  }

  async function load() {
    try {
      const v = await colabGet();
      if (!alive) return;
      setView(v);
      error = '';
      unavailable = false;
    } catch (e) {
      if (!alive) return;
      if (e && e.status === 503) unavailable = true;
      else if (!view) error = e.message || t('common.load_failed');
    }
    redraw();
  }

  async function save(field, patch) {
    if (saving) return;
    saving = field;
    saveError = '';
    redraw();
    try {
      const v = await colabPut(patch);
      if (!alive) return;
      setView(v);
      if (field === 'wait') waitDraft = null;
      if (field === 'model') modelDraft = null;
    } catch (e) {
      if (!alive) return;
      saveError = e.message;
    }
    saving = '';
    redraw();
  }

  async function check() {
    if (checking) return;
    checking = true;
    redraw();
    try {
      const c = await appRequest('POST', '/api/colab/auth/check');
      if (!alive) return;
      checked = c;
      if (view) view = { ...view, adc: c.adc, status: { ...view.status, auth: c.auth, configured: !!(c.auth && c.auth.present) } };
    } catch (e) {
      if (alive) toast(e.message, 'error');
    }
    checking = false;
    redraw();
    load();
  }

  async function importAuth() {
    if (importing) return;
    if (lifecycle === 'days' && !(Number.isInteger(days) && days >= 1 && days <= 90)) {
      daysError = t('settings.colab.days_invalid', { min: 1, max: 90 });
      redraw();
      return;
    }
    importing = true;
    daysError = '';
    redraw();
    try {
      await appRequest('POST', '/api/colab/auth/import', lifecycle === 'days' ? { lifecycle, days } : { lifecycle });
      if (alive) toast(t('settings.colab.imported'), 'success');
    } catch (e) {
      if (alive) toast(e.message, 'error');
    }
    importing = false;
    checked = null;
    await load();
  }

  // Where the stored credential came from (auth.source): "app" = the in-app
  // sign-in, whose token the user's own gcloud never held; anything else
  // ("gcloud", or a back end that does not say) shares it with gcloud.
  const fromOwnGcloud = () => ((view && view.status && view.status.auth) || {}).source !== 'app';

  async function disconnect() {
    if (disconnecting) return;
    const ok = await confirmDialog({
      title: t('settings.colab.disconnect_title'),
      message: t('settings.colab.disconnect_message'),
      body: h(
        'ul',
        { class: 'colab-points' },
        h('li', null, icon('server', { size: 14 }), h('span', null, t('settings.colab.disconnect_point_release'))),
        h('li', null, icon('key-round', { size: 14 }), h('span', null, t('settings.colab.disconnect_point_revoke'))),
        // From the user's own gcloud: its application-default login uses this
        // very token. In-app sign-in: an isolated copy, but Google revokes the
        // account's whole grant to gcloud's client, so an own gcloud login
        // with the same account may need signing in again too.
        h('li', null, icon('triangle-alert', { size: 14 }), h('span', null, t(fromOwnGcloud() ? 'settings.colab.disconnect_point_gcloud' : 'settings.colab.disconnect_point_gcloud_app'))),
      ),
      okText: t('settings.colab.disconnect_ok'),
      danger: true,
    });
    if (!ok || !alive) return;
    disconnecting = true;
    redraw();
    try {
      const r = await appRequest('DELETE', '/api/colab/auth');
      if (!alive) return;
      const texts = [t('settings.colab.disconnected')];
      for (const w of (r && r.warnings) || []) texts.push(U.errorInfoText(w));
      for (const n of (r && r.notes) || []) texts.push(U.codedText('notes', n, ''));
      toast(U.clauseJoin(texts.filter(Boolean)), r && r.warnings && r.warnings.length ? 'warn' : 'success');
    } catch (e) {
      if (alive) toast(e.message, 'error');
    }
    disconnecting = false;
    checked = null;
    await load();
  }

  // ---- render ----

  const note = (ic, text, cls = '') => h('p', { class: 'set-note' + (cls ? ' ' + cls : '') }, icon(ic, { size: 13 }), h('span', null, text));

  // 3.1.0: the in-app sign-in — install the Google Cloud CLI component when
  // it is missing, then "sign in to Google" (waiting, the fallback link,
  // cancel, failures with gcloud's message: as in the onboarding).
  function appLoginBlock() {
    if (!loginStarted) {
      loginStarted = true;
      appLogin.start();
    }
    const parts = [h('p', { class: 'colab-login-hint' }, t('settings.colab.app_login_hint'))];
    if (appLogin.loading) {
      parts.push(note('loader-circle', t('onboarding.loading')));
      return h('div', { class: 'colab-login' }, parts);
    }
    const g = appLogin.gcloud();
    const ready = appLogin.gcloudReady();
    if (!ready && !appLogin.installBusy) {
      parts.push(
        h(
          'div',
          { class: 'set-inline colab-login-actions' },
          btn(t('settings.colab.install_gcloud'), { size: 'sm', kind: 'primary', icon: 'download', fkey: 'set:colab:gcloud', onclick: () => appLogin.install() }),
          g && g.download_bytes ? h('span', { class: 'dim small' }, t('settings.colab.gcloud_size', { size: U.fmtBytes(g.download_bytes) })) : null,
        ),
      );
    } else {
      const block = appLogin.gcloudBlock();
      if (block) parts.push(block);
    }
    if (appLogin.startError) parts.push(note('circle-alert', appLogin.startError, 'is-warn'));
    if (ready) {
      parts.push(...appLogin.loginBlock({ idleHint: false }));
      const a = appLogin.loginAction();
      parts.push(h('div', { class: 'set-inline colab-login-actions' }, btn(a.label, { size: 'sm', kind: 'primary', icon: a.icon, disabled: a.disabled, fkey: 'set:colab:login', onclick: a.onclick }), appLogin.cancelButton({ size: 'sm' })));
    }
    return h('div', { class: 'colab-login' }, parts);
  }

  // The earlier manual way, folded: run gcloud yourself, 检测授权, import.
  function advancedBlock() {
    const adc = view.adc || {};
    const open = advancedOpen != null ? advancedOpen : !!(checked || adc.detected);
    const d = h(
      'details',
      { class: 'colab-advanced', open },
      h('summary', { dataset: { fkey: 'set:colab:advanced' } }, t('settings.colab.advanced')),
      h('p', { class: 'dim small colab-advanced-hint' }, t('settings.colab.advanced_hint')),
      guide(),
      importRow(),
    );
    d.addEventListener('toggle', () => {
      advancedOpen = d.open;
    });
    return d;
  }
  const savingNote = (field) => (saving === field ? note('loader-circle', t('settings.saving')) : null);

  function privacyBlock() {
    return h(
      'div',
      { class: 'colab-privacy', role: 'note' },
      icon('shield-check', { size: 16 }),
      h('div', { class: 'colab-privacy-text' }, h('strong', null, t('settings.colab.privacy_title')), h('p', null, t('settings.colab.privacy')), h('p', { class: 'dim' }, t('settings.colab.fair_note'))),
    );
  }

  function guide() {
    const adc = view.adc || {};
    const cmd = adc.login_command || '';
    const found = adc.detected;
    const problem = !found && adc.problem ? U.errorInfoText({ code: adc.problem, params: { path: adc.path || '', field: '', type: '', scopes: '' } }) : '';
    return h(
      'ol',
      { class: 'colab-steps' },
      h('li', null, h('span', null, t('settings.colab.step1'))),
      h(
        'li',
        null,
        h('span', null, t('settings.colab.step2')),
        h(
          'div',
          { class: 'colab-cmd' },
          h('code', { class: 'mono colab-cmd-text' }, cmd),
          btn(t('settings.colab.copy_cmd'), {
            size: 'sm',
            kind: 'ghost',
            icon: 'copy',
            fkey: 'set:colab:copy',
            onclick: async () => {
              const ok = await copyText(cmd);
              toast(ok ? t('settings.colab.copied') : t('common.copy_failed'), ok ? 'success' : 'warn');
            },
          }),
        ),
      ),
      h(
        'li',
        null,
        h('span', null, t('settings.colab.step3')),
        h(
          'div',
          { class: 'set-inline colab-check' },
          btn(checking ? t('settings.colab.checking') : t('settings.colab.check'), { size: 'sm', icon: checking ? 'loader-circle' : 'scan-search', disabled: checking, fkey: 'set:colab:check', onclick: check }),
          checked || found || problem
            ? found
              ? note('circle-check', t('settings.colab.adc_found'), 'is-ok')
              : note(checked ? 'circle-alert' : 'info', problem || t('settings.colab.adc_missing'), checked ? 'is-warn' : '')
            : null,
        ),
      ),
    );
  }

  function importRow() {
    const adc = view.adc || {};
    const opts = LIFECYCLES.map((v) => [v, v === 'days' ? t('colab.lifecycle.days', { n: days, days }) : t(`colab.lifecycle.${v}`)]);
    const daysInput = h('input', {
      class: 'input input-num colab-days',
      type: 'number',
      min: '1',
      max: '90',
      step: '1',
      value: String(days),
      disabled: lifecycle !== 'days' || importing,
      'aria-label': t('settings.colab.days_label'),
      dataset: { fkey: 'set:colab:days' },
    });
    const commitDays = () => {
      const n = Number(daysInput.value.trim());
      if (n === days) return;
      days = n;
      daysError = Number.isInteger(n) && n >= 1 && n <= 90 ? '' : t('settings.colab.days_invalid', { min: 1, max: 90 });
      redraw();
    };
    daysInput.addEventListener('change', commitDays);
    daysInput.addEventListener('keydown', (e) => e.key === 'Enter' && commitDays());
    return ui.formRow(
      t('settings.colab.lifecycle_label'),
      h(
        'div',
        { class: 'colab-import' },
        ui.segmented('set:colab:lifecycle', opts, lifecycle, (v) => ((lifecycle = v), redraw()), t('settings.colab.lifecycle_label')),
        h(
          'div',
          { class: 'set-inline colab-import-line' },
          lifecycle === 'days' ? h('label', { class: 'set-inline colab-days-wrap' }, h('span', { class: 'dim small' }, t('settings.colab.days_label')), daysInput) : null,
          btn(importing ? t('settings.colab.importing') : view.status.auth && view.status.auth.present ? t('settings.colab.reimport') : t('settings.colab.import'), {
            size: 'sm',
            kind: view.status.auth && view.status.auth.present ? 'ghost' : 'primary',
            icon: importing ? 'loader-circle' : 'key-round',
            disabled: importing || !adc.detected || !!daysError,
            title: adc.detected ? undefined : t('settings.colab.import_need_adc'),
            fkey: 'set:colab:import',
            onclick: importAuth,
          }),
        ),
      ),
      t('settings.colab.lifecycle_hint'),
      daysError ? note('circle-alert', daysError, 'is-err') : null,
    );
  }

  // "导入于 {date}" with the credential's source when the back end gives it.
  function importedText(a) {
    const date = U.fmtDateTime(a.imported_at);
    if (a.source === 'app') return t('settings.colab.imported_at_app', { date });
    if (a.source === 'gcloud') return t('settings.colab.imported_at_gcloud', { date });
    return t('settings.colab.imported_at', { date });
  }

  function authRow() {
    const a = view.status.auth || {};
    if (!a.present) {
      return ui.formRow(t('settings.colab.status_label'), h('span', { class: 'chip chip-quiet' }, t('settings.colab.status_none')), t('settings.colab.status_none_hint'));
    }
    const lc = a.lifecycle || '';
    const lcText = lc === 'session' || lc === 'manual' ? t(`colab.lifecycle.${lc}`) : /^\d+d$/.test(lc) ? t('colab.lifecycle.days', { n: parseInt(lc, 10), days: parseInt(lc, 10) }) : lc;
    const left = a.expires_at ? t('settings.colab.expires', { date: U.fmtDateTime(a.expires_at), left: durText(a.remaining_sec) || durText(0) }) : authLeftText(a);
    return ui.formRow(
      t('settings.colab.status_label'),
      h(
        'div',
        { class: 'colab-auth' },
        h('div', { class: 'set-inline colab-auth-line' }, h('span', { class: 'chip chip-ok' }, icon('circle-check', { size: 12 }), t('settings.colab.status_ok')), h('span', { class: 'dim small' }, lcText)),
        left ? h('span', { class: 'colab-auth-left' }, left) : null,
        a.imported_at ? h('span', { class: 'dim small' }, importedText(a)) : null,
      ),
      null,
      btn(disconnecting ? t('settings.colab.disconnecting') : t('settings.colab.disconnect'), { size: 'sm', kind: 'danger', icon: 'lock-open', disabled: disconnecting, fkey: 'set:colab:disconnect', onclick: disconnect }),
    );
  }

  function enabledRow() {
    const s = view.settings;
    const authed = !!(view.status.auth && view.status.auth.present);
    return ui.formRow(
      t('settings.colab.enabled_label'),
      ui.switchBtn(!!s.enabled, t('settings.colab.enabled_label'), () => save('enabled', { enabled: !s.enabled }), 'set:colab:enabled'),
      t('settings.colab.enabled_hint'),
      [savingNote('enabled'), !authed && s.enabled ? note('info', t('settings.colab.enabled_need_auth'), 'is-warn') : null, view.held ? note('circle-pause', t('settings.colab.held'), 'is-warn') : null],
    );
  }

  function accelRow() {
    const s = view.settings;
    const list = ACCELERATORS.includes(s.accelerator) ? ACCELERATORS : [s.accelerator, ...ACCELERATORS];
    const sel = h(
      'select',
      { class: 'select mono colab-sel', 'aria-label': t('settings.colab.accel_label'), disabled: !!saving, dataset: { fkey: 'set:colab:accel' }, onchange: (e) => save('accel', { accelerator: e.target.value }) },
      list.map((a) => h('option', { value: a, selected: a === s.accelerator }, a)),
    );
    return ui.formRow(t('settings.colab.accel_label'), sel, t('settings.colab.accel_hint'), savingNote('accel'));
  }

  function modelRow() {
    const s = view.settings;
    const typed = modelDraft != null ? modelDraft : s.model;
    const inp = h('input', {
      class: 'input mono colab-model',
      value: typed,
      list: 'colab-models',
      spellcheck: 'false',
      autocomplete: 'off',
      disabled: !!saving,
      'aria-label': t('settings.colab.model_label'),
      dataset: { fkey: 'set:colab:model' },
    });
    const commit = () => {
      const v = inp.value.trim();
      modelDraft = v;
      if (v && v !== s.model) save('model', { model: v });
    };
    inp.addEventListener('change', commit);
    inp.addEventListener('keydown', (e) => e.key === 'Enter' && commit());
    const precs = PRECISIONS.includes(s.compute_type) ? PRECISIONS : [s.compute_type, ...PRECISIONS];
    const prec = h(
      'select',
      { class: 'select mono colab-sel', 'aria-label': t('settings.colab.precision_label'), disabled: !!saving, dataset: { fkey: 'set:colab:precision' }, onchange: (e) => save('model', { compute_type: e.target.value }) },
      precs.map((p) => h('option', { value: p, selected: p === s.compute_type }, p)),
    );
    return ui.formRow(
      t('settings.colab.model_label'),
      h('div', { class: 'set-inline colab-model-line' }, inp, h('datalist', { id: 'colab-models' }, MODELS.map((m) => h('option', { value: m }))), prec),
      t('settings.colab.model_hint'),
      savingNote('model'),
    );
  }

  function tierRow() {
    const s = view.settings;
    const opts = [
      ['paid', t('settings.colab.tier.paid')],
      ['free', t('settings.colab.tier.free')],
    ];
    return ui.formRow(
      t('settings.colab.tier_label'),
      ui.segmented('set:colab:tier', opts, s.tier, (v) => save('tier', { tier: v }), t('settings.colab.tier_label')),
      s.tier === 'free' ? null : t('settings.colab.tier_paid_hint'),
      [s.tier === 'free' ? note('triangle-alert', t('settings.colab.tier_free_hint'), 'is-warn') : null, savingNote('tier')],
    );
  }

  function waitRow() {
    const s = view.settings;
    const inp = h('input', {
      class: 'input input-num colab-wait',
      type: 'number',
      min: '1',
      max: '720',
      step: '1',
      value: String(waitDraft != null ? waitDraft : s.max_queue_wait_min),
      disabled: !!saving,
      'aria-label': t('settings.colab.wait_label'),
      dataset: { fkey: 'set:colab:wait' },
    });
    const commit = () => {
      const raw = inp.value.trim();
      const n = raw === '' ? NaN : Number(raw);
      if (n === s.max_queue_wait_min) {
        waitDraft = null;
        waitError = '';
        return;
      }
      waitDraft = raw;
      if (!Number.isInteger(n) || n < 1 || n > 720) {
        waitError = t('settings.colab.wait_invalid', { min: 1, max: 720 });
        redraw();
        return;
      }
      waitError = '';
      save('wait', { max_queue_wait_min: n });
    };
    inp.addEventListener('change', commit);
    inp.addEventListener('keydown', (e) => e.key === 'Enter' && commit());
    return ui.formRow(
      t('settings.colab.wait_label'),
      h('span', { class: 'set-inline' }, inp, h('span', { class: 'dim small' }, t('settings.colab.minutes'))),
      t('settings.colab.wait_hint'),
      [waitError ? note('circle-alert', waitError, 'is-err') : null, savingNote('wait')],
    );
  }

  function batteryRow() {
    const s = view.settings;
    return ui.formRow(
      t('settings.colab.battery_label'),
      ui.switchBtn(!!s.battery_prefer, t('settings.colab.battery_label'), () => save('battery', { battery_prefer: !s.battery_prefer }), 'set:colab:battery'),
      t('settings.colab.battery_hint'),
      savingNote('battery'),
    );
  }

  function render(shell) {
    shell.append(ui.head('zap', t('settings.colab.title')));
    if (unavailable) {
      shell.append(h('div', { class: 'card-bd' }, note('info', t('settings.colab.unavailable'))));
      return;
    }
    if (!view) {
      shell.append(h('div', { class: 'card-bd' }, error ? errorBlock(t('settings.colab.load_error', { error }), retry) : skeleton(4)));
      return;
    }
    const authed = !!(view.status.auth && view.status.auth.present);
    shell.append(
      h(
        'div',
        { class: 'card-bd set-form colab-form' },
        privacyBlock(),
        saveError ? h('p', { class: 'set-note is-warn', role: 'alert' }, icon('circle-alert', { size: 13 }), h('span', null, saveError)) : null,
        h('h3', { class: 'colab-sub' }, t('settings.colab.auth_title')),
        authed ? null : appLoginBlock(),
        authRow(),
        authed ? null : advancedBlock(),
        h('h3', { class: 'colab-sub' }, t('settings.colab.use_title')),
        enabledRow(),
        accelRow(),
        modelRow(),
        tierRow(),
        waitRow(),
        batteryRow(),
      ),
    );
  }

  function retry() {
    error = '';
    redraw();
    load();
  }

  return {
    render,
    load,
    // SSE "changed settings": another window changed Colab (or the auth)
    onSettingsChanged() {
      if (!saving && !importing && !disconnecting) load();
    },
    // SSE "changed colab": the in-app sign-in moved on
    onChanged(ev) {
      appLogin.onChanged(ev);
    },
    destroy() {
      alive = false;
      appLogin.destroy();
    },
  };
}
