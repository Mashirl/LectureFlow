// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// A small bundler for content scripts, which cannot be ES modules: it
// follows an entry module's relative imports and writes one classic script
// in which every module is a function scope returning its exports. Node
// standard library only (AMO wants a reproducible build), and only for the
// module syntax this extension writes:
//   import { a, b as c } from './x.js';     import * as x from './x.js';
//   import './x.js';                        (side effects)
//   export function / async function / class / const / let;  export { a, b as c };
// Anything else (default exports, dynamic import, import.meta, imports of
// packages) is refused, so a new construct fails the build instead of
// shipping broken.

import fs from 'node:fs';
import path from 'node:path';

const IMPORT_RE = /^import\s+(?:\{([\s\S]*?)\}\s+from\s+|\*\s+as\s+([A-Za-z_$][\w$]*)\s+from\s+)?['"]([^'"]+)['"];?[ \t]*$/gm;
const EXPORT_DECL_RE = /^export\s+(async\s+function\s*\*?|function\s*\*?|class|const|let|var)\s+([A-Za-z_$][\w$]*)/gm;
const EXPORT_LIST_RE = /^export\s*\{([^}]*)\}\s*;?[ \t]*$/gm;

function names(list) {
  return list
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
    .map((s) => {
      const m = /^([A-Za-z_$][\w$]*)(?:\s+as\s+([A-Za-z_$][\w$]*))?$/.exec(s);
      if (!m) throw new Error(`bundle: cannot read "${s}"`);
      return { from: m[1], to: m[2] || m[1] };
    });
}

/** Bundles entry (an absolute path) into one classic script; returns its text. */
export function bundleClassic(entry, { banner = '' } = {}) {
  const mods = new Map(); // file → { id, body, deps }
  const order = [];

  function load(file) {
    if (mods.has(file)) {
      if (mods.get(file) === null) throw new Error(`bundle: import cycle at ${file}`);
      return mods.get(file);
    }
    mods.set(file, null);
    let src = fs.readFileSync(file, 'utf8');
    if (/\bimport\s*\(|import\.meta|^export\s+default\b/m.test(src)) throw new Error(`bundle: unsupported module syntax in ${file}`);
    const header = [];
    src = src.replace(IMPORT_RE, (_, named, star, spec) => {
      if (!spec.startsWith('./') && !spec.startsWith('../')) throw new Error(`bundle: only relative imports (${spec} in ${file})`);
      const dep = load(path.resolve(path.dirname(file), spec));
      if (named !== undefined) {
        const parts = names(named).map((n) => (n.from === n.to ? n.from : `${n.from}: ${n.to}`));
        if (parts.length) header.push(`const { ${parts.join(', ')} } = ${dep.id};`);
      } else if (star) header.push(`const ${star} = ${dep.id};`);
      return '';
    });
    if (/^\s*import\s/m.test(src)) throw new Error(`bundle: an import was not understood in ${file}`);
    const exported = [];
    src = src.replace(EXPORT_LIST_RE, (_, list) => {
      for (const n of names(list)) exported.push(`${n.to}: ${n.from}`);
      return '';
    });
    src = src.replace(EXPORT_DECL_RE, (m, kind, name) => {
      exported.push(name);
      return m.replace(/^export\s+/, '');
    });
    if (/^\s*export\s/m.test(src)) throw new Error(`bundle: an export was not understood in ${file}`);
    const mod = { id: `__lf_mod${order.length}`, file, body: [...header, src.trim(), `return { ${exported.join(', ')} };`].join('\n') };
    mods.set(file, mod);
    order.push(mod);
    return mod;
  }

  load(path.resolve(entry));
  const root = path.dirname(path.resolve(entry));
  const parts = order.map((m) => `// ---- ${path.relative(root, m.file).split(path.sep).join('/')}\nconst ${m.id} = (() => {\n${m.body}\n})();`);
  return `${banner}(() => {\n'use strict';\n${parts.join('\n\n')}\n})();\n`;
}
