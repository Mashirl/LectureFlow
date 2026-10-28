// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Requests of ext web3 (web-builder, 2026-10-27), kept out of api.js like
// downloads.js and settings_update.js do: the same conventions (JSON,
// ApiError with the translated text, ?mock= in mock mode), but nothing is
// toasted here — the callers show failures where they happen.
//
//   GET    /api/lectures/{id}/angles/{name}  a second camera angle's video (Range)
//   POST   /api/lectures/{id}/angles         {"path"} (desktop: a local file) or a
//                                            multipart "file" (web mode) → the lecture
//   DELETE /api/lectures/{id}/angles/{name}  → the lecture
//   GET    /api/nmhost                       nmhost.Status (the browser extension)
//   POST   /api/nmhost/register              registers again → nmhost.Status

import { ApiError, errorText, MOCK, MOCK_SCENARIO } from './api.js';
import { t } from './i18n.js';

const enc = encodeURIComponent;

function withMock(path) {
  if (!MOCK) return path;
  return path + (path.includes('?') ? '&' : '?') + 'mock=' + enc(MOCK_SCENARIO);
}

async function request(method, path, body) {
  const init = { method, cache: 'no-store', headers: {} };
  if (body instanceof FormData) {
    init.body = body; // multipart: the browser sets the boundary
  } else if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(withMock(path), init);
  } catch {
    throw new ApiError(0, t('common.net.unreachable'));
  }
  const text = await res.text().catch(() => '');
  let v = null;
  try {
    v = text ? JSON.parse(text) : null;
  } catch {
    v = null;
  }
  if (!res.ok) throw new ApiError(res.status, errorText(res.status, v), v);
  if (text && v === null) throw new ApiError(res.status, t('common.net.bad_response'));
  return v;
}

export const angleURL = (id, name) => withMock(`/api/lectures/${enc(id)}/angles/${enc(name)}`);
export const addAngle = (id, path) => request('POST', `/api/lectures/${enc(id)}/angles`, { path });
export function uploadAngle(id, file) {
  const form = new FormData();
  form.append('file', file, file.name);
  return request('POST', `/api/lectures/${enc(id)}/angles`, form);
}
export const removeAngle = (id, name) => request('DELETE', `/api/lectures/${enc(id)}/angles/${enc(name)}`);
export const nmhostStatus = () => request('GET', '/api/nmhost');
export const nmhostRegister = () => request('POST', '/api/nmhost/register');
