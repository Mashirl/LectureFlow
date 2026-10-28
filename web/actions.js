// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Lecture and course operations shared by the dashboard, the course page,
// context menus and the lecture page (v1.1 A2.3, A3, A4.3, A7, A12.2).
// Every function returns what the back end returned (updated lectures,
// courses); callers render that — nothing is changed optimistically.

import { api, ApiError, errorInfoText } from './api.js';
import { h, toast, confirmDialog, choiceDialog, inputDialog, download } from './dom.js';
import { confirmStageAction } from './pipeline.js';
import * as U from './util.js';
import { t } from './i18n.js';
import { openRecordedAtDialog } from './calendar_ui.js';
import { zhOn, zhMode, zhConvertible, zhFileName, zhConvertAll } from './zh_display.js';
import { openKindDialog } from './kind_dialog.js';
import { kindTag } from './kinds.js';
import { useColabMenuItem, useColab } from './settings_colab.js';
// (lecture_english.js imports exportItems from here: the cycle only meets
// inside functions, never at the top level of either module)
import { lectureDownloadItems } from './lecture_english.js';

let courseLookup = () => null;

// setCourseLookup(fn(courseId) → Course|null): used for the A3.2 warning
// (the glossary is still being extracted) before launching.
export function setCourseLookup(fn) {
  courseLookup = fn;
}

// ---------- one request per operation (3.2.0 P2) ----------

// A double click on a row's "启动", "重试" or "更新" sent the request twice:
// an "already launched" warning after the success, or a second model-choice
// dialog on top of the first. An operation still on its way (from the first
// click to the answer, its confirmation and model choice included) is
// shared: calling it again returns the same promise, so it is sent once and
// its toast shown once. key names the operation and what it acts on.
const inflight = new Map(); // key → promise

export function once(key, run) {
  const had = inflight.get(key);
  if (had) return had;
  let p;
  try {
    p = Promise.resolve(run());
  } catch (e) {
    p = Promise.reject(e);
  }
  const shared = p.finally(() => {
    if (inflight.get(key) === shared) inflight.delete(key);
  });
  inflight.set(key, shared);
  return shared;
}

const idsKey = (lectures) => lectures.map((l) => l.id).sort().join(',');

// ---------- A4.3 model choice ----------

// The model-choice dialogs open now, by operation key (3.2.0 P2).
const choosing = new Set();

// Runs run(choice); a 409 needs_model_choice is answered with the dialog and
// run again with the chosen model_choice. Resolves to run's result, or null
// when the user cancelled the dialog. Other errors propagate (already shown).
// `what` names the operation for the dialog: 'update' | 'retry' |
// 'rerun:{stage}' (anything else: a rerun). key (optional) names the
// operation and its lectures: while its dialog is open, the same operation
// gets no second dialog (it resolves to null, as if cancelled).
export async function withModelChoice(run, what, key) {
  try {
    return await run(undefined);
  } catch (e) {
    if (!(e instanceof ApiError) || !e.needsModelChoice) throw e;
    if (key && choosing.has(key)) return null;
    if (key) choosing.add(key);
    let choice;
    try {
      choice = await modelChoiceDialog(e.body.diff || [], what);
    } finally {
      if (key) choosing.delete(key);
    }
    if (!choice) return null;
    return run(choice);
  }
}

function diffList(diff) {
  const shown = diff.slice(0, 8);
  return h(
    'ul',
    { class: 'diff-list' },
    shown.map((d) =>
      h(
        'li',
        { class: 'diff-item' },
        h(
          'div',
          { class: 'diff-item-head' },
          d.lecture_title ? h('span', { class: 'diff-item-lec' }, d.lecture_title) : null,
          h('span', { class: 'chip chip-quiet' }, U.stageName(d.stage)),
        ),
        h('div', { class: 'diff-row' }, h('span', { class: 'diff-k' }, t('common.model_choice.old')), h('span', { class: 'diff-v' + (d.snapshot_usable ? '' : ' is-broken') }, U.modelLabel(d.snapshot)), !d.snapshot_usable && diffReason(d) ? h('span', { class: 'diff-reason' }, diffReason(d)) : null),
        h('div', { class: 'diff-row' }, h('span', { class: 'diff-k' }, t('common.model_choice.new')), h('span', { class: 'diff-v diff-v-new' }, U.modelLabel(d.current))),
        promptChange(d),
      ),
    ),
    diff.length > shown.length ? h('li', { class: 'diff-more' }, t('common.model_choice.more', { n: diff.length - shown.length })) : null,
  );
}

// Why the snapshot's model cannot be used (ModelDiffView.reason_info, P14.3).
function diffReason(d) {
  return errorInfoText(d.reason_info, d.reason || '');
}

// The operation the dialog asks about (see withModelChoice).
function whatText(what) {
  if (what === 'update') return t('common.model_choice.what_update');
  if (what === 'retry') return t('common.model_choice.what_retry');
  if (typeof what === 'string' && what.startsWith('rerun:')) return t('common.model_choice.what_rerun', { stage: U.stageName(what.slice('rerun:'.length)) });
  return what && what !== 'rerun' ? what : t('common.model_choice.what_default');
}

// v2 G5.5: "prompt v1 → v2" when the snapshot was made with another prompt
// version than the current one (the labels may then be identical).
function promptChange(d) {
  const a = d.snapshot && d.snapshot.prompt_version;
  const b = d.current && d.current.prompt_version;
  if (!a || !b || a === b) return null;
  return h('div', { class: 'diff-row diff-prompt' }, h('span', { class: 'diff-k' }, ''), h('span', { class: 'chip chip-quiet' }, t('common.model_choice.prompt', { from: a, to: b })));
}

export function modelChoiceDialog(diff, what) {
  const unusable = diff.filter((d) => !d.snapshot_usable);
  const lectures = new Set(diff.map((d) => d.lecture_id).filter(Boolean));
  const prompts = diff.some((d) => promptChange(d));
  const key = `common.model_choice.message${prompts ? '_prompt' : ''}${lectures.size > 1 ? '_many' : ''}`;
  return choiceDialog({
    title: t('common.model_choice.title'),
    message: t(key, { what: whatText(what), n: lectures.size }),
    body: diffList(diff),
    choices: [
      {
        value: 'snapshot',
        label: t('common.model_choice.snapshot'),
        hint: unusable.length ? t('common.model_choice.unusable', { reason: diffReason(unusable[0]) || t('common.model_choice.unusable_default') }) : t('common.model_choice.snapshot_hint'),
        disabled: unusable.length > 0,
      },
      { value: 'current', label: t('common.model_choice.current'), hint: t('common.model_choice.current_hint') },
    ],
    initial: unusable.length ? 'current' : 'snapshot',
    okText: t('common.continue'),
  });
}

// ---------- results ----------

// The text of one failed BatchResult: its coded error (v2 P14.3), else the
// transition text.
export function batchError(r) {
  return errorInfoText(r.error_info, r.error) || t('common.unknown_error');
}

// Toast for a BatchResponse / CourseOpResult; returns the updated lectures.
// verb: launch | update | cancel | delete (common.batch.{verb}.*).
function summarize(results, verb) {
  const ok = results.filter((r) => r.ok);
  const bad = results.filter((r) => !r.ok);
  const k = `common.batch.${verb}`;
  if (bad.length === 0) toast(t(`${k}.ok`, { n: ok.length }), 'success');
  else if (ok.length === 0) toast(bad.length > 1 ? t(`${k}.failed_many`, { error: batchError(bad[0]), n: bad.length }) : t(`${k}.failed`, { error: batchError(bad[0]) }), 'warn');
  else toast(t(`${k}.partial`, { ok: ok.length, bad: bad.length, error: batchError(bad[0]) }), 'warn');
  return ok.map((r) => r.lecture).filter(Boolean);
}

async function glossaryCheck(courseIds) {
  const pending = [...new Set(courseIds)].map((id) => courseLookup(id)).filter((c) => c && c.glossary_pending);
  if (!pending.length) return true;
  return confirmDialog({
    title: t('common.glossary_check.title'),
    message: pending.length === 1 ? t('common.glossary_check.message_course', { course: pending[0].code || pending[0].name }) : t('common.glossary_check.message'),
    okText: t('common.glossary_check.ok'),
  });
}

// ---------- lecture operations ----------

export function launchLectures(lectures) {
  const list = lectures.filter((l) => (l.lecture_actions || []).includes('launch'));
  if (!list.length) return Promise.resolve([]);
  return once(`launch:${idsKey(list)}`, async () => {
    if (!(await glossaryCheck(list.map((l) => l.course_id)))) return [];
    if (list.length === 1) {
      const l = await api.launchLecture(list[0].id);
      toast(t('common.lectures.launched', { title: list[0].title }), 'success');
      return l ? [l] : [];
    }
    const res = await api.launchLectures(list.map((l) => l.id));
    return summarize((res && res.results) || [], 'launch');
  });
}

export function launchCourseDrafts(course) {
  return once(`launch_drafts:${course.id}`, async () => {
    if (!(await glossaryCheck([course.id]))) return null;
    const res = await api.launchDrafts(course.id);
    if (res) summarize(res.results || [], 'launch');
    return res;
  });
}

export function updateLectures(lectures) {
  const list = lectures.filter((l) => (l.lecture_actions || []).includes('update'));
  if (!list.length) return Promise.resolve([]);
  const key = `update:${idsKey(list)}`;
  return once(key, async () => {
    if (list.length === 1) {
      const l = await withModelChoice((choice) => api.updateLecture(list[0].id, choice), 'update', key);
      if (!l) return [];
      toast(t('common.lectures.update_started', { title: list[0].title }), 'success');
      return [l];
    }
    const res = await withModelChoice((choice) => api.updateLectures(list.map((l) => l.id), choice), 'update', key);
    return res ? summarize(res.results || [], 'update') : [];
  });
}

export function updateCourseStale(course) {
  const key = `update_stale:${course.id}`;
  return once(key, async () => {
    const res = await withModelChoice((choice) => api.updateStale(course.id, choice), 'update', key);
    if (res) summarize(res.results || [], 'update');
    return res;
  });
}

export function cancelLectures(lectures) {
  const list = lectures.filter((l) => (l.lecture_actions || []).includes('cancel'));
  if (!list.length) return Promise.resolve([]);
  return once(`cancel:${idsKey(list)}`, () => cancelConfirmed(list));
}

async function cancelConfirmed(list) {
  const ok = await confirmDialog({
    title: list.length === 1 ? t('common.lectures.cancel_title') : t('common.lectures.cancel_title_many', { n: list.length }),
    message: list.length === 1 ? t('common.lectures.cancel_message', { title: list[0].title }) : t('common.lectures.cancel_message_many'),
    okText: t('common.lectures.cancel_ok'),
    cancelText: t('common.dont_cancel'),
    danger: true,
  });
  if (!ok) return [];
  const res = await api.cancelLectures(list.map((l) => l.id));
  return summarize((res && res.results) || [], 'cancel');
}

// A7: the dialog lists the titles; lectures with a stage queued or running
// (or a running bound material: can_delete false) are cancelled first
// (force). Resolves to the deleted ids.
const isBusy = (l) => !l.can_delete || (l.lecture_actions || []).includes('cancel');

// Materials bound to these lectures are deleted with them (ON DELETE
// CASCADE): their file names, read from the courses (best effort).
async function boundMaterials(lectures) {
  const ids = new Set(lectures.map((l) => l.id));
  const names = [];
  for (const cid of new Set(lectures.map((l) => l.course_id))) {
    try {
      const c = await api.course(cid, { quiet: true });
      for (const m of (c && c.materials) || []) if (m.lecture_id && ids.has(m.lecture_id)) names.push(m.filename);
    } catch {
      // the dialog just does not list them
    }
  }
  return names;
}

function confirmForce(n) {
  return confirmDialog({
    title: t('common.lectures.force_title'),
    message: n === 1 ? t('common.lectures.force_message') : t('common.lectures.force_message_many', { n }),
    okText: t('common.lectures.force_ok'),
    danger: true,
  });
}

export function deleteLectures(lectures) {
  if (!lectures.length) return Promise.resolve([]);
  return once(`delete:${idsKey(lectures)}`, () => deleteConfirmed(lectures));
}

async function deleteConfirmed(lectures) {
  const running = lectures.filter(isBusy);
  const mats = await boundMaterials(lectures);
  const shown = lectures.slice(0, 8);
  const ok = await confirmDialog({
    title: lectures.length === 1 ? t('common.lectures.delete_title') : t('common.lectures.delete_title_many', { n: lectures.length }),
    message: t('common.lectures.delete_message'),
    body: h(
      'div',
      { class: 'dlg-list-wrap' },
      h(
        'ul',
        { class: 'dlg-list' },
        shown.map((l) => h('li', null, kindTag(l, { class: 'ktag-inline' }), ' ', l.title, isBusy(l) ? h('span', { class: 'chip chip-run' }, t('common.lectures.busy_chip')) : null)),
        lectures.length > shown.length ? h('li', { class: 'dim' }, t('common.lectures.delete_more', { n: lectures.length })) : null,
      ),
      running.length ? h('p', { class: 'dlg-warn' }, t('common.lectures.delete_running', { n: running.length })) : null,
      mats.length ? h('p', { class: 'dlg-warn' }, lectures.length === 1 ? t('common.lectures.delete_materials', { files: U.listJoin(mats) }) : t('common.lectures.delete_materials_many', { files: U.listJoin(mats) })) : null,
    ),
    okText: t('common.delete'),
    danger: true,
  });
  if (!ok) return [];
  const force = running.length > 0;
  if (lectures.length === 1) {
    const l = lectures[0];
    try {
      await api.deleteLecture(l.id, force, { quiet: !force });
    } catch (e) {
      // a stage started between the dialog and the request: offer force
      if (force || !(e instanceof ApiError) || e.status !== 409) {
        if (!force) toast(e.message, 'error');
        throw e;
      }
      if (!(await confirmForce(1))) return [];
      await api.deleteLecture(l.id, true);
    }
    toast(t('common.lectures.deleted', { title: l.title }), 'success');
    return [l.id];
  }
  const res = await api.deleteLectures(lectures.map((l) => l.id), force);
  let results = (res && res.results) || [];
  // (not named isBusy: a local const of that name would shadow the
  // module-level isBusy used at the top of this function, which then throws)
  const busyResult = (r) => !!r.error_info && r.error_info.code === 'lecture.running';
  const busy = force ? [] : results.filter((r) => !r.ok && busyResult(r));
  if (busy.length && (await confirmForce(busy.length))) {
    const again = await api.deleteLectures(busy.map((r) => r.id), true);
    const byId = new Map(((again && again.results) || []).map((r) => [r.id, r]));
    results = results.map((r) => byId.get(r.id) || r);
  }
  summarize(results, 'delete');
  return results.filter((r) => r.ok).map((r) => r.id);
}

// Stage action from a menu or a pipeline card (confirmation already done by
// the pipeline card when confirmed is true). use_colab (an asr stage Colab
// handed back while no local device can take it) is "改用 Colab 处理" with
// its own confirmation and endpoint.
// The same action of the same stage from two places (the row and the failure
// notice, a double click) is sent once (3.2.0 P2).
export function stageAction(lec, stage, action, confirmed = false) {
  const key = `stage:${lec.id}:${stage}:${action}`;
  return once(key, async () => {
    if (action === 'use_colab') return useColab(lec);
    if (!confirmed && !(await confirmStageAction(stage, action))) return null;
    // "修复断句" (textrepair-int): the asr rerun with text_only; the model
    // question, when asked, concerns the stages after asr (once() above
    // keeps it to one dialog)
    if (action === 'rerun_text_only') return withModelChoice((choice) => api.lectureTextRepair(lec.id, choice), t('common.model_choice.what_text_repair'));
    const what = action === 'retry' ? 'retry' : `rerun:${stage}`;
    return withModelChoice((choice) => api.lectureStageAction(lec.id, stage, action, choice), what, key);
  });
}

// The retry of a lecture's task that asks no model question (the playback
// file "stream", the title task "title"), sent once (3.2.0 P2).
export function taskRetry(lec, stage) {
  return once(`stage:${lec.id}:${stage}:retry`, () => api.lectureStageAction(lec.id, stage, 'retry'));
}

// "Fill in word timings": a lecture transcribed before word timestamps were
// recorded (SPEC-v2 P4.2) splits its subtitles at whole sentences only. The
// back end offers lecture_actions fill_words; filling them in is the asr
// rerun (the stages after it run again; needs_model_choice as for any rerun).
export function fillWords(lec) {
  return once(`fill_words:${lec.id}`, async () => {
    const ok = await confirmDialog({ title: t('lecture.fill_words.title'), message: t('lecture.fill_words.message'), okText: t('lecture.fill_words.ok'), danger: true });
    if (!ok) return null;
    return stageAction(lec, 'asr', 'rerun', true);
  });
}

// v2 P13.5 "regenerate title and blurb": a title the user set is overwritten only
// after a confirmation (force). Resolves to true when the task was queued;
// the new title arrives with the lecture's SSE change.
export function regenerateTitle(lec) {
  return once(`regenerate_title:${lec.id}`, () => regenerate(lec));
}

async function regenerate(lec) {
  const send = (force) => api.regenerateTitle(lec.id, force, { quiet: true });
  let force = false;
  if (lec.title_source === 'user') {
    force = await confirmDialog({ title: t('lecture.title.confirm_title'), message: t('lecture.title.confirm_message', { title: lec.title }), okText: t('lecture.title.confirm_ok') });
    if (!force) return false;
  }
  try {
    await send(force);
  } catch (e) {
    if (!(e instanceof ApiError) || e.code !== 'title.user_set') {
      toast(e.message, 'error');
      return false;
    }
    // the title became the user's meanwhile
    const ok = await confirmDialog({ title: t('lecture.title.confirm_title'), message: t('lecture.title.confirm_message', { title: lec.title }), okText: t('lecture.title.confirm_ok') });
    if (!ok) return false;
    try {
      await send(true);
    } catch (e2) {
      toast(e2.message, 'error');
      return false;
    }
  }
  toast(t('lecture.title.started'), 'info');
  return true;
}

export async function renameLectureDialog(lec) {
  const v = await inputDialog({ title: t('common.lectures.rename_title'), label: t('common.lectures.rename_label'), value: lec.title, okText: t('common.save') });
  if (!v || v === lec.title) return null;
  return api.renameLecture(lec.id, v);
}

// ---------- menus (A12.2) ----------

// [file suffix, menu.export.{kind}, icon]
const EXPORT_KINDS = [
  ['.bilingual.md', 'md_bilingual', 'file-text'],
  ['.bilingual.vtt', 'vtt_bilingual', 'captions'],
  ['.zh.vtt', 'vtt_zh', 'captions'],
  ['.cite.json', 'cite_json', 'braces'],
  ['.en.txt', 'txt_en', 'file-text'],
];

export function exportItems(exports) {
  const items = [];
  const rest = [...(exports || [])];
  const converted = []; // P14.6: the text files again, converted to the display script
  const add = (item, f) => {
    items.push(item);
    if (zhConvertible(f.name)) converted.push({ id: `${item.id || 'export:file'}:zh`, label: item.label, icon: item.icon, hint: item.hint, onSelect: () => downloadConverted(api.exportURL(f.url), f.name) });
  };
  for (const [suffix, kind, ic] of EXPORT_KINDS) {
    const i = rest.findIndex((f) => f.name.endsWith(suffix));
    if (i < 0) continue;
    const f = rest.splice(i, 1)[0];
    add({ id: `export:${kind}`, label: t(`menu.export.${kind}`), icon: ic, hint: U.fmtBytes(f.size), onSelect: () => download(api.exportURL(f.url), f.name) }, f);
  }
  for (const f of rest) add({ label: f.name, icon: 'file-text', hint: U.fmtBytes(f.size), onSelect: () => download(api.exportURL(f.url), f.name) }, f);
  if (zhOn() && converted.length) items.push('sep', { id: 'export:zh_hant', label: t('menu.export.zh_hant'), icon: 'languages', hint: t(`menu.export.zh_variant_${zhMode()}`), items: converted });
  return items;
}

/**
 * Downloads a text export with its Chinese converted to the Traditional
 * display script (P14.6 "导出文件也可以在导出时选择是否转换"): fetched by
 * the browser, converted, saved from a Blob as "name.zh-Hant-TW.ext" (or
 * -HK). The file on the server is not changed.
 */
export async function downloadConverted(url, name) {
  const out = zhFileName(name);
  try {
    const res = await fetch(url, { cache: 'no-store' });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const text = await res.text();
    const type = (res.headers.get('Content-Type') || 'text/plain; charset=utf-8').split(';')[0].trim();
    const blob = new Blob([zhConvertAll(text)], { type: `${type}; charset=utf-8` });
    const href = URL.createObjectURL(blob);
    download(href, out);
    setTimeout(() => URL.revokeObjectURL(href), 60000);
  } catch (e) {
    toast(t('menu.export.zh_failed', { name: out, error: e.message || t('common.unknown_error') }), 'error');
  }
}

// h: {onLectures(lectures[]), onDeleted(ids[]), onRename(lec)?} — the view
// refreshes from what the back end returned. Every item has a stable `id`
// (callers filter by it, e.g. 'open'; labels depend on the language).
export function lectureMenuItems(lec, hd) {
  const acts = lec.lecture_actions || [];
  const done = (p) =>
    p.then(
      (r) => {
        if (r && r.length !== undefined) hd.onLectures(r);
        else if (r) hd.onLectures([r]);
      },
      () => {},
    );
  const reruns = (lec.stages || []).filter((s) => (s.allowed_actions || []).includes('rerun'));
  // "修复断句" (textrepair-int): offered by the back end on the asr stage
  const textRepair = (lec.stages || []).some((s) => s.stage === 'asr' && (s.allowed_actions || []).includes('rerun_text_only'));
  return [
    { id: 'open', label: t('menu.open'), icon: 'external-link', onSelect: () => (location.hash = `#/lecture/${encodeURIComponent(lec.id)}`) },
    lec.playable ? { id: 'play', label: t('player.entry.play'), icon: 'play', onSelect: () => (location.hash = `#/play/${encodeURIComponent(lec.id)}`) } : null, // SPEC-v2 P1
    lec.counts && lec.counts.patches_pending > 0 ? { id: 'review', label: t('menu.lecture.review', { n: lec.counts.patches_pending }), icon: 'list-checks', onSelect: () => (location.hash = `#/lecture/${encodeURIComponent(lec.id)}/review`) } : null,
    'sep',
    acts.includes('launch') ? { id: 'launch', label: t('menu.lecture.launch'), icon: 'play', onSelect: () => done(launchLectures([lec])) } : null,
    acts.includes('cancel') ? { id: 'cancel', label: t('menu.lecture.cancel'), icon: 'ban', onSelect: () => done(cancelLectures([lec])) } : null,
    acts.includes('update') ? { id: 'update', label: t('menu.lecture.update'), icon: 'refresh-cw', onSelect: () => done(updateLectures([lec])) } : null,
    // SPEC-v3 V14.4: "改用 Colab 处理" while Colab is on and authorized (settings_colab.js)
    useColabMenuItem(lec, done),
    reruns.length
      ? {
          id: 'rerun',
          label: t('menu.lecture.rerun'),
          icon: 'rotate-cw',
          items: reruns.map((s) => ({ id: `rerun:${s.stage}`, label: U.stageName(s.stage), hint: U.statusName(U.displayStatus(s)), onSelect: () => done(stageAction(lec, s.stage, 'rerun')) })),
        }
      : null,
    // the export files, and once the lecture is playable the English
    // subtitles and transcript made on demand (3.1.0 "transcribe only")
    { id: 'export', label: t('menu.lecture.export'), icon: 'download', items: lectureDownloadItems(lec) || [] },
    'sep',
    { id: 'rename', label: t('menu.lecture.rename'), icon: 'pencil', hint: 'F2', onSelect: () => (hd.onRename ? hd.onRename(lec) : done(renameLectureDialog(lec))) },
    // v2 P13: numbers come from the recording time (editing the number is gone, P13.2)
    { id: 'recorded_at', label: t('lecture.menu.recorded_at'), icon: 'calendar', onSelect: () => openRecordedAtDialog(lec, { onSaved: (l) => hd.onLectures([l]) }) },
    // lecture kinds: the kind (lecture, tutorial, …) and with it the citation number
    { id: 'kind', label: t('kind.menu.change'), icon: 'tag', onSelect: () => openKindDialog(lec, { onSaved: (l) => hd.onLectures([l]) }) },
    // P13.5: offered by the back end (summary done or stale, organizer role configured)
    acts.includes('regenerate_title') ? { id: 'regenerate_title', label: t('lecture.menu.regenerate_title'), icon: 'sparkles', onSelect: () => regenerateTitle(lec) } : null,
    acts.includes('fill_words') ? { id: 'fill_words', label: t('lecture.menu.fill_words'), icon: 'captions', onSelect: () => done(fillWords(lec)) } : null,
    textRepair ? { id: 'rerun_text_only', label: t('common.action.rerun_text_only'), icon: 'wand-sparkles', onSelect: () => done(stageAction(lec, 'asr', 'rerun_text_only')) } : null,
    'sep',
    {
      id: 'delete',
      label: t('menu.lecture.delete'),
      icon: 'trash-2',
      danger: true,
      hint: 'Del',
      onSelect: () =>
        deleteLectures([lec]).then(
          (ids) => ids.length && hd.onDeleted(ids),
          () => {},
        ),
    },
  ];
}

// Course card menu (Dashboard and course list).
export function courseMenuItems(card, hd = {}) {
  const acts = card.course_actions || [];
  const open = () => (location.hash = `#/course/${encodeURIComponent(card.id)}`);
  const after = (p) =>
    p.then(
      (r) => hd.onChanged && hd.onChanged(r),
      () => {},
    );
  return [
    { id: 'open', label: t('menu.open'), icon: 'external-link', onSelect: open },
    { id: 'syllabus', label: t('menu.course.syllabus'), icon: 'file-text', onSelect: () => (location.hash = `#/course/${encodeURIComponent(card.id)}/syllabus`) },
    'sep',
    acts.includes('launch_drafts') ? { id: 'launch_drafts', label: t('menu.course.launch_drafts', { n: card.draft_count }), icon: 'play', onSelect: () => after(launchCourseDrafts(card)) } : null,
    acts.includes('update_stale') ? { id: 'update_stale', label: t('menu.course.update_stale', { n: card.stale_count ?? card.stale_lecture_count }), icon: 'refresh-cw', onSelect: () => after(updateCourseStale(card)) } : null,
    acts.includes('export_cite') ? { id: 'export_cite', label: t('menu.course.export_cite'), icon: 'braces', onSelect: () => download(api.courseCiteURL(card.id), `${card.code || card.name}.cite.json`) } : null,
    // P14.6: the same file converted to the Traditional display script
    acts.includes('export_cite') && zhOn()
      ? { id: 'export_cite_zh', label: t('menu.course.export_cite_zh'), icon: 'languages', hint: t(`menu.export.zh_variant_${zhMode()}`), onSelect: () => downloadConverted(api.courseCiteURL(card.id), `${card.code || card.name}.cite.json`) }
      : null,
  ];
}
