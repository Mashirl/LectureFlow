// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// For the extension's own pages (popup, settings): reads the language the
// user chose in the settings and the matching _locales catalogs, before
// the page draws anything (src/lib/i18n.js).

import { LANGUAGE_KEY, isLanguage, loadCatalog } from '../lib/i18n.js';
import { useCatalog } from './dom.js';

/** The chosen language's code ("" = the browser's), now in use by t(). */
export async function applyChosenLanguage() {
  const b = globalThis.browser;
  let code = '';
  try {
    code = String((await b.storage.local.get(LANGUAGE_KEY))[LANGUAGE_KEY] || '');
  } catch {
    code = '';
  }
  if (!isLanguage(code)) {
    useCatalog(null);
    return '';
  }
  const catalog = await loadCatalog(code, async (dir) => {
    const res = await fetch(b.runtime.getURL(`_locales/${dir}/messages.json`));
    if (!res.ok) throw new Error(String(res.status));
    return res.json();
  });
  useCatalog(catalog);
  return catalog ? code : '';
}
