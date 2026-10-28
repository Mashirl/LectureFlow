// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// The settings page's "Downloads" section (SPEC-ext 下载队列, ext-brief
// §6.2): how many recordings download at once (download_concurrency, 1–6,
// default 3) and how many connections one file uses (download_connections,
// 1–16, default 4; the queue never uses more than the recording platform's
// profile allows). Read and written through GET/PATCH /api/app/settings in
// both modes.
//
// Like the App section (settings_app.js): a change is sent at once and the
// section re-renders from the response; a failed save shows the stored
// value again with the error.

import { h, skeleton, errorBlock } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import * as U from './util.js';
import { appRequest } from './settings_app.js';

// the back end's limits (internal/fetch: MinConcurrency … DefaultConnections)
export const CONCURRENCY = { min: 1, max: 6, def: 3 };
export const CONNECTIONS = { min: 1, max: 16, def: 4 };

const range = (r) => Array.from({ length: r.max - r.min + 1 }, (_, i) => r.min + i);
// the stored value; a server without the field has the default
const valueOf = (v, r) => (Number.isInteger(v) && v >= r.min && v <= r.max ? v : r.def);

// createDownloadsSection({head, formRow, segmented, rerender}): the helpers
// of view_settings.js; rerender() redraws the block.
export function createDownloadsSection(ui) {
  let alive = true;
  let settings = null; // GET /api/app/settings
  let error = '';
  let saving = ''; // field being saved
  let saveError = '';
  const redraw = () => alive && ui.rerender();

  async function load() {
    try {
      const s = await appRequest('GET', '/api/app/settings');
      if (!alive) return;
      settings = s;
      error = '';
    } catch (e) {
      if (!alive) return;
      if (!settings) error = e.message || t('common.load_failed');
    }
    redraw();
  }

  async function patch(field, value) {
    if (saving) return;
    saving = field;
    saveError = '';
    redraw();
    try {
      const s = await appRequest('PATCH', '/api/app/settings', { [field]: value });
      if (!alive) return;
      settings = s;
    } catch (e) {
      if (!alive) return;
      saveError = e.message;
    }
    saving = '';
    redraw();
  }

  const savingNote = () => h('p', { class: 'set-note' }, icon('loader-circle', { size: 13, class: 'spin' }), h('span', null, t('settings.saving')));

  function concurrencyRow() {
    const value = valueOf(settings.download_concurrency, CONCURRENCY);
    const options = range(CONCURRENCY).map((n) => [String(n), U.fmtInt(n)]);
    return ui.formRow(
      t('settings.downloads.concurrency_label'),
      ui.segmented('set:dl:concurrency', options, String(value), (v) => patch('download_concurrency', Number(v)), t('settings.downloads.concurrency_label')),
      t('settings.downloads.concurrency_hint', { n: U.count(CONCURRENCY.def) }),
      saving === 'download_concurrency' ? savingNote() : null,
    );
  }

  function connectionsRow() {
    const value = valueOf(settings.download_connections, CONNECTIONS);
    const select = h(
      'select',
      {
        class: 'select dl-conn',
        'aria-label': t('settings.downloads.connections_label'),
        disabled: !!saving,
        dataset: { fkey: 'set:dl:connections' },
        value: String(value),
        onchange: (e) => patch('download_connections', Number(e.target.value)),
      },
      range(CONNECTIONS).map((n) => h('option', { value: String(n) }, U.fmtInt(n))),
    );
    return ui.formRow(
      t('settings.downloads.connections_label'),
      h('div', { class: 'set-inline dl-conn-line' }, select, h('span', { class: 'dim small' }, t('settings.downloads.connections_cap'))),
      t('settings.downloads.connections_hint', { n: U.count(CONNECTIONS.def) }),
      saving === 'download_connections' ? savingNote() : null,
    );
  }

  function render(shell) {
    shell.append(ui.head('download', t('settings.downloads.title')));
    if (!settings) {
      shell.append(h('div', { class: 'card-bd' }, error ? errorBlock(t('settings.downloads.load_error', { error }), retry) : skeleton(2)));
      return;
    }
    shell.append(
      h(
        'div',
        { class: 'card-bd set-form dl-form' },
        h('p', { class: 'set-intro' }, t('settings.downloads.intro')),
        saveError ? h('p', { class: 'set-note is-warn', role: 'alert' }, icon('circle-alert', { size: 13 }), h('span', null, saveError)) : null,
        concurrencyRow(),
        connectionsRow(),
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
    // SSE "changed settings": another window changed them
    async onSettingsChanged() {
      if (saving) return;
      try {
        const s = await appRequest('GET', '/api/app/settings');
        if (!alive) return;
        if (JSON.stringify(s) !== JSON.stringify(settings)) {
          settings = s;
          redraw();
        }
      } catch {
        // keep what is shown
      }
    },
    destroy() {
      alive = false;
    },
  };
}
