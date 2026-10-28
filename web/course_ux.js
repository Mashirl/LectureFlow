// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Course page requests of 2026-10-27 (course-ux; NOTES.md):
//   - batch rerun: one stage of the selected lectures, confirmed once. The
//     dialog lists only the stages at least one selected lecture may rerun
//     (allowed_actions), asks the server what would happen (dry run: how
//     many run, which are skipped and why, whether the recorded models
//     differ from the settings) and takes the model choice in the same
//     dialog, so there is a single question (POST /api/courses/{id}/rerun);
//   - week overrides: a teaching material hung under a week (supplement),
//     any lecture made the content of another week (make-up, the yellow
//     "补" tag). Menus here; drag and drop in lecture_list.js. Numbers come
//     from the back end; P13.6's renumbering confirmation applies.

import { api, ApiError } from './api.js';
import { h, openDialog, toast } from './dom.js';
import { t } from './i18n.js';
import * as U from './util.js';
import { withModelChoice } from './actions.js';
import { withRenumber, parseYMD, mondayOf, addDays, weekRangeText, fmtRecordedAt } from './calendar_ui.js';
import { isMaterial, lectureLabel } from './kinds.js';
import { weekChoices } from './material_bind.js';

const STAGES = ['prep', 'asr', 'summary', 'repair', 'translate', 'export'];
const LLM_STAGES = ['summary', 'repair', 'translate'];

// What rerunning a stage does to the lectures (literal keys for the i18n check).
const WHAT = {
  prep: () => t('course.rerun.what.prep'),
  asr: () => t('course.rerun.what.asr'),
  summary: () => t('course.rerun.what.summary'),
  repair: () => t('course.rerun.what.repair'),
  translate: () => t('course.rerun.what.translate'),
  export: () => t('course.rerun.what.export'),
};

/** True when the lecture may rerun stage now (the back end's allowed_actions). */
export function canRerun(lec, stage) {
  const st = (lec.stages || []).find((s) => s.stage === stage);
  return !!st && (st.allowed_actions || []).includes('rerun');
}

/** The stages at least one of the lectures may rerun, in pipeline order. */
export function rerunStages(lectures) {
  return STAGES.filter((st) => lectures.some((l) => canRerun(l, st)));
}

// ---------- batch rerun ----------

/**
 * The batch rerun of lectures (all of course courseId). Resolves to the
 * updated lectures ([] when cancelled or nothing ran).
 */
export async function batchRerun(courseId, lectures) {
  const stages = rerunStages(lectures);
  if (!stages.length) return [];
  const ids = lectures.map((l) => l.id);
  const byId = new Map(lectures.map((l) => [l.id, l]));
  const picked = await openDialog(
    t('course.rerun.title'),
    (close) => {
      const name = 'rr' + Math.random().toString(36).slice(2);
      let stage = '';
      let plan = null; // dry-run result for stage
      let seq = 0;
      let choice = '';
      const summary = h('div', { class: 'rr-summary', 'aria-live': 'polite' });
      const ok = h('button', { type: 'submit', class: 'btn btn-primary', disabled: true }, t('course.rerun.ok', { n: 0 }));
      const radios = stages.map((st) => {
        const n = lectures.filter((l) => canRerun(l, st)).length;
        return h(
          'label',
          { class: 'dlg-choice rr-stage' },
          h('input', { type: 'radio', name, value: st, onchange: () => pick(st) }),
          h('span', { class: 'dlg-choice-text' }, h('span', { class: 'dlg-choice-label' }, U.stageName(st)), h('span', { class: 'dlg-choice-hint' }, t('course.rerun.stage_count', { n: U.count(n), total: U.count(lectures.length) }))),
        );
      });
      if (radios[0]) radios[0].querySelector('input').autofocus = true;

      async function pick(st) {
        stage = st;
        plan = null;
        choice = '';
        const my = ++seq;
        sync();
        summary.replaceChildren(h('p', { class: 'dlg-hint' }, t('course.rerun.checking')));
        try {
          const res = await api.rerunCourse(courseId, st, ids, { dryRun: true, quiet: true });
          if (my !== seq) return;
          plan = res;
          renderPlan();
        } catch (e) {
          if (my !== seq) return;
          summary.replaceChildren(h('p', { class: 'dlg-field-error', role: 'alert' }, t('course.rerun.check_failed', { error: e.message })));
        }
        sync();
      }

      function sync() {
        const n = plan ? plan.queued : 0;
        ok.textContent = t('course.rerun.ok', { n: U.count(n) });
        ok.disabled = !plan || n === 0 || ((plan.diff || []).length > 0 && !choice);
        ok.className = 'btn ' + (stage === 'prep' || stage === 'asr' ? 'btn-danger-solid' : 'btn-primary');
      }

      function lecLine(id, reason) {
        const l = byId.get(id);
        return h('li', null, h('span', { class: 'rr-lec' }, l ? lectureLabel(l) : id), reason ? h('span', { class: 'rr-why' }, U.errorInfoText(reason, reason.detail || '')) : null);
      }

      function list(items) {
        const shown = items.slice(0, 6);
        return h('ul', { class: 'dlg-list rr-list' }, shown.map((it) => lecLine(it.lecture_id, it.reason)), items.length > shown.length ? h('li', { class: 'dim' }, t('course.rerun.more', { n: U.count(items.length - shown.length) })) : null);
      }

      function modelChoice(diff) {
        const unusable = diff.filter((d) => !d.snapshot_usable);
        const lecs = new Map();
        for (const d of diff) {
          if (!lecs.has(d.lecture_id)) lecs.set(d.lecture_id, []);
          lecs.get(d.lecture_id).push(d);
        }
        const shown = [...lecs.entries()].slice(0, 4);
        const cname = name + 'm';
        const opt = (value, label, hint, disabled) =>
          h(
            'label',
            { class: 'dlg-choice' + (disabled ? ' is-disabled' : '') },
            h('input', { type: 'radio', name: cname, value, disabled, onchange: () => ((choice = value), sync()) }),
            h('span', { class: 'dlg-choice-text' }, h('span', { class: 'dlg-choice-label' }, label), hint ? h('span', { class: 'dlg-choice-hint' }, hint) : null),
          );
        return h(
          'div',
          { class: 'rr-model' },
          h('p', { class: 'dlg-msg' }, t('course.rerun.model', { n: U.count(lecs.size) })),
          h(
            'ul',
            { class: 'dlg-list rr-list' },
            shown.map(([id, ds]) =>
              h(
                'li',
                null,
                h('span', { class: 'rr-lec' }, byId.get(id) ? lectureLabel(byId.get(id)) : ds[0].lecture_title || id),
                ds.map((d) => h('span', { class: 'rr-why' }, t('course.rerun.diff_row', { stage: U.stageName(d.stage), old: U.modelLabel(d.snapshot), new: U.modelLabel(d.current) }))),
              ),
            ),
            lecs.size > shown.length ? h('li', { class: 'dim' }, t('course.rerun.more', { n: U.count(lecs.size - shown.length) })) : null,
          ),
          h(
            'div',
            { class: 'dlg-choices', role: 'radiogroup' },
            opt('snapshot', t('common.model_choice.snapshot'), unusable.length ? t('common.model_choice.unusable', { reason: U.errorInfoText(unusable[0].reason_info, unusable[0].reason || '') || t('common.model_choice.unusable_default') }) : t('common.model_choice.snapshot_hint'), unusable.length > 0),
            opt('current', t('common.model_choice.current'), t('common.model_choice.current_hint'), false),
          ),
        );
      }

      function renderPlan() {
        const res = plan.results || [];
        const skipped = res.filter((r) => r.result === 'skipped');
        const failing = res.filter((r) => r.result === 'failed');
        const llm = [stage, ...STAGES.slice(STAGES.indexOf(stage) + 1)].filter((s) => LLM_STAGES.includes(s));
        const parts = [
          plan.queued > 0 ? h('p', { class: 'dlg-msg rr-will' }, t('course.rerun.will', { n: U.count(plan.queued), stage: U.stageName(stage) })) : h('p', { class: 'dlg-msg rr-will' }, t('course.rerun.none', { stage: U.stageName(stage) })),
          plan.queued > 0 ? h('p', { class: 'dlg-msg' + (stage === 'prep' || stage === 'asr' ? ' dlg-warn' : '') }, WHAT[stage]()) : null,
          plan.queued > 0 && llm.length ? h('p', { class: 'dlg-hint rr-cost' }, t('course.rerun.cost', { stages: U.listJoin(llm.map(U.stageName)) })) : null,
          skipped.length ? h('div', { class: 'rr-group' }, h('p', { class: 'dlg-hint' }, t('course.rerun.skipped', { n: U.count(skipped.length) })), list(skipped)) : null,
          failing.length ? h('div', { class: 'rr-group' }, h('p', { class: 'dlg-warn' }, t('course.rerun.failing', { n: U.count(failing.length) })), list(failing)) : null,
          plan.queued > 0 && (plan.diff || []).length ? modelChoice(plan.diff) : null,
        ];
        summary.replaceChildren(...parts.filter(Boolean)); // (replaceChildren would show null as text)
      }

      return h(
        'form',
        {
          class: 'rr-form',
          onsubmit: (e) => {
            e.preventDefault();
            if (!ok.disabled) close({ stage, choice });
          },
        },
        h('p', { class: 'dlg-msg' }, t('course.rerun.intro', { n: U.count(lectures.length) })),
        h('div', { class: 'dlg-choices rr-stages', role: 'radiogroup', 'aria-label': t('course.rerun.title') }, radios),
        summary,
        h('div', { class: 'dlg-actions' }, h('button', { type: 'button', class: 'btn', onclick: () => close(null) }, t('common.cancel')), ok),
      );
    },
    { wide: true, className: 'dlg-rerun' },
  );
  if (!picked) return [];
  // the choice made in the dialog; a 409 now (the settings changed since)
  // asks with the usual dialog
  const res = await withModelChoice((c) => api.rerunCourse(courseId, picked.stage, ids, { choice: c || picked.choice || undefined }), `rerun:${picked.stage}`);
  if (!res) return [];
  const queued = res.results.filter((r) => r.result === 'queued');
  const failed = res.results.filter((r) => r.result === 'failed');
  const stageText = U.stageName(picked.stage);
  if (queued.length && !failed.length) {
    toast(res.skipped ? t('course.rerun.toast.skipped', { n: U.count(queued.length), stage: stageText, skipped: U.count(res.skipped) }) : t('course.rerun.toast.ok', { n: U.count(queued.length), stage: stageText }), 'success');
  }
  if (failed.length) toast(t('course.rerun.toast.failed', { n: U.count(failed.length), error: U.errorInfoText(failed[0].reason, '') }), 'warn');
  return queued.map((r) => r.lecture).filter(Boolean);
}

// ---------- week overrides ----------

/** The Monday of teaching week `week` of the course (null without a term start), as calendar.WeekRange. */
export function weekMonday(course, week) {
  const ts = course && parseYMD(course.term_start);
  if (!ts || !(week >= 1)) return null;
  const start = mondayOf(ts);
  let mon = addDays(start, 7 * (week - 1));
  const skips = ((course && course.skip_weeks) || [])
    .map(parseYMD)
    .filter(Boolean)
    .map(mondayOf)
    .sort((a, b) => a - b);
  for (const s of skips) {
    if (s > mon) break;
    if (s >= start) mon = addDays(mon, 7);
  }
  return mon;
}

/** The lecture's week override when the back end applies it (the course has a term start). */
export function appliedOverride(lec) {
  const o = lec && lec.week_override;
  return o && o.applied && o.week > 0 ? o : null;
}

/** A teaching material that may be hung under a week (the course has weeks). */
export function canHang(lec, course) {
  if (!course || !course.term_start || !isMaterial(lec)) return false;
  const o = lec.week_override;
  return !o || o.kind === 'supplement';
}

/** The yellow "补" tag of a make-up lecture, or null. */
export function makeupTag(lec) {
  const o = lec.week_override;
  if (!o || o.kind !== 'makeup') return null;
  const when = fmtRecordedAt(lec);
  const tip = when ? t('course.week.tag_tip', { week: o.week, when }) : t('course.week.tag_tip_undated', { week: o.week });
  return h('span', { class: 'chip chip-makeup', title: tip, 'aria-label': tip }, t('course.week.tag'));
}

// PUT / DELETE /api/lectures/{id}/week with P13.6's confirmation; resolves
// to the updated lecture or null (kept the old numbers, or failed: shown).
async function send(fn, okText) {
  try {
    const l = await withRenumber(fn);
    if (l) toast(okText, 'success');
    return l;
  } catch (e) {
    toast(e instanceof ApiError ? e.message : String(e), 'error');
    return null;
  }
}

/** Hangs lec under week as its supplement (null when weeks are the same). */
export function hangUnder(lec, week) {
  const o = lec.week_override;
  if (o && o.kind === 'supplement' && o.week === week) return Promise.resolve(null);
  return send((confirm) => api.setLectureWeek(lec.id, week, 'supplement', confirm, { quiet: true }), t('course.week.toast.hung', { week }));
}

/** Removes lec's override (supplement or make-up). */
export function clearOverride(lec) {
  const o = lec.week_override;
  if (!o) return Promise.resolve(null);
  return send((confirm) => api.clearLectureWeek(lec.id, confirm, { quiet: true }), o.kind === 'makeup' ? t('course.week.toast.unmakeup') : t('course.week.toast.unhung', { week: o.week }));
}

function makeup(lec, week) {
  return send((confirm) => api.setLectureWeek(lec.id, week, 'makeup', confirm, { quiet: true }), t('course.week.toast.makeup', { week }));
}

// The weeks to offer: 1–12 at least, up to the latest week in use.
function weekItems(course, lectures, current, onPick) {
  const weeks = weekChoices(lectures, course.materials || []);
  if (current && !weeks.includes(current)) weeks.push(current);
  return weeks.map((w) => {
    const mon = weekMonday(course, w);
    return {
      id: `week:${w}`,
      label: t('kind.week', { week: w }),
      hint: w === current ? t('course.week.menu.current') : mon ? weekRangeText(mon) : '',
      checked: w === current,
      onSelect: () => onPick(w),
    };
  });
}

/**
 * Menu items of the week overrides of lec (course page): hang a teaching
 * material under a week / detach it; make any lecture a week's make-up /
 * undo it. Without a term start (no weeks) only the undo of an existing
 * override. done(lecture) receives the updated lecture.
 */
export function weekMenuItems(lec, course, lectures, done) {
  const o = lec.week_override;
  const after = (p) => p.then((l) => l && done(l));
  if (!course || !course.term_start) {
    // no weeks (the term start was cleared): the override does nothing, but can be removed
    if (!o) return [];
    return [{ id: o.kind === 'makeup' ? 'week_unmakeup' : 'week_unhang', label: o.kind === 'makeup' ? t('course.week.menu.unmakeup', { week: o.week }) : t('course.week.menu.unhang', { week: o.week }), icon: 'undo-2', onSelect: () => after(clearOverride(lec)) }];
  }
  const items = [];
  if (canHang(lec, course)) {
    items.push({ id: 'week_hang', label: t('course.week.menu.hang'), icon: 'layers', items: weekItems(course, lectures, o && o.kind === 'supplement' ? o.week : 0, (w) => after(hangUnder(lec, w))) });
    if (o) items.push({ id: 'week_unhang', label: t('course.week.menu.unhang', { week: o.week }), icon: 'undo-2', onSelect: () => after(clearOverride(lec)) });
  }
  if (!o || o.kind === 'makeup') {
    items.push({ id: 'week_makeup', label: t('course.week.menu.makeup'), icon: 'calendar', items: weekItems(course, lectures, o ? o.week : 0, (w) => (o && o.week === w ? null : after(makeup(lec, w)))) });
    if (o) items.push({ id: 'week_unmakeup', label: t('course.week.menu.unmakeup', { week: o.week }), icon: 'undo-2', onSelect: () => after(clearOverride(lec)) });
  }
  return items;
}

/** The submenu items of "attach to a week" (the drag handle's keyboard alternative). */
export function hangItems(lec, course, lectures, done) {
  const o = lec.week_override;
  const after = (p) => p.then((l) => l && done(l));
  const items = weekItems(course, lectures, o && o.kind === 'supplement' ? o.week : 0, (w) => after(hangUnder(lec, w)));
  if (o) items.push('sep', { id: 'week_unhang', label: t('course.week.menu.unhang', { week: o.week }), icon: 'undo-2', onSelect: () => after(clearOverride(lec)) });
  return items;
}
