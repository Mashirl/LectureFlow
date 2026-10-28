// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// A JSON Schema (2020-12) validator for the subset the extension's own
// schemas use. The same code checks a locally imported profile in the
// extension and every profile and fixture in the tests, so the schema file
// stays the single source of truth. An unsupported keyword is an error, not
// silently ignored.

const ANNOTATIONS = new Set(['$schema', '$id', '$comment', 'title', 'description', 'examples', 'default', 'deprecated', 'readOnly', 'writeOnly', '$defs', 'format']);
const SUPPORTED = new Set([
  ...ANNOTATIONS,
  '$ref', 'type', 'enum', 'const',
  'properties', 'required', 'additionalProperties', 'patternProperties', 'propertyNames', 'minProperties', 'maxProperties',
  'items', 'minItems', 'maxItems', 'uniqueItems',
  'pattern', 'minLength', 'maxLength',
  'minimum', 'maximum', 'exclusiveMinimum', 'exclusiveMaximum', 'multipleOf',
  'oneOf', 'anyOf', 'allOf', 'not',
]);

export class SchemaError extends Error {}

function typeOf(v) {
  if (v === null) return 'null';
  if (Array.isArray(v)) return 'array';
  if (typeof v === 'number') return Number.isInteger(v) ? 'integer' : 'number';
  return typeof v;
}

function typeMatches(want, v) {
  const t = typeOf(v);
  if (want === t) return true;
  if (want === 'number' && t === 'integer') return true;
  return false;
}

function deepEqual(a, b) {
  if (a === b) return true;
  if (typeof a !== typeof b || a === null || b === null || typeof a !== 'object') return false;
  if (Array.isArray(a) !== Array.isArray(b)) return false;
  const ka = Object.keys(a);
  const kb = Object.keys(b);
  if (ka.length !== kb.length) return false;
  return ka.every((k) => deepEqual(a[k], b[k]));
}

function pointer(path) {
  return path.length === 0 ? '/' : '/' + path.map((p) => String(p).replace(/~/g, '~0').replace(/\//g, '~1')).join('/');
}

/**
 * Validates value against schema. Returns a list of { path, message }
 * (empty when valid). Throws SchemaError for a schema it cannot handle.
 */
export function validate(schema, value) {
  const errors = [];
  const root = schema;
  const regexCache = new Map();
  const re = (p) => {
    if (!regexCache.has(p)) regexCache.set(p, new RegExp(p, 'u'));
    return regexCache.get(p);
  };

  function resolveRef(ref) {
    if (!ref.startsWith('#/')) throw new SchemaError(`unsupported $ref ${ref}`);
    let node = root;
    for (const part of ref.slice(2).split('/')) {
      const key = part.replace(/~1/g, '/').replace(/~0/g, '~');
      node = node?.[key];
      if (node === undefined) throw new SchemaError(`unresolved $ref ${ref}`);
    }
    return node;
  }

  function check(s, v, path, out) {
    if (s === true) return;
    if (s === false) {
      out.push({ path: pointer(path), message: 'not allowed' });
      return;
    }
    for (const k of Object.keys(s)) if (!SUPPORTED.has(k)) throw new SchemaError(`unsupported keyword ${k} at ${pointer(path)}`);
    if (s.$ref) check(resolveRef(s.$ref), v, path, out);

    if (s.type !== undefined) {
      const types = Array.isArray(s.type) ? s.type : [s.type];
      if (!types.some((t) => typeMatches(t, v))) {
        out.push({ path: pointer(path), message: `must be ${types.join(' or ')}` });
        return;
      }
    }
    if (s.const !== undefined && !deepEqual(s.const, v)) out.push({ path: pointer(path), message: `must be ${JSON.stringify(s.const)}` });
    if (s.enum && !s.enum.some((e) => deepEqual(e, v))) out.push({ path: pointer(path), message: `must be one of ${s.enum.map((e) => JSON.stringify(e)).join(', ')}` });

    if (typeof v === 'string') {
      const len = [...v].length;
      if (s.minLength !== undefined && len < s.minLength) out.push({ path: pointer(path), message: `shorter than ${s.minLength}` });
      if (s.maxLength !== undefined && len > s.maxLength) out.push({ path: pointer(path), message: `longer than ${s.maxLength}` });
      if (s.pattern !== undefined && !re(s.pattern).test(v)) out.push({ path: pointer(path), message: `does not match ${s.pattern}` });
    }
    if (typeof v === 'number') {
      if (s.minimum !== undefined && v < s.minimum) out.push({ path: pointer(path), message: `less than ${s.minimum}` });
      if (s.maximum !== undefined && v > s.maximum) out.push({ path: pointer(path), message: `greater than ${s.maximum}` });
      if (s.exclusiveMinimum !== undefined && v <= s.exclusiveMinimum) out.push({ path: pointer(path), message: `not greater than ${s.exclusiveMinimum}` });
      if (s.exclusiveMaximum !== undefined && v >= s.exclusiveMaximum) out.push({ path: pointer(path), message: `not less than ${s.exclusiveMaximum}` });
      if (s.multipleOf !== undefined && Math.abs(v / s.multipleOf - Math.round(v / s.multipleOf)) > 1e-9) out.push({ path: pointer(path), message: `not a multiple of ${s.multipleOf}` });
    }
    if (Array.isArray(v)) {
      if (s.minItems !== undefined && v.length < s.minItems) out.push({ path: pointer(path), message: `fewer than ${s.minItems} items` });
      if (s.maxItems !== undefined && v.length > s.maxItems) out.push({ path: pointer(path), message: `more than ${s.maxItems} items` });
      if (s.uniqueItems && v.some((x, i) => v.findIndex((y) => deepEqual(x, y)) !== i)) out.push({ path: pointer(path), message: 'items must be unique' });
      if (s.items !== undefined) v.forEach((x, i) => check(s.items, x, [...path, i], out));
    }
    if (v !== null && typeof v === 'object' && !Array.isArray(v)) {
      const keys = Object.keys(v);
      if (s.minProperties !== undefined && keys.length < s.minProperties) out.push({ path: pointer(path), message: `fewer than ${s.minProperties} properties` });
      if (s.maxProperties !== undefined && keys.length > s.maxProperties) out.push({ path: pointer(path), message: `more than ${s.maxProperties} properties` });
      for (const r of s.required || []) if (!Object.hasOwn(v, r)) out.push({ path: pointer([...path, r]), message: 'is required' });
      const props = s.properties || {};
      const pats = Object.entries(s.patternProperties || {});
      for (const k of keys) {
        if (s.propertyNames !== undefined) check(s.propertyNames, k, [...path, k], out);
        let matched = false;
        if (Object.hasOwn(props, k)) {
          matched = true;
          check(props[k], v[k], [...path, k], out);
        }
        for (const [p, ps] of pats) {
          if (re(p).test(k)) {
            matched = true;
            check(ps, v[k], [...path, k], out);
          }
        }
        if (!matched && s.additionalProperties !== undefined) {
          if (s.additionalProperties === false) out.push({ path: pointer([...path, k]), message: 'is not allowed' });
          else check(s.additionalProperties, v[k], [...path, k], out);
        }
      }
    }
    if (s.allOf) s.allOf.forEach((sub) => check(sub, v, path, out));
    if (s.anyOf) {
      const ok = s.anyOf.some((sub) => {
        const e = [];
        check(sub, v, path, e);
        return e.length === 0;
      });
      if (!ok) out.push({ path: pointer(path), message: 'matches none of anyOf' });
    }
    if (s.oneOf) {
      const results = s.oneOf.map((sub) => {
        const e = [];
        check(sub, v, path, e);
        return e;
      });
      const n = results.filter((e) => e.length === 0).length;
      if (n !== 1) {
        if (n === 0) {
          // Report the branch with the fewest errors: usually the intended one.
          const best = results.reduce((a, b) => (b.length < a.length ? b : a));
          out.push({ path: pointer(path), message: 'matches none of oneOf' }, ...best);
        } else out.push({ path: pointer(path), message: 'matches more than one of oneOf' });
      }
    }
    if (s.not !== undefined) {
      const e = [];
      check(s.not, v, path, e);
      if (e.length === 0) out.push({ path: pointer(path), message: 'must not match' });
    }
  }

  check(schema, value, [], errors);
  return errors;
}

/** Throws SchemaError if the schema uses a keyword validate() lacks. */
export function assertSupported(schema) {
  const walk = (s, path) => {
    if (typeof s === 'boolean') return;
    if (s === null || typeof s !== 'object') throw new SchemaError(`bad schema at ${path}`);
    for (const k of Object.keys(s)) if (!SUPPORTED.has(k)) throw new SchemaError(`unsupported keyword ${k} at ${path}`);
    for (const k of ['properties', 'patternProperties', '$defs']) for (const [n, sub] of Object.entries(s[k] || {})) walk(sub, `${path}/${k}/${n}`);
    for (const k of ['items', 'additionalProperties', 'not', 'propertyNames']) if (s[k] !== undefined && typeof s[k] === 'object') walk(s[k], `${path}/${k}`);
    for (const k of ['oneOf', 'anyOf', 'allOf']) (s[k] || []).forEach((sub, i) => walk(sub, `${path}/${k}/${i}`));
  };
  walk(schema, '#');
}
