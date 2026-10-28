// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// The second view of a lecture on the lecture page (ext web3; user request
// 2026-10-27 no. 4: a second video stream can be added to an existing
// lecture). The ⋯ menu (and the header's right-click menu) offers
// "添加第二视角…" for a lecture without one, "替换第二视角…" and
// "移除第二视角" for a lecture with one. The video needs no sound: the
// player shows it as picture in picture next to the lecture's own video
// (player_angles.js).
//
//   desktop app  the system file dialog; the path is sent
//                (POST /api/lectures/{id}/angles {"path"}), the server reads
//                the file itself
//   web mode     the browser's file dialog; the file is uploaded (multipart)
//   remove       asks first (DELETE /api/lectures/{id}/angles/{name})
//
// The server answers the lecture with its angles; failures (upload.*,
// angle.*) are shown as toasts. While a file is being saved a toast stays
// up (a large video takes a while) and the menu offers nothing for that
// lecture. Nothing is changed optimistically.

import { h, toast, confirmDialog } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import { DESKTOP, pickLocalFiles, localPath } from './api.js';
import { addAngle, uploadAngle, removeAngle } from './api_ext.js';
import { lectureAngles } from './player_angles.js';
import { MEDIA_ACCEPT, isMediaFile, listJoin, MEDIA_EXT_LIST } from './util.js';

// The media extensions (util.js): the server refuses any other file
// (upload.not_media, security review 2 M3), so the dialogs offer only these.
const ACCEPT = MEDIA_ACCEPT;
const busy = new Set(); // lecture IDs with a request under way

// A toast that stays until the request ends.
function busyToast(text) {
  const root = document.getElementById('toasts');
  if (!root) return () => {};
  const el = h('div', { class: 'toast toast-info', role: 'status' }, icon('loader-circle', { size: 16, class: 'toast-icon spin' }), h('span', { class: 'toast-msg', text }));
  root.append(el);
  return () => el.remove();
}

// The browser's file dialog (web mode); null when cancelled.
function pickBrowserFile() {
  return new Promise((resolve) => {
    const input = h('input', { type: 'file', accept: ACCEPT, hidden: true });
    const done = (f) => {
      input.remove();
      resolve(f);
    };
    input.addEventListener('change', () => done((input.files && input.files[0]) || null));
    input.addEventListener('cancel', () => done(null));
    document.body.append(input);
    input.click();
  });
}

async function saveSecondView(lec, replace, onLecture) {
  let send;
  let name;
  if (DESKTOP) {
    const files = await pickLocalFiles({ accept: ACCEPT, multiple: false });
    const path = files && files.length ? localPath(files[0]) : '';
    if (!path) return; // cancelled
    name = files[0].name;
    send = () => addAngle(lec.id, path);
  } else {
    const file = await pickBrowserFile();
    if (!file) return;
    name = file.name;
    send = () => uploadAngle(lec.id, file);
  }
  // the system dialog also offers "all files": refuse other types here, before sending
  if (!isMediaFile({ name })) {
    toast(t('lecture.angle.not_media', { name, exts: listJoin(MEDIA_EXT_LIST) }), 'warn');
    return;
  }
  busy.add(lec.id);
  const done = busyToast(t('lecture.angle.saving', { name }));
  try {
    const l = await send();
    toast(t(replace ? 'lecture.angle.replaced' : 'lecture.angle.added'), 'success');
    if (l && onLecture) onLecture(l);
  } catch (e) {
    toast(t('lecture.angle.failed', { error: e.message }), 'error');
  } finally {
    busy.delete(lec.id);
    done();
  }
}

async function removeSecondView(lec, name, onLecture) {
  const ok = await confirmDialog({ title: t('lecture.angle.remove_title'), message: t('lecture.angle.remove_msg'), okText: t('lecture.angle.remove_ok'), danger: true });
  if (!ok) return;
  busy.add(lec.id);
  try {
    const l = await removeAngle(lec.id, name);
    toast(t('lecture.angle.removed'), 'success');
    if (l && onLecture) onLecture(l);
  } catch (e) {
    toast(t('lecture.angle.remove_failed', { error: e.message }), 'error');
  } finally {
    busy.delete(lec.id);
  }
}

// The ⋯ menu's items for the second view ([] while a request runs).
export function angleMenuItems(lec, onLecture) {
  if (!lec || busy.has(lec.id)) return [];
  const angles = lectureAngles(lec);
  if (!angles.length) return ['sep', { id: 'add_angle', label: t('lecture.angle.add'), icon: 'upload', onSelect: () => saveSecondView(lec, false, onLecture) }];
  return [
    'sep',
    { id: 'replace_angle', label: t('lecture.angle.replace'), icon: 'upload', onSelect: () => saveSecondView(lec, true, onLecture) },
    { id: 'remove_angle', label: t('lecture.angle.remove'), icon: 'x', onSelect: () => removeSecondView(lec, angles[0].name, onLecture) },
  ];
}
