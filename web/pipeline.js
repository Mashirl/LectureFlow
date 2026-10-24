// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Stage pipeline bar with hover cards (SPEC §9.5, §9.8; v1.1 A2.1, A12.1).
// Rounded segments, one per stage; the state is shown by colour *and*
// pattern (queued dots, running flow, stale / cancelled / outdated stripes).
// Stage names appear in the hover card, never inside the bar; the running
// stage's percentage is shown next to the bar. A queued asr stage the power
// policy put back in line keeps its checkpoint (StageView.checkpoint): its
// segment is filled that far and the share is shown, muted, next to the bar
// while nothing runs. Every action button comes from the stage's
// allowed_actions.
// SPEC-v3 V14.5: an asr stage that runs (or ran) on Google Colab carries a
// small remote mark on its segment; while it runs, its card lists the
// remote steps 等卡 → 上传音频 → 远端转写 → 下载结果 → 校验 with the current
// one marked and the time left (stats.remote).

import { h, toast, confirmDialog, copyText } from './dom.js';
import { icon } from './icons.js';
import * as U from './util.js';
import { t } from './i18n.js';
import * as C from './settings_colab.js';

let cardSeq = 0;

// stages: []StageView. opts:
//   key       unique prefix for focus keys (e.g. "lec:{id}")
//   sys, res  () => latest System / Resources (live numbers in waiting_for texts)
//   onAction  (stage, action) => Promise, called after confirmation
//   size      "sm" (default) | "lg" (stage names under the bar)
//   pct       show the running percentage next to the bar (default true)
export function pipelineBar(stages, opts) {
  const size = opts.size || 'sm';
  const segs = h('div', { class: 'pipe-segs' });
  const bar = h('div', { class: `pipe pipe-${size}`, role: 'group', 'aria-label': opts.label || t('common.pipe.label') }, segs);
  const n = stages.length;
  stages.forEach((st, i) => segs.append(segment(st, i, n, opts)));
  const pctEl = opts.pct === false ? null : h('span', { class: 'pipe-pct', 'aria-hidden': 'true' });
  if (pctEl) bar.append(pctEl);
  const syncPct = () => {
    if (!pctEl) return;
    const run = U.runningStage(stages);
    // nothing runs: the share a queued stage keeps in its checkpoint
    const ck = run ? null : stages.find((s) => U.checkpointProgress(s) != null);
    pctEl.textContent = run ? U.pct(run.progress) : ck ? U.pct(U.checkpointProgress(ck)) : '';
    pctEl.classList.toggle('is-ckpt', !!ck);
    if (ck) pctEl.title = U.stageStatusText(ck);
    else pctEl.removeAttribute('title');
  };
  syncPct();
  // In-place progress update from an SSE job event (no re-render, keeps hover/focus).
  bar.setProgress = (stage, progress) => {
    const wrap = bar.querySelector(`.seg-wrap[data-stage="${CSS.escape(stage)}"]`);
    if (!wrap) return;
    const st = stages.find((s) => s.stage === stage);
    if (st) st.progress = progress;
    const fill = wrap.querySelector('.seg-fill');
    const p = st ? U.barProgress(st) : progress;
    if (fill) fill.style.width = p != null ? U.pct(p) : '';
    const status = wrap.querySelector('.card-status-text');
    if (status && st) status.textContent = U.stageStatusText(st);
    const btn = wrap.querySelector('.seg');
    if (btn && st) btn.setAttribute('aria-label', segLabel(st));
    syncPct();
  };
  return bar;
}

function segLabel(st) {
  const label = t('common.pipe.seg_label', { stage: U.stageName(st.stage), status: U.stageStatusText(st) });
  return C.stageOnColab(st) ? U.clauseJoin([label, t('common.pipe.remote_label')]) : label;
}

function segment(st, i, n, opts) {
  const fkey = `${opts.key}:seg:${st.stage}`;
  const ds = U.displayStatus(st);
  const cardId = `segcard-${++cardSeq}`;
  const btn = h(
    'button',
    {
      type: 'button',
      class: 'seg' + (U.hasWarning(st) ? ' seg-warn' : ''),
      dataset: { st: ds, fkey },
      'aria-label': segLabel(st),
      'aria-describedby': cardId,
    },
    h('span', { class: 'seg-bar' }, h('span', { class: 'seg-fill', style: { width: U.barProgress(st) != null ? U.pct(U.barProgress(st)) : '' } })),
  );
  const card = h('div', { class: 'seg-card' + (i >= n / 2 ? ' seg-card-end' : ''), id: cardId, role: 'tooltip' }, cardBody(st, opts));
  const remote = C.stageOnColab(st);
  if (remote) btn.append(h('span', { class: 'seg-remote', 'aria-hidden': 'true' }, icon('server', { size: 9 })));
  const wrap = h('div', { class: 'seg-wrap' + (remote ? ' is-remote' : ''), dataset: { stage: st.stage } }, btn, card);
  if (opts.size === 'lg') wrap.append(h('span', { class: 'seg-name', 'aria-hidden': 'true' }, U.stageName(st.stage)));
  // open the card upwards when there is no room below it
  const flip = () => {
    const r = wrap.getBoundingClientRect();
    wrap.classList.toggle('seg-up', r.bottom + 300 > window.innerHeight && r.top > 320);
  };
  wrap.addEventListener('pointerenter', flip);
  wrap.addEventListener('focusin', flip);
  wrap.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && wrap.contains(document.activeElement)) {
      e.stopPropagation();
      document.activeElement.blur();
    }
  });
  return wrap;
}

// The text of key with the params named in whole each in an unbreakable
// span (the translation decides the order and the words around them).
const MARK = '\uE000';
function keptWhole(key, params, whole) {
  const marked = { ...params };
  whole.forEach((k, i) => (marked[k] = `${MARK}${i}${MARK}`));
  return t(key, marked)
    .split(MARK)
    .map((s, i) => (i % 2 ? h('span', { class: 'nowrap' }, String(params[whole[Number(s)]])) : s))
    .filter((x) => x !== '');
}

function cardBody(st, opts) {
  const sys = opts.sys ? opts.sys() : null;
  const res = opts.res ? opts.res() : null;
  const ds = U.displayStatus(st);
  const tone = U.hasWarning(st) ? 'warn' : ds;
  const rows = [];
  rows.push(
    h(
      'div',
      { class: 'card-head' },
      h('strong', { class: 'card-stage' }, U.stageName(st.stage)),
      h('span', { class: `card-status st-${tone}` }, h('span', { class: 'card-dot', dataset: { st: ds } }), h('span', { class: 'card-status-text', text: U.stageStatusText(st) })),
    ),
  );
  if (ds === 'outdated') rows.push(h('p', { class: 'card-note card-note-outdated' }, t('stale.outdated_hint')));
  if (st.status === 'stale') {
    const reason = U.stageStaleText(st) || t('stale.reason.input_changed');
    rows.push(h('p', { class: 'card-note card-note-stale' }, t('stale.card', { reason })));
  }
  if (st.waiting_for) rows.push(kv(t('common.pipe.k_wait'), U.waitingForText(st.waiting_for, sys, res)));
  // SPEC-v3 V14.5: the remote steps of a Colab run
  if (st.stats && st.stats.remote && st.status === 'running') rows.push(...remoteSteps(st.stats.remote, sys, res));
  // power management: how far the checkpoint of a queued stage reaches (the
  // status above says how much is done)
  if (U.checkpointProgress(st) != null) {
    const ck = st.checkpoint;
    rows.push(kv(t('common.pipe.k_checkpoint'), t('common.pipe.checkpoint', { at: U.fmtClock(ck.resume_ms), total: U.fmtClock(ck.duration_ms) })));
  }
  const note = U.noteText(st);
  if (note) rows.push(kv(t('common.pipe.k_note'), note));
  if (st.attempt > 0) rows.push(kv(t('common.pipe.k_attempts'), t('common.pipe.attempts', { n: st.attempt })));
  const time = timeText(st);
  if (time) rows.push(kv(t('common.pipe.k_time'), time));
  if (U.hasUsage(st.stats)) rows.push(kv(t('common.pipe.k_tokens'), U.usageText(st.stats)));
  // asr (SPEC-v2 I6): "device · model · precision · language"
  if (st.model && st.model.label) rows.push(kv(st.stage === 'asr' ? t('common.pipe.k_device') : t('common.pipe.k_model'), U.modelLabel(st.model)));
  // power management: a run resumed from its checkpoint on another device
  // (snapshot parts), one line per part: time range · device · model · precision
  const parts = st.stage === 'asr' ? U.asrParts(st.model) : null;
  if (parts) {
    // a line breaks only between the device name's words, never inside a
    // time or "model · precision" (large-v3-turbo has hyphens)
    const lines = parts.map((p) =>
      h('span', { class: 'card-part' }, keptWhole('common.pipe.part', { from: U.fmtClock(p.from_ms), to: U.fmtClock(p.to_ms), device: U.deviceLabel(p.device_id, sys, res), combo: U.comboText(p.model, p.precision) }, ['from', 'to', 'combo'])),
    );
    rows.push(kv(t('common.pipe.k_parts'), lines));
  }
  // asr (phase 3b): the worker's timing of the last run, i.e. transcription
  // time and audio length, RTF, model load (first compile noted)
  if (st.stage === 'asr' && st.asr_timing) {
    const tm = U.asrTimingText(st.asr_timing);
    if (tm.time) rows.push(kv(t('common.pipe.k_asr_time'), tm.time));
    if (tm.speed) rows.push(kv(t('common.pipe.k_speed'), tm.speed));
    if (tm.load) rows.push(kv(t('common.pipe.k_load'), tm.load));
  }
  if (st.error) {
    const warn = U.hasWarning(st);
    const coded = st.error_info || (warn && st.warning_info) ? U.stageErrorText(st) : '';
    const label = st.status === 'failed' ? t('common.pipe.k_error') : warn ? t('common.pipe.k_warning') : t('common.pipe.k_last_error');
    rows.push(
      h(
        'div',
        { class: warn ? 'card-error card-warning' : 'card-error' },
        h('div', { class: 'card-k' }, label),
        // v2 P14.3: the coded error in the UI language above the original text
        coded && coded !== st.error ? h('p', { class: 'card-errmsg' }, coded) : null,
        // A warning is shown in full; a failure shows its first 300 characters (SPEC §9.8).
        h('pre', { class: 'card-errtext', text: warn ? st.error : U.excerpt(st.error, 300) }),
        h(
          'button',
          {
            type: 'button',
            class: 'btn btn-sm btn-quiet',
            dataset: { fkey: `${opts.key}:copy:${st.stage}` },
            onclick: async () => {
              const ok = await copyText(st.error);
              toast(ok ? t('common.copied_all') : t('common.copy_failed'), ok ? 'success' : 'warn');
            },
          },
          icon('copy', { size: 13 }),
          warn ? t('common.pipe.copy_warning') : t('common.pipe.copy_error'),
        ),
      ),
    );
  }
  const actions = (st.allowed_actions || []).map((a) =>
    h(
      'button',
      {
        type: 'button',
        class: a === 'retry' ? 'btn btn-sm btn-primary' : a === 'cancel' ? 'btn btn-sm btn-danger' : 'btn btn-sm',
        dataset: { fkey: `${opts.key}:act:${st.stage}:${a}` },
        onclick: () => runAction(st, a, opts),
      },
      icon(a === 'retry' ? 'rotate-cw' : a === 'cancel' ? 'ban' : 'refresh-cw', { size: 13 }),
      U.actionName(a),
    ),
  );
  if (actions.length && opts.onAction) rows.push(h('div', { class: 'card-actions' }, actions));
  return h('div', { class: 'seg-card-body' }, rows);
}

// The five remote steps (stats.remote.steps, in order) with the current one
// marked, its worker stage or waiting reason, and the time left.
function remoteSteps(rs, sys, res) {
  const steps = rs.steps && rs.steps.length ? rs.steps : ['wait_accelerator', 'upload_audio', 'remote_transcribe', 'download_result', 'verify'];
  const at = steps.indexOf(rs.step);
  const out = [kv(t('common.pipe.k_remote'), t('common.pipe.remote_where', { accelerator: rs.accelerator || '' }))];
  const items = steps.map((s, i) => {
    const state = at < 0 ? 'todo' : i < at ? 'done' : i === at ? 'now' : 'todo';
    let detail = '';
    if (i === at && rs.waiting_for) detail = U.waitingForText(rs.waiting_for, sys, res);
    else if (i === at && rs.stage && rs.stage !== 'transcribe' && s === 'remote_transcribe') detail = C.stageName(rs.stage);
    return h(
      'li',
      { class: `rstep is-${state}`, 'aria-current': state === 'now' ? 'step' : undefined },
      h('span', { class: 'rstep-dot', 'aria-hidden': 'true' }, state === 'done' ? icon('check', { size: 10 }) : null),
      h('span', { class: 'rstep-name' }, C.stepName(s)),
      detail ? h('span', { class: 'rstep-detail' }, detail) : null,
    );
  });
  out.push(h('ol', { class: 'rsteps', 'aria-label': t('common.pipe.k_remote_steps') }, items));
  if (rs.eta_sec > 0) out.push(kv(t('common.pipe.k_remote_eta'), C.durText(rs.eta_sec)));
  return out;
}

function kv(k, v) {
  return h('div', { class: 'card-line' }, h('span', { class: 'card-k' }, k), h('span', { class: 'card-v' }, v));
}

function timeText(st) {
  if (st.duration_ms != null) return U.fmtDuration(st.duration_ms);
  if (st.status === 'running' && st.started_at) {
    const ms = Date.now() - new Date(st.started_at).getTime();
    if (ms >= 0) return t('common.pipe.elapsed', { time: U.fmtDuration(ms) });
  }
  return '';
}

// What a rerun deletes and redoes, per stage (common.rerun.{stage}).
const RERUN_STAGES = new Set(['prep', 'asr', 'summary', 'repair', 'translate', 'export', 'glossary_seed', 'glossary_review', 'material_extract', 'material_terms']);

export function rerunText(stage) {
  return RERUN_STAGES.has(stage) ? t(`common.rerun.${stage}`) : t('common.rerun.default');
}

// Confirmation for the destructive stage actions (SPEC §4.6); true = go on.
export async function confirmStageAction(stage, action) {
  const name = U.stageName(stage);
  if (action === 'rerun') {
    return confirmDialog({
      title: t('common.rerun.title', { stage: name }),
      message: t('common.rerun.message', { text: rerunText(stage) }),
      okText: t('common.rerun.ok'),
      danger: stage === 'prep' || stage === 'asr',
    });
  }
  if (action === 'cancel') {
    return confirmDialog({
      title: t('common.stage_cancel.title', { stage: name }),
      message: t('common.stage_cancel.message'),
      okText: t('common.stage_cancel.ok'),
      cancelText: t('common.dont_cancel'),
      danger: true,
    });
  }
  return true;
}

async function runAction(st, action, opts) {
  if (!(await confirmStageAction(st.stage, action))) return;
  try {
    await opts.onAction(st.stage, action);
  } catch {
    // the API layer already showed the error
  }
}
