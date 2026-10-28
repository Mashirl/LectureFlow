// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Prints the Chrome extension id of the development build (from the "key"
// in manifest.chrome.json) and the origin the native host must allow.

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { chromeExtensionId } from './lib.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const { key } = JSON.parse(fs.readFileSync(path.join(root, 'manifest.chrome.json'), 'utf8'));
const id = chromeExtensionId(key);
console.log(id);
console.log(`chrome-extension://${id}/`);
