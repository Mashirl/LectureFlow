// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// The player's transcript stream (SPEC-v2 P5): one entry per sentence
// (time, English = text, Chinese), the current sentence highlighted and
// kept in the middle; manual scrolling pauses that for 5 seconds and shows
// player.side.back_to_current; a click jumps to the sentence (the play state does not
// change); search in both languages with Enter / Shift+Enter moving between
// results (scrolling only); marks for modified sentences (thin line) and
// pending review changes (amber, hover card with before / after / reason);
// the sentence context menu.

import { h, clear } from './dom.js';
import { t } from './i18n.js';
import { pIcon } from './player_icons.js';
import { bindContextMenu } from './menu.js';
import { diffView } from './widgets.js';
import { fmtPlayerTime } from './player_util.js';
import { zh as zhText, zhLang, zhQuery, markNeedles } from './zh_display.js';

const MANUAL_PAUSE_MS = 5000; // P5

export function isModified(s) {
  return s.human_text != null || (s.asr_text != null && s.text !== s.asr_text);
}

export function isPending(s) {
  return !!(s.patch && s.patch.review === 'pending');
}

// opts: {onSeek(sentence), menuItems(sentence) → items, duration() → ms}
export function createTranscript(opts) {
  let sentences = [];
  let rows = [];
  let current = -1;
  let display = 'both';
  let order = 'en';
  let manualUntil = 0;
  let searchHold = false; // the user moved through search results: no auto-scroll until cleared
  let ours = false; // the list is being scrolled by us (auto-scroll), not by the user
  let oursTimer = 0;
  let query = '';
  let zhq = zhQuery(''); // the query over the Chinese (P14.6: Traditional terms find the stored Simplified)
  let hits = [];
  let hitPos = -1;
  let focusIdx = 0;
  let resumeTimer = 0;
  let searchTimer = 0;

  const input = h('input', {
    type: 'search',
    class: 'input pl-search-input',
    placeholder: t('player.side.search_placeholder'),
    'aria-label': t('player.side.search_label'),
    spellcheck: 'false',
    autocomplete: 'off',
  });
  const count = h('span', { class: 'pl-search-count', 'aria-live': 'polite' });
  const prevBtn = h('button', { type: 'button', class: 'icon-btn pl-search-nav', 'aria-label': t('player.side.search_prev'), title: t('player.side.search_prev'), onclick: () => moveHit(-1) }, pIcon('chevron-down', { size: 15, class: 'is-up' }));
  const nextBtn = h('button', { type: 'button', class: 'icon-btn pl-search-nav', 'aria-label': t('player.side.search_next'), title: t('player.side.search_next'), onclick: () => moveHit(1) }, pIcon('chevron-down', { size: 15 }));
  const search = h('div', { class: 'pl-search' }, pIcon('search', { size: 15, class: 'pl-search-icon' }), input, count, prevBtn, nextBtn);
  const list = h('div', { class: 'pl-list', role: 'list', 'aria-label': t('player.side.title'), tabindex: '-1' });
  const backBtn = h('button', { type: 'button', class: 'btn btn-sm pl-back-current', hidden: true, onclick: () => backToCurrent() }, pIcon('locate', { size: 14 }), h('span', { class: 'btn-label' }, t('player.side.back_to_current')));
  const card = h('div', { class: 'pl-hovercard', role: 'tooltip', hidden: true });
  const el = h('div', { class: 'pl-transcript', dataset: { display, order } }, search, list, backBtn, card);

  // ---- rows ----

  function textNodes(text, q) {
    if (!q || !text) return [text || ''];
    const lower = text.toLowerCase();
    const out = [];
    let i = 0;
    for (;;) {
      const j = lower.indexOf(q, i);
      if (j < 0) break;
      if (j > i) out.push(text.slice(i, j));
      out.push(h('mark', null, text.slice(j, j + q.length)));
      i = j + q.length;
    }
    out.push(text.slice(i));
    return out;
  }

  function fillText(row, s) {
    const en = row.querySelector('.pl-row-en');
    const zh = row.querySelector('.pl-row-zh');
    clear(en).append(...textNodes(s.text, query));
    clear(zh);
    if (s.zh) zh.append(...markNeedles(zhText(s.zh), query ? zhq.needles : [], (x) => h('mark', null, x)));
    else zh.append(h('span', { class: 'pl-row-none' }, t('player.side.untranslated')));
    row.classList.toggle('no-zh', !s.zh);
    row.classList.toggle('is-stale-zh', s.zh_status === 'stale');
  }

  function renderRow(s, i) {
    const pending = isPending(s);
    const row = h(
      'div',
      {
        class: 'pl-row' + (isModified(s) ? ' is-mod' : '') + (pending ? ' is-pending' : ''),
        role: 'listitem',
        tabindex: i === focusIdx ? '0' : '-1',
        dataset: { idx: String(i) },
      },
      h('span', { class: 'pl-row-time' }, fmtPlayerTime(s.start_ms, opts.duration())),
      h('div', { class: 'pl-row-text' }, h('p', { class: 'pl-row-en', lang: 'en' }), h('p', { class: 'pl-row-zh', lang: zhLang() })),
      pending
        ? h(
            'button',
            { type: 'button', class: 'pl-row-pend', tabindex: '-1', 'aria-label': t('player.side.pending_mark'), dataset: { idx: String(i) } },
            h('span', { class: 'pl-pend-glyph', 'aria-hidden': 'true' }),
          )
        : null,
    );
    fillText(row, s);
    return row;
  }

  function render() {
    hideCard();
    clear(list);
    rows = sentences.map(renderRow);
    if (!rows.length) list.append(h('p', { class: 'pl-list-empty' }, t('player.side.empty')));
    else list.append(...rows);
    if (current >= 0 && rows[current]) rows[current].classList.add('is-cur');
    runSearch(false);
  }

  // ---- current sentence and auto-scroll (P5) ----

  function inView(row) {
    const a = list.getBoundingClientRect();
    const b = row.getBoundingClientRect();
    return b.bottom > a.top + 4 && b.top < a.bottom - 4 && a.height > 0;
  }

  function scrollToRow(row, smooth) {
    if (!row || !list.clientHeight) return;
    const top = row.offsetTop - list.clientHeight / 2 + row.offsetHeight / 2;
    ours = true;
    clearTimeout(oursTimer);
    oursTimer = setTimeout(() => (ours = false), smooth ? 1500 : 250);
    const reduced = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    list.scrollTo({ top: Math.max(0, top), behavior: smooth && !reduced ? 'smooth' : 'auto' });
  }

  function autoScrollAllowed() {
    return performance.now() >= manualUntil && !searchHold;
  }

  function syncBackButton() {
    const row = rows[current];
    backBtn.hidden = !row || !list.clientHeight || inView(row);
  }

  function markManual() {
    manualUntil = performance.now() + MANUAL_PAUSE_MS;
    clearTimeout(resumeTimer);
    resumeTimer = setTimeout(syncBackButton, MANUAL_PAUSE_MS + 50);
    requestAnimationFrame(syncBackButton);
  }

  function backToCurrent() {
    manualUntil = 0;
    searchHold = false;
    const row = rows[current];
    if (row) scrollToRow(row, true);
    backBtn.hidden = true;
  }

  list.addEventListener('wheel', markManual, { passive: true });
  list.addEventListener('pointerdown', (e) => {
    if (e.target === list) markManual(); // the scrollbar
  });
  list.addEventListener('scroll', () => {
    if (!ours) markManual();
    syncBackButton();
    hideCard();
  });
  list.addEventListener('scrollend', () => {
    if (!ours) return;
    clearTimeout(oursTimer);
    oursTimer = setTimeout(() => (ours = false), 30);
  });

  // ---- clicks, keyboard, menu ----

  list.addEventListener('click', (e) => {
    const pend = e.target.closest('.pl-row-pend');
    if (pend) {
      e.stopPropagation();
      showCard(Number(pend.dataset.idx), pend);
      return;
    }
    const row = e.target.closest('.pl-row');
    if (!row) return;
    const sel = window.getSelection && window.getSelection();
    if (sel && sel.toString().trim() && row.contains(sel.anchorNode)) return; // selecting text to copy
    setFocusRow(Number(row.dataset.idx), false);
    opts.onSeek(sentences[Number(row.dataset.idx)]);
  });

  list.addEventListener('keydown', (e) => {
    const row = e.target.closest && e.target.closest('.pl-row');
    if (!row) return;
    const i = Number(row.dataset.idx);
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      setFocusRow(Math.max(0, Math.min(rows.length - 1, i + (e.key === 'ArrowDown' ? 1 : -1))), true);
      markManual();
    } else if (e.key === 'Enter') {
      e.preventDefault();
      opts.onSeek(sentences[i]);
    }
  });

  function setFocusRow(i, focus) {
    if (rows[focusIdx]) rows[focusIdx].tabIndex = -1;
    focusIdx = i;
    if (rows[i]) {
      rows[i].tabIndex = 0;
      if (focus) {
        rows[i].focus({ preventScroll: true });
        if (!inView(rows[i])) rows[i].scrollIntoView({ block: 'nearest' });
      }
    }
  }

  bindContextMenu(list, (e) => {
    const row = e.target.closest && e.target.closest('.pl-row');
    if (!row) return null;
    const sel = window.getSelection && window.getSelection();
    if (e.type === 'contextmenu' && sel && sel.toString().trim() && row.contains(sel.anchorNode)) return null; // browser menu to copy the selection
    return opts.menuItems(sentences[Number(row.dataset.idx)]);
  });

  // ---- pending change card (P5: before / after / reason on hover) ----

  let cardFor = -1;
  function showCard(i, anchor) {
    const s = sentences[i];
    if (!s || !s.patch) return;
    cardFor = i;
    clear(card).append(
      h('div', { class: 'pl-hc-title' }, h('span', { class: 'pl-pend-glyph', 'aria-hidden': 'true' }), t('player.side.pending_title')),
      h('div', { class: 'pl-hc-row' }, h('span', { class: 'pl-hc-k' }, t('player.side.before')), h('span', { class: 'pl-hc-v', lang: 'en' }, s.asr_text)),
      h('div', { class: 'pl-hc-row' }, h('span', { class: 'pl-hc-k' }, t('player.side.after')), h('span', { class: 'pl-hc-v', lang: 'en' }, diffView(s.asr_text, s.text))),
      s.patch.reason ? h('div', { class: 'pl-hc-row' }, h('span', { class: 'pl-hc-k' }, t('player.side.reason')), h('span', { class: 'pl-hc-v', lang: 'en' }, s.patch.reason)) : null,
      h('p', { class: 'pl-hc-foot' }, t('player.side.pending_foot')),
    );
    card.hidden = false;
    const r = anchor.getBoundingClientRect();
    const host = el.getBoundingClientRect();
    const cw = card.offsetWidth;
    const ch = card.offsetHeight;
    let top = r.bottom + 6;
    if (top + ch > window.innerHeight - 8) top = Math.max(8, r.top - ch - 6);
    card.style.top = `${top - host.top}px`;
    card.style.left = `${Math.max(8, Math.min(host.width - cw - 8, r.right - host.left - cw))}px`;
  }

  function hideCard() {
    cardFor = -1;
    card.hidden = true;
  }

  list.addEventListener('mouseover', (e) => {
    const pend = e.target.closest && e.target.closest('.pl-row-pend');
    if (pend && Number(pend.dataset.idx) !== cardFor) showCard(Number(pend.dataset.idx), pend);
  });
  list.addEventListener('mouseout', (e) => {
    const pend = e.target.closest && e.target.closest('.pl-row-pend');
    if (pend && !(e.relatedTarget && (pend.contains(e.relatedTarget) || card.contains(e.relatedTarget)))) hideCard();
  });
  card.addEventListener('mouseleave', hideCard);
  list.addEventListener('focusin', (e) => {
    const row = e.target.closest && e.target.closest('.pl-row.is-pending');
    if (row) showCard(Number(row.dataset.idx), row.querySelector('.pl-row-pend'));
    else hideCard();
  });
  list.addEventListener('focusout', (e) => {
    if (!list.contains(e.relatedTarget)) hideCard();
  });

  // ---- search (P5) ----

  function matches(s, q) {
    return (s.text || '').toLowerCase().includes(q) || zhq.test(s.zh);
  }

  // Searched again when the Simplified form of a Traditional query is ready.
  const searchAgain = () => runSearch(false);

  function runSearch(scroll) {
    const q = input.value.trim().toLowerCase();
    const changed = q !== query;
    const before = new Set(hits);
    query = q;
    zhq = zhQuery(q, searchAgain);
    hits = q ? sentences.map((s, i) => (matches(s, q) ? i : -1)).filter((i) => i >= 0) : [];
    for (const i of new Set([...before, ...hits])) {
      if (rows[i]) {
        rows[i].classList.toggle('is-hit', hits.includes(i));
        rows[i].classList.remove('is-hit-cur');
        fillText(rows[i], sentences[i]);
      }
    }
    if (changed) {
      hitPos = -1;
      if (!q) searchHold = false;
    } else if (hitPos >= hits.length) hitPos = hits.length - 1;
    if (hitPos >= 0 && rows[hits[hitPos]]) rows[hits[hitPos]].classList.add('is-hit-cur');
    syncCount();
    if (scroll && hits.length && changed) moveHit(1);
  }

  function syncCount() {
    const has = !!query;
    count.textContent = !has ? '' : hits.length ? t('player.side.search_count', { i: hitPos >= 0 ? hitPos + 1 : 0, n: hits.length }) : t('player.side.search_none');
    count.classList.toggle('is-none', has && !hits.length);
    prevBtn.disabled = nextBtn.disabled = !hits.length;
  }

  // Enter / Shift+Enter: scroll the transcript only, the playback position
  // does not change (P5); a click on the result jumps there.
  function moveHit(dir) {
    if (!hits.length) return;
    if (hitPos >= 0 && rows[hits[hitPos]]) rows[hits[hitPos]].classList.remove('is-hit-cur');
    hitPos = hitPos < 0 ? (dir > 0 ? firstHitAfterCurrent() : lastHitBeforeCurrent()) : (hitPos + dir + hits.length) % hits.length;
    const row = rows[hits[hitPos]];
    row.classList.add('is-hit-cur');
    searchHold = true;
    scrollToRow(row, false);
    setFocusRow(hits[hitPos], false);
    syncCount();
    requestAnimationFrame(syncBackButton);
  }

  function firstHitAfterCurrent() {
    const i = hits.findIndex((x) => x >= Math.max(0, current));
    return i >= 0 ? i : 0;
  }
  function lastHitBeforeCurrent() {
    for (let i = hits.length - 1; i >= 0; i--) if (hits[i] <= current) return i;
    return hits.length - 1;
  }

  input.addEventListener('input', () => {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => runSearch(true), 150);
  });
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.isComposing) {
      e.preventDefault();
      clearTimeout(searchTimer);
      if (input.value.trim().toLowerCase() !== query) runSearch(false);
      moveHit(e.shiftKey ? -1 : 1);
    } else if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      if (input.value) {
        input.value = '';
        runSearch(false);
        backToCurrent();
      } else input.blur();
    }
  });

  return {
    el,
    setSentences(list) {
      sentences = list || [];
      if (current >= sentences.length) current = -1;
      if (focusIdx >= sentences.length) focusIdx = 0;
      render();
    },
    setLoading() {
      clear(list).append(...Array.from({ length: 8 }, (_, i) => h('div', { class: 'pl-row is-skel', 'aria-hidden': 'true' }, h('span', { class: 'skel', style: { width: '36px' } }), h('div', { class: 'pl-row-text' }, h('div', { class: 'skel', style: { width: `${[92, 70, 84, 64][i % 4]}%` } }), h('div', { class: 'skel', style: { width: `${[60, 76, 52, 70][i % 4]}%` } })))));
    },
    setError(message, onRetry) {
      clear(list).append(
        h(
          'div',
          { class: 'pl-list-error', role: 'alert' },
          h('p', null, h('strong', null, t('player.side.load_failed')), ' ', message || ''),
          h('button', { type: 'button', class: 'btn btn-sm', onclick: onRetry }, t('common.retry')),
        ),
      );
    },
    setCurrent(i, { force = false } = {}) {
      if (i === current && !force) return;
      if (rows[current]) rows[current].classList.remove('is-cur');
      current = i;
      const row = rows[i];
      if (row) {
        row.classList.add('is-cur');
        if (force || autoScrollAllowed()) scrollToRow(row, !force);
      }
      syncBackButton();
    },
    // Called when the transcript becomes visible again (it could not scroll while hidden).
    reveal() {
      requestAnimationFrame(() => {
        const row = rows[current];
        if (row && autoScrollAllowed()) scrollToRow(row, false);
        syncBackButton();
      });
    },
    setDisplay(d, o) {
      display = d === 'off' ? 'both' : d; // P5: shares the display language; "off" only hides the subtitles
      order = o;
      el.dataset.display = display;
      el.dataset.order = order;
    },
    focusSearch() {
      input.focus();
      input.select();
    },
    updateSentence(s) {
      const i = sentences.findIndex((x) => x.idx === s.idx);
      if (i < 0) return;
      sentences[i] = s;
      const row = renderRow(s, i);
      if (i === current) row.classList.add('is-cur');
      if (hits.includes(i)) row.classList.add('is-hit');
      rows[i].replaceWith(row);
      rows[i] = row;
    },
    destroy() {
      clearTimeout(resumeTimer);
      clearTimeout(searchTimer);
      clearTimeout(oursTimer);
    },
  };
}
