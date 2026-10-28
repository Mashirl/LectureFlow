// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Static checks of the source tree and the build: manifests and
// permissions, design tokens, icons, SPDX headers, no colour literals in
// the UI, schemas the validator can read.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { extractTokens, resolveLightDark, tokensCss, chromeExtensionId, mergeManifest, renderIcon, svgChildren, encodePng, zlibStored, crc32 } from '../../scripts/lib.mjs';
import { inflateSync } from 'node:zlib';
import { validate, assertSupported, SchemaError } from '../../src/lib/jsonschema.js';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const repo = path.resolve(root, '..');
const read = (...p) => fs.readFileSync(path.join(root, ...p), 'utf8');
const json = (...p) => JSON.parse(read(...p));

function walk(dir, out = []) {
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) walk(p, out);
    else out.push(p);
  }
  return out;
}

test('manifests: the permission table of SPEC-ext plus scripting; no <all_urls>', () => {
  const base = json('manifest.base.json');
  assert.equal(base.manifest_version, 3);
  assert.deepEqual([...base.permissions].sort(), ['activeTab', 'cookies', 'nativeMessaging', 'scripting', 'storage', 'webRequest']);
  assert.deepEqual(base.optional_host_permissions, ['https://*/*']);
  assert.equal(base.incognito, 'not_allowed');
  for (const f of ['manifest.base.json', 'manifest.chrome.json', 'manifest.firefox.json']) {
    const text = read(f);
    assert.ok(!text.includes('<all_urls>'), f);
    assert.ok(!/"host_permissions"/.test(text), `${f}: host access only through optional permissions`);
    assert.ok(!/webRequestBlocking|"tabs"|"history"|"downloads"/.test(text), f);
  }
  const chrome = mergeManifest(base, json('manifest.chrome.json'));
  assert.deepEqual(chrome.background, { service_worker: 'background/main.js', type: 'module' });
  assert.match(chrome.key, /^MIIBIjAN[A-Za-z0-9+/=]+$/); // an RSA public key, never a private one
  assert.equal(chromeExtensionId(chrome.key), 'lbfjnfhpnaafingpjkoefobbjnmheehl');
  const firefox = mergeManifest(base, json('manifest.firefox.json'));
  assert.deepEqual(firefox.background, { scripts: ['background/main.js'], type: 'module' });
  assert.equal(firefox.browser_specific_settings.gecko.id, 'lectureflow@qu1ncy.dev');
  assert.equal(firefox.browser_specific_settings.gecko.strict_min_version, '140.0');
  assert.deepEqual(firefox.browser_specific_settings.gecko.data_collection_permissions.required, ['authenticationInfo', 'browsingActivity', 'websiteContent']);
  assert.equal(firefox.key, undefined);
  // no private key anywhere in the extension
  for (const f of walk(root).filter((p) => !p.includes(`${path.sep}node_modules${path.sep}`) && !p.includes(`${path.sep}dist${path.sep}`))) {
    if (/\.(png|ico)$/.test(f)) continue;
    assert.ok(!new RegExp('PRIVATE' + ' KEY').test(fs.readFileSync(f, 'utf8')), f);
  }
});

test('Chrome extension id from a key: the documented algorithm', () => {
  // first 32 hex digits of SHA-256 of the DER key, 0-f mapped to a-p
  const id = chromeExtensionId(Buffer.from('test').toString('base64'));
  assert.match(id, /^[a-p]{32}$/);
  // sha256("test") = 9f86d081884c7d659a2feaa0c55ad015a3bf…
  assert.equal(id, 'jpignaibiiemhngfjkcpokkamffknabf');
});

test('design tokens: light and dark sets from web/style.css with the same keys', () => {
  const t = extractTokens(fs.readFileSync(path.join(repo, 'web', 'style.css'), 'utf8'));
  assert.deepEqual(Object.keys(t.light), Object.keys(t.dark));
  assert.ok(Object.keys(t.light).length > 50);
  assert.equal(t.light['--accent'], '#1f6feb');
  assert.equal(t.dark['--accent'], '#3b8cff');
  assert.equal(t.light['--r-md'], '8px');
  assert.equal(t.light['--r-lg'], '12px');
  for (const v of [...Object.values(t.light), ...Object.values(t.dark)]) assert.ok(!v.includes('light-dark('), v);
  assert.ok(t.langRules.some((r) => r.startsWith('[lang]:lang(zh-CN)') && r.includes('Microsoft YaHei') && r.includes('PingFang SC')));
  assert.equal(resolveLightDark('0 1px light-dark(rgb(1 2 3 / 0.1), rgb(4 5 6)), x light-dark(#fff, #000)', 1), '0 1px rgb(4 5 6), x #000');
  const css = tokensCss(t);
  assert.match(css, /@media \(prefers-color-scheme: dark\)/);
});

test('UI code carries no colour literals and no inline scripts', () => {
  const ui = [...walk(path.join(root, 'src', 'popup')), ...walk(path.join(root, 'src', 'options')), ...walk(path.join(root, 'src', 'confirm')), ...walk(path.join(root, 'src', 'ui')), path.join(root, 'src', 'content', 'inpage.js'), path.join(root, 'src', 'content', 'inpage.css')];
  const colour = /#[0-9a-fA-F]{3,8}\b|\brgba?\(|\bhsla?\(|\b(white|black|red|blue|green|gray|grey|orange|purple)\b(?=\s*[;}"'])/;
  for (const f of ui) {
    const text = fs.readFileSync(f, 'utf8').replace(/^(\/\/|\/\*|<!--).*$/gm, '');
    assert.ok(!colour.test(text), `${path.relative(root, f)}: ${text.match(colour)}`);
    if (f.endsWith('.html')) {
      assert.ok(!/<script(?![^>]*\bsrc=)[^>]*>/.test(text), `${f}: inline script`);
      assert.ok(!/\son[a-z]+=/.test(text), `${f}: inline handler`);
    }
  }
  // no innerHTML or eval anywhere in the extension's code
  for (const f of walk(path.join(root, 'src')).filter((p) => p.endsWith('.js') && !p.includes(`${path.sep}vendor${path.sep}`))) {
    const text = fs.readFileSync(f, 'utf8');
    assert.ok(!/\.innerHTML\s*=|\beval\(|new Function\(/.test(text), path.relative(root, f));
  }
});

test('every source file starts with the SPDX header (vendored files excepted)', () => {
  const files = walk(root).filter((p) => /\.(js|mjs|cjs|css|html)$/.test(p) && !/[\\/](node_modules|dist|vendor)[\\/]/.test(p));
  assert.ok(files.length > 30);
  for (const f of files) {
    const head = fs.readFileSync(f, 'utf8').split(/\r?\n/).slice(0, 6).join('\n');
    assert.ok(head.includes('SPDX-License-Identifier: AGPL-3.0-or-later'), path.relative(root, f));
  }
  // the vendored polyfill is byte-for-byte the published file
  const poly = fs.readFileSync(path.join(root, 'src', 'vendor', 'browser-polyfill.js'));
  const sha = (b) => import('node:crypto').then((c) => c.createHash('sha256').update(b).digest('hex'));
  return sha(poly).then((h) => assert.equal(h, 'cfc53c3525587467c783ef1c6fdefbaf1d60ed197ea2fe0d45899b18069cf496'));
});

test('locales: the same keys in every language, placeholders declared', () => {
  const en = json('_locales', 'en', 'messages.json');
  const zh = json('_locales', 'zh_CN', 'messages.json');
  assert.deepEqual(Object.keys(zh).sort(), Object.keys(en).sort());
  for (const [k, v] of Object.entries(en)) {
    for (const m of v.message.matchAll(/\$([A-Z0-9_]+)\$/g)) assert.ok(v.placeholders?.[m[1].toLowerCase()], `${k}: ${m[0]}`);
  }
  // every key the UI asks for exists
  const used = new Set();
  for (const f of [...walk(path.join(root, 'src', 'popup')), ...walk(path.join(root, 'src', 'options')), ...walk(path.join(root, 'src', 'confirm')), ...walk(path.join(root, 'src', 'ui')), ...walk(path.join(root, 'src', 'content'))]) {
    const text = fs.readFileSync(f, 'utf8');
    for (const m of text.matchAll(/data-i18n="([A-Za-z_]+)"|\bt\('([A-Za-z_]+)'/g)) used.add(m[1] || m[2]);
  }
  for (const k of used) assert.ok(en[k], k);
});

test('schemas use only what the validator supports, and the validator works', () => {
  for (const f of ['profile.schema.json', 'report.schema.json']) assertSupported(json('schema', f));
  assert.throws(() => validate({ type: 'object', if: {} }, {}), SchemaError);
  const s = { type: 'object', additionalProperties: false, required: ['a'], properties: { a: { type: 'integer', minimum: 1 }, b: { oneOf: [{ type: 'string' }, { type: 'array', items: { type: 'string' }, minItems: 1 }] } } };
  assert.deepEqual(validate(s, { a: 1, b: ['x'] }), []);
  assert.ok(validate(s, { a: 0 }).length);
  assert.ok(validate(s, { a: 1, c: 2 }).some((e) => e.path === '/c'));
  assert.ok(validate(s, { a: 1, b: [] }).length);
  assert.ok(validate(s, { a: 1.5 }).length);
  assert.ok(validate(s, {}).some((e) => e.path === '/a'));
});

test('icons: the mark renders to valid PNGs; Tabler SVGs become data', () => {
  const png = renderIcon(16, '#1f6feb', '#ffffff');
  assert.deepEqual([...png.subarray(0, 8)], [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
  assert.equal(png.readUInt32BE(16), 16);
  assert.equal(encodePng(1, 1, new Uint8Array([1, 2, 3, 4])).length > 40, true);
  // stored zlib blocks: valid for any inflater, identical bytes everywhere
  const big = Buffer.alloc(70000, 7);
  assert.deepEqual(inflateSync(zlibStored(big)), big);
  assert.deepEqual(inflateSync(zlibStored(Buffer.alloc(0))), Buffer.alloc(0));
  assert.equal(crc32(Buffer.from('123456789')), 0xcbf43926);
  const kids = svgChildren(read('src', 'vendor', 'tabler', 'lock.svg'));
  assert.equal(kids.length, 3);
  assert.ok(kids.every(([tag, a]) => tag === 'path' && a.d));
});

test('the build writes both targets, with drafts left out and tokens in place', async () => {
  const { execFileSync } = await import('node:child_process');
  execFileSync(process.execPath, [path.join(root, 'scripts', 'build.mjs')], { cwd: root, stdio: 'pipe' });
  for (const t of ['chrome', 'firefox']) {
    const dir = path.join(root, 'dist', t);
    const m = JSON.parse(fs.readFileSync(path.join(dir, 'manifest.json'), 'utf8'));
    assert.equal(m.version, json('package.json').version);
    assert.equal(m.host_permissions, undefined);
    for (const f of ['background/main.js', 'popup/popup.html', 'options/options.html', 'confirm/confirm.html', 'confirm/confirm.js', 'content/observe.js', 'content/observe-main.js', 'content/inpage.js', 'ui/card.css', 'ui/inpage-css.js', 'ui/tokens.css', 'ui/tokens.js', 'ui/icons.js', 'build-info.js', 'icons/icon-128.png', 'schema/profile.schema.json', 'profiles/index.json', 'profiles/LICENSE', 'vendor/browser-polyfill.js', 'vendor/NOTICE', '_locales/zh_CN/messages.json']) {
      assert.ok(fs.existsSync(path.join(dir, f)), `${t}: ${f}`);
    }
    assert.ok(!fs.existsSync(path.join(dir, 'vendor', 'tabler', 'lock.svg')));
    assert.ok(!fs.existsSync(path.join(dir, 'lib', 'replay.js')));
    assert.ok(!fs.existsSync(path.join(dir, 'content', 'inpage.css')), 'inlined into ui/inpage-css.js');
    // content scripts are classic scripts: the bundle has no module syntax left
    const inpage = fs.readFileSync(path.join(dir, 'content', 'inpage.js'), 'utf8');
    assert.ok(inpage.startsWith('// SPDX-License-Identifier: AGPL-3.0-or-later'));
    assert.ok(!/^\s*(import|export)\s/m.test(inpage), 'no import or export statements');
    new (await import('node:vm')).Script(inpage);
    assert.match(fs.readFileSync(path.join(dir, 'ui', 'inpage-css.js'), 'utf8'), /:host\b/);
    const index = JSON.parse(fs.readFileSync(path.join(dir, 'profiles', 'index.json'), 'utf8'));
    const expected = fs.readdirSync(path.join(root, 'profiles')).filter((f) => f.endsWith('.json') && !JSON.parse(read('profiles', f)).draft).sort();
    assert.deepEqual(index.profiles, expected);
    assert.ok(index.profiles.includes('panopto.json'));
    assert.match(fs.readFileSync(path.join(dir, 'build-info.js'), 'utf8'), new RegExp(`"target":"${t}"`));
  }
});

test('store packages: deterministic zips, Chrome without the development key', async () => {
  const { execFileSync } = await import('node:child_process');
  const { packageTarget } = await import('../../scripts/package.mjs');
  execFileSync(process.execPath, [path.join(root, 'scripts', 'build.mjs')], { cwd: root, stdio: 'pipe' });
  const first = fs.readFileSync(packageTarget('chrome'));
  execFileSync(process.execPath, [path.join(root, 'scripts', 'build.mjs')], { cwd: root, stdio: 'pipe' });
  const second = fs.readFileSync(packageTarget('chrome'));
  assert.ok(first.equals(second), 'two builds, same bytes');
  // read the zip back: every entry's CRC matches, the manifest has no key
  const eocd = first.lastIndexOf(Buffer.from([0x50, 0x4b, 0x05, 0x06]));
  const count = first.readUInt16LE(eocd + 10);
  let p = first.readUInt32LE(eocd + 16);
  const names = [];
  for (let i = 0; i < count; i++) {
    const nameLen = first.readUInt16LE(p + 28);
    const name = first.toString('utf8', p + 46, p + 46 + nameLen);
    const local = first.readUInt32LE(p + 42);
    const size = first.readUInt32LE(local + 18);
    const data = first.subarray(local + 30 + first.readUInt16LE(local + 26), local + 30 + first.readUInt16LE(local + 26) + size);
    assert.equal(crc32(data), first.readUInt32LE(p + 16), name);
    if (name === 'manifest.json') assert.equal(JSON.parse(data.toString('utf8')).key, undefined);
    names.push(name);
    p += 46 + nameLen;
  }
  assert.ok(names.includes('background/main.js') && names.includes('manifest.json'));
  assert.deepEqual(names, [...names].sort());
  const ff = fs.readFileSync(packageTarget('firefox'));
  assert.ok(ff.includes(Buffer.from('lectureflow@qu1ncy.dev')));
});

test('the content-script bundler: named, aliased and star imports, export lists; refuses what it cannot do', async () => {
  const { bundleClassic } = await import('../../scripts/bundle.mjs');
  const os = await import('node:os');
  const vm = await import('node:vm');
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-bundle-'));
  const file = (name, ...lines) => fs.writeFileSync(path.join(dir, name), lines.join('\n') + '\n');
  try {
    file('a.js', 'export const A = 1;', 'export function twice(x) {', '  return x * 2;', '}', 'const hidden = 5;', 'export { hidden as five };');
    file('b.js', "import { A, twice as dbl } from './a.js';", "import * as all from './a.js';", 'export class B {', '  get v() {', '    return dbl(A) + all.five;', '  }', '}');
    file('entry.js', 'import {', '  B,', "} from './b.js';", 'globalThis.result = new B().v;');
    const out = bundleClassic(path.join(dir, 'entry.js'), { banner: '// banner\n' });
    assert.ok(out.startsWith('// banner'));
    assert.ok(!/^\s*(import|export)\s/m.test(out));
    const ctx = vm.createContext({});
    vm.runInContext(out, ctx);
    assert.equal(ctx.result, 7);
    file('bad.js', 'export default 1;');
    assert.throws(() => bundleClassic(path.join(dir, 'bad.js')), /unsupported/);
    file('pkg.js', "import { x } from 'some-package';");
    assert.throws(() => bundleClassic(path.join(dir, 'pkg.js')), /relative/);
    file('dyn.js', "const m = import('./a.js');");
    assert.throws(() => bundleClassic(path.join(dir, 'dyn.js')), /unsupported/);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
