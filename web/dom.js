// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Small DOM helpers: element builder, buttons, toasts, dialogs, drop targets.

import { icon } from './icons.js';
import { t } from './i18n.js';
import { DESKTOP, attachDroppedPaths, pickLocalFiles } from './api.js';

export function h(tag, props, ...children) {
  const el = document.createElement(tag);
  let value;
  if (props) {
    for (const [k, v] of Object.entries(props)) {
      if (v == null || v === false) continue;
      if (k === 'class') el.className = v;
      else if (k === 'text') el.textContent = v;
      else if (k === 'value') value = v;
      else if (k === 'style' && typeof v === 'object') Object.assign(el.style, v);
      else if (k === 'dataset') Object.assign(el.dataset, v);
      else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
      else if (v === true) el.setAttribute(k, '');
      else el.setAttribute(k, String(v));
    }
  }
  append(el, children);
  // After the children, so a <select> already has its options.
  if (value !== undefined) el.value = value;
  return el;
}

export function append(el, children) {
  for (const c of children) {
    if (c == null || c === false || c === true) continue;
    if (Array.isArray(c)) append(el, c);
    else if (c instanceof Node) el.append(c);
    else el.append(document.createTextNode(String(c)));
  }
  return el;
}

export function clear(el) {
  while (el.firstChild) el.removeChild(el.firstChild);
  return el;
}

// Re-render while keeping keyboard focus on the element with the same
// data-fkey, so background refreshes (SSE) do not throw the user's focus away.
export function preserveFocus(fn) {
  const a = document.activeElement;
  const key = a && a.dataset ? a.dataset.fkey : null;
  fn();
  if (key) {
    const el = document.querySelector(`[data-fkey="${CSS.escape(key)}"]`);
    if (el && el !== document.activeElement) el.focus({ preventScroll: true });
  }
}

// ---- buttons ----

// btn(label, {icon: 'play', kind: 'primary'|'danger'|'ghost'|'quiet', size: 'sm', onclick, title, fkey})
export function btn(label, o = {}) {
  const cls = ['btn'];
  if (o.kind) cls.push('btn-' + o.kind);
  if (o.size) cls.push('btn-' + o.size);
  if (o.class) cls.push(o.class);
  return h(
    'button',
    {
      type: o.type || 'button',
      class: cls.join(' '),
      title: o.title,
      disabled: o.disabled,
      'aria-label': o.ariaLabel,
      dataset: o.fkey ? { fkey: o.fkey } : undefined,
      onclick: o.onclick,
    },
    o.icon ? icon(o.icon, { size: o.size === 'sm' ? 14 : 16 }) : null,
    label ? h('span', { class: 'btn-label' }, label) : null,
    o.count != null ? h('span', { class: 'btn-count' }, String(o.count)) : null,
  );
}

// Square icon-only button with an accessible name (also the tooltip).
export function iconBtn(name, label, o = {}) {
  return h(
    'button',
    {
      type: 'button',
      class: 'icon-btn' + (o.class ? ' ' + o.class : ''),
      'aria-label': label,
      title: o.title || label,
      'aria-haspopup': o.menu ? 'menu' : undefined,
      'aria-pressed': o.pressed == null ? undefined : String(!!o.pressed),
      dataset: o.fkey ? { fkey: o.fkey } : undefined,
      onclick: o.onclick,
    },
    icon(name, { size: o.size || 16 }),
  );
}

// Placeholder blocks while data loads.
export function skeleton(lines = 3, cls = '') {
  return h(
    'div',
    { class: 'skel-group ' + cls, 'aria-hidden': 'true' },
    Array.from({ length: lines }, (_, i) => h('div', { class: 'skel', style: { width: `${[92, 74, 84, 60, 88][i % 5]}%` } })),
  );
}

// An error inside a card, with a retry button (loading failed).
export function errorBlock(message, onRetry) {
  return h(
    'div',
    { class: 'error-block', role: 'alert' },
    icon('circle-alert', { size: 18 }),
    h('div', { class: 'error-block-text' }, h('strong', null, t('common.load_failed')), h('span', null, message || t('common.unknown_error'))),
    onRetry ? btn(t('common.retry'), { size: 'sm', icon: 'rotate-cw', onclick: onRetry }) : null,
  );
}

// ---- toasts (SPEC §9.8) ----
// info / success / warn disappear after 4 seconds; error stays until closed.

const TOAST_ICONS = { info: 'circle-alert', success: 'circle-check', warn: 'triangle-alert', error: 'circle-alert' };

export function toast(message, kind = 'info') {
  const root = document.getElementById('toasts');
  if (!root || !message) return;
  for (const el of root.children) {
    if (el.dataset.msg === message && el.dataset.kind === kind) {
      const n = Number(el.dataset.count || 1) + 1;
      el.dataset.count = String(n);
      el.querySelector('.toast-count').textContent = `×${n}`;
      if (kind !== 'error') armToast(el);
      return;
    }
  }
  const el = h(
    'div',
    { class: `toast toast-${kind}`, role: kind === 'error' ? 'alert' : 'status', dataset: { msg: message, kind } },
    icon(TOAST_ICONS[kind] || 'circle-alert', { size: 16, class: 'toast-icon' }),
    h('span', { class: 'toast-msg', text: message }),
    h('span', { class: 'toast-count' }),
    h('button', { type: 'button', class: 'toast-close', 'aria-label': t('common.toast_close'), title: t('common.close'), onclick: () => el.remove() }, icon('x', { size: 14 })),
  );
  root.append(el);
  if (kind !== 'error') armToast(el);
}

function armToast(el) {
  clearTimeout(el._timer);
  el._timer = setTimeout(() => el.remove(), 4000);
}

// ---- dialogs (native <dialog>: focus trap and Esc for free) ----

// openDialog(title, build(close) → Node, {wide, className}) → Promise of the
// value passed to close (null on Esc / cancel).
export function openDialog(title, build, opts = {}) {
  return new Promise((resolve) => {
    const prev = document.activeElement;
    const dlg = h('dialog', { class: 'dlg' + (opts.wide ? ' dlg-wide' : '') + (opts.className ? ' ' + opts.className : ''), 'aria-label': title });
    // Toasts outside a modal sit behind its backdrop (inert, blurred): while
    // the dialog is open it hosts them, then they go back to where they were
    // (the page, or a wizard that opened this dialog), as wizard.js does.
    const toasts = document.getElementById('toasts');
    const home = toasts && toasts.parentNode;
    let done = false;
    const close = (v) => {
      if (done) return;
      done = true;
      if (toasts && toasts.parentNode === dlg) (home && home.isConnected ? home : document.body).append(toasts);
      dlg.close();
      dlg.remove();
      if (prev && prev.isConnected && prev.focus) prev.focus();
      resolve(v);
    };
    dlg.addEventListener('cancel', (e) => {
      e.preventDefault();
      close(null);
    });
    // a click on the backdrop (outside the dialog box) cancels
    dlg.addEventListener('mousedown', (e) => {
      if (e.target !== dlg) return;
      const r = dlg.getBoundingClientRect();
      if (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) close(null);
    });
    dlg.append(h('h2', { class: 'dlg-title', text: title }), build(close));
    document.body.append(dlg);
    dlg.showModal();
    if (toasts) dlg.append(toasts);
  });
}

// focusCancel: the cancel button has the initial focus (always for danger;
// also for a non-destructive choice the user should think twice about).
export function confirmDialog({ title, message, okText = t('common.ok'), cancelText = t('common.cancel'), danger = false, focusCancel = danger, body = null }) {
  return openDialog(title, (close) =>
    h(
      'div',
      null,
      message ? h('p', { class: 'dlg-msg', text: message }) : null,
      body,
      h(
        'div',
        { class: 'dlg-actions' },
        h('button', { type: 'button', class: 'btn', autofocus: focusCancel, onclick: () => close(false) }, cancelText),
        h('button', { type: 'button', class: danger ? 'btn btn-danger-solid' : 'btn btn-primary', autofocus: !focusCancel, onclick: () => close(true) }, okText),
      ),
    ),
  ).then((v) => v === true);
}

export function inputDialog({ title, label, value = '', okText = t('common.ok'), placeholder = '', type = 'text', hint = '', validate = null }) {
  return openDialog(title, (close) => {
    const input = h('input', { type, class: 'input', value, placeholder, autofocus: true, required: true, 'aria-label': label });
    const ok = h('button', { type: 'submit', class: 'btn btn-primary' }, okText);
    const err = h('p', { class: 'dlg-field-error', role: 'alert' });
    const sync = () => {
      const v = input.value.trim();
      const msg = v && validate ? validate(v) : '';
      err.textContent = msg || '';
      ok.disabled = !v || !!msg;
    };
    input.addEventListener('input', sync);
    sync();
    return h(
      'form',
      {
        onsubmit: (e) => {
          e.preventDefault();
          if (!ok.disabled) close(input.value.trim());
        },
      },
      h('label', { class: 'dlg-field' }, h('span', { class: 'dlg-label', text: label }), input, hint ? h('span', { class: 'dlg-hint', text: hint }) : null, err),
      h('div', { class: 'dlg-actions' }, h('button', { type: 'button', class: 'btn', onclick: () => close(null) }, t('common.cancel')), ok),
    );
  });
}

// Radio choice; resolves to the chosen value or null. A choice may be
// {disabled: true, hint} (shown, not selectable); `initial` preselects.
export function choiceDialog({ title, message, choices, okText = t('common.ok'), cancelText = t('common.cancel'), initial = null, body = null }) {
  return openDialog(title, (close) => {
    const name = 'c' + Math.random().toString(36).slice(2);
    const first = initial != null ? choices.findIndex((c) => c.value === initial && !c.disabled) : -1;
    const ok = h('button', { type: 'submit', class: 'btn btn-primary', disabled: first < 0 }, okText);
    const radios = choices.map((c, i) =>
      h(
        'label',
        { class: 'dlg-choice' + (c.disabled ? ' is-disabled' : '') },
        h('input', { type: 'radio', name, value: String(i), checked: i === first, disabled: c.disabled, autofocus: i === (first < 0 ? 0 : first), onchange: () => (ok.disabled = false) }),
        h('span', { class: 'dlg-choice-text' }, h('span', { class: 'dlg-choice-label' }, c.label), c.hint ? h('span', { class: 'dlg-choice-hint' }, c.hint) : null),
      ),
    );
    return h(
      'form',
      {
        onsubmit: (e) => {
          e.preventDefault();
          const sel = e.currentTarget.querySelector(`input[name="${name}"]:checked`);
          if (sel) close(choices[Number(sel.value)].value);
        },
      },
      message ? h('p', { class: 'dlg-msg', text: message }) : null,
      body,
      h('div', { class: 'dlg-choices', role: 'radiogroup' }, radios),
      h('div', { class: 'dlg-actions' }, h('button', { type: 'button', class: 'btn', onclick: () => close(null) }, cancelText), ok),
    );
  });
}

// A row of buttons; resolves to the clicked action's value or null.
export function actionsDialog({ title, message, actions, cancelText = t('common.cancel') }) {
  return openDialog(title, (close) =>
    h(
      'div',
      null,
      message ? h('p', { class: 'dlg-msg', text: message }) : null,
      h(
        'div',
        { class: 'dlg-actions' },
        h('button', { type: 'button', class: 'btn', autofocus: true, onclick: () => close(null) }, cancelText),
        actions.map((a) => h('button', { type: 'button', class: a.danger ? 'btn btn-danger-solid' : 'btn btn-primary', onclick: () => close(a.value) }, a.label)),
      ),
    ),
  );
}

// ---- <details class="menu"> dropdowns (transcript sentence menu) ----

export function installMenuCloser() {
  document.addEventListener('click', (e) => {
    for (const d of document.querySelectorAll('details.menu[open]')) if (!d.contains(e.target)) d.open = false;
  });
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    for (const d of document.querySelectorAll('details.menu[open]')) {
      d.open = false;
      const s = d.querySelector('summary');
      if (s && d.contains(document.activeElement)) s.focus();
    }
  });
}

export function closeMenuOf(el) {
  const d = el.closest('details.menu');
  if (d) d.open = false;
}

// ---- clipboard ----

export async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    const ta = h('textarea', { style: { position: 'fixed', top: '0', left: '0', opacity: '0' } });
    ta.value = text;
    document.body.append(ta);
    ta.select();
    let ok = false;
    try {
      ok = document.execCommand('copy');
    } catch {
      ok = false;
    }
    ta.remove();
    return ok;
  }
}

// Starts a download of url (export files) without leaving the page.
export function download(url, name) {
  const a = h('a', { href: url, download: name || '' });
  document.body.append(a);
  a.click();
  a.remove();
}

// ---- file drop ----

function hasFiles(e) {
  return !!e.dataTransfer && Array.from(e.dataTransfer.types || []).includes('Files');
}

// Makes `el` accept dropped files; adds class "drag-over" while hovering.
export function makeDropTarget(el, onFiles) {
  let depth = 0;
  el.addEventListener('dragenter', (e) => {
    if (!hasFiles(e)) return;
    depth++;
    el.classList.add('drag-over');
  });
  el.addEventListener('dragover', (e) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'copy';
  });
  el.addEventListener('dragleave', (e) => {
    if (!hasFiles(e)) return;
    depth = Math.max(0, depth - 1);
    if (depth === 0) el.classList.remove('drag-over');
  });
  el.addEventListener('drop', (e) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    e.stopPropagation();
    depth = 0;
    el.classList.remove('drag-over');
    // desktop app: dropped files carry their local path (SPEC-v3 V1.4, api.js)
    const files = attachDroppedPaths(Array.from(e.dataTransfer.files || []));
    if (files.length) onFiles(files);
  });
}

// A hidden <input type=file> and a function that opens it. In the desktop
// app open() shows the system dialog instead (local files, copied by the
// server: SPEC-v3 V1.4); cancelling it does nothing.
export function filePicker({ accept, multiple, onFiles }) {
  const input = h('input', {
    type: 'file',
    accept,
    multiple,
    class: 'visually-hidden',
    tabindex: '-1',
    'aria-hidden': 'true',
    onchange: () => {
      const files = Array.from(input.files || []);
      input.value = '';
      if (files.length) onFiles(files);
    },
  });
  const open = !DESKTOP
    ? () => input.click()
    : async () => {
        const picked = await pickLocalFiles({ accept, multiple });
        if (picked === null) input.click(); // no system dialog: the browser's
        else if (picked.length) onFiles(multiple ? picked : picked.slice(0, 1));
      };
  return { input, open };
}

// A visible drop zone with a keyboard-accessible "choose files" button.
// target=false when an enclosing element is the drop target instead.
export function dropzone({ text, accept, multiple, buttonText = t('common.choose_files'), onFiles, target = true, compact = false }) {
  const picker = filePicker({ accept, multiple, onFiles });
  const zone = h(
    'div',
    { class: 'dropzone' + (compact ? ' dropzone-compact' : '') },
    icon('upload', { size: 16, class: 'dropzone-icon' }),
    h('span', { class: 'dropzone-text', text }),
    h('button', { type: 'button', class: 'btn btn-sm', onclick: picker.open }, buttonText),
    picker.input,
  );
  if (target) makeDropTarget(zone, onFiles);
  return zone;
}

// Stops the browser from navigating away when a file is dropped outside a
// drop zone (which would leave the app).
export function preventStrayDrops() {
  window.addEventListener('dragover', (e) => {
    if (hasFiles(e)) e.preventDefault();
  });
  window.addEventListener('drop', (e) => {
    if (hasFiles(e)) e.preventDefault();
  });
}

// <input> types that take no typed text: keyboard shortcuts stay active on
// them (a slider just dragged, a checkbox just clicked, SPEC-v2 P6.3).
const NON_TEXT_INPUTS = new Set(['button', 'checkbox', 'color', 'file', 'hidden', 'image', 'radio', 'range', 'reset', 'submit']);

// True when keys typed at el go into a text field: a text-like <input>
// (text, search, number, date, …), a <textarea>, a <select> (it selects
// options by typed letters) or editable content. Sliders, checkboxes,
// radios and buttons are not text fields.
export function isTypingTarget(el) {
  if (!el || el.nodeType !== 1) return false;
  if (el.isContentEditable) return true;
  const tag = el.tagName;
  if (tag === 'TEXTAREA' || tag === 'SELECT') return true;
  if (tag === 'INPUT') return !NON_TEXT_INPUTS.has(String(el.type || 'text').toLowerCase());
  return false;
}
