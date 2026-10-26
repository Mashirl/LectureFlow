// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// "Transcribe only" (SPEC-v3 V4.2 step 4, 3.1.0): a lecture launched
// without any model provider. Its API stages wait with waiting_for
// "no_provider" (internal/sched transcribe_only.go) and continue by
// themselves once a provider is set up. Meanwhile the lecture page and the
// player show the English only (no empty Chinese column), and the English
// subtitles and transcript can be downloaded as soon as asr is done
// (GET /api/lectures/{id}/english/{vtt|txt}, made from the sentences, not
// by the export stage).

import { api } from './api.js';
import { h, download } from './dom.js';
import { icon } from './icons.js';
import { menuButton } from './menu.js';
import { exportItems } from './actions.js';
import { t } from './i18n.js';

export const WAIT_NO_PROVIDER = 'no_provider';

const LLM_STAGES = ['summary', 'repair', 'translate'];

/** True while one of the lecture's API stages waits for a model provider. */
export function waitsForProvider(lec) {
  return !!(lec && Array.isArray(lec.stages)) && lec.stages.some((s) => LLM_STAGES.includes(s.stage) && (s.status === 'pending' || s.status === 'queued') && s.waiting_for === WAIT_NO_PROVIDER);
}

/**
 * True when the lecture has (and for now gets) no Chinese at all: no
 * sentence has a translation and its API stages wait for a provider. The
 * pages then show the English only.
 */
export function englishOnly(lec) {
  if (!waitsForProvider(lec)) return false;
  const c = lec.counts || {};
  return (c.zh_ok || 0) + (c.zh_qa_failed || 0) + (c.zh_stale || 0) === 0;
}

/** URL of the lecture's on-demand English file (format "vtt" or "txt"). */
export function englishURL(lectureId, format) {
  return api.exportURL(`/api/lectures/${encodeURIComponent(lectureId)}/english/${format}`);
}

/**
 * Menu items of the English subtitles (.vtt) and transcript (.txt); the
 * server names the files "{number} {English title}.en.vtt" (P13.6).
 */
export function englishItems(lec) {
  return [
    { id: 'english:vtt', label: t('menu.export.vtt_en'), icon: 'captions', onSelect: () => download(englishURL(lec.id, 'vtt')) },
    { id: 'english:txt', label: t('menu.export.txt_en'), icon: 'file-text', onSelect: () => download(englishURL(lec.id, 'txt')) },
  ];
}

/**
 * The lecture page's download menu: the export files (when export ran)
 * and, once the lecture is playable (asr done), the English files made on
 * demand. The export's own English text is left out: the on-demand one has
 * the same content and is always current. null when there is nothing.
 */
export function lectureDownloadItems(lec) {
  const exports = (lec.exports || []).filter((f) => !f.name.endsWith('.en.txt'));
  const items = exportItems(exports);
  if (!lec.playable) return (lec.exports || []).length ? exportItems(lec.exports) : null;
  const en = englishItems(lec);
  if (!items.length) return en;
  // before the "Traditional Chinese" group exportItems ends with
  const sep = items.indexOf('sep');
  if (sep < 0) return [...items, 'sep', ...en];
  return [...items.slice(0, sep), 'sep', ...en, ...items.slice(sep)];
}

export function lectureDownloadMenu(lec, fkey) {
  return menuButton(
    h('button', { type: 'button', class: 'btn btn-sm', dataset: { fkey } }, icon('download', { size: 14 }), h('span', { class: 'btn-label' }, t('common.download')), icon('chevron-down', { size: 13, class: 'btn-caret' })),
    () => lectureDownloadItems(lec) || [],
  );
}
