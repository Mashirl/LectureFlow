// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Course card of the dashboard's course overview and the course list (v1.1
// A9, A12.2 course-card context menu). Works with api.CourseCard and
// api.Course.

import { h } from './dom.js';
import { icon } from './icons.js';
import { bindContextMenu } from './menu.js';
import { courseMenuItems } from './actions.js';
import * as U from './util.js';
import { t } from './i18n.js';

// t(key, {n}) with the number in bold: the catalog text is split at the
// formatted number.
function boldCount(key, n) {
  const s = t(key, { n });
  const num = String(n);
  const i = s.indexOf(num);
  if (i < 0) return [s];
  return [s.slice(0, i), h('b', null, num), s.slice(i + num.length)].filter((x) => x !== '');
}

// Course → the CourseCard shape (same field names as the dashboard's cards).
export function cardOf(c) {
  if (c.stale_count !== undefined) return c;
  return {
    id: c.id,
    name: c.name,
    code: c.code,
    lecture_count: c.lecture_count,
    term_count: c.term_count,
    draft_count: c.draft_count || 0,
    stale_count: c.stale_lecture_count || 0,
    running_count: c.running_count || 0,
    patches_pending: c.patches_pending || 0,
    last_activity_at: c.last_activity_at || c.created_at,
    course_actions: c.course_actions || [],
    glossary_pending: c.glossary_pending,
    glossary_busy: U.glossaryBusyText(c),
  };
}

export function courseCard(card, { onChanged } = {}) {
  const chips = [];
  if (card.running_count) chips.push(h('span', { class: 'chip chip-run' }, h('span', { class: 'chip-dot' }), t('course.card.running', { n: card.running_count })));
  if (card.draft_count) chips.push(h('span', { class: 'chip chip-draft' }, t('course.card.drafts', { n: card.draft_count })));
  if (card.stale_count) chips.push(h('span', { class: 'chip chip-warn' }, t('course.card.stale', { n: card.stale_count })));
  if (card.patches_pending) chips.push(h('span', { class: 'chip chip-quiet' }, t('course.card.reviews', { n: card.patches_pending })));
  const el = h(
    'a',
    { class: 'ccard', href: `#/course/${encodeURIComponent(card.id)}`, dataset: { fkey: 'ccard:' + card.id } },
    h(
      'div',
      { class: 'ccard-top' },
      card.code ? h('span', { class: 'ccard-code' }, card.code) : h('span', { class: 'ccard-code is-none' }, t('course.card.no_code')),
      card.glossary_pending ? h('span', { class: 'ccard-flag', title: t('course.glossary_pending_tip') }, icon('sparkles', { size: 13 }), card.glossary_busy || t('course.card.glossary_pending')) : null,
    ),
    h('div', { class: 'ccard-name', title: card.name }, card.name),
    h('div', { class: 'ccard-stats' }, h('span', null, boldCount('course.card.lectures', card.lecture_count)), h('span', null, boldCount('course.card.terms', card.term_count))),
    h('div', { class: 'ccard-chips' }, chips.length ? chips : h('span', { class: 'ccard-quiet' }, card.lecture_count ? t('course.card.all_ready') : t('course.card.no_lectures'))),
    h('div', { class: 'ccard-foot' }, icon('clock', { size: 12 }), U.relTime(card.last_activity_at) || '—'),
  );
  bindContextMenu(el, () => courseMenuItems(card, { onChanged }));
  return el;
}

export function addCourseCard(onClick) {
  return h(
    'button',
    { type: 'button', class: 'ccard ccard-add', onclick: onClick, dataset: { fkey: 'ccard:add' } },
    h('span', { class: 'ccard-add-icon' }, icon('plus', { size: 18 })),
    h('span', null, t('course.add')),
  );
}
