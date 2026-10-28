// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Chapters in the player (SPEC-v2 P7): generated on demand (never part of the
// pipeline). The button says chapters.generate until there are chapters, shows the
// generation while it runs, and lists the chapters afterwards; when the input
// changed since (Stale) it offers chapters.stale_regenerate and never
// regenerates by itself. Completion comes as SSE changed {kind: "chapters"}.

import { h, clear, toast } from './dom.js';
import { t } from './i18n.js';
import { api } from './api.js';
import { openMenu } from './menu.js';
import { pIcon } from './player_icons.js';
import { fmtPlayerTime, codeText } from './player_util.js';
import { lastStartingBefore } from './player_subs.js';
import { zh, zhLang } from './zh_display.js';

// opts: {lectureId, onSeek(ms), display() → 'both'|'zh'|'en'|'off', duration() → ms, onChange()}
export function createChapters(opts) {
  let view; // undefined = loading, else ChaptersView (never generated: no chapters, 200 since power management)
  let loadError = '';
  let starting = false;
  let seq = 0;
  const btn = h('button', { type: 'button', class: 'btn btn-sm pl-ch-btn', hidden: true, dataset: { fkey: 'pl:chapters' }, onclick: () => onClick() });

  function chapters() {
    return (view && view.chapters) || [];
  }

  // The Chinese title is shown in the display script (SPEC-v2 P14.6).
  function title(c) {
    return opts.display() === 'en' ? c.title_en || zh(c.title_zh) : zh(c.title_zh) || c.title_en;
  }

  // The language of title(c): chapter titles are lecture content (lang for
  // the fonts, SPEC-v2 P14.5; zh-CN, or zh-TW / zh-HK once converted).
  function titleLang(c) {
    const en = opts.display() === 'en' ? !!c.title_en : !c.title_zh;
    return en ? 'en' : zhLang();
  }

  function running() {
    return starting || !!(view && view.running);
  }

  function render() {
    clear(btn);
    btn.hidden = view === undefined && !loadError;
    btn.disabled = running();
    btn.classList.toggle('is-stale', !!(view && view.stale && chapters().length));
    btn.removeAttribute('aria-haspopup');
    let tip;
    if (loadError) {
      btn.append(pIcon('list', { size: 14 }), h('span', { class: 'btn-label' }, t('chapters.button')));
      btn.disabled = true;
      tip = t('chapters.load_failed', { error: loadError });
    } else if (running()) {
      btn.append(pIcon('loader-circle', { size: 14, class: 'spin' }), h('span', { class: 'btn-label' }, t('chapters.generating')));
      tip = t('chapters.generating_hint');
    } else if (!chapters().length) {
      btn.append(pIcon('sparkles', { size: 14 }), h('span', { class: 'btn-label' }, t('chapters.generate')));
      if (view && view.error) btn.append(h('span', { class: 'pl-dot is-err', 'aria-hidden': 'true' }));
      tip = view && view.error ? t('chapters.failed_hint', { error: codeText(view.error) }) : t('chapters.generate_hint');
    } else {
      btn.setAttribute('aria-haspopup', 'menu');
      btn.append(pIcon('list', { size: 14 }), h('span', { class: 'btn-label' }, t('chapters.button')), h('span', { class: 'btn-count' }, String(chapters().length)));
      if (view.stale) btn.append(h('span', { class: 'pl-ch-stale' }, t('chapters.stale_short')));
      tip = view.stale ? t('chapters.stale_regenerate') : view.model ? t('chapters.made_by', { model: view.model }) : '';
    }
    btn.title = tip || '';
  }

  async function load() {
    const my = ++seq;
    try {
      const v = await api.chapters(opts.lectureId, { quiet: true });
      if (my !== seq) return;
      const wasRunning = running();
      view = v;
      loadError = '';
      starting = false;
      if (wasRunning && !v.running) announce(v);
    } catch (e) {
      if (my !== seq) return;
      if (e.status !== 0) loadError = e.message;
    }
    render();
    opts.onChange();
  }

  function announce(v) {
    if (v.error) toast(t('chapters.failed_toast', { error: codeText(v.error) }), 'warn');
    else if (v.chapters && v.chapters.length) toast(t('chapters.done_toast', { n: v.chapters.length }), 'success');
  }

  async function generate() {
    if (running()) return;
    starting = true;
    render();
    try {
      const v = await api.generateChapters(opts.lectureId);
      if (v) view = v;
      starting = false;
      if (v && !v.running) announce(v);
    } catch {
      starting = false; // shown by the API layer (e.g. the chapters role is not set up)
    }
    render();
    opts.onChange();
  }

  function menuItems() {
    const list = chapters();
    const cur = currentIndex(opts.time ? opts.time() : 0);
    const items = [];
    if (view.stale) items.push({ label: t('chapters.stale_regenerate'), icon: 'refresh-cw', onSelect: generate }, 'sep');
    else if (view.error) items.push({ label: t('chapters.retry_after_failure'), icon: 'refresh-cw', hint: t('chapters.last_failed'), onSelect: generate }, 'sep');
    list.forEach((c, i) => items.push({ label: title(c), lang: titleLang(c), icon: i === cur ? 'play' : null, hint: fmtPlayerTime(c.start_ms, opts.duration()), onSelect: () => opts.onSeek(c.start_ms) }));
    if (!view.stale && !view.error) items.push('sep', { label: t('chapters.regenerate'), icon: 'refresh-cw', onSelect: generate });
    return items;
  }

  function onClick() {
    if (running()) return;
    if (!chapters().length) {
      generate();
      return;
    }
    openMenu(menuItems(), { anchor: btn }, { returnFocus: btn, align: 'right' });
  }

  function currentIndex(ms) {
    return lastStartingBefore(chapters(), ms);
  }

  // ext web3: the chapter list from elsewhere (the current chapter's name)
  function openList(anchor) {
    if (running() || !chapters().length) return;
    openMenu(menuItems(), { anchor }, { returnFocus: anchor });
  }

  render();
  load();

  return {
    btn,
    load,
    chapters,
    title,
    titleLang,
    currentIndex,
    running,
    generate,
    openList,
    get stale() {
      return !!(view && view.stale);
    },
    rerender: render,
  };
}

// The chapter list of the audio-mode panel (P8.1: the video area shows the title and the chapters).
export function chapterList(ch, { current, onSeek, duration }) {
  const list = ch.chapters();
  if (!list.length) return null;
  return h(
    'ol',
    { class: 'pl-audio-chapters' },
    list.map((c, i) =>
      h(
        'li',
        null,
        h(
          'button',
          { type: 'button', class: 'pl-audio-ch' + (i === current ? ' is-cur' : ''), 'aria-current': i === current ? 'true' : null, onclick: () => onSeek(c.start_ms) },
          h('span', { class: 'pl-audio-ch-time' }, fmtPlayerTime(c.start_ms, duration)),
          h('span', { class: 'pl-audio-ch-title', lang: ch.titleLang(c) }, ch.title(c)),
        ),
      ),
    ),
  );
}
