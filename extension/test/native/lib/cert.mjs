// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// A throw-away self-signed TLS certificate for the native end-to-end test's
// local HTTPS server, made in memory with the Node standard library (no
// openssl): a P-256 key, an X.509 v3 certificate with the given DNS names,
// valid for two days. The test browser runs with --ignore-certificate-errors
// and only ever reaches 127.0.0.1, so nothing trusts this certificate; it
// exists because the profile's site access is https only, like the store
// build's optional_host_permissions.

import crypto from 'node:crypto';

function derLength(n) {
  if (n < 0x80) return Buffer.from([n]);
  const bytes = [];
  for (let x = n; x > 0; x >>= 8) bytes.unshift(x & 0xff);
  return Buffer.from([0x80 | bytes.length, ...bytes]);
}
const tlv = (tag, body) => Buffer.concat([Buffer.from([tag]), derLength(body.length), body]);
const seq = (...items) => tlv(0x30, Buffer.concat(items));
const set = (...items) => tlv(0x31, Buffer.concat(items));
const ctx = (n, body) => tlv(0xa0 + n, body);
const utf8 = (s) => tlv(0x0c, Buffer.from(s, 'utf8'));
const octets = (b) => tlv(0x04, b);
const bits = (b) => tlv(0x03, Buffer.concat([Buffer.from([0]), b]));
const integer = (b) => tlv(0x02, b[0] & 0x80 ? Buffer.concat([Buffer.from([0]), b]) : b);
const utcTime = (d) => tlv(0x17, Buffer.from(`${d.toISOString().replace(/[-:T]/g, '').slice(2, 14)}Z`, 'ascii'));

function oid(dotted) {
  const p = dotted.split('.').map(Number);
  const out = [40 * p[0] + p[1]];
  for (const v of p.slice(2)) {
    const stack = [v & 0x7f];
    for (let x = v >> 7; x > 0; x >>= 7) stack.unshift((x & 0x7f) | 0x80);
    out.push(...stack);
  }
  return tlv(0x06, Buffer.from(out));
}

const pem = (label, der) => `-----BEGIN ${label}-----\n${der.toString('base64').match(/.{1,64}/g).join('\n')}\n-----END ${label}-----\n`;

/** { cert, key } (PEM) for the DNS names; the first one is the subject's CN. */
export function selfSignedCert(names) {
  const { privateKey, publicKey } = crypto.generateKeyPairSync('ec', { namedCurve: 'prime256v1' });
  const name = seq(set(seq(oid('2.5.4.3'), utf8(names[0]))));
  const ecdsaSha256 = seq(oid('1.2.840.10045.4.3.2'));
  const now = Date.now();
  const san = seq(oid('2.5.29.17'), octets(seq(...names.map((n) => tlv(0x82, Buffer.from(n, 'ascii'))))));
  const tbs = seq(
    ctx(0, integer(Buffer.from([2]))),
    integer(crypto.randomBytes(12)),
    ecdsaSha256,
    name,
    seq(utcTime(new Date(now - 3600e3)), utcTime(new Date(now + 2 * 86400e3))),
    name,
    publicKey.export({ type: 'spki', format: 'der' }),
    ctx(3, seq(san)),
  );
  const signature = crypto.sign('sha256', tbs, privateKey);
  const der = seq(tbs, ecdsaSha256, bits(signature));
  return { cert: pem('CERTIFICATE', der), key: privateKey.export({ type: 'pkcs8', format: 'pem' }) };
}
