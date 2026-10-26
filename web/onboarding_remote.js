// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// The download step when the user chose the remote GPU (3.1.0,
// .claude/v31-brief.md §1, §2.2): instead of the transcription components,
//   1. the gcloud tool component (started in the background when the data
//      folder was confirmed, startChosenInstall; started here otherwise),
//      with the same task progress as the local download;
//   2. "sign in to Google": the in-app sign-in of colab_login.js (shared
//      with the settings page's Colab section);
//   3. once signed in: authorized, Colab is switched on (the user chose
//      it), and the paid / free tier can be picked (PUT /api/colab/settings,
//      the settings page's texts).
// Already signed in (GET /api/colab): straight to 3. Closing the app and
// coming back resumes from the server's state (a sign-in still waiting is
// followed again).

import { h, btn } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import { obApi } from './onboarding_api.js';
import { createColabLogin } from './colab_login.js';

const TIERS = ['paid', 'free'];

export function remoteStep(env) {
  const S = {
    loading: true,
    unavailable: false, // this server has no Colab back end (503)
    colab: null, // ColabView
    colabError: '',
    saving: '',
  };
  let alive = true;
  const login = createColabLogin({
    rerender: () => alive && env.rerender(),
    onAuthorized: loadColab,
    taskId: () => env.data.gcloud_task_id || '',
    saveTaskId: (id) => {
      env.data.gcloud_task_id = id;
      env.save();
    },
    fkey: 'ob:remote',
  });

  const authorized = () => !!(S.colab && S.colab.status && S.colab.status.auth && S.colab.status.auth.present);

  start();

  async function start() {
    S.loading = true;
    S.colabError = '';
    let colab = null;
    try {
      colab = await obApi.colab({ quiet: true });
    } catch (e) {
      if (e && (e.status === 503 || e.status === 404)) S.unavailable = true;
      else S.colabError = e.message;
    }
    if (!alive) return;
    S.colab = colab;
    if (authorized()) await ensureEnabled();
    else if (!S.unavailable) await login.start({ install: true });
    S.loading = false;
    if (alive) env.rerender();
  }

  async function loadColab() {
    const v = await obApi.colab({ quiet: true }).catch(() => null);
    if (!alive || !v) return;
    S.colab = v;
    if (authorized()) await ensureEnabled();
  }

  // The user chose the remote GPU: switch Colab on once it is authorized.
  async function ensureEnabled() {
    if (!S.colab || !S.colab.settings || S.colab.settings.enabled) return;
    try {
      const v = await obApi.colabSettings({ enabled: true });
      if (alive && v) S.colab = v;
    } catch {
      // shown by the error handler; the switch stays in the settings
    }
  }

  async function setTier(tier) {
    if (S.saving || !S.colab || S.colab.settings.tier === tier) return;
    S.saving = 'tier';
    env.rerender();
    try {
      const v = await obApi.colabSettings({ tier });
      if (alive && v) S.colab = v;
    } catch {
      // shown by the error handler
    }
    S.saving = '';
    if (alive) env.rerender();
  }

  // ---- render ----

  function tierBlock() {
    const s = S.colab.settings || {};
    return h(
      'fieldset',
      { class: 'ob-field' },
      h('legend', { class: 'ob-label' }, t('settings.colab.tier_label')),
      h(
        'div',
        { class: 'ob-tiers', role: 'radiogroup' },
        TIERS.map((tier) =>
          h(
            'label',
            { class: 'ob-tier' + (s.tier === tier ? ' is-on' : '') },
            h('input', { type: 'radio', name: 'ob-colab-tier', value: tier, checked: s.tier === tier, disabled: !!S.saving, dataset: { fkey: 'ob:remote:tier:' + tier }, onchange: () => setTier(tier) }),
            h('span', { class: 'ob-tier-text' }, h('span', { class: 'ob-tier-name' }, t(`settings.colab.tier.${tier}`)), h('span', { class: 'ob-tier-desc' }, t(`settings.colab.tier_${tier}_hint`))),
          ),
        ),
      ),
    );
  }

  function startFailed() {
    const error = S.colabError || login.startError;
    if (!error) return null;
    return h(
      'div',
      { class: 'ob-callout is-warn', role: 'alert' },
      icon('circle-alert', { size: 16 }),
      h('div', null, h('p', null, t('onboarding.download.start_failed', { error })), h('p', { class: 'dim' }, t('onboarding.download.start_failed_todo'))),
    );
  }

  function body() {
    if (S.loading || (!authorized() && login.loading)) return h('div', { class: 'ob-loading' }, icon('loader-circle', { size: 18, class: 'spin' }), h('span', null, t('onboarding.download.starting')));
    if (S.unavailable) return h('p', { class: 'ob-callout is-info' }, icon('info', { size: 16 }), h('span', null, t('settings.colab.unavailable')));
    if (authorized()) return [h('p', { class: 'ob-callout is-ok', role: 'status' }, icon('circle-check', { size: 16 }), h('span', null, t('onboarding.remote.done'))), tierBlock()];
    const g = login.gcloudBlock();
    return [startFailed(), g ? h('div', { class: 'ob-section' }, g) : null, h('div', { class: 'ob-section' }, h('h2', { class: 'ob-h2' }, icon('key-round', { size: 16 }), t('onboarding.remote.login_title')), login.loginBlock())];
  }

  return {
    title: () => t('onboarding.remote.title'),
    desc: () => t('onboarding.remote.desc'),
    body,
    secondary() {
      if (!S.loading && (S.colabError || login.startError)) {
        return [btn(t('common.retry'), { kind: 'ghost', icon: 'rotate-cw', fkey: 'ob:remote:restart', onclick: () => start() })];
      }
      const c = login.cancelButton();
      return c ? [c] : [];
    },
    primary() {
      if (S.loading) return { label: t('onboarding.remote.login'), icon: 'log-in', disabled: true };
      if (S.unavailable || authorized()) return { label: t('onboarding.continue'), icon: 'arrow-right', onclick: env.next };
      return login.loginAction();
    },
    onChanged(ev) {
      login.onChanged(ev);
    },
    destroy() {
      alive = false;
      login.destroy();
    },
  };
}
