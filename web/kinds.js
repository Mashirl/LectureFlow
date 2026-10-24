// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Lecture kinds (lecture kinds, NOTES.md): the global catalog of
// GET /api/lecture-kinds — the built-in kinds (named here, in the interface
// language) and the user's own (named by the user, never translated) — and
// how a lecture shows its kind and number: "Tutorial" with a quieter
// "No. 2" under it in the list, "Week 3 · Tutorial · No. 2" elsewhere.
// Kinds, numbers and whether "No. n" is shown (show_n, from the course's
// numbering mode) all come from the back end.

import { api } from './api.js';
import { h } from './dom.js';
import { t } from './i18n.js';
import { zh } from './zh_display.js';

export const BUILTIN_KINDS = ['lecture', 'tutorial', 'seminar', 'past_lecture', 'material'];
const BUILTIN_GROUP = { lecture: 'session', tutorial: 'session', seminar: 'session', past_lecture: 'material', material: 'material' };

let catalog = [];
let pending = null;

/**
 * (Re)loads the catalog; window event "kindschange" tells the pages. Quiet:
 * without it the built-in kinds are still named, a custom kind shows its id.
 */
export function loadKinds() {
  if (pending) return pending;
  pending = api
    .lectureKinds({ quiet: true })
    .then(
      (list) => {
        catalog = Array.isArray(list) ? list : [];
        window.dispatchEvent(new CustomEvent('kindschange'));
        return catalog;
      },
      () => catalog,
    )
    .finally(() => {
      pending = null;
    });
  return pending;
}

/** The catalog in its order (built-in kinds first). */
export function kindList() {
  return [...catalog].sort((a, b) => a.order - b.order);
}

export function kindById(id) {
  return catalog.find((k) => k.id === id) || null;
}

/** A lecture's kind id; "lecture" until the back end has decided one. */
export function kindOf(lec) {
  return (lec && lec.kind) || 'lecture';
}

/**
 * The name of a kind: form "short" for lists and tags ("Tutorial"), "full"
 * where there is room ("Tutorial", zh 小组辅导).
 */
export function kindName(id, form = 'short') {
  const k = kindById(id);
  if (k && k.name) return k.name;
  if (BUILTIN_KINDS.includes(id)) return t(`kind.builtin.${id}.${form === 'full' ? 'full' : 'short'}`);
  return k ? k.code : id;
}

export function kindGroup(id) {
  const k = kindById(id);
  return k ? k.group : BUILTIN_GROUP[id] || 'session';
}

export function kindCode(id) {
  const k = kindById(id);
  return k ? k.code : '';
}

/** Past lectures, additional materials and custom kinds of that group. */
export function isMaterial(lec) {
  return kindGroup(kindOf(lec)) === 'material';
}

/** The teaching week of a lecture (or a dashboard item), 0 when it has none. */
export function weekOf(lec) {
  return lec && lec.numbered !== false && lec.week > 0 ? lec.week : 0;
}

/** "No. 2" (materials: their own counter) when the back end says to show it, else "". */
export function nthText(lec) {
  if (!lec || !lec.show_n || !(lec.kind_n > 0)) return '';
  // materials are counted as items, not class sessions ("第 2 份")
  return t(isMaterial(lec) ? 'kind.nth_material' : 'kind.nth', { n: lec.kind_n });
}

/**
 * "Week 3 · Tutorial · No. 2" (opts.week false: without the week; opts.form
 * "full": the full kind name). Works for lectures and dashboard items.
 */
export function lectureTagText(lec, opts = {}) {
  const parts = [];
  const w = weekOf(lec);
  if (opts.week !== false && w) parts.push(t('kind.week', { week: w }));
  parts.push(kindName(kindOf(lec), opts.form));
  const n = nthText(lec);
  if (n) parts.push(n);
  return parts.join(t('kind.sep'));
}

/**
 * "Week 3 · Tutorial · No. 2 {title}" — a lecture in menus and dialogs (the
 * title's Chinese in the display script, P14.6).
 */
export function lectureLabel(lec) {
  return t('kind.label', { tag: lectureTagText(lec), title: zh(lec.title || '') });
}

/**
 * The kind column of the lecture list: the kind in the accent colour and,
 * when shown, "No. 2" under it, quieter and smaller.
 */
export function kindTag(lec, opts = {}) {
  const id = kindOf(lec);
  const n = nthText(lec);
  return h(
    'span',
    { class: 'ktag' + (isMaterial(lec) ? ' is-material' : '') + (opts.class ? ' ' + opts.class : ''), title: opts.title || undefined },
    h('span', { class: 'ktag-k' }, kindName(id)),
    n ? h('span', { class: 'ktag-n' }, n) : null,
  );
}

/**
 * Where a lecture's kind came from (kind_source); "date": the recording is
 * more than 30 days older than the term start (a past lecture).
 */
export function kindSourceText(src) {
  return t(`kind.source.${['filename', 'llm', 'user', 'date'].includes(src) ? src : 'none'}`);
}

/** Search text of a lecture's kind: both names of a built-in kind. */
export function kindSearchText(lec) {
  const id = kindOf(lec);
  return [kindName(id), kindName(id, 'full'), kindCode(id)].join(' ');
}
