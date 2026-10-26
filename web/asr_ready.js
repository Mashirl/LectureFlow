// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// "Cannot transcribe yet" (3.1.0 M2, .claude/v31-brief.md §1): when this
// computer has no enabled, usable local transcription device and Colab is
// not signed in, the onboarding's last step, the workbench and the course
// page say so prominently, with two ways out: "install local transcription
// as recommended" and "set up the remote GPU" (both open the onboarding's
// transcription-mode step with that choice made).
//
// A local device counts when GET /api/asr/devices lists it present and
// enabled; in the desktop app its back end's runtime component must also be
// installed (GET /api/components: a venv component with that backend) —
// the CPU row exists on every Windows computer, also before any component
// is there. The remote GPU counts when GET /api/colab has a credential and
// Colab is switched on. When any of these requests fails nothing is shown
// (no false alarm).
//
// While a transcription runtime (or the gcloud tool) is being installed
// the notice says "preparing transcription" instead, with the download's
// progress (lead, 3.1.0): updated in place from the SSE "components" event,
// or every 1.5 s from GET /api/components/tasks when the stream is quiet.

import { h, clear, btn } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import * as U from './util.js';
import { onComponentsEvent, componentsEventFresh } from './api.js';
import { obApi } from './onboarding_api.js';
import { taskActive } from './components_ui.js';

const PROGRESS_MS = 1500;

/**
 * Resolves to {ready, local, remote, installing, desktop, watched}, or null
 * when it cannot be told (a request failed). watched: the component ids
 * whose install counts as "preparing transcription".
 */
export async function asrReadiness() {
  const [app, devices, colab] = await Promise.all([
    obApi.app({ quiet: true }).catch(() => null),
    obApi.devices({ quiet: true }).catch(() => null),
    // 503 / 404: this server has no Colab back end
    obApi.colab({ quiet: true }).catch((e) => (e && (e.status === 503 || e.status === 404) ? { none: true } : null)),
  ]);
  if (!Array.isArray(devices) || !colab) return null;
  const desktop = !!app && app.mode === 'desktop';
  let comps = [];
  let tasks = [];
  if (desktop) {
    const list = await obApi.components({ quiet: true }).catch(() => null);
    if (!list) return null;
    comps = list.components || [];
    tasks = list.tasks || [];
  }
  const runtimes = comps.filter((c) => c.kind === 'venv' && c.backend);
  const usable = new Set(runtimes.filter((c) => c.state === 'installed' || (c.state === 'installing' && c.installed_version)).map((c) => c.backend));
  const local = devices.some((d) => d && d.present && d.enabled && d.kind !== 'remote' && d.backend !== 'colab' && (!desktop || usable.has(d.backend)));
  const remote = !colab.none && !!(colab.status && colab.status.auth && colab.status.auth.present) && !!(colab.settings && colab.settings.enabled);
  const watched = [...runtimes.map((c) => c.id), 'gcloud'];
  const installing = preparing(tasks, watched).length > 0;
  return { ready: local || remote, local, remote, installing, desktop, watched };
}

// the active tasks that install a watched component
function preparing(tasks, watched) {
  return (tasks || []).filter((tk) => taskActive(tk) && (tk.ids || []).some((id) => watched.includes(id)));
}

/** Where the two entries lead by default: the onboarding's mode step with that choice. */
export const LOCAL_ENTRY = '#/onboarding/asr_mode/local';
export const REMOTE_ENTRY = '#/onboarding/asr_mode/remote';

/**
 * The notice of a page. opts:
 *   variant   'page' (workbench, course page) | 'onboarding' (the last step)
 *   onLocal   the "install as recommended" entry (default: LOCAL_ENTRY)
 *   onRemote  the "set up the remote GPU" entry (default: REMOTE_ENTRY)
 * Returns {el, refresh(), onChanged(ev), destroy()}: el stays in the page
 * and is filled in place; onChanged takes the SSE "changed" events.
 */
export function asrNotice({ variant = 'page', onLocal, onRemote } = {}) {
  const el = h('div', { class: 'asr-notice-slot', dataset: { variant } });
  let alive = true;
  let seq = 0;
  let state = null;
  let timer = null;
  let progressTimer = null;
  let sig = '';
  let progress = null; // {text, fill, bar} of the "preparing" form

  const go = (fn, hash) => () => (fn ? fn() : (location.hash = hash));

  async function refresh() {
    const n = ++seq;
    const r = await asrReadiness().catch(() => null);
    if (!alive || n !== seq) return;
    const before = JSON.stringify(state);
    state = r;
    if (JSON.stringify(state) !== before) render();
    if (state && !state.ready && state.installing) pollProgress();
    else clearTimeout(progressTimer);
  }

  function later(ms = 600) {
    clearTimeout(timer);
    timer = setTimeout(refresh, ms);
  }

  // The tasks' states (and their items'): what changes the notice itself.
  const tasksSig = (tasks) => (tasks || []).map((x) => `${x.id}:${x.state}`).join(';');

  // an install started or ended (the server detects the devices again); the
  // progress of the "preparing" form is updated in place
  const unsubscribe = onComponentsEvent((ev) => {
    const tasks = (ev && ev.tasks) || [];
    showProgress(tasks);
    const s = tasksSig(tasks);
    if (s === sig) return;
    sig = s;
    later(800);
  });

  // the event stream is quiet: GET /api/components/tasks
  function pollProgress() {
    clearTimeout(progressTimer);
    progressTimer = setTimeout(async () => {
      if (!alive || !state || state.ready || !state.installing) return;
      if (!componentsEventFresh()) {
        const tasks = await obApi.tasks({ quiet: true }).catch(() => null);
        if (!alive) return;
        if (tasks) {
          showProgress(tasks);
          const s = tasksSig(tasks);
          if (s !== sig) {
            sig = s;
            refresh();
            return;
          }
        }
      }
      pollProgress();
    }, PROGRESS_MS);
  }

  function showProgress(tasks) {
    if (!progress || !state || !state.installing) return;
    const active = preparing(tasks, state.watched || []);
    if (!active.length) return; // refresh() follows from the state change
    let done = 0;
    let total = 0;
    for (const tk of active) {
      total += tk.total_bytes || 0;
      done += Math.min(tk.done_bytes || 0, tk.total_bytes || 0);
    }
    const frac = total > 0 ? done / total : 0;
    const phase = active.every((tk) => tk.state === 'verifying' || tk.state === 'installing') ? t(`compui.task.state.${active[0].state}`) : '';
    progress.text.textContent = phase || (total > 0 ? t('compui.task.progress', { done: U.fmtBytes(done), total: U.fmtBytes(total) }) + ' · ' + U.fmtPercent(frac, 0) : '');
    progress.fill.style.width = `${Math.round(frac * 100)}%`;
    progress.bar.setAttribute('aria-valuenow', String(Math.round(frac * 100)));
  }

  function render() {
    clear(el);
    progress = null;
    if (!state || state.ready) return;
    if (state.installing) {
      const text = h('span', { class: 'asr-notice-progress-text' });
      const fill = h('div', { class: 'cu-bar-fill', style: { width: '0%' } });
      const bar = h('div', { class: 'cu-bar asr-notice-bar', role: 'progressbar', 'aria-label': t('asr_ready.installing'), 'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': '0' }, fill);
      progress = { text, fill, bar };
      el.append(
        h(
          'section',
          { class: 'asr-notice is-preparing', role: 'status', 'aria-label': t('asr_ready.installing') },
          h('span', { class: 'asr-notice-icon', 'aria-hidden': 'true' }, icon('loader-circle', { size: 18, class: 'spin' })),
          h(
            'div',
            { class: 'asr-notice-main' },
            h('p', { class: 'asr-notice-title' }, t('asr_ready.installing')),
            h('p', { class: 'asr-notice-text' }, t('asr_ready.installing_hint')),
            bar,
            h('p', { class: 'asr-notice-progress' }, text, ' ', h('a', { class: 'ob-inline-link', href: '#/settings/components', dataset: { fkey: 'asr:progress' } }, t('asr_ready.progress'))),
          ),
        ),
      );
      // the first figures at once
      obApi
        .tasks({ quiet: true })
        .then((tasks) => alive && tasks && showProgress(tasks))
        .catch(() => {});
      return;
    }
    el.append(
      h(
        'section',
        { class: 'asr-notice', role: 'status', 'aria-label': t('asr_ready.title') },
        h('span', { class: 'asr-notice-icon', 'aria-hidden': 'true' }, icon('triangle-alert', { size: 18 })),
        h('div', { class: 'asr-notice-main' }, h('p', { class: 'asr-notice-title' }, t('asr_ready.title')), h('p', { class: 'asr-notice-text' }, t('asr_ready.text'))),
        h(
          'div',
          { class: 'asr-notice-actions' },
          btn(t('asr_ready.local'), { size: 'sm', kind: 'primary', icon: 'cpu', fkey: 'asr:local', onclick: go(onLocal, LOCAL_ENTRY) }),
          btn(t('asr_ready.remote'), { size: 'sm', icon: 'cloud', fkey: 'asr:remote', onclick: go(onRemote, REMOTE_ENTRY) }),
        ),
      ),
    );
  }

  refresh();

  return {
    el,
    refresh,
    get state() {
      return state;
    },
    onChanged(ev) {
      if (ev && (ev.kind === 'colab' || ev.kind === 'devices' || ev.kind === 'settings')) later();
    },
    destroy() {
      alive = false;
      clearTimeout(timer);
      clearTimeout(progressTimer);
      unsubscribe();
    },
  };
}
