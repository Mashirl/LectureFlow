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
// one marked and the time left. 3.2.0 fix: stats.remote is only what the
// lecture was loaded with (job events carry the progress alone, so the
// views do not reload while the stage runs, and the card stayed on 等卡);
// the steps now follow the device's card in the resources (remotes[].jobs[]
// of this lecture, opts.lecture, every 2 s), redrawn when the card opens and
// with every progress event, and fall back to stats.remote. The other lines
// that change while a stage runs or waits are redrawn the same way: the time
// it has run (it stayed at what it was when the lecture was loaded), the
// waiting reason (live numbers) and the tokens; and when a card opens on a
// running stage the resources do not list (or that counts tokens), or on a
// queued stage that waits, the lecture is read again — at most once per
// lecture in LECTURE_REFRESH_MS — and those lines follow it, without
// reloading the view (freshStage).
// stream-parallel: a source that must be transcoded gets its playback file
// from a background task after prep, in parallel with asr (opts.stream =
// Lecture.stream): while it runs the prep segment carries a small play mark,
// and prep's card has a "播放文件" line (generating n%, ready, failed…),
// updated in place by setProgress('stream', p).

import { h, toast, confirmDialog, copyText, everySecond, stopEverySecond } from './dom.js';
import { icon } from './icons.js';
import * as U from './util.js';
import { t } from './i18n.js';
import * as C from './settings_colab.js';
import { api } from './api.js';

let cardSeq = 0;

// stages: []StageView. opts:
//   key       unique prefix for focus keys (e.g. "lec:{id}")
//   sys, res  () => latest System / Resources (live numbers in waiting_for texts)
//   onAction  (stage, action) => Promise, called after confirmation
//   size      "sm" (default) | "lg" (stage names under the bar)
//   pct       show the running percentage next to the bar (default true)
//   stream    Lecture.stream (the playback file), shown with prep
//   lecture   the lecture's id: a remote stage's live steps (remotes[].jobs[])
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
  // 'stream' (the background playback file) updates prep's card line.
  bar.setProgress = (stage, progress) => {
    if (stage === 'stream') {
      if (!streamGenerating(opts.stream)) return;
      opts.stream.progress = progress;
      const line = bar.querySelector('.card-stream');
      if (line) line.textContent = streamText(opts.stream);
      return;
    }
    const wrap = bar.querySelector(`.seg-wrap[data-stage="${CSS.escape(stage)}"]`);
    if (!wrap) return;
    const st = stages.find((s) => s.stage === stage);
    if (st) st.progress = progress;
    const fill = wrap.querySelector('.seg-fill');
    const p = st ? U.barProgress(st) : progress;
    if (fill) fill.style.width = p != null ? U.pct(p) : '';
    const status = wrap.querySelector('.card-status-text');
    if (status && st) status.textContent = U.stageStatusText(st);
    if (wrap.refresh) wrap.refresh(); // the lines that change while it runs follow (3.2.0)
    const btn = wrap.querySelector('.seg');
    if (btn && st) btn.setAttribute('aria-label', segLabel(st, opts));
    syncPct();
  };
  return bar;
}

function segLabel(st, opts) {
  const label = t('common.pipe.seg_label', { stage: U.stageName(st.stage), status: U.stageStatusText(st) });
  if (streamMark(st, opts)) return U.clauseJoin([label, t('common.pipe.stream_bg_label')]);
  return C.stageOnColab(st) ? U.clauseJoin([label, t('common.pipe.remote_label')]) : label;
}

// ---- the playback file (stream-parallel) ----

function streamGenerating(stream) {
  return !!stream && stream.status === 'generating';
}

// prep's segment carries the mark while the playback file is made in the background
function streamMark(st, opts) {
  return st.stage === 'prep' && streamGenerating(opts && opts.stream);
}

// A job event of lec's playback-file task (stage "stream", one of lec.aux):
// a progress event of the status lec already shows updates lec and bar
// (when given) in place and returns true; anything else (a status change,
// another stage) returns false and the caller reloads as before. Keeps the
// minutes of a background transcode from reloading a view every 500 ms.
export function streamProgress(lec, bar, ev) {
  if (!lec || !ev || ev.stage !== 'stream') return false;
  const job = (lec.aux || []).find((a) => a.stage === 'stream');
  if (!job || job.status !== ev.status) return false;
  job.progress = ev.progress;
  if (bar) bar.setProgress('stream', ev.progress);
  else if (streamGenerating(lec.stream)) lec.stream.progress = ev.progress;
  return true;
}

// The "播放文件" line of prep's card (Lecture.stream.status).
function streamText(stream) {
  switch (stream.status) {
    case 'generating':
      return t('common.pipe.stream_generating', { pct: U.pct(stream.progress) });
    case 'ready':
      return stream.transcoded ? t('common.pipe.stream_ready_transcoded') : t('common.stream_status.ready');
    case 'pending':
      return t('common.pipe.stream_pending');
    case 'audio_only':
    case 'no_space':
    case 'failed':
      return t(`common.stream_status.${stream.status}`);
  }
  return '';
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
      'aria-label': segLabel(st, opts),
      'aria-describedby': cardId,
    },
    h('span', { class: 'seg-bar' }, h('span', { class: 'seg-fill', style: { width: U.barProgress(st) != null ? U.pct(U.barProgress(st)) : '' } })),
  );
  const card = h('div', { class: 'seg-card' + (i >= n / 2 ? ' seg-card-end' : ''), id: cardId, role: 'tooltip' }, cardBody(st, opts));
  const remote = C.stageOnColab(st);
  if (remote) btn.append(h('span', { class: 'seg-remote', 'aria-hidden': 'true' }, icon('server', { size: 9 })));
  if (streamMark(st, opts)) {
    // the remote mark's look (settings_v3.css), placed on the segment like it
    btn.style.position = 'relative';
    btn.append(h('span', { class: 'seg-remote seg-bg', 'aria-hidden': 'true' }, icon('play', { size: 9 })));
  }
  const wrap = h('div', { class: 'seg-wrap' + (remote ? ' is-remote' : ''), dataset: { stage: st.stage } }, btn, card);
  if (opts.size === 'lg') wrap.append(h('span', { class: 'seg-name', 'aria-hidden': 'true' }, U.stageName(st.stage)));
  // the lines that change while the stage runs (3.2.0): redrawn from st and
  // the latest system / resources
  wrap.refresh = () => refreshCard(card, st, opts);
  // open the card upwards when there is no room below it; bring it up to
  // date (and read the lecture again when only that can)
  const flip = () => {
    const r = wrap.getBoundingClientRect();
    wrap.classList.toggle('seg-up', r.bottom + 300 > window.innerHeight && r.top > 320);
    wrap.refresh();
    const fresh = freshStage(st, opts);
    if (fresh)
      fresh.then((f) => {
        if (!f || f.status !== st.status) return; // a status change reloads the view anyway
        // the progress stays the job events' (newer than a stats flush)
        Object.assign(st, { stats: f.stats, waiting_for: f.waiting_for, started_at: f.started_at });
        wrap.refresh();
      });
  };
  // The card is open while the pointer is over the segment or the focus is
  // in it, unless Esc closed it. While it is open, a stage that runs or
  // waits has its lines redrawn every second (the time it has run, a
  // retry's countdown: 3.2.0 L12); a closed card runs no timer.
  let hovered = false;
  let focused = false;
  const isOpen = () => (hovered || focused) && !wrap.classList.contains('is-dismissed');
  const tick = () => {
    if (!isOpen()) return false;
    wrap.refresh();
    return true;
  };
  const opened = () => {
    flip();
    if (st.status === 'running' || st.waiting_for) everySecond(wrap, tick);
  };
  const closed = () => {
    if (hovered || focused) return;
    wrap.classList.remove('is-dismissed'); // the next hover or focus opens it again
    stopEverySecond(wrap);
  };
  // Esc closes the card — under the pointer too (WCAG 1.4.13) — and leaves
  // the focus on the segment (3.2.0 L13; blur() left it on the page). true
  // when there was an open card to close.
  wrap.dismiss = () => {
    if (!isOpen()) return false;
    const a = document.activeElement;
    if (a && a !== btn && wrap.contains(a)) btn.focus({ preventScroll: true });
    wrap.classList.add('is-dismissed');
    stopEverySecond(wrap);
    return true;
  };
  wrap.addEventListener('pointerenter', () => {
    hovered = true;
    hoveredWrap = wrap;
    watchEscape();
    wrap.classList.remove('is-dismissed');
    opened();
  });
  wrap.addEventListener('pointerleave', () => {
    hovered = false;
    if (hoveredWrap === wrap) hoveredWrap = null;
    closed();
  });
  wrap.addEventListener('focusin', () => {
    if (!focused) wrap.classList.remove('is-dismissed'); // the focus came from outside
    focused = true;
    opened();
  });
  wrap.addEventListener('focusout', (e) => {
    focused = !!(e.relatedTarget && wrap.contains(e.relatedTarget));
    closed();
  });
  wrap.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && wrap.dismiss()) e.stopPropagation();
  });
  return wrap;
}

// Esc while the pointer rests on a segment (the focus elsewhere) closes its
// card (3.2.0 L13); one listener for every bar, added with the first hover.
let hoveredWrap = null;
let escapeWatched = false;
function watchEscape() {
  if (escapeWatched) return;
  escapeWatched = true;
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape' || !hoveredWrap) return;
    if (!hoveredWrap.isConnected) {
      hoveredWrap = null;
      return;
    }
    hoveredWrap.dismiss();
  });
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
  // the lines that change while the stage runs or waits are redrawn in place
  // (refreshCard, 3.2.0); a queued stage's waiting reason may appear later
  if (st.waiting_for || st.status === 'queued') rows.push(liveLine(t('common.pipe.k_wait'), U.stageWaitingText(st, sys, res), 'card-wait'));
  // SPEC-v3 V14.5: the remote steps of a Colab (or SSH) run, live (3.2.0)
  const rs = remoteNow(st, opts);
  if (st.status === 'running') rows.push(h('div', { class: 'card-remote', style: { display: 'contents' } }, ...(rs ? remoteSteps(rs, sys, res) : [])));
  // power management: how far the checkpoint of a queued stage reaches (the
  // status above says how much is done)
  if (U.checkpointProgress(st) != null) {
    const ck = st.checkpoint;
    rows.push(kv(t('common.pipe.k_checkpoint'), t('common.pipe.checkpoint', { at: U.fmtClock(ck.resume_ms), total: U.fmtClock(ck.duration_ms) })));
  }
  const note = U.noteText(st);
  if (note) rows.push(kv(t('common.pipe.k_note'), note));
  // stream-parallel: the playback file, made after prep (in the background for a transcode)
  if (st.stage === 'prep' && opts.stream && streamText(opts.stream)) {
    rows.push(kv(U.stageName('stream'), h('span', { class: 'card-stream' }, streamText(opts.stream))));
  }
  if (st.attempt > 0) rows.push(kv(t('common.pipe.k_attempts'), t('common.pipe.attempts', { n: st.attempt })));
  const time = timeText(st);
  if (time || st.status === 'running') rows.push(liveLine(t('common.pipe.k_time'), time, 'card-time'));
  // tokens: a running stage's first usage arrives with a stats flush
  const hasUsage = U.hasUsage(st.stats);
  if (hasUsage || st.status === 'running') rows.push(liveLine(t('common.pipe.k_tokens'), hasUsage ? U.usageText(st.stats) : '', 'card-tokens'));
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
        class: a === 'retry' || a === 'use_colab' ? 'btn btn-sm btn-primary' : a === 'cancel' ? 'btn btn-sm btn-danger' : 'btn btn-sm',
        dataset: { fkey: `${opts.key}:act:${st.stage}:${a}` },
        onclick: () => runAction(st, a, opts),
      },
      // use_colab: "改用 Colab 处理" (colabfix; its own confirmation, actions.js);
      // rerun_text_only: "修复断句" (textrepair-int; confirmTextRepair)
      icon(a === 'retry' ? 'rotate-cw' : a === 'cancel' ? 'ban' : a === 'use_colab' ? 'upload' : a === 'rerun_text_only' ? 'wand-sparkles' : 'refresh-cw', { size: 13 }),
      U.actionName(a),
    ),
  );
  if (actions.length && opts.onAction) rows.push(h('div', { class: 'card-actions' }, actions));
  return h('div', { class: 'seg-card-body' }, rows);
}

const REMOTE_STEPS = ['wait_accelerator', 'upload_audio', 'remote_transcribe', 'download_result', 'verify'];

// The card lines that change while a stage runs or waits (3.2.0), redrawn in
// place: the time it has run, the waiting reason (live numbers of system /
// resources, e.g. free VRAM), the tokens and the remote steps. These lines
// are built for every stage that may show them (hidden while empty), so a
// value that appears later has its place; the other lines change only with
// the status, which reloads the view.
function refreshCard(card, st, opts) {
  const sys = opts.sys ? opts.sys() : null;
  const res = opts.res ? opts.res() : null;
  setLine(card.querySelector('.card-time'), timeText(st));
  setLine(card.querySelector('.card-wait'), U.stageWaitingText(st, sys, res)); // a retry counts down (L12)
  setLine(card.querySelector('.card-tokens'), U.hasUsage(st.stats) ? U.usageText(st.stats) : '');
  const box = card.querySelector('.card-remote');
  const rs = remoteNow(st, opts);
  if (box) box.replaceChildren(...(rs ? remoteSteps(rs, sys, res) : []));
}

// A card line (kv) with its value replaced; hidden while it has none.
function setLine(line, text) {
  if (!line) return;
  line.style.display = text ? '' : 'none';
  const v = line.querySelector('.card-v');
  if (v && text) v.textContent = text;
}

// A card that opens reads the lecture again (job events carry the progress
// alone; stats and waiting_for are what the lecture was loaded with): on a
// running stage whose device's card in the resources does not list the
// lecture (the remote steps fall back to stats.remote; tokens), or that
// counts tokens; on a queued stage with a waiting reason (the scheduler's
// reason changes without an event). At most once per lecture in
// LECTURE_REFRESH_MS; the promise of the stage as read (null when it could
// not be read), or null when there is nothing to read or it was read just
// now.
export const LECTURE_REFRESH_MS = 5000;
const lectureReads = new Map(); // lecture id → when it was last read

export function freshStage(st, opts) {
  const id = opts && opts.lecture;
  if (!id || !st) return null;
  // running: the resources do not list the lecture, it counts tokens, or the
  // lecture was loaded before the first stats flush of the run (no
  // stats.remote yet: the steps are the card's, its accelerator the stats')
  const want =
    st.status === 'running'
      ? !cardJob(opts) || U.hasUsage(st.stats) || !(st.stats && st.stats.remote)
      : st.status === 'queued' && !!st.waiting_for;
  if (!want) return null;
  const now = Date.now();
  const last = lectureReads.get(id);
  if (last != null && now - last < LECTURE_REFRESH_MS) return null;
  lectureReads.set(id, now);
  return api
    .lecture(id, { quiet: true })
    .then((lec) => (lec && Array.isArray(lec.stages) ? lec.stages.find((s) => s.stage === st.stage) || null : null))
    .catch(() => null);
}

// The lecture's job on its device's card in the resources, with the card
// (null: none).
function cardJob(opts) {
  const res = opts && opts.res ? opts.res() : null;
  const id = opts && opts.lecture;
  if (!id || !res || !Array.isArray(res.remotes)) return null;
  for (const card of res.remotes) {
    for (const j of (card && card.jobs) || []) if (j.lecture_id === id) return { job: j, card };
  }
  return null;
}

// The remote state of a running stage (null when it does not run remotely):
// for asr, its device's card in the resources (remotes[].jobs[] of
// opts.lecture) when it lists the lecture — live, even when the lecture was
// loaded before the run's first stats flush (no stats.remote: the five
// steps, the card's accelerator when it names one) — else stats.remote (as
// the lecture was loaded, or read again when the card opened). kind and
// label say where it runs (remoteWhere).
export function remoteNow(st, opts) {
  if (!st || st.status !== 'running') return null;
  const rs = st.stats && st.stats.remote;
  const on = st.stage === 'asr' ? cardJob(opts) : null;
  if (!on) return rs ? { ...rs, kind: deviceKind(st) } : null;
  const { job: j, card } = on;
  const m = cardStep(j.stage);
  return {
    steps: REMOTE_STEPS,
    ...rs,
    accelerator: (rs && rs.accelerator) || cardAccelerator(card),
    kind: card.kind || deviceKind(st),
    label: card.label || '',
    step: m.step,
    stage: m.stage,
    // waiting reasons are the stage's own (stats); only while it waits
    waiting_for: m.step === 'wait_accelerator' && rs ? rs.waiting_for : undefined,
    eta_sec: typeof j.eta_sec === 'number' ? j.eta_sec : -1,
  };
}

// The accelerator a device's card names: Colab's session (Card.colab is its
// colab.Status), an SSH server's name (stats.remote.accelerator of an SSH
// run is the device's label too); '' when it names none.
function cardAccelerator(card) {
  if (card.kind === 'ssh') return card.label || '';
  return (card.colab && card.colab.accelerator) || '';
}

// "ssh" for a stage bound to an SSH server, else "colab".
function deviceKind(st) {
  const a = st.model && st.model.asr;
  return a && typeof a.device_id === 'string' && a.device_id.startsWith('ssh:') ? 'ssh' : 'colab';
}

// Where a remote run is: an SSH server by its name; Colab with its
// accelerator, or the card's label ("Google Colab") while the accelerator
// is not known; '' when nothing says (the line is left out).
function remoteWhere(rs) {
  if (rs.kind === 'ssh') return rs.accelerator || rs.label || '';
  if (rs.accelerator) return t('common.pipe.remote_where', { accelerator: rs.accelerator });
  return rs.label || '';
}

// A card job's stage as a remote step and the worker's stage: SSH cards name
// the step itself; Colab's name the worker's stage while it transcribes
// (check, install, download_model, load_model, transcribe, write) and wait,
// upload, download or verify otherwise (internal/resources/remotes.go).
export function cardStep(stage) {
  if (REMOTE_STEPS.includes(stage)) return { step: stage, stage: '' };
  switch (stage) {
    case 'wait':
    case '':
    case undefined:
    case null:
      return { step: 'wait_accelerator', stage: '' };
    case 'upload':
      return { step: 'upload_audio', stage: '' };
    case 'download':
      return { step: 'download_result', stage: '' };
    case 'verify':
      return { step: 'verify', stage: '' };
  }
  return { step: 'remote_transcribe', stage };
}

// The five remote steps (stats.remote.steps, in order) with the current one
// marked, its worker stage or waiting reason, and the time left.
export function remoteSteps(rs, sys, res) {
  const steps = rs.steps && rs.steps.length ? rs.steps : REMOTE_STEPS;
  const at = steps.indexOf(rs.step);
  const where = remoteWhere(rs);
  const out = where ? [kv(t('common.pipe.k_remote'), where, 'card-where')] : [];
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

function kv(k, v, cls) {
  return h('div', { class: cls ? `card-line ${cls}` : 'card-line' }, h('span', { class: 'card-k' }, k), h('span', { class: 'card-v' }, v));
}

// A card line refreshCard redraws (class cls), hidden while text is empty.
function liveLine(k, text, cls) {
  const line = kv(k, text, cls);
  if (!text) line.style.display = 'none';
  return line;
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
  if (action === 'rerun_text_only') return confirmTextRepair();
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

// "修复断句" (textrepair-int; the asr stage's rerun_text_only): says what it
// does not do (transcribe again), what it keeps (the sentences a person
// edited), what runs again and what it costs, and what is cleared (the
// review's accept / reject records).
function confirmTextRepair() {
  return confirmDialog({
    title: t('lecture.text_repair.title'),
    message: t('lecture.text_repair.message'),
    body: h(
      'ul',
      { class: 'dlg-list' },
      ['no_transcribe', 'keep_reviewed', 'downstream', 'patches'].map((k) => h('li', null, t(`lecture.text_repair.${k}`))),
    ),
    okText: t('lecture.text_repair.ok'),
  });
}

// A card's action while the same one is on its way (a double click on
// "重试": two requests, two model-choice dialogs) does nothing (3.2.0 P2);
// this covers every bar (lectures, materials, the course's stages).
const acting = new Set(); // `${opts.key}|${stage}|${action}`

async function runAction(st, action, opts) {
  const k = `${opts.key}|${st.stage}|${action}`;
  if (acting.has(k)) return;
  acting.add(k);
  try {
    if (!(await confirmStageAction(st.stage, action))) return;
    await opts.onAction(st.stage, action);
  } catch {
    // the API layer already showed the error
  } finally {
    acting.delete(k);
  }
}
