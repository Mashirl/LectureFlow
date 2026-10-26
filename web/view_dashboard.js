// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// "#/" Dashboard (v1.1 A9): in progress, to-do (drafts, reviews, stale
// lectures, failures of the last 24 h), recently finished, today's usage and
// the course overview. Everything comes from GET /api/dashboard; actions
// re-fetch it afterwards (no optimistic changes).
//
// lecture kinds: a lecture is named by its course code and kind ("CSC1003 ·
// Week 3 · Tutorial") instead of W03L2. The lecture lists are grids whose
// rows share columns (CSS subgrid): the codes, the kinds and the titles
// start on the same vertical lines, and the date, length and cost of the
// recently finished list are right-aligned columns with a reserved place
// for the "to review" badge.
//
// SPEC-v3 V14.5: while a Colab session exists a line under the title says
// how it goes ("Colab · T4 运行中 · 使用时限剩余 2 小时 13 分 · 本次还能处理约
// 2 节"), updated in place from every resource snapshot.
//
// 3.1.0 M2: with nothing to transcribe with (no usable local device, Colab
// not signed in) a prominent "cannot transcribe yet" notice with its two
// entries comes first, also on the empty workbench (asr_ready.js).

import { api } from './api.js';
import { h, clear, btn, skeleton, errorBlock, preserveFocus, confirmDialog, toast } from './dom.js';
import { icon } from './icons.js';
import { pipelineBar } from './pipeline.js';
import { bindContextMenu, menuButton } from './menu.js';
import { lectureMenuItems, exportItems, stageAction, modelChoiceDialog, batchError } from './actions.js';
import { courseCard, addCourseCard } from './course_card.js';
import * as U from './util.js';
import { t } from './i18n.js';
import { tariffUsageBlock, hasPeaks } from './tariff_ui.js';
import { zh } from './zh_display.js';
import { lectureTagText, lectureLabel } from './kinds.js';
import { createUpdateHint } from './update_hint.js';
import * as C from './settings_colab.js';
import { asrNotice } from './asr_ready.js';

const enc = encodeURIComponent;

export function mountDashboard(root, ctx) {
  let data = null;
  let error = '';
  let settings = null;
  let alive = true;
  let busy = new Set(); // action keys in flight (buttons disabled meanwhile)
  const pipes = new Map(); // lecture id → {bar, lec, statusEl}
  const timers = new Map();

  const el = h('div', { class: 'page page-dashboard' });
  root.append(el);
  // SPEC-v3 V8: a quiet line at the bottom when the desktop app has an update
  const updateHint = createUpdateHint({ variant: 'dashboard' });
  // SPEC-v3 V14.5: the Colab line (kept across renders, filled in place)
  const colabSlot = h('div', { class: 'dash-colab-slot', role: 'status' });
  // 3.1.0 M2: "cannot transcribe yet" (kept across renders, filled in place)
  const asrSlot = asrNotice({ variant: 'page' });

  async function load() {
    try {
      const d = await api.dashboard();
      if (!alive) return;
      data = d;
      error = '';
    } catch (e) {
      if (!alive) return;
      if (!data) error = e.message || t('common.load_failed');
    }
    render();
  }

  async function loadSettings() {
    try {
      const s = await api.settings();
      if (!alive) return;
      settings = s;
      if (data) render();
    } catch {
      // optional: the usage card works without the budget line
    }
  }

  function schedule(key, fn, ms = 400) {
    clearTimeout(timers.get(key));
    timers.set(
      key,
      setTimeout(() => {
        timers.delete(key);
        fn();
      }, ms),
    );
  }

  // ---- render ----

  function render() {
    preserveFocus(() => {
      clear(el);
      pipes.clear();
      if (!data && !error) {
        const sk = (k, n) => h('section', { class: `card card-${k} is-loading` }, h('div', { class: 'card-hd' }, h('div', { class: 'skel skel-title' })), h('div', { class: 'card-bd' }, skeleton(n)));
        el.append(head(null), h('div', { class: 'dash' }, h('div', { class: 'dash-col dash-main' }, sk('ip', 6), sk('recent', 5)), h('div', { class: 'dash-col dash-side' }, sk('todo', 5), sk('usage', 4))));
        return;
      }
      if (error) {
        el.append(head(null), h('section', { class: 'card' }, h('div', { class: 'card-bd' }, errorBlock(error, retry))));
        return;
      }
      if (data.courses.length === 0) {
        el.append(asrSlot.el, emptyState(), updateHint.el);
        return;
      }
      fillColab();
      el.append(
        head(data),
        asrSlot.el,
        colabSlot,
        devicePrompt() || '',
        h(
          'div',
          { class: 'dash' },
          h('div', { class: 'dash-col dash-main' }, inProgressCard(), recentCard()),
          h('div', { class: 'dash-col dash-side' }, todoCard(), (usageEl = usageCard())),
          coursesCard(),
        ),
        updateHint.el,
      );
    });
  }

  function retry() {
    error = '';
    data = null;
    render();
    load();
  }

  function head(d) {
    const now = new Date();
    const parts = [U.fmtDayWithWeekday(now)];
    if (d) {
      if (d.in_progress.length) parts.push(t('dashboard.head.in_progress', { n: d.in_progress.length }));
      if (d.draft_total) parts.push(t('dashboard.head.drafts', { n: d.draft_total }));
      if (d.review_total) parts.push(t('dashboard.head.reviews', { n: d.review_total }));
      if (!d.in_progress.length && !d.draft_total && !d.review_total && !d.stale_total && !d.failures.length) parts.push(t('dashboard.head.all_clear'));
    }
    return h(
      'header',
      { class: 'page-head' },
      h('div', { class: 'page-titles' }, h('h1', { class: 'page-title' }, t('dashboard.title')), h('p', { class: 'page-sub' }, parts.join(' · '))),
      h('div', { class: 'page-actions' }, btn(t('course.add'), { icon: 'plus', onclick: ctx.newCourse, fkey: 'dash:add' })),
    );
  }

  // "Colab · T4 运行中 · 使用时限剩余 2 小时 13 分 · 本次还能处理约 2 节"
  function fillColab() {
    clear(colabSlot);
    const res = ctx.resources();
    const r = res && res.remote;
    if (!r || !C.ACTIVE_STATES.has(r.state)) {
      colabSlot.hidden = true;
      return;
    }
    colabSlot.hidden = false;
    const texts = [t('dashboard.colab.head', { accelerator: r.accelerator, state: C.stateName(r.state) })];
    if (r.window_total_sec > 0) texts.push(t('dashboard.colab.window_left', { time: C.durText(Math.max(0, r.window_total_sec - (r.window_used_sec || 0))) || C.durText(0) }));
    if (r.state !== 'stopping') texts.push(r.lectures_left > 0 ? t('dashboard.colab.lectures_left', { n: r.lectures_left }) : t('dashboard.colab.lectures_none'));
    if (r.current && r.current.step) texts.push(t('dashboard.colab.step', { step: C.stepName(r.current.step) }));
    colabSlot.append(h('p', { class: 'dash-colab', title: t('resources.remote.title') }, icon('server', { size: 15 }), h('span', null, texts.join(' · '))));
  }

  // SPEC-v2 I9: a calm, closable notice about new transcription devices that
  // were never tested ("test now" opens the device's test button).
  function devicePrompt() {
    const ps = data.device_prompts || [];
    if (!ps.length) return null;
    const text =
      ps.length === 1
        ? t('devices.prompt.text', { device: ps[0].label })
        : t('devices.prompt.text_many', { devices: U.listJoin(ps.map((p) => t('settings.quote', { name: p.label }))) });
    const key = 'dash:devprompt';
    return h(
      'section',
      { class: 'dash-prompt', 'aria-label': t('devices.prompt.title') },
      h('span', { class: 'dash-prompt-icon', 'aria-hidden': 'true' }, icon('gauge', { size: 18 })),
      h('div', { class: 'dash-prompt-main' }, h('p', { class: 'dash-prompt-text' }, text), h('p', { class: 'dash-prompt-hint' }, t('devices.prompt.hint'))),
      h(
        'div',
        { class: 'dash-prompt-actions' },
        h('a', { class: 'btn btn-sm btn-primary', href: `#/settings/asr/${enc(ps[0].device_id)}`, dataset: { fkey: `${key}:go` } }, h('span', { class: 'btn-label' }, t('devices.prompt.go'))),
        btn(t('devices.prompt.dismiss'), { size: 'sm', kind: 'ghost', disabled: busy.has(key), fkey: `${key}:dismiss`, onclick: () => run(key, () => Promise.all(ps.map((p) => api.dismissDevicePrompt(p.device_id)))) }),
      ),
    );
  }

  function card(cls, ic, title, count, tools, body) {
    return h(
      'section',
      { class: `card ${cls}`, 'aria-label': title },
      h(
        'header',
        { class: 'card-hd' },
        h('h2', { class: 'card-title' }, icon(ic, { size: 16 }), h('span', null, title), count != null ? h('span', { class: 'card-count' }, String(count)) : null),
        tools ? h('div', { class: 'card-tools' }, tools) : null,
      ),
      h('div', { class: 'card-bd' }, body),
    );
  }

  function emptyLine(ic, text) {
    return h('div', { class: 'card-empty' }, icon(ic, { size: 18 }), h('span', null, text));
  }

  // The course code and kind cells of a lecture row (lecture, review group or
  // failure: all carry kind, week, kind_n, show_n).
  function lref(code, item) {
    const tag = lectureTagText(item);
    return [h('span', { class: 'lref-code' }, code || ''), h('span', { class: 'lref-tag', title: tag }, tag)];
  }

  const menuHandlers = { onLectures: () => load(), onDeleted: () => load() };

  // ---- in progress ----

  function inProgressCard() {
    const list = data.in_progress;
    return card(
      'card-ip',
      'activity',
      t('dashboard.in_progress.title'),
      list.length || null,
      null,
      list.length ? h('div', { class: 'ip-list lgrid' }, list.map(ipRow)) : emptyLine('circle-check', t('dashboard.in_progress.empty')),
    );
  }

  function statusFor(lec) {
    const sum = U.lectureSummary(lec.stages, { sys: ctx.system(), res: ctx.resources(), staleText: U.lectureStaleText(lec), draft: lec.draft });
    let text = sum.text;
    if (sum.kind === 'running' && sum.stage.started_at) {
      const ms = Date.now() - new Date(sum.stage.started_at).getTime();
      if (ms > 0) text = t('dashboard.in_progress.running', { status: text, duration: U.fmtDuration(ms) });
    }
    return { text, tone: sum.tone };
  }

  function ipRow(lec) {
    const bar = pipelineBar(lec.stages, { key: `dash:${lec.id}`, sys: ctx.system, res: ctx.resources, onAction: (stage, action) => stageAction(lec, stage, action, true).then((r) => r && load()) });
    const st = statusFor(lec);
    const statusEl = h('div', { class: `ip-status tone-${st.tone}`, title: st.text }, st.text); // truncated: full text on hover (P14.5)
    pipes.set(lec.id, { bar, lec, statusEl });
    const row = h(
      'div',
      { class: 'ip-row lgrid-row' },
      lref(lec.course_code, lec),
      h('div', { class: 'ip-main' }, h('a', { class: 'ip-link', href: `#/lecture/${enc(lec.id)}`, title: `${lec.course_name} · ${zh(lec.title)}`, dataset: { fkey: `dash:open:${lec.id}` } }, zh(lec.title)), statusEl),
      h('div', { class: 'ip-pipe' }, bar),
    );
    bindContextMenu(row, () => lectureMenuItems(lec, menuHandlers));
    return row;
  }

  // ---- to do ----

  function todoCard() {
    const d = data;
    const groups = [];
    if (d.draft_total) {
      groups.push(
        todoGroup(
          'file-text',
          t('dashboard.todo.drafts'),
          d.draft_total,
          btn(t('course.launch_drafts'), { size: 'sm', kind: 'primary', icon: 'play', disabled: busy.has('drafts'), onclick: launchAllDrafts, fkey: 'dash:drafts' }),
          h('div', { class: 'todo-sub' }, d.drafts.map((c) => h('a', { class: 'todo-chip', href: `#/course/${enc(c.course_id)}` }, c.course_code || c.course_name, h('b', null, String(c.count))))),
        ),
      );
    }
    if (d.review_total) {
      const shown = d.reviews.slice(0, 5);
      groups.push(
        todoGroup(
          'list-checks',
          t('dashboard.todo.reviews'),
          d.review_total,
          null,
          h(
            'ul',
            { class: 'todo-list lgrid lgrid-review' },
            shown.map((r) =>
              h(
                'li',
                { class: 'lgrid-row' },
                h(
                  'a',
                  { class: 'todo-link lgrid-row', href: `#/lecture/${enc(r.lecture_id)}/review`, title: `${r.course_name} · ${r.lecture_title}` },
                  lref(r.course_code, r),
                  h('span', { class: 'todo-link-title' }, r.lecture_title),
                  h('span', { class: 'todo-num' }, String(r.count)),
                  icon('chevron-right', { size: 14, class: 'todo-go' }),
                ),
              ),
            ),
            d.reviews.length > shown.length ? h('li', { class: 'todo-more' }, t('dashboard.todo.more_reviews', { n: d.reviews.length - shown.length })) : null,
          ),
          t('dashboard.todo.reviews_note'),
        ),
      );
    }
    if (d.stale_total) {
      groups.push(
        todoGroup(
          'refresh-cw',
          t('dashboard.todo.stale'),
          d.stale_total,
          btn(t('dashboard.todo.update_all'), { size: 'sm', icon: 'refresh-cw', disabled: busy.has('stale'), onclick: updateAllStale, fkey: 'dash:stale' }),
          h('div', { class: 'todo-sub' }, d.stale.map((c) => h('a', { class: 'todo-chip todo-chip-warn', href: `#/course/${enc(c.course_id)}` }, c.course_code || c.course_name, h('b', null, String(c.count))))),
          t('dashboard.todo.stale_note'),
        ),
      );
    }
    if (d.failures.length) {
      groups.push(
        todoGroup(
          'circle-alert',
          t('dashboard.todo.failures'),
          d.failures.length,
          null,
          h('ul', { class: 'todo-list lgrid lgrid-fail' }, d.failures.map(failureRow)),
          null,
          'is-error',
        ),
      );
    }
    return card('card-todo', 'inbox', t('dashboard.todo.title'), null, null, groups.length ? h('div', { class: 'todo' }, groups) : emptyLine('circle-check', t('dashboard.todo.empty')));
  }

  function todoGroup(ic, title, count, action, body, note = null, cls = '') {
    return h(
      'div',
      { class: 'todo-group ' + cls },
      h('div', { class: 'todo-head' }, h('span', { class: 'todo-icon' }, icon(ic, { size: 15 })), h('span', { class: 'todo-title' }, title), h('span', { class: 'todo-count' }, String(count)), action ? h('span', { class: 'todo-action' }, action) : null),
      body,
      note ? h('p', { class: 'todo-note' }, note) : null,
    );
  }

  function failureRow(f) {
    const key = `retry:${f.lecture_id}:${f.stage}`;
    // the coded error (v2 P14.3), else the job's text
    const err = U.errorInfoText(f.error_info, f.error || '');
    return h(
      'li',
      { class: 'fail-row lgrid-row' },
      lref(f.course_code, f),
      h('a', { class: 'fail-title', href: `#/lecture/${enc(f.lecture_id)}`, title: `${f.course_name} · ${lectureLabel({ ...f, title: f.lecture_title })}` }, f.lecture_title),
      h('span', { class: 'fail-end' }, (f.allowed_actions || []).includes('retry') ? btn(t('common.retry'), { size: 'sm', icon: 'rotate-cw', disabled: busy.has(key), fkey: 'dash:' + key, onclick: () => retryFailure(f, key) }) : null),
      // the stage, course and time, and the error, under the title
      h(
        'div',
        { class: 'fail-sub' },
        h('div', { class: 'fail-meta' }, h('span', { class: 'tone-err' }, t('dashboard.failure.stage', { stage: U.stageName(f.stage) })), ' · ', f.course_name, f.finished_at ? ` · ${U.relTime(f.finished_at)}` : ''),
        err ? h('div', { class: 'fail-err', title: err }, U.excerpt(err.split('\n')[0], 120)) : null,
      ),
    );
  }

  async function run(key, fn) {
    if (busy.has(key)) return;
    busy.add(key);
    render();
    try {
      await fn();
    } catch {
      // shown by the API layer
    } finally {
      busy.delete(key);
      if (alive) load();
    }
  }

  function retryFailure(f, key) {
    run(key, () => stageAction({ id: f.lecture_id }, f.stage, 'retry', true));
  }

  // A3: launch the drafts of every course; one warning for all courses whose
  // glossary is still being extracted.
  function launchAllDrafts() {
    run('drafts', async () => {
      const pending = data.drafts.map((c) => ctx.courses().find((x) => x.id === c.course_id)).filter((c) => c && c.glossary_pending);
      if (pending.length) {
        const ok = await confirmDialog({
          title: t('dashboard.glossary_pending.title'),
          message: t('dashboard.glossary_pending.message', { courses: U.listJoin(pending.map((c) => c.code || c.name)) }),
          okText: t('dashboard.glossary_pending.ok'),
        });
        if (!ok) return;
      }
      let ok = 0;
      const errors = [];
      for (const c of data.drafts) {
        const res = await api.launchDrafts(c.course_id);
        for (const r of (res && res.results) || []) r.ok ? ok++ : errors.push(batchError(r));
      }
      toast(errors.length ? t('dashboard.launched_partial', { ok, n: errors.length, error: errors[0] }) : t('dashboard.launched', { n: ok }), errors.length ? 'warn' : 'success');
    });
  }

  // A2.3: update every course; the model choice is asked once for all of
  // them (courses without differences are updated right away).
  function updateAllStale() {
    run('stale', async () => {
      let ok = 0;
      const errors = [];
      const ask = [];
      const diff = [];
      const tally = (res) => {
        for (const r of (res && res.results) || []) r.ok ? ok++ : errors.push(batchError(r));
      };
      for (const c of data.stale) {
        try {
          tally(await api.updateStale(c.course_id));
        } catch (e) {
          if (!e.needsModelChoice) throw e;
          ask.push(c);
          diff.push(...(e.body.diff || []));
        }
      }
      if (ask.length) {
        const choice = await modelChoiceDialog(diff, 'update');
        if (choice) for (const c of ask) tally(await api.updateStale(c.course_id, choice));
      }
      if (ok || errors.length) toast(errors.length ? t('dashboard.updated_partial', { ok, n: errors.length, error: errors[0] }) : t('dashboard.updated', { n: ok }), errors.length ? 'warn' : 'success');
    });
  }

  // ---- recently finished ----

  function recentCard() {
    const list = data.recent_done;
    return card('card-recent', 'circle-check', t('dashboard.recent.title'), null, null, list.length ? h('div', { class: 'rd-list lgrid' }, list.map(recentRow)) : emptyLine('clock', t('dashboard.recent.empty')));
  }

  function recentRow(lec) {
    const exp = lec.stages.find((s) => s.stage === 'export');
    // three right-aligned columns (empty cells keep their place)
    const meta = [exp && exp.finished_at ? U.relTime(exp.finished_at) : '', lec.duration_ms ? U.fmtClock(lec.duration_ms) : '', costText(lec) || ''];
    const dl = lec.exports.length
      ? menuButton(
          h('button', { type: 'button', class: 'icon-btn', 'aria-label': t('course.lectures.download_label', { title: zh(lec.title) }), title: t('dashboard.recent.download'), dataset: { fkey: `dash:dl:${lec.id}` } }, icon('download', { size: 16 })),
          () => exportItems(lec.exports),
        )
      : null;
    const row = h(
      'div',
      { class: 'rd-row lgrid-row' },
      lref(lec.course_code, lec),
      h('a', { class: 'rd-title', href: `#/lecture/${enc(lec.id)}`, title: `${lec.course_name} · ${zh(lec.title)}` }, zh(lec.title)),
      h('span', { class: 'rd-badge' }, lec.counts.patches_pending > 0 ? h('a', { class: 'chip chip-review', href: `#/lecture/${enc(lec.id)}/review`, title: t('course.lectures.review_tip') }, t('course.lectures.review', { n: lec.counts.patches_pending })) : null),
      h(
        'span',
        { class: 'rd-meta' },
        meta.map((m, i) => h('span', { class: `rd-m rd-m${i}` }, m)),
      ),
      h(
        'span',
        { class: 'rd-end' },
        // SPEC-v2 P1: "play" in the recently finished list
        lec.playable ? h('a', { class: 'icon-btn', href: `#/play/${enc(lec.id)}`, 'aria-label': t('player.entry.play_named', { title: zh(lec.title) }), title: t('player.entry.play'), dataset: { fkey: `dash:play:${lec.id}` } }, icon('play', { size: 16 })) : null,
        dl,
      ),
    );
    bindContextMenu(row, () => lectureMenuItems(lec, menuHandlers));
    return row;
  }

  // The lecture's estimated cost, only when all priced stages used providers
  // of one currency (a sum across currencies would be meaningless).
  function costText(lec) {
    if (!lec.usage || lec.usage.estimated_cost == null) return null;
    const prov = (ctx.resources() && ctx.resources().providers) || [];
    const curs = new Set();
    for (const st of lec.stages) {
      if (!st.stats || st.stats.estimated_cost == null) continue;
      const p = st.model && prov.find((x) => x.id === st.model.provider_id);
      curs.add(p ? p.currency || '' : '?');
    }
    if (curs.size !== 1 || curs.has('?')) return null;
    return U.fmtMoney(lec.usage.estimated_cost, [...curs][0] || null);
  }

  // ---- today's usage ----

  let usageEl = null;
  let tariffKey = '';
  let waitKey = U.waitContextKey(ctx.resources()); // onResources

  // v2 P12.4: the provider's peak state from the resource snapshot
  function tariffOf(id) {
    const res = ctx.resources();
    const p = res && (res.providers || []).find((x) => x.id === id);
    return p && p.tariff_state && hasPeaks(p.tariff_state) ? p.tariff_state : null;
  }

  // The usage card follows the peak state and its countdown (SSE resources,
  // compared once a minute) without re-rendering the rest of the page.
  function tariffSig() {
    const res = ctx.resources();
    const states = ((res && res.providers) || []).map((p) => (p.tariff_state ? [p.id, p.tariff_state.peak, p.tariff_state.next_change, p.tariff_state.missing_holidays, p.tariff_state.today_peak] : null));
    return JSON.stringify([Math.floor(Date.now() / 60000), states]);
  }

  function refreshUsage() {
    const sig = tariffSig();
    if (sig === tariffKey) return;
    tariffKey = sig;
    if (!data || !usageEl || !usageEl.isConnected) return;
    preserveFocus(() => {
      const n = usageCard();
      usageEl.replaceWith(n);
      usageEl = n;
    });
  }

  function usageCard() {
    const list = data.usage_today;
    const totals = new Map();
    for (const u of list) if (u.cost != null) totals.set(u.currency || '', (totals.get(u.currency || '') || 0) + u.cost);
    const budget = settings && settings.general && settings.general.daily_budget > 0 ? settings.general.daily_budget : 0;
    const spent = [...totals.values()].reduce((a, b) => a + b, 0);
    return card(
      'card-usage',
      'wallet',
      t('dashboard.usage.title'),
      null,
      null,
      h(
        'div',
        { class: 'ucard' },
        h(
          'div',
          { class: 'usage-total' },
          totals.size
            ? [...totals.entries()].map(([cur, v]) => h('span', { class: 'usage-money' }, U.fmtMoney(v, cur || null)))
            : h('span', { class: 'usage-money is-quiet' }, list.some((u) => u.prompt_tokens) ? t('dashboard.usage.no_prices') : '0'),
          h('span', { class: 'usage-total-k' }, t('dashboard.usage.estimated')),
        ),
        budget
          ? h(
              'div',
              { class: 'usage-budget', title: t('dashboard.usage.budget_tip') },
              h(
                'div',
                { class: 'usage-budget-top' },
                h('span', null, t('dashboard.usage.budget', { amount: budget })),
                h('span', { class: spent >= budget ? 'tone-err' : 'dim' }, t('dashboard.usage.used', { pct: Math.min(999, Math.round((spent / budget) * 100)) })),
              ),
              h('div', { class: 'meter ' + (spent >= budget ? 'meter-warn' : 'meter-accent') }, h('span', { class: 'meter-fill', style: { width: `${Math.min(100, (spent / budget) * 100).toFixed(1)}%` } })),
            )
          : null,
        list.length ? h('div', { class: 'usage-list' }, list.map(usageRow)) : emptyLine('wallet', t('dashboard.usage.no_providers')),
      ),
    );
  }

  function usageRow(u) {
    const ts = tariffOf(u.provider_id);
    const unpriced = u.cost_unpriced_requests > 0 ? u.cost_unpriced_requests : 0;
    const unpricedTip = unpriced ? t('resources.unpriced_tip', { n: unpriced }) : '';
    if (!u.requests && !u.prompt_tokens && !u.completion_tokens) {
      const idle = h('div', { class: 'urow is-idle' }, h('span', { class: 'urow-name' }, u.name), h('span', { class: 'urow-idle' }, t('dashboard.usage.idle')));
      return ts ? h('div', { class: 'urow-wrap' }, idle, tariffUsageBlock(ts)) : idle;
    }
    return h(
      'div',
      { class: 'urow' },
      h(
        'div',
        { class: 'urow-top' },
        h('span', { class: 'urow-name' }, u.name),
        h(
          'span',
          { class: 'urow-cost' },
          // D3: without an estimate (cost null) only tokens are shown
          u.cost != null ? U.fmtMoney(u.cost, u.currency) : h('span', { class: 'dim', title: unpriced ? unpricedTip : undefined }, t('dashboard.usage.no_price')),
        ),
      ),
      // part of today's requests had no price: the amount is too low
      u.cost != null && unpriced ? h('div', { class: 'urow-tariff-gap', title: unpricedTip }, icon('info', { size: 12 }), t('resources.unpriced')) : null,
      h(
        'div',
        { class: 'urow-nums' },
        h('span', null, h('span', { class: 'dim' }, `${t('dashboard.usage.input')} `), U.fmtTokens(u.prompt_tokens)),
        h('span', null, h('span', { class: 'dim' }, `${t('dashboard.usage.output')} `), U.fmtTokens(u.completion_tokens)),
        h('span', null, h('span', { class: 'dim' }, `${t('dashboard.usage.requests')} `), String(u.requests)),
      ),
      h(
        'div',
        { class: 'urow-cache', title: t('dashboard.usage.cache_tip') },
        h('span', { class: 'dim' }, t('dashboard.usage.cache')),
        h('div', { class: 'meter meter-ok' }, h('span', { class: 'meter-fill', style: { width: `${((u.cache_hit_rate || 0) * 100).toFixed(1)}%` } })),
        h('span', { class: 'urow-rate' }, u.cache_hit_rate != null ? U.fmtPercent(u.cache_hit_rate) : '--'),
      ),
      ts ? tariffUsageBlock(ts) : null,
    );
  }

  // ---- course overview ----

  function coursesCard() {
    return card(
      'card-courses',
      'library',
      t('course.list.title'),
      data.courses.length,
      h('a', { class: 'card-link', href: '#/courses' }, t('dashboard.courses.all'), icon('chevron-right', { size: 14 })),
      h('div', { class: 'ccards' }, data.courses.map((c) => courseCard(c, { onChanged: load })), addCourseCard(ctx.newCourse)),
    );
  }

  // ---- empty state (no courses) ----

  function emptyState() {
    return h(
      'div',
      { class: 'onboard' },
      h('div', { class: 'onboard-icon' }, icon('graduation-cap', { size: 30 })),
      h('h1', { class: 'onboard-title' }, t('dashboard.onboard.title')),
      h('p', { class: 'onboard-text' }, t('dashboard.onboard.text')),
      btn(t('course.add'), { kind: 'primary', icon: 'plus', onclick: ctx.newCourse, fkey: 'dash:first' }),
      h(
        'ol',
        { class: 'onboard-steps' },
        [
          ['book-open', 'add'],
          ['upload', 'drop'],
          ['sparkles', 'auto'],
        ].map(([ic, k], i) =>
          h(
            'li',
            null,
            h('span', { class: 'onboard-step-icon' }, icon(ic, { size: 16 })),
            h('span', { class: 'onboard-step-t' }, `${i + 1}. ${t(`dashboard.onboard.${k}`)}`),
            h('span', { class: 'onboard-step-s' }, t(`dashboard.onboard.${k}_hint`)),
          ),
        ),
      ),
    );
  }

  // ---- live updates ----

  function onJob(ev) {
    if (ev.scope !== 'lecture') return;
    const ref = pipes.get(ev.scope_id);
    if (!ref) {
      schedule('load', load);
      return;
    }
    const st = ref.lec.stages.find((s) => s.stage === ev.stage);
    if (st && st.status === ev.status) {
      ref.bar.setProgress(ev.stage, ev.progress);
      const s = statusFor(ref.lec);
      ref.statusEl.textContent = s.text;
      ref.statusEl.title = s.text;
    } else {
      schedule('load', load);
    }
  }

  render(); // skeleton until the first response
  load();
  loadSettings();
  const tick = setInterval(() => alive && data && render(), 60000); // relative times
  const onKinds = () => alive && data && render(); // kind names (lecture kinds)
  window.addEventListener('kindschange', onKinds);

  return {
    refresh: load,
    onJob,
    onResources(r) {
      refreshUsage();
      if (data && !error && colabSlot.isConnected) fillColab();
      // the waiting texts name devices and follow the power policy: redraw
      // when that changes (the first snapshot may come after the dashboard)
      const k = U.waitContextKey(r);
      if (k !== waitKey) {
        waitKey = k;
        if (data && !error) render();
      }
    },
    onChanged(ev) {
      if (['lecture', 'course', 'patches', 'devices'].includes(ev.kind)) schedule('load', load); // devices: the I9 notice
      if (ev.kind === 'settings') loadSettings();
      updateHint.onChanged(ev);
      asrSlot.onChanged(ev);
    },
    destroy() {
      alive = false;
      updateHint.destroy();
      asrSlot.destroy();
      clearInterval(tick);
      window.removeEventListener('kindschange', onKinds);
      for (const tm of timers.values()) clearTimeout(tm);
    },
  };
}
