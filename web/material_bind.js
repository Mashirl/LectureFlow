// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Slides by teaching week (lecture kinds, NOTES.md): what a slide file applies
// to — a teaching week (every lecture, tutorial and seminar of it), one
// lecture (the v1 binding), or the whole course — as one <select>, and the
// groups of the materials card. The week guessed from the file name
// (week_suggested) is preselected by the back end on upload when the course
// has a term start; the select marks it "from the file name". Slides that
// apply to the whole course although their name gives a week (uploaded
// before the term start was set, or made course-wide) get a one-click
// suggestion on the course page (suggestedWeek).

import { h } from './dom.js';
import { t } from './i18n.js';
import { lectureLabel, weekOf } from './kinds.js';

/** The binding of a slide file (bind, or the v1 lecture_id). */
export function bindOf(m) {
  if (m.bind) return m.bind;
  return m.lecture_id ? { scope: 'lecture', week: null, lecture_id: m.lecture_id } : { scope: 'course', week: null, lecture_id: null };
}

/** The weeks offered: twelve, or up to the last week a lecture or a slide file uses. */
export function weekChoices(lectures, materials) {
  let max = 12;
  for (const l of lectures || []) max = Math.max(max, weekOf(l));
  for (const m of materials || []) {
    const b = bindOf(m);
    if (b.scope === 'week' && b.week) max = Math.max(max, b.week);
    if (m.week_suggested) max = Math.max(max, m.week_suggested);
  }
  return Array.from({ length: max }, (_, i) => i + 1);
}

function valueOf(b) {
  if (b.scope === 'week' && b.week) return `w:${b.week}`;
  if (b.scope === 'lecture' && b.lecture_id) return `l:${b.lecture_id}`;
  return 'c';
}

function bindFromValue(v) {
  if (v.startsWith('w:')) return { scope: 'week', week: Number(v.slice(2)), lecture_id: null };
  if (v.startsWith('l:')) return { scope: 'lecture', week: null, lecture_id: v.slice(2) };
  return { scope: 'course', week: null, lecture_id: null };
}

/** The scope as text: "Week 3", "Only: Week 2 · Lecture …", "Whole course". */
export function bindText(m, lectures) {
  const b = bindOf(m);
  if (b.scope === 'week') return t('kind.week', { week: b.week });
  if (b.scope === 'lecture') {
    const l = (lectures || []).find((x) => x.id === b.lecture_id);
    return l ? t('course.materials.only', { lecture: lectureLabel(l) }) : t('course.materials.only_missing');
  }
  return t('course.materials.general');
}

/** Bound to the week its file name names: the back end's suggestion kept. */
export function fromFileName(m) {
  const b = bindOf(m);
  return b.scope === 'week' && !!m.week_suggested && b.week === m.week_suggested;
}

/**
 * The week to suggest for slides that apply to the whole course although
 * their file name gives one (the back end binds by name only once the
 * course has a term start), else 0. Slides bound to a lecture or a week
 * are the user's choice and get no suggestion.
 */
export function suggestedWeek(m) {
  return bindOf(m).scope === 'course' && m.week_suggested > 0 ? m.week_suggested : 0;
}

/**
 * The binding <select>. opts: {lectures (null: not loaded yet; [] in the
 * add-course wizard), weeks, label, fkey, onChange(bind)}.
 */
export function bindSelect(m, opts) {
  const lectures = opts.lectures || [];
  const cur = valueOf(bindOf(m));
  const known = !cur.startsWith('l:') || lectures.some((l) => `l:${l.id}` === cur);
  const sel = h(
    'select',
    { class: 'select mat-bind', 'aria-label': opts.label, dataset: { fkey: opts.fkey }, value: cur, onchange: () => opts.onChange(bindFromValue(sel.value)) },
    h('option', { value: 'c' }, t('course.materials.general')),
    h(
      'optgroup',
      { label: t('course.materials.bind_weeks') },
      opts.weeks.map((w) => h('option', { value: `w:${w}` }, m.week_suggested === w ? t('course.materials.week_suggested', { week: w }) : t('kind.week', { week: w }))),
    ),
    lectures.length || !known
      ? h(
          'optgroup',
          { label: t('course.materials.bind_lectures') },
          lectures.map((l) => h('option', { value: `l:${l.id}` }, t('course.materials.only', { lecture: lectureLabel(l) }))),
          known ? null : h('option', { value: cur }, opts.lectures ? t('course.materials.only_missing') : t('course.materials.only_unloaded')),
        )
      : null,
  );
  // the select takes the row's free width; a long title ends in "…" and is shown in full on hover
  sel.title = sel.selectedOptions.length ? sel.selectedOptions[0].textContent : '';
  return sel;
}

/**
 * The groups of the materials card: the whole course first, then the
 * teaching weeks (slides bound to one lecture under that lecture's week),
 * then slides of lectures without a week. → [{key, title, items}]
 */
export function materialGroups(materials, lectures) {
  const byWeek = new Map();
  const course = [];
  const loose = [];
  for (const m of materials) {
    const b = bindOf(m);
    let w = 0;
    if (b.scope === 'week') w = b.week || 0;
    else if (b.scope === 'lecture') {
      const l = (lectures || []).find((x) => x.id === b.lecture_id);
      w = l ? weekOf(l) : 0;
    }
    if (b.scope === 'course') course.push(m);
    else if (w) {
      if (!byWeek.has(w)) byWeek.set(w, []);
      byWeek.get(w).push(m);
    } else loose.push(m);
  }
  const out = [];
  if (course.length) out.push({ key: 'course', title: t('course.materials.general'), items: course });
  for (const w of [...byWeek.keys()].sort((a, b) => a - b)) out.push({ key: `w${w}`, title: t('kind.week', { week: w }), items: byWeek.get(w) });
  if (loose.length) out.push({ key: 'lecture', title: t('course.materials.group_lecture'), items: loose });
  return out;
}
