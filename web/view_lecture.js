// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// #/lecture/{id}[/review]: the lecture header — title, L number, course,
// pipeline (pipeline.js), the stale notice with "Update", lecture actions from
// lecture_actions (and the same context menu as a lecture row, A12.2),
// counts, usage and prefix-cache hit rate (§17.7), export downloads incl.
// cite.json (A11) — and the two tabs, transcript (§9.6) and review (§9.7).
// lecture kinds: the eyebrow says "CSC1003 · Week 1 · Lecture · No. 2" (the
// citation number on hover), and the slides this lecture uses are listed
// with where they come from (its week, itself, the whole course).

import { api } from './api.js';
import { h, clear, btn, iconBtn, preserveFocus, toast } from './dom.js';
import { icon } from './icons.js';
import { pipelineBar } from './pipeline.js';
import { menuButton, bindContextMenu } from './menu.js';
import { downloadMenu } from './widgets.js';
import * as A from './actions.js';
import * as Clip from './audiobar.js';
import { mountTranscript } from './view_transcript.js';
import { mountReview } from './view_review.js';
import * as U from './util.js';
import { t } from './i18n.js';
import { lectureTitles, fmtRecordedAt, openRecordedAtDialog } from './calendar_ui.js';
import { lectureTagText } from './kinds.js';
import { openKindDialog } from './kind_dialog.js';

const enc = encodeURIComponent;

// A stage's error in a notice: the translated text (error_info), with the
// server's own text as the hover detail when the two differ (v2 P14.3).
function errorSpan(st, text) {
  const detail = st.error && st.error !== text ? st.error : text;
  return h('span', { class: 'lec-notice-err', title: U.excerpt(detail, 300) }, U.excerpt(text, 300));
}

export function mountLecture(root, route, ctx) {
  const id = route.id;
  let tab = route.tab;
  let lecture = null;
  let loadError = '';
  let gone = false; // the lecture was deleted (404)
  let goneCourse = ''; // its course, for the "back to course" link
  let alive = true;
  let sub = null;
  let bar = null;
  let timer = null;
  let shownCost = null;

  const el = h('div', { class: 'page page-lecture' });
  const headEl = h('div', { class: 'lec-head' });
  const tabsEl = h('div', { class: 'lec-tabs', role: 'tablist', 'aria-label': t('lecture.tab.aria') });
  const panelEl = h('div', { class: 'lec-tabpanel', role: 'tabpanel' });
  el.append(headEl, tabsEl, panelEl);
  root.append(el);
  // Right click / Shift+F10 on the header: the lecture row's menu (A12.2).
  bindContextMenu(headEl, () => (lecture ? A.lectureMenuItems(lecture, handlers).filter((it) => !(it && it.id === 'open')) : null));

  const handlers = {
    onLectures(ls) {
      const l = ls.find((x) => x.id === id);
      if (l && alive) {
        lecture = l;
        render();
      }
    },
    onDeleted() {
      location.hash = lecture ? `#/course/${enc(lecture.course_id)}` : '#/';
    },
  };

  // Quiet: the page shows load errors itself. A 404 means the lecture was
  // deleted (e.g. from another tab while this page was open): show the
  // not-found state instead of a stale header and an error toast. Other
  // errors on a background reload keep the last data and are toasted
  // (connection failures are covered by the SSE reconnect banner).
  async function load() {
    try {
      const l = await api.lecture(id, { quiet: true });
      if (!alive) return;
      lecture = l;
      loadError = '';
      gone = false;
    } catch (e) {
      if (!alive) return;
      if (e && e.status === 404) {
        gone = true;
        lecture = null;
        loadError = e.message || t('errors.lecture.not_found');
        if (sub) {
          sub.destroy();
          sub = null;
        }
        bar = null;
      } else if (!lecture) {
        loadError = (e && e.message) || t('common.load_failed');
      } else if (e && e.status > 0) {
        toast(e.message, 'error');
      }
    }
    render();
  }

  function retry() {
    loadError = '';
    render();
    load();
  }

  function render() {
    if (!lecture) {
      clear(headEl);
      clear(tabsEl);
      clear(panelEl);
      if (loadError) {
        const back = goneCourse ? h('a', { class: 'btn btn-primary', href: `#/course/${enc(goneCourse)}` }, t('lecture.page.back_to_course')) : null;
        headEl.append(
          h(
            'div',
            { class: 'empty-state' },
            h('div', { class: 'empty-icon' }, icon(gone ? 'search' : 'circle-alert', { size: 26 })),
            h('h1', { text: gone ? t('player.not_found') : t('lecture.page.load_failed') }),
            h('p', { text: loadError }),
            h(
              'div',
              { class: 'empty-actions' },
              gone ? back : btn(t('lecture.page.reload'), { kind: 'primary', icon: 'rotate-cw', onclick: retry }),
              h('a', { class: 'btn', href: '#/' }, t('player.to_dashboard')),
            ),
          ),
        );
      } else {
        headEl.append(skeletonHead());
      }
      return;
    }
    ctx.setCurrentCourse(lecture.course_id);
    goneCourse = lecture.course_id;
    renderHead();
    renderTabs();
    if (!sub) mountTab();
    else sub.setLecture(lecture);
  }

  function skeletonHead() {
    return h(
      'div',
      { class: 'lec-skel', 'aria-hidden': 'true' },
      h('div', { class: 'skel', style: { width: '160px', margin: '0 0 14px' } }),
      h('div', { class: 'skel skel-eyebrow' }),
      h('div', { class: 'skel skel-h1', style: { width: '420px' } }),
      h('div', { class: 'skel', style: { width: '260px', margin: '6px 0 18px' } }),
      h('div', { class: 'card lec-panel' }, h('div', { class: 'card-bd' }, h('div', { class: 'skel', style: { height: '10px', margin: '6px 0 22px' } }), h('div', { class: 'skel', style: { width: '70%' } }))),
    );
  }

  // ---- header ----

  const run = (p) =>
    p.then(
      (r) => r && handlers.onLectures(Array.isArray(r) ? r : [r]),
      () => {},
    );

  function headActions(lec) {
    const acts = lec.lecture_actions || [];
    const out = [];
    // SPEC-v2 P1: Play once asr is done (primary when everything is done)
    if (lec.playable) out.push(btn(t('player.entry.play'), { kind: U.isAllDone(lec.stages) && !acts.includes('update') ? 'primary' : '', icon: 'play', fkey: `lecd:${lec.id}:play`, title: t('player.entry.play_hint'), onclick: () => (location.hash = `#/play/${encodeURIComponent(lec.id)}`) }));
    if (acts.includes('launch')) out.push(btn(t('lecture.page.launch'), { kind: 'primary', icon: 'play', fkey: `lecd:${lec.id}:launch`, onclick: () => run(A.launchLectures([lec])) }));
    // "Update" sits in the stale notice when there is one.
    if (acts.includes('update') && !U.lectureStaleText(lec)) out.push(btn(t('lecture.page.update'), { kind: 'warn', icon: 'refresh-cw', fkey: `lecd:${lec.id}:update`, onclick: () => run(A.updateLectures([lec])) }));
    if (acts.includes('cancel')) out.push(btn(t('lecture.page.cancel'), { kind: 'danger', icon: 'ban', fkey: `lecd:${lec.id}:cancel`, onclick: () => run(A.cancelLectures([lec])) }));
    if (lec.exports.length) {
      const dl = downloadMenu(lec.exports, `lecd:${lec.id}:dl`);
      if (!U.isAllDone(lec.stages)) dl.title = t('lecture.page.exports_outdated');
      out.push(dl);
    }
    out.push(
      menuButton(iconBtn('ellipsis', t('lecture.page.menu'), { fkey: `lecd:${lec.id}:menu`, menu: true, class: 'icon-btn-bordered' }), () =>
        A.lectureMenuItems(lec, handlers).filter((it) => !(it && it.id === 'open')),
      ),
    );
    return out;
  }

  // The lecture's estimated cost, only when every priced stage used providers
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
    if (curs.size > 1 || curs.has('?')) return U.fmtCost(lec.usage.estimated_cost);
    return U.fmtMoney(lec.usage.estimated_cost, [...curs][0] || null);
  }

  function stat(label, value, sub, opts = {}) {
    return h(
      opts.href ? 'a' : 'div',
      { class: 'lec-stat' + (opts.cls ? ' ' + opts.cls : ''), href: opts.href, title: opts.title, dataset: opts.href ? { fkey: `lecd:stat:${label}` } : undefined },
      h('span', { class: 'lec-stat-k' }, opts.icon ? icon(opts.icon, { size: 13 }) : null, label),
      h('span', { class: 'lec-stat-v' }, value),
      sub ? h('span', { class: 'lec-stat-sub' }, sub) : null,
    );
  }

  function stats(lec) {
    const c = lec.counts;
    const u = lec.usage;
    const rate = U.cacheHitRate(u);
    const used = U.hasUsage(u);
    const cost = costText(lec);
    shownCost = cost;
    const zhSub = [];
    if (c.zh_qa_failed) zhSub.push(h('span', { class: 'tone-err' }, t('lecture.stat.qa_failed', { n: U.count(c.zh_qa_failed) })));
    if (c.zh_stale) zhSub.push(h('span', { class: 'tone-warn' }, t('lecture.stat.retranslating', { n: U.count(c.zh_stale) })));
    return h(
      'div',
      { class: 'lec-stats' },
      stat(t('lecture.stat.sentences'), c.sentences ? U.fmtInt(c.sentences) : '–', lec.duration_ms ? t('lecture.page.duration', { time: U.fmtClock(lec.duration_ms) }) : null, { icon: 'audio-lines' }),
      stat(t('lecture.stat.zh_ok'), c.sentences ? U.fmtInt(c.zh_ok) : '–', zhSub.length ? zhSub.reduce((acc, n, i) => (i ? [...acc, ' · ', n] : [n]), []) : c.sentences ? t('lecture.stat.total', { n: U.count(c.sentences) }) : null, { icon: 'languages' }),
      stat(t('lecture.stat.pending'), U.fmtInt(c.patches_pending), c.patches_pending ? t('lecture.stat.pending_sub') : t('lecture.stat.pending_none'), {
        icon: 'list-checks',
        href: c.patches_pending ? `#/lecture/${enc(lec.id)}/review` : null,
        cls: c.patches_pending ? 'is-warn' : '',
        title: c.patches_pending ? t('lecture.stat.open_review') : null,
      }),
      stat(t('lecture.stat.tokens'), used ? `${U.fmtTokens(u.prompt_tokens)} / ${U.fmtTokens(u.completion_tokens)}` : '–', used ? t('lecture.stat.tokens_sub') : t('lecture.stat.tokens_none'), { icon: 'activity' }),
      h(
        'div',
        { class: 'lec-stat', title: t('lecture.stat.cache_tip') },
        h('span', { class: 'lec-stat-k' }, icon('gauge', { size: 13 }), t('lecture.stat.cache')),
        h('span', { class: 'lec-stat-v' }, rate != null ? U.fmtPercent(rate) : '–'),
        rate != null ? h('span', { class: 'meter meter-ok lec-stat-meter' }, h('span', { class: 'meter-fill', style: { width: `${Math.round(rate * 100)}%` } })) : h('span', { class: 'lec-stat-sub' }, t('lecture.stat.cache_none')),
      ),
      stat(t('lecture.stat.cost'), cost || '–', cost ? t('lecture.stat.cost_sub') : used ? t('lecture.stat.cost_none') : null, { icon: 'coins' }),
    );
  }

  // The slide files this lecture uses and why (lecture kinds: slides by week).
  function slides(lec) {
    const list = lec.materials || [];
    if (!list.length) return null;
    const via = (m) => (m.via === 'week' ? t('lecture.slides.via_week', { week: m.week }) : m.via === 'lecture' ? t('lecture.slides.via_lecture') : t('lecture.slides.via_course'));
    return h(
      'div',
      { class: 'lec-slides' },
      h('span', { class: 'lec-slides-k', title: t('lecture.slides.tip') }, icon('layers', { size: 13 }), t('lecture.slides.title')),
      h(
        'ul',
        { class: 'lec-slides-list' },
        list.map((m) => h('li', { class: 'lec-slide', title: `${m.filename} · ${via(m)}` }, icon('file-text', { size: 13, class: 'dim' }), h('span', { class: 'lec-slide-name' }, m.filename), h('span', { class: 'lec-slide-via' }, via(m)))),
      ),
    );
  }

  function notices(lec) {
    const acts = lec.lecture_actions || [];
    const out = [];
    if (lec.draft) {
      out.push(
        h(
          'div',
          { class: 'lec-notice is-draft' },
          icon('info', { size: 16 }),
          h('span', { class: 'lec-notice-text' }, t('lecture.notice.draft')),
          acts.includes('launch') ? btn(t('lecture.page.launch'), { kind: 'primary', icon: 'play', size: 'sm', fkey: `lecd:${lec.id}:launch2`, onclick: () => run(A.launchLectures([lec])) }) : null,
        ),
      );
    }
    // A failure (or cancellation) says what happened and offers the stage's
    // own next step (its allowed_actions), not only in the hover card (§9.8).
    for (const st of lec.stages) {
      const failed = U.isFinalFailure(st);
      if (!failed && st.status !== 'cancelled') continue;
      const canRetry = (st.allowed_actions || []).includes('retry');
      const err = failed ? U.stageErrorText(st) : '';
      out.push(
        h(
          'div',
          { class: 'lec-notice ' + (failed ? 'is-failed' : 'is-cancelled'), role: failed ? 'alert' : 'status' },
          icon(failed ? 'circle-alert' : 'ban', { size: 16 }),
          h(
            'span',
            { class: 'lec-notice-text' },
            h('b', null, t(failed ? 'lecture.notice.failed' : 'lecture.notice.cancelled', { stage: U.stageName(st.stage) })),
            err ? errorSpan(st, err) : failed ? null : t('lecture.notice.cancelled_hint'),
          ),
          canRetry ? btn(t('common.retry'), { kind: failed ? 'primary' : '', icon: 'rotate-cw', size: 'sm', fkey: `lecd:${lec.id}:retry:${st.stage}`, onclick: () => run(A.stageAction(lec, st.stage, 'retry', true)) }) : null,
        ),
      );
    }
    // v2 P13.5: a failed title task is shown here only, with its retry
    const ta = (lec.aux || []).find((a) => a.stage === 'title' && a.status === 'failed');
    if (ta) {
      const taErr = U.stageErrorText(ta);
      out.push(
        h(
          'div',
          { class: 'lec-notice is-cancelled', role: 'status' },
          icon('info', { size: 16 }),
          h('span', { class: 'lec-notice-text' }, h('b', null, t('lecture.detail.title_failed')), taErr ? errorSpan(ta, taErr) : null),
          (ta.allowed_actions || []).includes('retry') ? btn(t('common.retry'), { icon: 'rotate-cw', size: 'sm', fkey: `lecd:${lec.id}:retry:title`, onclick: () => run(api.lectureStageAction(lec.id, 'title', 'retry')) }) : null,
        ),
      );
    }
    const staleText = U.lectureStaleText(lec);
    if (staleText) {
      out.push(
        h(
          'div',
          { class: 'lec-notice is-stale', role: 'status' },
          icon('refresh-cw', { size: 16 }),
          h('span', { class: 'lec-notice-text' }, staleText),
          acts.includes('update') ? btn(t('lecture.page.update'), { kind: 'warn', icon: 'refresh-cw', size: 'sm', fkey: `lecd:${lec.id}:update`, onclick: () => run(A.updateLectures([lec])) }) : null,
        ),
      );
    }
    return out;
  }

  function renderHead() {
    const lec = lecture;
    const course = ctx.courses().find((c) => c.id === lec.course_id);
    preserveFocus(() => {
      clear(headEl);
      bar = pipelineBar(lec.stages, {
        key: `lecd:${lec.id}`,
        size: 'lg',
        sys: ctx.system,
        res: ctx.resources,
        onAction: (stage, action) => A.stageAction(lec, stage, action, true).then((r) => r && handlers.onLectures([r])),
      });
      const staleText = U.lectureStaleText(lec);
      const sum = U.lectureSummary(lec.stages, { sys: ctx.system(), res: ctx.resources(), staleText, draft: lec.draft });
      // the full stale text is in the notice below, with its "Update" button
      if (sum.kind === 'stale' && staleText) sum.text = t('lecture.page.needs_update');
      const warn = U.warningHint(lec.stages);
      const tt = lectureTitles(lec);
      const sub = [t('lecture.page.created', { when: U.fmtDateTime(lec.created_at) })];
      if (lec.launched_at) sub.push(t('lecture.page.launched', { when: U.fmtDateTime(lec.launched_at) }));
      if (lec.duration_ms) sub.push(t('lecture.page.duration', { time: U.fmtClock(lec.duration_ms) }));
      headEl.append(
        h(
          'nav',
          { class: 'crumbs', 'aria-label': t('lecture.page.crumbs_aria') },
          h('a', { href: `#/course/${enc(lec.course_id)}`, dataset: { fkey: 'lecd:crumb' } }, icon('arrow-left', { size: 14 }), course ? h('span', null, course.code ? h('span', { class: 'mono crumbs-code' }, course.code) : null, course.name) : t('lecture.page.back_to_course')),
        ),
        h(
          'div',
          { class: 'page-head lec-titlebar' },
          h(
            'div',
            { class: 'page-titles' },
            h(
              'div',
              { class: 'page-eyebrow' },
              course && course.code ? h('span', { class: 'mono' }, course.code) : null,
              h(
                'button',
                {
                  type: 'button',
                  class: 'lec-kind',
                  title: t('lecture.page.kind_tip', { ref: U.lectureRef(course && course.code, lec.cite_label || lec.label) }),
                  dataset: { fkey: `lecd:${lec.id}:kind` },
                  onclick: () => openKindDialog(lec, { onSaved: (l) => handlers.onLectures([l]) }),
                },
                lectureTagText(lec, { form: 'full' }),
              ),
              lec.draft ? h('span', { class: 'chip chip-draft' }, t('lecture.page.draft')) : null,
              U.isAllDone(lec.stages) ? h('span', { class: 'chip chip-ok' }, icon('check', { size: 11 }), t('lecture.page.done')) : null,
            ),
            h('h1', { class: 'page-title', lang: tt.mainLang }, tt.main),
            // content languages (fonts, P14.5): the subtitle is the Chinese title
            tt.sub ? h('p', { class: 'lec-subtitle', lang: tt.subLang }, tt.sub) : null,
            tt.blurb ? h('p', { class: 'lec-blurb', title: tt.blurbFull, lang: tt.blurbLang }, tt.blurb) : null,
            h(
              'p',
              { class: 'page-sub' },
              sub.join(' · '),
              ' · ',
              // v2 P13.3.4: the recording time and where it came from (click to change)
              h(
                'button',
                { type: 'button', class: 'page-sub-link' + (lec.recorded_at ? '' : ' is-missing'), title: t('lecture.detail.recorded_edit'), dataset: { fkey: `lecd:${lec.id}:rec` }, onclick: () => openRecordedAtDialog(lec, { onSaved: (l) => handlers.onLectures([l]) }) },
                icon('calendar', { size: 13 }),
                lec.recorded_at ? t('lecture.detail.recorded', { when: fmtRecordedAt(lec), source: t(`calendar.recorded.source.${lec.recorded_at_source || 'none'}`) }) : t('lecture.detail.recorded_none'),
              ),
              lec.original_filename ? h('span', { class: 'lec-orig', title: lec.original_filename }, ' · ', t('lecture.detail.original', { name: lec.original_filename })) : null,
            ),
          ),
          h('div', { class: 'page-actions' }, headActions(lec)),
        ),
        h(
          'section',
          { class: 'card lec-panel', 'aria-label': t('lecture.page.panel_aria') },
          h(
            'div',
            { class: 'lec-pipe-row' },
            bar,
            h(
              'div',
              { class: 'lec-state' },
              h('span', { class: `lec-status tone-${sum.tone}` }, h('span', { class: `lec-status-dot is-${sum.tone}`, 'aria-hidden': 'true' }), sum.text),
              warn ? h('span', { class: 'chip chip-warn', title: t('lecture.page.warning_tip') }, icon('triangle-alert', { size: 11 }), warn) : null,
            ),
          ),
          notices(lec),
          slides(lec),
          stats(lec),
        ),
      );
    });
  }

  function renderTabs() {
    const n = lecture.counts.patches_pending;
    const sentences = lecture.counts.sentences;
    const mk = (key, label, ic, href, count, countCls) =>
      h(
        'a',
        { class: 'lec-tab' + (tab === key ? ' is-active' : ''), role: 'tab', 'aria-selected': tab === key ? 'true' : 'false', href, dataset: { fkey: `tab:${key}` } },
        icon(ic, { size: 15 }),
        label,
        count ? h('span', { class: `lec-tab-count ${countCls}` }, String(count)) : null,
      );
    preserveFocus(() => {
      clear(tabsEl);
      tabsEl.append(mk('transcript', t('lecture.tab.transcript'), 'file-text', `#/lecture/${enc(id)}`, sentences, ''), mk('review', t('lecture.tab.review'), 'list-checks', `#/lecture/${enc(id)}/review`, n, 'is-warn'));
    });
  }

  function mountTab() {
    if (sub) sub.destroy();
    Clip.stop();
    clear(panelEl);
    sub = tab === 'review' ? mountReview(panelEl, lecture, ctx) : mountTranscript(panelEl, lecture, ctx);
  }

  function scheduleReload() {
    clearTimeout(timer);
    timer = setTimeout(load, 250);
  }

  render(); // skeleton until the first response
  load();
  const onKinds = () => alive && lecture && renderHead(); // kind names (lecture kinds)
  window.addEventListener('kindschange', onKinds);

  return {
    update(r) {
      if (r.name !== 'lecture' || r.id !== id) return false;
      if (r.tab !== tab) {
        tab = r.tab;
        if (lecture) {
          renderTabs();
          mountTab();
        }
      }
      return true;
    },
    refresh() {
      // (after the reload: a lecture deleted meanwhile shows "not found"
      // without the tab also failing on its sentences/patches)
      load().then(() => {
        if (alive && sub && !gone) sub.refresh();
      });
    },
    onJob(ev) {
      if (!lecture || ev.scope !== 'lecture' || ev.scope_id !== id) return;
      const st = lecture.stages.find((s) => s.stage === ev.stage);
      if (st && st.status === ev.status && bar) {
        bar.setProgress(ev.stage, ev.progress);
      } else {
        scheduleReload();
      }
    },
    onChanged(ev) {
      if (ev.kind === 'lecture' && ev.id === id && !gone) scheduleReload();
      if (sub) sub.onChanged(ev);
    },
    onCoursesChanged() {
      if (lecture) renderHead();
    },
    onResources() {
      // The cost's currency comes from the providers (resources); redraw the
      // header only when that text changes (it arrives every 2 s).
      if (lecture && costText(lecture) !== shownCost) renderHead();
    },
    destroy() {
      alive = false;
      window.removeEventListener('kindschange', onKinds);
      clearTimeout(timer);
      if (sub) sub.destroy();
      Clip.release(id);
    },
  };
}
