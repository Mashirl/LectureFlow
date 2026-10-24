// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Context menus (v1.1 A12.2): right click, Shift+F10 and the Menu key open
// them; Esc closes (one level), arrow keys move, → / ← open and close
// submenus (▸), Enter / Space activate. Items that are not available are not
// passed in at all (SPEC §9.8: unavailable actions are not rendered).
//
// Item: {label, icon?, hint?, danger?, onSelect?, items?: Item[], checked?, lang?} | 'sep'
// checked (true/false) makes the item a menuitemradio with a check mark;
// lang marks a label written in another language (fonts, SPEC-v2 P14.5).
// Falsy entries are dropped, separators are collapsed, empty submenus removed.

import { h } from './dom.js';
import { icon } from './icons.js';

let root = null; // the open top-level menu element
let openedAt = 0;
let restoreFocus = null;

function normalize(items) {
  const out = [];
  for (const it of items || []) {
    if (!it) continue;
    if (it === 'sep') {
      if (out.length && out[out.length - 1] !== 'sep') out.push('sep');
      continue;
    }
    if (it.items) {
      const sub = normalize(it.items);
      if (!sub.length) continue;
      out.push({ ...it, items: sub });
      continue;
    }
    out.push(it);
  }
  while (out.length && out[out.length - 1] === 'sep') out.pop();
  return out;
}

export function closeMenu({ focus = true } = {}) {
  if (!root) return;
  root = null;
  for (const m of document.querySelectorAll('.ctx-menu')) m.remove(); // with open submenus
  document.removeEventListener('pointerdown', onOutside, true);
  window.removeEventListener('resize', onDismiss);
  window.removeEventListener('blur', onDismiss);
  document.removeEventListener('scroll', onScroll, true);
  window.removeEventListener('hashchange', onDismiss);
  let back = restoreFocus;
  restoreFocus = null;
  // the invoking element may have been re-rendered meanwhile (SSE refresh)
  if (back && !back.isConnected && back.dataset && back.dataset.fkey) back = document.querySelector(`[data-fkey="${CSS.escape(back.dataset.fkey)}"]`);
  if (focus && back && back.isConnected && back.focus) back.focus({ preventScroll: true });
}

function onOutside(e) {
  if (root && !e.target.closest('.ctx-menu')) closeMenu({ focus: false });
}
function onDismiss() {
  closeMenu({ focus: false });
}
function onScroll(e) {
  if (root && !(e.target instanceof Node && e.target.closest && e.target.closest('.ctx-menu'))) closeMenu({ focus: false });
}

function itemsOf(menu) {
  return Array.from(menu.querySelectorAll(':scope > .ctx-item'));
}

function focusItem(menu, i) {
  const list = itemsOf(menu);
  if (!list.length) return;
  const n = (i + list.length) % list.length;
  list[n].focus();
}

function place(menu, x, y, alignRightTo = null) {
  menu.style.left = '0px';
  menu.style.top = '0px';
  const r = menu.getBoundingClientRect();
  const vw = window.innerWidth;
  const vh = window.innerHeight;
  let left = x;
  let top = y;
  if (left + r.width > vw - 8) left = alignRightTo != null ? alignRightTo - r.width : vw - 8 - r.width;
  if (top + r.height > vh - 8) top = Math.max(8, vh - 8 - r.height);
  menu.style.left = `${Math.max(8, left)}px`;
  menu.style.top = `${Math.max(8, top)}px`;
}

function buildMenu(items, level) {
  const menu = h('div', { class: 'ctx-menu', role: 'menu', tabindex: '-1', dataset: { level: String(level) } });
  let openSub = null;
  let hoverTimer = null;
  const closeSub = () => {
    clearTimeout(hoverTimer);
    if (openSub) {
      openSub.menu.remove();
      openSub.item.setAttribute('aria-expanded', 'false');
      openSub = null;
    }
  };
  const showSub = (item, sub, focusFirst) => {
    if (openSub && openSub.item === item) {
      if (focusFirst) focusItem(openSub.menu, 0);
      return;
    }
    closeSub();
    const m = buildMenu(sub, level + 1);
    document.body.append(m);
    const r = item.getBoundingClientRect();
    place(m, r.right - 2, r.top - 5, r.left + 2);
    item.setAttribute('aria-expanded', 'true');
    openSub = { item, menu: m };
    m._parentItem = item;
    if (focusFirst) focusItem(m, 0);
  };
  menu._closeSub = closeSub;
  for (const it of items) {
    if (it === 'sep') {
      menu.append(h('div', { class: 'ctx-sep', role: 'separator' }));
      continue;
    }
    const hasSub = !!it.items;
    const radio = typeof it.checked === 'boolean';
    const el = h(
      'button',
      {
        type: 'button',
        class: 'ctx-item' + (it.danger ? ' is-danger' : ''),
        role: radio ? 'menuitemradio' : 'menuitem',
        'aria-checked': radio ? String(it.checked) : undefined,
        tabindex: '-1',
        'aria-haspopup': hasSub ? 'menu' : undefined,
        'aria-expanded': hasSub ? 'false' : undefined,
      },
      h('span', { class: 'ctx-icon' }, it.icon ? icon(it.icon, { size: 15 }) : radio && it.checked ? icon('check', { size: 15 }) : null),
      h('span', { class: 'ctx-label', lang: it.lang || undefined }, it.label),
      it.hint ? h('span', { class: 'ctx-hint' }, it.hint) : null,
      hasSub ? h('span', { class: 'ctx-arrow' }, icon('chevron-right', { size: 14 })) : null,
    );
    el.addEventListener('mouseenter', () => {
      el.focus({ preventScroll: true });
      clearTimeout(hoverTimer);
      if (hasSub) hoverTimer = setTimeout(() => showSub(el, it.items, false), 110);
      else hoverTimer = setTimeout(closeSub, 110);
    });
    el.addEventListener('click', (e) => {
      e.preventDefault();
      if (hasSub) {
        showSub(el, it.items, true);
        return;
      }
      closeMenu();
      if (it.onSelect) it.onSelect();
    });
    el.addEventListener('keydown', (e) => {
      const list = itemsOf(menu);
      const i = list.indexOf(el);
      if (e.key === 'ArrowDown') focusItem(menu, i + 1);
      else if (e.key === 'ArrowUp') focusItem(menu, i - 1);
      else if (e.key === 'Home') focusItem(menu, 0);
      else if (e.key === 'End') focusItem(menu, list.length - 1);
      else if (e.key === 'ArrowRight' && hasSub) showSub(el, it.items, true);
      else if (e.key === 'ArrowLeft' || (e.key === 'Escape' && level > 0)) {
        if (level === 0) return;
        const parent = menu._parentItem;
        const parentMenu = parent && parent.closest('.ctx-menu');
        if (parentMenu && parentMenu._closeSub) parentMenu._closeSub();
        if (parent) parent.focus();
      } else if (e.key === 'Escape') closeMenu();
      else if (e.key === 'Tab') closeMenu();
      else if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        el.click();
        return;
      } else return;
      e.preventDefault();
      e.stopPropagation();
    });
    menu.append(el);
  }
  menu.addEventListener('contextmenu', (e) => e.preventDefault());
  return menu;
}

// openMenu(items, {x, y} | {anchor: Element}, {returnFocus}) opens a menu at
// the pointer or below an anchor element. Returns false when nothing is shown.
export function openMenu(items, pos, opts = {}) {
  closeMenu({ focus: false });
  const list = normalize(items);
  if (!list.length) return false;
  restoreFocus = opts.returnFocus || document.activeElement;
  root = buildMenu(list, 0);
  document.body.append(root);
  if (pos.anchor) {
    const r = pos.anchor.getBoundingClientRect();
    const alignRight = opts.align === 'right';
    root.style.left = '0px';
    const w = root.getBoundingClientRect().width;
    place(root, alignRight ? r.right - w : r.left, r.bottom + 4, r.right);
  } else {
    place(root, pos.x, pos.y);
  }
  openedAt = performance.now();
  focusItem(root, 0);
  document.addEventListener('pointerdown', onOutside, true);
  window.addEventListener('resize', onDismiss);
  window.addEventListener('blur', onDismiss);
  document.addEventListener('scroll', onScroll, true);
  window.addEventListener('hashchange', onDismiss);
  return true;
}

const TEXT_FIELDS = 'input:not([type="checkbox"]):not([type="radio"]):not([type="button"]), textarea, select, [contenteditable="true"]';

// The element a keyboard-opened menu is anchored to.
function keyboardAnchor(el, scope) {
  const a = document.activeElement;
  return a && scope.contains(a) ? a : el;
}

// bindContextMenu(el, getItems(event) → items) wires right click, Shift+F10
// and the Menu key on el (and anything inside it). getItems may return null
// to fall back to the browser menu (e.g. inside a text field).
export function bindContextMenu(el, getItems) {
  el.addEventListener('contextmenu', (e) => {
    if (e.target.closest(TEXT_FIELDS)) return; // keep the browser menu (copy / paste)
    // A keyboard-opened menu may be followed by the browser's own event.
    if (root && performance.now() - openedAt < 400) {
      e.preventDefault();
      return;
    }
    const items = getItems(e);
    if (!items) return;
    e.preventDefault();
    e.stopPropagation();
    const fromKeyboard = e.button !== 0 && e.clientX === 0 && e.clientY === 0;
    if (fromKeyboard) {
      const a = keyboardAnchor(el, el);
      openMenu(items, { anchor: a }, { returnFocus: a });
    } else {
      openMenu(items, { x: e.clientX, y: e.clientY }, { returnFocus: document.activeElement && el.contains(document.activeElement) ? document.activeElement : el });
    }
  });
  el.addEventListener('keydown', (e) => {
    const isMenuKey = e.key === 'ContextMenu' || (e.key === 'F10' && e.shiftKey);
    if (!isMenuKey || e.target.closest(TEXT_FIELDS)) return;
    const items = getItems(e);
    if (!items) return;
    e.preventDefault();
    e.stopPropagation();
    const a = keyboardAnchor(el, el);
    openMenu(items, { anchor: a }, { returnFocus: a });
  });
}

// A "⋯" style button that opens the same menu below itself.
export function menuButton(btnEl, getItems, align = 'right') {
  btnEl.setAttribute('aria-haspopup', 'menu');
  btnEl.addEventListener('click', (e) => {
    e.stopPropagation();
    if (root && root._owner === btnEl) {
      closeMenu();
      return;
    }
    if (openMenu(getItems(), { anchor: btnEl }, { returnFocus: btnEl, align }) && root) root._owner = btnEl;
  });
  btnEl.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      if (openMenu(getItems(), { anchor: btnEl }, { returnFocus: btnEl, align }) && root) root._owner = btnEl;
    }
  });
  return btnEl;
}
