// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Review tab (SPEC §9.7, v1.1 A13): the queue of review=pending decide
// patches. Each card shows the previous and next sentence (grey), the word
// diff, the reason and the clip's progress bar. Keys: Space / Shift+Space
// play (1× / 0.75×), ← / → ±0.5 s, Home clip start, Shift+← sentence start
// (only while the current card has focus and no edit is open), A / R / E
// (Enter, Esc) / U / J / K.

import { api } from './api.js';
import { h, append, clear, toast, confirmDialog, preserveFocus, isTypingTarget, errorBlock } from './dom.js';
import { icon } from './icons.js';
import * as Clip from './audiobar.js';
import { diffView } from './widgets.js';
import * as U from './util.js';
import { t } from './i18n.js';
import { bindContextMenu } from './menu.js';

const busyText = () => t('review.busy');

// Review states of a card: text keys and the style class.
const RESULT = {
  accepted: { text: 'review.result.accepted', hint: 'review.result.accepted_hint', cls: 'ok' },
  rejected: { text: 'review.result.rejected', hint: 'review.result.rejected_hint', cls: 'err' },
  edited: { text: 'review.result.edited', hint: 'review.result.edited_hint', cls: 'human' },
  pending: { text: 'review.result.pending', hint: '', cls: 'pending' },
};

// "{n} to review" with the number in bold: the plural form is chosen for n, then
// the number is wrapped where it appears in the text.
function boldCount(key, n) {
  const s = t(key, { n });
  const num = String(n);
  const i = s.indexOf(num);
  if (i < 0) return [s];
  return [s.slice(0, i), h('b', null, num), s.slice(i + num.length)];
}

export function mountReview(root, lecture, ctx) {
  let lec = lecture;
  let alive = true;
  let loaded = false;
  let loadError = '';
  const items = new Map(); // patch id -> Patch (always the server's latest copy)
  const acted = new Set(); // ids reviewed in this session (kept visible after review)
  const undoStack = [];
  let retranslated = 0;
  let order = 'idx'; // idx | suspicious
  let currentId = null;
  let editingId = null;
  let draft = '';
  let busy = false;
  let bulk = null; // {done, total} while "accept all" runs
  let changedTimer = null;

  const noteEl = h('div', { class: 'rv-note', role: 'note' }, icon('circle-alert', { size: 16 }), h('span', null, t('review.note')));
  const toolbar = h('div', { class: 'rv-toolbar' });
  const keysEl = h(
    'p',
    { class: 'rv-keys' },
    keyHint([t('player.key.space')], t('review.keys.play')),
    keyHint(['Shift', t('player.key.space')], t('review.keys.slow')),
    keyHint(['←', '→'], t('review.keys.seek')),
    keyHint(['Home'], t('review.keys.clip_start')),
    keyHint(['Shift', '←'], t('review.keys.sentence_start')),
    keyHint(['A'], t('review.accept')),
    keyHint(['R'], t('review.reject')),
    keyHint(['E'], t('common.edit')),
    keyHint(['U'], t('review.keys.undo')),
    keyHint(['J', 'K'], t('review.keys.next_prev')),
  );
  const statusEl = h('div', { class: 'rv-status', 'aria-live': 'polite' });
  const listEl = h('div', { class: 'rv-list' });
  // SPEC-v2 P1: a review card's context menu opens the sentence in the player
  // (one second before it).
  bindContextMenu(listEl, (e) => {
    const c = e.target.closest && e.target.closest('.rcard');
    const p = c && items.get(c.dataset.id);
    if (!p || !lec.playable) return null;
    return [{ label: t('player.entry.open_in_player'), icon: 'external-link', onSelect: () => (location.hash = `#/play/${encodeURIComponent(lec.id)}?t=${Math.max(0, p.start_ms - 1000)}`) }];
  });
  root.append(noteEl, toolbar, keysEl, statusEl, listEl);

  function keyHint(keys, label) {
    return h('span', { class: 'rv-key' }, keys.map((k) => h('kbd', null, k)), h('span', null, label));
  }

  async function load() {
    try {
      const list = await api.patches(lec.id, 'pending');
      if (!alive) return;
      const keep = [...items.values()].filter((p) => acted.has(p.id));
      items.clear();
      for (const p of list || []) items.set(p.id, p);
      // Patches reviewed in this session stay on screen with their server state.
      for (const p of keep) if (!items.has(p.id)) items.set(p.id, p);
      loaded = true;
      loadError = '';
      if (!currentId || !items.has(currentId)) changeCurrent(firstPending());
    } catch (e) {
      if (!alive) return;
      if (!loaded) loadError = e.message || t('common.load_failed');
    }
    preserveFocus(render);
    prefetchAround();
  }

  function retry() {
    loadError = '';
    render();
    load();
  }

  function sortedIds() {
    const arr = [...items.values()];
    if (order === 'suspicious') arr.sort((a, b) => b.low_conf_count - a.low_conf_count || a.sentence_idx - b.sentence_idx);
    else arr.sort((a, b) => a.sentence_idx - b.sentence_idx);
    return arr.map((p) => p.id);
  }

  function pendingIds() {
    return sortedIds().filter((id) => items.get(id).review === 'pending');
  }

  function firstPending() {
    const ids = pendingIds();
    return ids.length ? ids[0] : sortedIds()[0] || null;
  }

  // Next pending card after `fromId` (wrapping around), or null.
  function nextPending(fromId) {
    const ids = sortedIds();
    const start = ids.indexOf(fromId);
    for (let k = 1; k <= ids.length; k++) {
      const id = ids[(start + k + ids.length) % ids.length];
      if (items.get(id).review === 'pending') return id;
    }
    return null;
  }

  function current() {
    return currentId ? items.get(currentId) || null : null;
  }

  const spec = (p) => ({ lectureId: lec.id, startMs: p.start_ms, endMs: p.end_ms });

  // Switching cards stops playback; the new card starts in its initial state
  // and does not play by itself (A13.2, §9.7).
  function changeCurrent(id) {
    if (id === currentId) return;
    currentId = id;
    Clip.stop();
  }

  // The current card and the next pending one load their clips ahead of time.
  function prefetchAround() {
    const p = current();
    if (!p) return;
    Clip.prefetch(spec(p));
    const n = nextPending(p.id);
    if (n && n !== p.id) Clip.prefetch(spec(items.get(n)));
  }

  // ---- rendering ----

  function destroyBars() {
    for (const b of listEl.querySelectorAll('.abar')) if (b.destroy) b.destroy();
  }

  function render() {
    clear(toolbar);
    clear(statusEl);
    destroyBars();
    clear(listEl);
    if (!loaded) {
      if (loadError) statusEl.append(h('div', { class: 'card rv-error' }, h('div', { class: 'card-bd' }, errorBlock(t('review.load_failed', { error: loadError }), retry))));
      else listEl.append(skeletonCard(), skeletonCard(), skeletonCard());
      return;
    }
    const pending = pendingIds();
    const canReview = lec.reviewable && !busy;
    const total = pending.length + acted.size;
    append(toolbar, [
      h('div', { class: 'seg-toggle', role: 'radiogroup', 'aria-label': t('review.sort.aria') }, sortButton('idx', t('review.sort.idx')), sortButton('suspicious', t('review.sort.suspicious'))),
      h(
        'div',
        { class: 'rv-progress' },
        h('span', { class: 'rv-progress-text' }, boldCount('review.progress.pending', pending.length), acted.size ? h('span', { class: 'dim' }, ` · ${t('review.progress.acted', { n: acted.size })}`) : null),
        total ? h('span', { class: 'meter meter-accent rv-meter', role: 'presentation' }, h('span', { class: 'meter-fill', style: { width: `${(acted.size / total) * 100}%` } })) : null,
      ),
      pending.length
        ? h(
            'button',
            { type: 'button', class: 'btn btn-sm rv-acceptall', disabled: !canReview, title: lec.reviewable ? t('review.accept_all_tip') : busyText(), dataset: { fkey: 'rv:acceptall' }, onclick: acceptAll },
            icon('check-check', { size: 14 }),
            h('span', { class: 'btn-label' }, t('review.accept_all')),
            h('span', { class: 'btn-count' }, String(pending.length)),
          )
        : null,
    ]);
    if (!lec.reviewable) statusEl.append(h('p', { class: 'banner banner-warn rv-busy' }, icon('clock', { size: 15 }), t('review.busy_banner')));
    if (bulk) statusEl.append(h('p', { class: 'banner banner-info' }, icon('loader-circle', { size: 15, class: 'spin' }), t('review.bulk_progress', { done: bulk.done, total: bulk.total })));
    if (pending.length === 0) {
      if (acted.size > 0) {
        statusEl.append(
          h(
            'div',
            { class: 'rv-done', tabindex: '-1', dataset: { fkey: 'rv:done' } },
            h('span', { class: 'rv-done-icon' }, icon('circle-check', { size: 26 })),
            h('div', null, h('h2', { class: 'rv-done-title' }, t('review.done.title', { n: retranslated })), h('p', { class: 'rv-done-sub' }, t('review.done.sub'))),
          ),
        );
      } else {
        statusEl.append(
          h(
            'div',
            { class: 'rv-done rv-empty' },
            h('span', { class: 'rv-done-icon' }, icon('circle-check', { size: 26 })),
            h('div', null, h('h2', { class: 'rv-done-title' }, t('review.empty.title')), h('p', { class: 'rv-done-sub' }, t('review.empty.sub'))),
          ),
        );
      }
    }
    for (const id of sortedIds()) listEl.append(card(items.get(id), canReview));
  }

  function skeletonCard() {
    return h(
      'div',
      { class: 'rcard is-skeleton', 'aria-hidden': 'true' },
      h('div', { class: 'skel', style: { width: '28%', marginTop: '2px' } }),
      h('div', { class: 'skel', style: { width: '62%' } }),
      h('div', { class: 'skel', style: { width: '88%', height: '14px' } }),
      h('div', { class: 'skel', style: { width: '54%' } }),
      h('div', { class: 'skel', style: { width: '100%', height: '6px', margin: '16px 0 4px' } }),
    );
  }

  function sortButton(value, label) {
    return h(
      'button',
      {
        type: 'button',
        class: 'seg-toggle-btn' + (order === value ? ' active' : ''),
        role: 'radio',
        'aria-checked': order === value ? 'true' : 'false',
        dataset: { fkey: `rv:sort:${value}` },
        onclick: () => {
          if (order === value) return;
          order = value;
          preserveFocus(render);
        },
      },
      label,
    );
  }

  function card(p, canReview) {
    const isCur = p.id === currentId;
    const pending = p.review === 'pending';
    const r = RESULT[p.review];
    const res = r ? { text: t(r.text), hint: r.hint ? t(r.hint) : '', cls: r.cls } : { text: U.REVIEW_NAMES[p.review] || p.review, cls: 'pending' };
    const action = (label, key, fn, kind, fkey) =>
      h(
        'button',
        { type: 'button', class: `btn btn-sm${kind ? ' btn-' + kind : ''}`, disabled: !canReview, title: canReview ? t('player.tip', { name: label, key }) : busyText(), 'aria-keyshortcuts': key, dataset: { fkey }, onclick: fn },
        h('span', { class: 'btn-label' }, label),
        h('kbd', null, key),
      );
    const actions = [];
    if (pending && editingId !== p.id) {
      actions.push(
        action(t('review.accept'), 'A', () => act(p.id, 'accept'), 'primary', `rc:${p.id}:a`),
        action(t('review.reject'), 'R', () => act(p.id, 'reject'), '', `rc:${p.id}:r`),
        action(t('common.edit'), 'E', () => startEdit(p.id), '', `rc:${p.id}:e`),
      );
    } else if (!pending) {
      actions.push(
        h(
          'button',
          { type: 'button', class: 'btn btn-sm btn-ghost', disabled: !canReview, title: canReview ? t('review.reset_tip') : busyText(), dataset: { fkey: `rc:${p.id}:reset` }, onclick: () => act(p.id, 'reset') },
          icon('rotate-cw', { size: 13 }),
          h('span', { class: 'btn-label' }, t('review.reset')),
        ),
      );
    }
    if (!lec.reviewable && (pending || actions.length)) actions.push(h('span', { class: 'rc-busy' }, icon('clock', { size: 13 }), busyText()));
    const c = h(
      'article',
      {
        class: `rcard rv-${p.review}` + (isCur ? ' current' : ''),
        tabindex: '0',
        'aria-label': t('review.card_aria', { n: p.sentence_idx + 1, state: res.text }),
        'aria-current': isCur ? 'true' : null,
        dataset: { fkey: `rc:${p.id}`, id: p.id },
        onfocusin: () => {
          if (currentId !== p.id) setCurrent(p.id, false);
        },
      },
      h(
        'div',
        { class: 'rc-head' },
        h('span', { class: 'rc-num' }, `#${p.sentence_idx + 1}`),
        h('span', { class: 'rc-ts mono' }, U.fmtClock(p.start_ms)),
        p.low_conf_count ? h('span', { class: 'chip chip-warn rc-lowconf', title: t('review.low_conf_tip', { order: t('review.sort.suspicious') }) }, t('review.low_conf', { n: p.low_conf_count })) : null,
        h('span', { class: `rc-state rc-state-${res.cls}`, title: res.hint || null }, pending ? null : icon(p.review === 'rejected' ? 'x' : 'check', { size: 12 }), res.text),
      ),
      h(
        'div',
        { class: 'rc-text' },
        p.prev_text ? h('p', { class: 'rc-ctx', text: p.prev_text }) : null,
        h('p', { class: 'rc-diff' }, diffView(p.before, p.after)),
        p.review === 'edited' && p.edited_text != null ? h('p', { class: 'rc-edited' }, h('span', { class: 'rc-k' }, t('review.your_edit')), diffView(p.before, p.edited_text)) : null,
        p.next_text ? h('p', { class: 'rc-ctx', text: p.next_text }) : null,
      ),
      // the model's reason is content in English (SPEC §13 repair prompt)
      h('p', { class: 'rc-reason' }, h('span', { class: 'rc-k' }, t('player.side.reason')), p.reason ? h('span', { lang: 'en' }, p.reason) : h('span', null, t('review.no_reason'))),
      Clip.audioBar(spec(p), { autoload: 'visible', fkey: `rc:${p.id}:ab`, label: t('transcript.audio_label', { n: p.sentence_idx + 1 }) }),
      editingId === p.id ? editBox(p) : null,
      actions.length ? h('div', { class: 'rc-actions' }, actions) : null,
    );
    return c;
  }

  function editBox(p) {
    const ta = h('textarea', { class: 'input rc-edit-ta', rows: '2', 'aria-label': t('review.edit.aria'), dataset: { fkey: `rc:${p.id}:ta` } });
    ta.value = draft;
    ta.addEventListener('input', () => (draft = ta.value));
    ta.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
        e.preventDefault();
        saveEdit(p.id);
      } else if (e.key === 'Escape') {
        e.preventDefault();
        e.stopPropagation();
        cancelEdit();
      }
    });
    return h(
      'div',
      { class: 'rc-edit' },
      ta,
      h(
        'div',
        { class: 'rc-edit-actions' },
        h('button', { type: 'button', class: 'btn btn-sm btn-primary', disabled: !lec.reviewable || busy, onclick: () => saveEdit(p.id) }, t('common.save')),
        h('button', { type: 'button', class: 'btn btn-sm', onclick: cancelEdit }, t('common.cancel')),
        h('span', { class: 'muted small' }, lec.reviewable ? t('review.edit.hint') : busyText()),
      ),
    );
  }

  function cardEl(id) {
    return id ? listEl.querySelector(`[data-fkey="${CSS.escape('rc:' + id)}"]`) : null;
  }

  function focusCurrent() {
    // Everything reviewed: move to the "review done" message.
    const done = statusEl.querySelector('[data-fkey="rv:done"]');
    if (done && pendingIds().length === 0) {
      done.focus({ preventScroll: true });
      done.scrollIntoView({ block: 'center' });
      return;
    }
    focusCard();
  }

  function focusCard() {
    const el = cardEl(currentId);
    if (el) {
      el.focus({ preventScroll: true });
      el.scrollIntoView({ block: 'nearest' });
    }
  }

  function setCurrent(id, focus) {
    const wasEditing = editingId && editingId !== id ? editingId : null;
    changeCurrent(id);
    if (wasEditing) {
      editingId = null;
      replaceCard(wasEditing);
    }
    for (const el of listEl.querySelectorAll('.rcard.current')) {
      el.classList.remove('current');
      el.removeAttribute('aria-current');
    }
    const el = cardEl(id);
    if (el) {
      el.classList.add('current');
      el.setAttribute('aria-current', 'true');
    }
    if (focus) focusCard();
    prefetchAround();
  }

  function replaceCard(id) {
    const old = cardEl(id);
    const p = items.get(id);
    if (!old || !p) return;
    for (const b of old.querySelectorAll('.abar')) if (b.destroy) b.destroy();
    old.replaceWith(card(p, lec.reviewable && !busy));
  }

  function move(delta) {
    const ids = sortedIds();
    if (!ids.length) return;
    let i = ids.indexOf(currentId);
    if (i < 0) i = delta > 0 ? -1 : 0;
    const j = Math.max(0, Math.min(ids.length - 1, i + delta));
    setCurrent(ids[j], true);
  }

  // ---- actions ----

  async function act(id, action, text) {
    if (busy) return;
    if (!lec.reviewable) {
      toast(busyText(), 'warn');
      return;
    }
    const p = items.get(id);
    if (!p) return;
    if (action !== 'reset' && p.review !== 'pending') return;
    busy = true;
    preserveFocus(render);
    let ok = false;
    try {
      const updated = await api.review(id, action, text);
      if (!alive) return;
      if (updated) {
        ok = true;
        items.set(updated.id, updated);
        retranslated += updated.retranslated || 0;
        if (action === 'reset') {
          const k = undoStack.lastIndexOf(updated.id);
          if (k >= 0) undoStack.splice(k, 1);
          changeCurrent(updated.id);
        } else {
          acted.add(updated.id);
          undoStack.push(updated.id);
          changeCurrent(nextPending(updated.id) || updated.id);
        }
        editingId = null;
      }
    } catch {
      // shown by the API layer (e.g. 409 review.busy)
    } finally {
      busy = false;
    }
    if (!alive) return;
    if (ok) {
      render();
      focusCurrent();
      prefetchAround();
    } else {
      preserveFocus(render);
    }
  }

  function startEdit(id) {
    const p = items.get(id);
    if (!p || p.review !== 'pending') return;
    if (!lec.reviewable) {
      toast(busyText(), 'warn');
      return;
    }
    if (currentId !== id) setCurrent(id, false);
    const prev = editingId;
    editingId = id;
    draft = p.edited_text != null ? p.edited_text : p.after;
    if (prev && prev !== id) replaceCard(prev);
    replaceCard(id);
    const ta = cardEl(id) && cardEl(id).querySelector('textarea');
    if (ta) {
      ta.focus();
      ta.setSelectionRange(ta.value.length, ta.value.length);
    }
  }

  function cancelEdit() {
    const id = editingId;
    editingId = null;
    if (id) replaceCard(id);
    focusCard();
  }

  function saveEdit(id) {
    const text = draft.trim();
    if (!text) {
      toast(t('review.edit.empty'), 'warn');
      return;
    }
    act(id, 'edit', text);
  }

  function undo() {
    const id = undoStack[undoStack.length - 1];
    if (!id) {
      toast(t('review.nothing_to_undo'), 'info');
      return;
    }
    act(id, 'reset');
  }

  async function acceptAll() {
    const ids = pendingIds();
    if (!ids.length || busy) return;
    const ok = await confirmDialog({
      title: t('review.accept_all_title'),
      message: t('review.accept_all_message', { n: ids.length }),
      okText: t('review.accept_all_ok', { n: ids.length }),
    });
    if (!ok || !alive) return;
    Clip.stop();
    busy = true;
    bulk = { done: 0, total: ids.length };
    render();
    try {
      for (const id of ids) {
        const updated = await api.review(id, 'accept');
        if (!alive) return;
        if (!updated) break;
        items.set(updated.id, updated);
        retranslated += updated.retranslated || 0;
        acted.add(updated.id);
        undoStack.push(updated.id);
        bulk.done++;
        render();
      }
    } catch {
      // shown by the API layer; stop at the first failure
    } finally {
      busy = false;
    }
    if (!alive) return;
    if (bulk.done < bulk.total) toast(t('review.bulk_partial', { done: bulk.done, rest: bulk.total - bulk.done }), 'warn');
    bulk = null;
    changeCurrent(nextPending(currentId) || currentId);
    render();
    focusCurrent();
  }

  // ---- keyboard ----

  function onKey(e) {
    if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey) return;
    if (!root.isConnected || document.querySelector('dialog[open], .ctx-menu')) return;
    if (isTypingTarget(e.target)) return; // the edit box handles Enter / Esc itself
    const p = current();
    if (e.key === ' ' || e.code === 'Space') {
      // A focused button or link keeps its native Space behaviour.
      if (e.target.closest && e.target.closest('button, a, summary')) return;
      if (!p) return;
      e.preventDefault();
      Clip.toggle(spec(p), e.shiftKey ? Clip.SLOW_RATE : 1);
      return;
    }
    // ← / → / Home / Shift+←: only while the current card has focus and no
    // edit is open (A13.2).
    if (e.key === 'ArrowLeft' || e.key === 'ArrowRight' || e.key === 'Home') {
      const cur = cardEl(currentId);
      if (!p || editingId || !cur || !cur.contains(document.activeElement)) return;
      e.preventDefault();
      if (e.key === 'Home') Clip.seekTo(spec(p), 0);
      else if (e.key === 'ArrowLeft' && e.shiftKey) Clip.seekSentence(spec(p));
      else Clip.seekBy(spec(p), e.key === 'ArrowLeft' ? -Clip.STEP_MS : Clip.STEP_MS);
      return;
    }
    const k = e.key.length === 1 ? e.key.toLowerCase() : '';
    if (!k || e.shiftKey) return;
    if (k === 'a' && p) act(p.id, 'accept');
    else if (k === 'r' && p) act(p.id, 'reject');
    else if (k === 'e' && p) startEdit(p.id);
    else if (k === 'u') undo();
    else if (k === 'j') move(1);
    else if (k === 'k') move(-1);
    else return;
    e.preventDefault();
  }
  document.addEventListener('keydown', onKey);

  render(); // skeleton until the first response
  load();

  return {
    setLecture(l) {
      const changed = l.reviewable !== lec.reviewable || l.id !== lec.id;
      lec = l;
      if (changed) preserveFocus(render);
    },
    refresh: load,
    onChanged(ev) {
      if (ev.kind === 'patches' && ev.id === lec.id) {
        clearTimeout(changedTimer);
        changedTimer = setTimeout(load, 400);
      }
    },
    destroy() {
      alive = false;
      document.removeEventListener('keydown', onKey);
      clearTimeout(changedTimer);
      Clip.stop();
      destroyBars();
      clear(root);
    },
  };
}
