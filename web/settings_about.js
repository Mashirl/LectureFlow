// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// The settings page's "About" section (SPEC-v3 V11, V7): the version, the
// project licence (AGPL-3.0-or-later, full text from GET /api/app/license)
// and the third-party notices (GET /api/app/notices, THIRD_PARTY_NOTICES.md
// rendered by markdown.js), each shown in a scrolling dialog.

import { h, btn, skeleton, errorBlock, toast, openDialog } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import { renderMarkdown } from './markdown.js';
import { appRequest, loadAppInfo } from './settings_app.js';

// Operating system names are product names (not translated).
const PLATFORMS = { windows: 'Windows', darwin: 'macOS', linux: 'Linux' };
const ARCHES = { amd64: 'x64', arm64: 'arm64' };

const texts = {}; // path → text, kept for the page session

export function createAboutSection(ui) {
  let alive = true;
  let app = null;
  let error = '';
  let loading = ''; // 'license' | 'notices'

  const redraw = () => alive && ui.rerender();

  async function load() {
    try {
      const a = await loadAppInfo();
      if (!alive) return;
      app = a;
      error = '';
    } catch (e) {
      if (!alive) return;
      if (!app) error = e.message || t('common.load_failed');
    }
    redraw();
  }

  async function fetchText(path) {
    if (texts[path] == null) texts[path] = await appRequest('GET', path, undefined, { as: 'text' });
    return texts[path];
  }

  async function show(kind) {
    if (loading) return;
    loading = kind;
    redraw();
    let text;
    try {
      text = await fetchText(kind === 'license' ? '/api/app/license' : '/api/app/notices');
    } catch (e) {
      if (alive) toast(e.message, 'error');
      return;
    } finally {
      loading = '';
      redraw();
    }
    const title = kind === 'license' ? t('settings.about.license_title') : t('settings.about.notices_title');
    openDialog(
      title,
      (close) =>
        h(
          'div',
          { class: 'about-doc-wrap' },
          kind === 'license'
            ? h('pre', { class: 'about-doc about-license', tabindex: '0', lang: 'en' }, text)
            : // markdown.js shows HTML as text: drop the generator's <!-- … --> comment
              h('div', { class: 'about-doc md about-notices', tabindex: '0' }, renderMarkdown(text.replace(/<!--[\s\S]*?-->\s*/g, ''))),
          h('div', { class: 'dlg-actions' }, h('button', { type: 'button', class: 'btn btn-primary', autofocus: true, onclick: () => close(null) }, t('common.close'))),
        ),
      { wide: true, className: 'about-dlg' },
    );
  }

  function render(shell) {
    shell.append(ui.head('info', t('settings.about.title')));
    if (!app) {
      shell.append(h('div', { class: 'card-bd' }, error ? errorBlock(t('settings.about.load_error', { error }), retry) : skeleton(3)));
      return;
    }
    const platform = [PLATFORMS[app.platform] || app.platform, ARCHES[app.arch] || app.arch].filter(Boolean).join(' · ');
    shell.append(
      h(
        'div',
        { class: 'card-bd set-form about-form' },
        ui.formRow(
          t('settings.about.version'),
          h(
            'div',
            { class: 'set-inline about-version' },
            h('span', { class: 'about-version-num mono' }, app.version),
            h('span', { class: 'chip chip-quiet' }, app.mode === 'desktop' ? t('settings.about.mode_desktop') : t('settings.about.mode_web')),
            platform ? h('span', { class: 'dim small' }, platform) : null,
          ),
        ),
        ui.formRow(
          t('settings.about.license'),
          [
            h('p', { class: 'about-text' }, t('settings.about.license_text')),
            btn(loading === 'license' ? t('settings.about.opening') : t('settings.about.license_view'), { size: 'sm', icon: 'shield-check', disabled: !!loading, onclick: () => show('license'), fkey: 'set:about:license' }),
          ],
          t('settings.about.license_hint'),
        ),
        ui.formRow(
          t('settings.about.notices'),
          [
            h('p', { class: 'about-text' }, t('settings.about.notices_text')),
            btn(loading === 'notices' ? t('settings.about.opening') : t('settings.about.notices_view'), { size: 'sm', icon: 'book-open', disabled: !!loading, onclick: () => show('notices'), fkey: 'set:about:notices' }),
          ],
        ),
        h('p', { class: 'set-note about-foot' }, icon('info', { size: 13 }), h('span', null, t('settings.about.no_telemetry'))),
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
    destroy() {
      alive = false;
    },
  };
}
