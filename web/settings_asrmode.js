// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// The transcription mode (ext-step1, SPEC-ext "主程序配套改动 1"): "本地" (this
// computer transcribes) or "只用云端" (only Google Colab). GET / PUT
// /api/asr-mode; the setup guide's "转写方式" step and the settings page's
// switch both write it. It decides what the diagnostics page checks (only
// Colab: no local inference rows, the Colab rows instead); which devices
// transcribe is still the device list's business.
//
// createASRModeRow(ui) is the switch as the first row of the settings
// page's Transcription block: it loads its own value, saves every change
// at once (unlike the block's other rows, which wait for its Save button)
// and shows only what the server returned.

import { h, toast } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import { appRequest } from './settings_app.js';

export const ASR_MODES = ['local', 'remote'];

/** GET /api/asr-mode → {mode, source}. */
export function loadASRMode() {
  return appRequest('GET', '/api/asr-mode');
}

/** PUT /api/asr-mode {mode} → {mode, source}. */
export function saveASRMode(mode) {
  return appRequest('PUT', '/api/asr-mode', { mode });
}

/**
 * ui: {formRow(label, control, hint, extra), segmented(name, options, value, onChange, label), rerender()}
 * (the settings page's own helpers). row() is null until the value is known.
 */
export function createASRModeRow(ui) {
  let alive = true;
  let mode = '';
  let saving = false;
  let error = '';
  let seq = 0;

  async function load() {
    const n = ++seq;
    try {
      const v = await loadASRMode();
      if (!alive || n !== seq || saving) return;
      mode = v && ASR_MODES.includes(v.mode) ? v.mode : 'local';
      error = '';
    } catch (e) {
      if (!alive || n !== seq) return;
      error = e.message || t('common.load_failed');
    }
    ui.rerender();
  }

  async function choose(next) {
    if (saving || next === mode) return;
    saving = true;
    error = '';
    seq++; // a load in flight must not overwrite the answer
    ui.rerender();
    try {
      const v = await saveASRMode(next);
      if (!alive) return;
      mode = v && ASR_MODES.includes(v.mode) ? v.mode : next;
      toast(t('settings.asr.asr_mode_saved'), 'success');
    } catch (e) {
      if (!alive) return;
      error = e.message || t('common.load_failed');
    } finally {
      saving = false;
      if (alive) ui.rerender();
    }
  }

  function row() {
    if (!mode && !error) return null;
    const label = t('settings.asr.asr_mode');
    const control = mode
      ? ui.segmented(
          'set:asr:asrmode',
          ASR_MODES.map((m) => [m, t(`settings.asr.asr_mode_${m}`)]),
          mode,
          choose,
          label,
        )
      : null;
    const extra = error
      ? h('p', { class: 'set-note is-warn', role: 'alert' }, icon('triangle-alert', { size: 13 }), h('span', null, error))
      : saving
        ? h('p', { class: 'set-note' }, icon('loader-circle', { size: 13, class: 'spin' }), h('span', null, t('settings.asr.asr_mode_saving')))
        : null;
    return ui.formRow(label, control, mode ? t(`settings.asr.asr_mode_hint_${mode}`) : '', extra);
  }

  return {
    load,
    row,
    destroy() {
      alive = false;
    },
  };
}
