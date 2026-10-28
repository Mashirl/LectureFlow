// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The send card (SPEC-ext 界面设计): the popup's content, and the card the
// in-page "发送至 LectureFlow" button opens on a platform's page. One
// implementation, two hosts: the popup (an extension page) and a content
// script's closed shadow DOM (src/content/inpage.js, bundled) — and the
// extension's confirmation window (src/confirm/), which says what the
// in-page card says.
//
// Views (src/lib/popupstate.js decides which): no tracks, LectureFlow not
// connected, ready to send, cannot send (imported, download switch off,
// protected), queued — plus "续传" for a needs_refresh task and the
// do-not-handle notice. Ticks follow src/lib/send.js. The card names tracks
// by id only; the background resolves them, reads cookies and sends.
//
// ask(cmd, extra) talks to the background:
//   detect {force} → { ok, view, origins, offers }
//   connection {recheck} → { ok, connection: { status, appVersion, courses, lastCourseId, os, reason, params } }
//   check → { ok, item, status }
//   send {trackIds, courseId, overrideDisabled, rememberPlatform} → { ok, taskId, position } | { ok: false, error, item }
//   refresh {trackIds} → { ok, taskId, state }
//   open {lectureId | taskId} → { ok }
//   status {taskId} → { ok, status }

import { el, icon, t, formatBytes } from './dom.js';
import { popupView, progressOf } from '../lib/popupstate.js';
import { defaultSelection, toggleSelection, selectionError, sendable, trackName, presetSelection, PRESETS } from '../lib/send.js';

/**
 * Why LectureFlow does not handle a site, by its category on the
 * do-not-handle list (src/lib/blocklist.js): YouTube, or online course
 * platforms; the general sentence for anything else.
 */
export function blockedText(category) {
  if (category === 'youtube') return t('unsupportedYoutube');
  if (category === 'courses') return t('unsupportedCourses');
  return t('unsupportedText');
}

/** A message for an error code from the background or LectureFlow. */
export function errorText(error) {
  const code = (error && error.code) || '';
  const params = (error && error.params) || {};
  switch (code) {
    case 'ext.site_blocked':
      return blockedText(params.category);
    case 'downloads.site_blocked':
      return t('errSiteBlocked');
    case 'course.not_found':
      return t('errCourseGone');
    case 'ext.stale':
      return t('errStale');
    case 'ext.selection.too_many_audio':
    case 'ext.selection.too_many_video':
    case 'enqueue.too_many_tracks':
      return t('oneEach');
    case 'ext.selection.protected':
    case 'enqueue.protected':
      return t('errProtected');
    case 'ext.selection.empty':
    case 'enqueue.no_tracks':
      return t('selectHint');
    case 'ext.no_course':
      return t('courseNone');
    case 'app.not_running':
    case 'ext.host_unavailable':
    case 'ext.closed':
      return t('errNotRunning');
    case 'ext.timeout':
      return t('errTimeout');
    case 'refresh.not_found':
      return t('errRefreshGone');
    case 'ext.info_failed':
      return t('errInfoFailed', [String(params.status || '')]);
    case 'ext.confirm_in_window':
      return t('errConfirmWindow');
    case 'ext.confirm_gone':
      return t('confirmGone');
    case 'request.invalid':
      return t('errInvalid', [String(params.field || '')]);
    default:
      return t('errGeneric', [code || '?']);
  }
}

/**
 * "对这个平台（Panopto）不再提醒" in the reminder before sending a possibly
 * restricted recording (user decision 2026-10-27): only for a platform
 * profile, never for generic detection; unticked unless the user ticks it.
 */
export function rememberBox(name, checked, onChange) {
  const box = el('input', { type: 'checkbox', id: 'override-remember' });
  box.checked = !!checked;
  box.addEventListener('change', () => onChange(box.checked));
  return el('label', { class: 'check-line', id: 'override-remember-line' }, box, el('span', { text: t('restrictedRemember', [name]) }));
}

/**
 * The restricted notice: the user's own text. When the reminder is off for
 * this platform, the same text without its last sentence ("发送前会再提醒一
 * 次"), and a line saying so.
 */
export function restrictedNotice({ remembered, name, banner }) {
  if (!remembered) return [banner(t('restrictedNotice'))];
  return [banner(t('restrictedNoticeQuiet')), el('p', { class: 'small muted', id: 'remembered-note', text: t('restrictedRememberedNote', [name]) })];
}

function kindIcon(kind) {
  return kind === 'video' ? 'video' : kind === 'audio' ? 'headphones' : 'file-unknown';
}

/**
 * createSendCard({ mount, ask, host: 'popup' | 'inpage' | 'window', onView, onGrant, openUrl })
 * Returns { start, onStatus, redetect, state }.
 */
export function createSendCard({ mount, ask, host = 'popup', onView = () => {}, onGrant = null, openUrl = () => {} }) {
  const inpage = host === 'inpage' || host === 'window'; // "确认发送" and its data note
  const S = {
    detection: null,
    origins: [],
    cookieHosts: [], // hosts whose cookies a send reads, offered one by one (popup)
    offers: [],
    conn: null,
    checked: false,
    checkItem: null,
    task: null,
    status: null,
    override: false,
    confirming: false, // the reminder before sending a possibly restricted recording
    remembered: false, // the user turned that reminder off for this platform
    rememberTick: false, // "对这个平台不再提醒", ticked in the open reminder
    preset: '', // main | dual | audio | custom
    picked: false, // the user changed the choice (a preset or a tick)
    refreshChoice: false, // "续传" took the choice the lecture was sent with
    advanced: false, // the full track list ("选择轨道…")
    selection: [],
    courseId: '',
    busy: '',
    message: null,
  };
  const root = el('div', { class: 'lf-card' });
  root.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape' && S.confirming) {
      ev.stopPropagation();
      closeConfirm(false);
    }
  });
  mount.replaceChildren(root);

  const tracks = () => (S.detection && S.detection.status === 'ok' ? S.detection.tracks : []);
  // embedded YouTube or course platform content the detection left out
  const blockedEmbeds = () => (S.detection && S.detection.status === 'ok' && Array.isArray(S.detection.blockedEmbeds) ? S.detection.blockedEmbeds : []);
  const view = () =>
    popupView({ detection: S.detection, connection: S.conn, checked: S.checked, checkItem: S.checkItem, task: S.task, override: S.override });
  const say = (text, tone = '') => {
    S.message = text ? { text, tone } : null;
    render();
  };

  // ---------------------------------------------------------- pieces

  const minutes = (sec) => (sec > 0 ? t('durationMinutes', [String(Math.max(1, Math.round(sec / 60)))]) : '');

  function lectureCard() {
    const d = S.detection;
    const lec = d.lecture || {};
    const generic = !d.profile;
    const title = lec.title && !generic ? el('h1', { class: 'lec-title', text: lec.title }) : el('h1', { class: 'lec-title generated', text: t('titleGenerated') });
    const source = d.profile ? t('profileSource', [d.profile.name, d.profile.version]) : t('sourceGeneric');
    const meta = [lec.recordedAt ? lec.recordedAt.replace('T', ' ') : '', minutes(lec.durationSec), source].filter(Boolean).join(' · ');
    const chips = el('div', { class: 'chips' });
    if (d.profile && !d.profile.reviewed) chips.append(el('span', { class: 'chip chip-idle', dataset: { flag: 'unreviewed' }, text: t('unreviewed') }));
    if (d.downloadPolicy === 'disabled') {
      chips.append(el('span', { class: 'chip chip-warn', dataset: { flag: 'disabled' }, text: S.override ? t('downloadDisabledOverridden') : t('downloadDisabled') }));
    }
    if (d.summary && d.summary.protected > 0) {
      chips.append(el('span', { class: 'chip chip-warn', dataset: { flag: 'protected' } }, icon('lock'), el('span', { text: t('protectedCount', [String(d.summary.protected)]) })));
    }
    const card = el('section', { class: 'card lecture', id: 'lecture' }, title, el('p', { class: 'lec-meta', text: meta }), chips);
    // A profile that reads the page's own response, which was not seen.
    if (d.observe && d.observe.declared && !d.observe.seen) {
      if (!d.authorized && onGrant && S.origins.length) {
        card.append(el('p', { class: 'hint', id: 'observe-hint' }, t('observeNeedsGrant'), el('button', { class: 'link-btn', onclick: onGrant, text: t('grantSite') })));
      } else if (d.authorized) card.append(el('p', { class: 'hint', id: 'observe-hint', text: t('observeNeedsReload') }));
    }
    return card;
  }

  function trackItem(tr, { selectable }) {
    const disabled = !selectable || !sendable(tr);
    const checked = S.selection.includes(tr.id);
    const sub = [tr.hint || (tr.label ? t(`kind_${tr.kind}`) : ''), t(`format_${tr.format}`), tr.label && tr.quality ? tr.quality : '', tr.role ? '' : t('sourceGeneric')].filter(Boolean).join(' · ');
    const chips = [];
    if (tr.protected) chips.push(el('span', { class: 'chip chip-warn' }, icon('lock'), el('span', { text: t('protected') })));
    if (tr.recommended && !tr.protected) chips.push(el('span', { class: 'chip chip-run', text: t('recommended') }));
    if (tr.unreadable) chips.push(el('span', { class: 'chip chip-idle', text: t('unreadable') }));
    const box = el('input', { type: 'checkbox', 'aria-label': t('selectTrack', [trackName(tr, t)]), dataset: { trackId: tr.id } });
    box.checked = checked;
    box.disabled = disabled || !!S.busy;
    box.addEventListener('change', () => onTick(tr, box));
    return el(
      'li',
      { class: 'track', dataset: { kind: tr.kind, format: tr.format, role: tr.role || '', protected: String(!!tr.protected), disabled: String(disabled), checked: String(checked) } },
      el(
        'label',
        { class: 'track-row', title: kindLabel(tr) },
        box,
        el('span', { class: 'track-text' }, el('span', { class: 'track-line' }, el('span', { class: 'track-name', text: trackName(tr, t) }), ...chips), el('span', { class: 'track-sub', text: sub })),
        el('span', { class: 'track-size', text: formatBytes(tr.sizeBytes, tr.sizeEstimated) }),
      ),
    );
  }
  const kindLabel = (tr) => t(`kind_${tr.kind}`);

  const PRESET_KEYS = { main: 'presetMain', dual: 'presetDual', audio: 'presetAudio' };

  /** What the current ticks are, in a line: "完整录像（MP4） · 216 MB". */
  function chosenLine() {
    const picked = tracks().filter((x) => S.selection.includes(x.id));
    if (picked.length === 0) return t('chooseContent');
    const names = picked.map((x) => trackName(x, t)).join(' + ');
    const sizes = picked.map((x) => x.sizeBytes);
    const total = sizes.every((n) => n > 0) ? formatBytes(sizes.reduce((x, y) => x + y, 0), picked.some((x) => x.sizeEstimated)) : '';
    return total ? `${names} · ${total}` : names;
  }

  /**
   * The three choices (user decision: no multiple choice): 主要音视频 /
   * 双机位 / 仅音频; "选择轨道…" opens the full list (advanced).
   */
  function trackSection({ selectable }) {
    const list = tracks();
    const presets = S.detection.presets || null;
    const seg = el('div', { class: 'seg', id: 'presets', role: 'group', 'aria-label': t('contentLabel') });
    for (const p of PRESETS) {
      const ids = presetSelection(list, p, presets);
      const b = el('button', { type: 'button', dataset: { preset: p }, 'aria-pressed': String(S.preset === p), title: ids ? null : t(`${PRESET_KEYS[p]}None`), text: t(PRESET_KEYS[p]) });
      b.disabled = !selectable || !ids || !!S.busy;
      b.addEventListener('click', () => {
        S.preset = p;
        S.selection = ids;
        S.picked = true;
        render();
      });
      seg.append(b);
    }
    const toggle = el(
      'button',
      {
        class: 'link-btn',
        type: 'button',
        id: 'advanced',
        'aria-expanded': String(S.advanced),
        onclick: () => {
          S.advanced = !S.advanced;
          render();
        },
      },
      el('span', { text: S.advanced ? t('hideTracks') : t('chooseTracks', [String(list.length)]) }),
    );
    return el(
      'section',
      { class: 'sec' },
      el('div', { class: 'field-wrap' }, el('span', { class: 'field-label', text: t('contentLabel') }), seg),
      el('div', { class: 'sec-head' }, el('span', { class: 'small muted', id: 'chosen', text: chosenLine() }), toggle),
      S.advanced
        ? el(
            'div',
            { class: 'sec', id: 'advanced-list' },
            el('div', { class: 'sec-head' }, el('h2', { id: 'count', dataset: { count: String(list.length) }, text: t('tracksDetected', [String(list.length)]) }), el('span', { class: 'sec-note', text: t('oneEach') })),
            el('ul', { class: 'tracks', id: 'tracks' }, list.map((tr) => trackItem(tr, { selectable }))),
          )
        : null,
    );
  }

  function courseField(enabled) {
    const c = S.conn || {};
    const courses = c.courses || [];
    if (courses.length === 0) return el('p', { class: 'note', id: 'no-course', text: t('courseNone') });
    const select = el(
      'select',
      { class: 'input', id: 'course', 'aria-label': t('courseLabel') },
      courses.map((x) => el('option', { value: String(x.id), text: String(x.id) === c.lastCourseId ? `${x.name}${t('courseLast')}` : x.name })),
    );
    select.value = S.courseId;
    select.disabled = !enabled || !!S.busy;
    select.addEventListener('change', () => {
      S.courseId = select.value;
    });
    return el('label', { class: 'field-inline' }, el('span', { class: 'field-label', text: t('courseLabel') }), select);
  }

  function primary(textKey, enabled, onclick) {
    const b = el('button', { class: 'btn btn-primary btn-block', id: 'primary', onclick }, el('span', { text: t(textKey) }));
    b.disabled = !enabled;
    return b;
  }

  function banner(text, tone, id, withIcon = false) {
    return el('p', { class: `note${tone ? ' note-' + tone : ''}`, id }, withIcon ? el('span', { class: 'note-icon' }, icon('alert-triangle')) : null, text);
  }

  const platformName = () => (S.detection && S.detection.profile ? S.detection.profile.name : '');
  const restrictedBanners = () => restrictedNotice({ remembered: S.remembered, name: platformName(), banner: (text) => banner(text, 'warn', 'reason', true) });

  function openButton(target) {
    return el('button', { class: 'btn', id: 'open', onclick: () => openInApp(target) }, el('span', { text: t('openInApp') }));
  }

  function stateBlock(iconName, titleText, ...rest) {
    return el('section', { class: 'card state' }, el('span', { class: 'state-icon' }, icon(iconName)), el('h1', { id: 'state-title', text: titleText }), ...rest);
  }

  function sendBlock({ enabled, textKey, onclick, withCourse }) {
    const err = selectionError(tracks(), S.selection, { maxVideo: 2 });
    const canSend = enabled && !err && !S.busy && (!withCourse || !!S.courseId);
    const busyKey = S.busy === 'refresh' ? 'refreshing' : 'sending';
    return el(
      'section',
      { class: 'send' },
      withCourse ? courseField(enabled) : null,
      primary(S.busy ? busyKey : textKey, canSend, onclick),
      el('p', { class: 'data-note', id: 'data-note', text: t(inpage ? 'inpageNote' : 'dataNote') }),
    );
  }

  // ------------------------------------------------------------ views

  const renderLoading = () => [el('section', { class: 'card state' }, el('p', { class: 'muted', id: 'state-title', text: t('detecting') }))];

  const renderUnsupported = (category) => [stateBlock('world', t('unsupportedTitle'), el('p', { class: 'muted', id: 'unsupported-text', dataset: { category: category || '' }, text: blockedText(category) }))];

  function renderNone(reason) {
    const why = reason === 'no_access' ? t('noAccess') : reason === 'not_a_page' ? t('notAPage') : '';
    const offers = S.offers.map((o) =>
      el('p', { class: 'note', dataset: { offer: o.id } }, t(o.embedded ? 'offerEmbedded' : 'offerOpaque', [o.name]), ' ', onGrant ? el('button', { class: 'link-btn', onclick: () => onGrant(o.origins), text: t('offerGrant', [o.name]) }) : null),
    );
    return [
      stateBlock(
        'file-unknown',
        t('noTracks'),
        el('p', { class: 'muted', id: 'empty', text: t('emptyHint') }),
        why ? el('p', { class: 'small faint', text: why }) : null,
        blockedEmbeds().length ? el('p', { class: 'small muted', id: 'blocked-embed', dataset: { categories: blockedEmbeds().join(' ') }, text: t('blockedEmbedNote') }) : null,
        el('div', { class: 'btn-row' }, el('button', { class: 'btn', id: 'redetect', onclick: () => detect(true) }, icon('refresh'), el('span', { text: t('redetect') }))),
      ),
      ...offers,
    ];
  }

  function renderDisconnected() {
    const c = S.conn || {};
    let text = t('discNotInstalled');
    if (c.reason === 'app.not_running' && c.params && (c.params.reason === 'start_failed' || c.params.reason === 'timeout')) text = t('discNotRunning');
    else if (c.reason === 'protocol.unsupported') text = t('discProtocol');
    const unsupportedOs = c.os === 'linux' || c.os === 'cros' || c.os === 'openbsd';
    const n = tracks().length;
    return [
      el('p', { class: 'summary-line', id: 'count', dataset: { count: String(n) }, text: t('tracksDetected', [String(n)]) }),
      stateBlock(
        'plug-connected-x',
        t('disconnectedTitle'),
        el('p', { class: 'muted', id: 'disc-text', text }),
        unsupportedOs ? el('p', { class: 'note note-warn', id: 'os-note', text: t('discUnsupportedOs') }) : null,
        el(
          'div',
          { class: 'btn-row' },
          el('button', { class: 'btn', id: 'recheck', onclick: () => connect(true) }, icon('refresh'), el('span', { text: t('connRecheck') })),
          el('button', { class: 'btn btn-ghost', id: 'get-app', onclick: () => openUrl('https://github.com/ShouldBeQuincy/LectureFlow/releases/latest') }, icon('download'), el('span', { text: t('getLectureFlow') })),
        ),
      ),
    ];
  }

  function renderReady(reason) {
    const out = [lectureCard()];
    if (reason === 'failed') out.push(banner(t('againFailed'), '', 'again'));
    else if (reason === 'cancelled') out.push(banner(t('againCancelled'), '', 'again'));
    if (S.detection.downloadPolicy === 'disabled') out.push(...restrictedBanners());
    out.push(trackSection({ selectable: true }));
    out.push(sendBlock({ enabled: true, textKey: inpage ? 'confirmSend' : 'sendButton', onclick: () => send(), withCourse: true }));
    return out;
  }

  function renderBlocked(reason) {
    const out = [lectureCard()];
    if (reason === 'imported') {
      out.push(banner(t('blockedImported'), 'ok', 'reason'));
      const lectureId = S.checkItem && S.checkItem.lectureId;
      out.push(el('div', { class: 'btn-row' }, openButton(lectureId ? { lectureId } : { taskId: S.checkItem && S.checkItem.taskId })));
      out.push(primary(inpage ? 'confirmSend' : 'sendButton', false, null));
      return out;
    }
    if (reason === 'disabled') {
      // Possibly restricted: nothing chosen at first; the user picks, and
      // is reminded once before it is sent (unless they turned that off
      // for this platform).
      out.push(...restrictedBanners());
      out.push(trackSection({ selectable: true }));
      out.push(sendBlock({ enabled: true, textKey: inpage ? 'confirmSend' : 'sendButton', onclick: () => send(), withCourse: true }));
      return out;
    }
    out.push(banner(t('blockedProtected'), 'warn', 'reason', true));
    out.push(trackSection({ selectable: false }));
    out.push(primary(inpage ? 'confirmSend' : 'sendButton', false, null));
    return out;
  }

  const renderRefresh = () => [
    lectureCard(),
    banner(t('refreshText'), 'warn', 'reason'),
    trackSection({ selectable: true }),
    sendBlock({ enabled: true, textKey: 'refreshButton', onclick: refresh, withCourse: false }),
  ];

  function renderQueued() {
    const taskId = (S.task && S.task.taskId) || (S.checkItem && S.checkItem.taskId) || 0;
    const st = S.status || { state: (S.task && S.task.state) || (S.checkItem && S.checkItem.state) || 'queued', position: (S.task && S.task.position) || 0, bytesDone: 0, bytesTotal: -1 };
    const state = st.state || 'queued';
    const tone = { queued: 'run', downloading: 'run', merging: 'run', importing: 'run', done: 'ok', failed: 'bad', needs_refresh: 'warn', cancelled: 'idle' }[state] || 'run';
    const head = el('div', { class: 'status-head' }, el('span', { class: `chip chip-${tone}`, id: 'task-state', dataset: { state }, text: t(`state_${state}`) }));
    if (state === 'queued' && st.position > 0) head.append(el('span', { class: 'pos', id: 'position', text: t('position', [String(st.position)]) }));
    const p = progressOf(st);
    const fill = el('div', { class: 'bar-fill' });
    if (p.percent >= 0) fill.style.width = `${p.percent}%`;
    const bar = el('div', { class: 'bar', dataset: { indeterminate: String(p.percent < 0 && state !== 'queued' && state !== 'done') } }, fill);
    let detail = '';
    if (state === 'downloading' || state === 'merging' || state === 'importing') {
      detail = p.percent >= 0 ? t('progress', [String(p.percent), formatBytes(p.done), formatBytes(p.total)]) : p.done > 0 ? t('progressUnknown', [formatBytes(p.done)]) : '';
    }
    let note = t('closeHint');
    if (state === 'done') note = t('doneHint');
    else if (state === 'failed') note = st.error ? `${t('failedHint')} (${st.error.code})` : t('failedHint');
    else if (state === 'cancelled') note = t('cancelledHint');
    const card = el(
      'section',
      { class: 'card status', id: 'status', dataset: { state } },
      el('h1', { class: 'lec-title', id: 'state-title', text: t('queuedTitle') }),
      head,
      bar,
      detail ? el('p', { class: 'small muted', id: 'progress', text: detail }) : null,
      el('p', { class: 'small muted', text: note }),
    );
    const target = st.lectureId ? { lectureId: st.lectureId } : { taskId };
    return [lectureCard(), card, el('div', { class: 'btn-row' }, openButton(target))];
  }

  function overlay() {
    if (!S.confirming) return null;
    const dialog = el(
      'section',
      { class: 'card dialog', role: 'alertdialog', 'aria-modal': 'true', 'aria-labelledby': 'override-title' },
      el('h2', { class: 'dialog-title', id: 'override-title' }, el('span', { class: 'dialog-icon' }, icon('alert-triangle')), el('span', { text: t('restrictedTitle') })),
      el('p', { id: 'override-text', text: t('restrictedConfirm') }),
      platformName()
        ? rememberBox(platformName(), S.rememberTick, (on) => {
            S.rememberTick = on;
          })
        : null,
      el(
        'div',
        { class: 'dialog-actions' },
        el('button', { class: 'btn', id: 'override-cancel', onclick: () => closeConfirm(false) }, el('span', { text: t('cancel') })),
        el('button', { class: 'btn btn-warn', id: 'override-confirm', onclick: () => closeConfirm(true) }, el('span', { text: t('sendAnyway') })),
      ),
    );
    return el('div', { class: 'overlay', id: 'override' }, dialog);
  }

  /**
   * "续传" starts with the choice the lecture was sent with (双机位 needs
   * both pictures again), unless the user already chose; once.
   */
  function refreshChoice() {
    if (S.refreshChoice || S.picked || !S.detection || !S.detection.lastPreset) return;
    S.refreshChoice = true;
    const ids = presetSelection(tracks(), S.detection.lastPreset, S.detection.presets || null);
    if (!ids) return;
    S.preset = S.detection.lastPreset;
    S.selection = ids;
  }

  function render() {
    const v = view();
    if (v.view === 'refresh') refreshChoice();
    let parts;
    switch (v.view) {
      case 'unsupported':
        parts = renderUnsupported(v.reason);
        break;
      case 'none':
        parts = renderNone(v.reason);
        break;
      case 'disconnected':
        parts = renderDisconnected();
        break;
      case 'ready':
        parts = renderReady(v.reason);
        break;
      case 'blocked':
        parts = renderBlocked(v.reason);
        break;
      case 'refresh':
        parts = renderRefresh();
        break;
      case 'queued':
        parts = renderQueued();
        break;
      default:
        parts = renderLoading();
    }
    if (S.message) parts.push(el('p', { class: `note${S.message.tone ? ' note-' + S.message.tone : ''}`, id: 'msg', role: 'status', text: S.message.text }));
    parts.push(overlay());
    root.dataset.view = v.view;
    root.dataset.reason = v.reason || '';
    root.replaceChildren(...parts.filter(Boolean));
    if (S.confirming) {
      const c = root.querySelector('#override-cancel');
      if (c) c.focus();
    }
    onView(v, S);
  }

  // ---------------------------------------------------------- actions

  function onTick(tr, box) {
    S.selection = toggleSelection(tracks(), S.selection, tr.id, box.checked);
    const same = PRESETS.find((p) => {
      const ids = presetSelection(tracks(), p, S.detection.presets || null);
      return ids && ids.length === S.selection.length && ids.every((x) => S.selection.includes(x));
    });
    S.preset = same || 'custom';
    S.picked = true;
    render();
  }

  function closeConfirm(confirmed) {
    S.confirming = false;
    if (confirmed) {
      S.override = true;
      send({ remember: S.rememberTick && !!platformName() });
      return;
    }
    render();
  }

  async function send({ remember = false } = {}) {
    if (S.busy) return;
    if (S.detection.downloadPolicy === 'disabled' && !S.override) {
      if (S.remembered) {
        // The user turned the reminder off for this platform: the override
        // goes with the send, no reminder.
        S.override = true;
      } else {
        // SPEC-ext: warn before overriding the platform's switch (once).
        S.confirming = true;
        S.rememberTick = false;
        render();
        return;
      }
    }
    S.busy = 'send';
    S.message = null;
    render();
    const r = await ask('send', { trackIds: S.selection, courseId: S.courseId, overrideDisabled: S.override, rememberPlatform: remember });
    S.busy = '';
    if (remember) S.remembered = true;
    if (r.ok) {
      S.task = { taskId: r.taskId, state: 'queued', position: r.position || 0 };
      S.status = { taskId: r.taskId, state: 'queued', position: r.position || 0, bytesDone: 0, bytesTotal: -1 };
      if (S.conn) S.conn.lastCourseId = S.courseId;
      render();
      pullStatus(r.taskId);
      return;
    }
    if (r.error && r.error.code === 'queue.duplicate') {
      const p = r.error.params || {};
      S.checked = true;
      S.checkItem = r.item || { sessionId: '', state: p.state, taskId: p.taskId, lectureId: p.lectureId || '' };
      if (S.checkItem.state !== 'imported' && S.checkItem.state !== 'needs_refresh') S.task = { taskId: S.checkItem.taskId, state: S.checkItem.state, position: 0 };
      render();
      return;
    }
    if (r.error && r.error.code === 'course.not_found') connect(true);
    say(errorText(r.error), 'bad');
  }

  async function refresh() {
    if (S.busy) return;
    S.busy = 'refresh';
    S.message = null;
    render();
    const r = await ask('refresh', { trackIds: S.selection });
    S.busy = '';
    if (r.ok) {
      S.task = { taskId: r.taskId, state: r.state || 'queued', position: 0 };
      S.status = { taskId: r.taskId, state: r.state || 'queued', position: 0, bytesDone: 0, bytesTotal: -1 };
      render();
      pullStatus(r.taskId);
      return;
    }
    say(errorText(r.error), 'bad');
  }

  async function openInApp(target) {
    const r = await ask('open', target);
    say(r.ok ? t('opened') : errorText(r.error), r.ok ? 'ok' : 'bad');
  }

  async function pullStatus(taskId) {
    const r = await ask('status', { taskId });
    if (r.ok && r.status) onStatus(r.status);
  }

  function onStatus(status) {
    if (!status) return;
    const taskId = (S.task && S.task.taskId) || (S.checkItem && S.checkItem.taskId);
    if (!taskId || status.taskId !== taskId) return;
    if (status.state === 'needs_refresh') {
      // The credentials ran out while the card is open: offer "续传".
      S.task = null;
      S.status = null;
      S.checkItem = { ...(S.checkItem || {}), taskId, state: 'needs_refresh' };
      render();
      return;
    }
    S.status = status;
    if (!S.task) S.task = { taskId, state: status.state, position: status.position || 0 };
    else S.task.state = status.state;
    if (status.state === 'done' && status.lectureId && S.checkItem) S.checkItem.lectureId = status.lectureId;
    render();
  }

  // ---------------------------------------------------------- loading

  async function detect(force = false) {
    const r = await ask('detect', { force });
    if (!r.ok) S.detection = { status: 'no_access' };
    else {
      S.detection = r.view;
      S.origins = r.origins || [];
      S.cookieHosts = r.cookieHosts || [];
      S.offers = r.offers || [];
    }
    if (!r.ok && r.error) S.message = { text: errorText(r.error), tone: 'bad' };
    if (S.detection.status === 'ok') {
      S.selection = defaultSelection(tracks(), { downloadPolicy: S.detection.downloadPolicy, presets: S.detection.presets || null });
      S.preset = S.selection.length ? 'main' : '';
      S.picked = false;
      S.refreshChoice = false;
      S.override = false;
      S.remembered = !!S.detection.remembered;
    }
    S.checked = false;
    S.checkItem = null;
    render();
    await check();
  }

  async function connect(recheck = false) {
    S.conn = { status: 'checking' };
    render();
    const r = await ask('connection', { recheck });
    S.conn = r.ok ? r.connection : { status: 'unavailable', reason: r.error && r.error.code };
    if (S.conn.status === 'connected') {
      const ids = (S.conn.courses || []).map((c) => String(c.id));
      const last = S.conn.lastCourseId || '';
      S.courseId = ids.includes(S.courseId) ? S.courseId : ids.includes(last) ? last : ids[0] || '';
    }
    render();
    await check();
  }

  // The session's place in LectureFlow, once both detection and hello are in.
  let checking = null;
  async function check() {
    if (!S.detection || S.detection.status !== 'ok' || tracks().length === 0) return;
    if (!S.conn || S.conn.status !== 'connected' || S.checked || checking) return;
    checking = ask('check');
    const r = await checking;
    checking = null;
    S.checked = true;
    if (r.ok) {
      S.checkItem = r.item || null;
      if (r.item && r.status) S.status = r.status;
    } else S.checkItem = null; // could not check: the send path checks again
    render();
  }

  async function start() {
    render();
    await Promise.all([detect(true), connect(false)]);
  }

  return { start, onStatus, redetect: () => detect(true), state: S, render };
}
