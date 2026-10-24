// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// The privacy details page (#/privacy, SPEC-v3 V4.2 step 2 "详细说明"): a
// local page, all text through t(). It describes what stays on this
// computer, what is sent where and when, and that nothing is collected.

import { h } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';

const SECTIONS = [
  { id: 'local', icon: 'hard-drive', items: 3 },
  { id: 'sent', icon: 'server', items: 4 },
  { id: 'keys', icon: 'key-round', items: 2 },
  { id: 'downloads', icon: 'download', items: 2 },
  { id: 'colab', icon: 'cpu', items: 1 },
  { id: 'none', icon: 'shield-check', items: 3 },
  { id: 'delete', icon: 'trash-2', items: 2 },
];

/** Mounts #/privacy. from: the previous route's name ("onboarding" → back to it). */
export function mountPrivacy(main, ctx, from) {
  const back = from === 'onboarding' ? '#/onboarding' : from === 'settings' ? '#/settings' : null;
  const el = h(
    'div',
    { class: 'ob-page ob-privacy' },
    h(
      'header',
      { class: 'ob-top' },
      h('span', { class: 'ob-brand' }, h('span', { class: 'brand-mark' }, icon('audio-lines', { size: 16, stroke: 2.4 })), h('span', { class: 'ob-brand-name' }, 'LectureFlow')),
      h('span', { class: 'ob-grow' }),
      h(
        'a',
        { class: 'btn btn-ghost btn-sm', href: back || '#/', dataset: { fkey: 'privacy:back' }, onclick: (e) => !back && history.length > 1 && (e.preventDefault(), history.back()) },
        icon('arrow-left', { size: 14 }),
        h('span', null, back === '#/onboarding' ? t('privacy.back_onboarding') : t('privacy.back')),
      ),
    ),
    h(
      'article',
      { class: 'ob-card ob-doc' },
      h('div', { class: 'ob-icon', 'aria-hidden': 'true' }, icon('shield-check', { size: 22 })),
      h('h1', { class: 'ob-title', tabindex: '-1' }, t('privacy.title')),
      h('p', { class: 'ob-desc' }, t('privacy.intro')),
      SECTIONS.map((s) =>
        h(
          'section',
          { class: 'ob-doc-sec' },
          h('h2', { class: 'ob-h2' }, icon(s.icon, { size: 16 }), h('span', null, t(`privacy.${s.id}.title`))),
          h('ul', { class: 'ob-doc-list' }, Array.from({ length: s.items }, (_, i) => h('li', null, t(`privacy.${s.id}.${i + 1}`)))),
        ),
      ),
      h('p', { class: 'ob-hint' }, t('privacy.footer')),
    ),
  );
  main.append(el);
  const title = el.querySelector('.ob-title');
  if (title) title.focus({ preventScroll: true });
  return { destroy() {} };
}
