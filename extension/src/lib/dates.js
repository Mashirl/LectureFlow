// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Recording dates and durations read from a page, normalised to what the
// bridge protocol accepts (Lecture.RecordedAt: "YYYY-MM-DD" or
// "YYYY-MM-DDTHH:MM"). Pure: nothing here depends on the computer's time
// zone.
//
// A recording time is the wall clock the page states, in the zone the page
// states it in, and is never converted to the computer's zone. That zone is
// normally the university's: Panopto names a recording after the lecture
// theatre's clock ("Wed Oct 21 09:35:00 BST 2026"). It is also what
// LectureFlow reads from the name of a file downloaded by hand ("… 09-35-00
// BST 2026.mp4" is 09:35) and what its term calendar counts weeks in, so a
// lecture sent from a computer in another zone (a student following the
// course from abroad) gets the same date, time and teaching week as the
// same recording uploaded as a file.

const MONTHS = ['jan', 'feb', 'mar', 'apr', 'may', 'jun', 'jul', 'aug', 'sep', 'oct', 'nov', 'dec'];

const pad = (n, w = 2) => String(n).padStart(w, '0');

function fmtWall(y, mo, d, h, mi) {
  return `${pad(y, 4)}-${pad(mo)}-${pad(d)}T${pad(h)}:${pad(mi)}`;
}

function validDate(y, mo, d) {
  if (mo < 1 || mo > 12 || d < 1 || d > 31 || y < 1970 || y > 2200) return false;
  const t = new Date(Date.UTC(y, mo - 1, d));
  return t.getUTCMonth() === mo - 1 && t.getUTCDate() === d;
}

function monthIndex(name) {
  const i = MONTHS.indexOf(String(name).slice(0, 3).toLowerCase());
  return i < 0 ? 0 : i + 1;
}

/**
 * Normalises a recording time read from a page to the wall time it states.
 * Accepts:
 *   2026-10-21 | 2026/10/21 | 2026.10.21                → "2026-10-21"
 *   2026-10-21T09:35[:00] | "2026-10-21 09:35"          → "2026-10-21T09:35"
 *   2026-10-21T09:35:00Z | 2026-10-21T09:35:00+01:00    → "2026-10-21T09:35"
 *   "Wed Oct 21 09:35:00 BST 2026", "Oct 21 09:35:00 BST 2026",
 *   "21 Oct 2026 09:35", "Oct 21, 2026 9:35 AM",
 *   "Wed Oct 21 2026 09:35:00 GMT+0100 (…)"             → "2026-10-21T09:35"
 * A zone (an abbreviation, "GMT+1", an RFC 3339 offset or Z) says which
 * clock the time was read from; the time is kept as that clock shows it and
 * the zone is dropped (see the top of this file). Returns "" when nothing
 * fits: a guess would be worse than no date.
 */
export function normalizeRecordedAt(raw) {
  const s = String(raw || '').replace(/\s+/g, ' ').trim();
  if (!s) return '';

  let m = /^(\d{4})[-/.](\d{1,2})[-/.](\d{1,2})$/.exec(s);
  if (m) {
    const [y, mo, d] = [Number(m[1]), Number(m[2]), Number(m[3])];
    return validDate(y, mo, d) ? `${y}-${pad(mo)}-${pad(d)}` : '';
  }
  m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2})(\.\d+)?)?(Z|[+-]\d{2}:\d{2})$/i.exec(s);
  if (m) {
    // Date.parse checks the rest (seconds, the offset's range)
    const [y, mo, d, h, mi] = m.slice(1, 6).map(Number);
    if (!validDate(y, mo, d) || h > 23 || mi > 59 || Number.isNaN(Date.parse(s.toUpperCase()))) return '';
    return fmtWall(y, mo, d, h, mi);
  }
  m = /^(\d{4})[-/.](\d{1,2})[-/.](\d{1,2})[ T](\d{1,2}):(\d{2})(?::\d{2}(?:\.\d+)?)?$/.exec(s);
  if (m) {
    const [y, mo, d, h, mi] = m.slice(1).map(Number);
    if (!validDate(y, mo, d) || h > 23 || mi > 59) return '';
    return fmtWall(y, mo, d, h, mi);
  }

  // Month-name forms, with an optional weekday, time and zone. The zone
  // ("BST", "GMT-5", "(British Summer Time)") is skipped like the weekday.
  const tokens = s.replace(/,/g, ' ').split(' ').filter(Boolean);
  let y = 0, mo = 0, d = 0, h = null, mi = 0, ampm = '';
  for (const tok of tokens) {
    // A month name or its abbreviation ("Oct", "Sept.", "October"); weekday
    // names never start with a month's first three letters.
    if (!mo && /^[A-Za-z]{3,9}\.?$/.test(tok) && monthIndex(tok)) {
      mo = monthIndex(tok);
      continue;
    }
    if (/^(am|pm)$/i.test(tok)) {
      ampm = tok.toLowerCase();
      continue;
    }
    const t = /^(\d{1,2}):(\d{2})(?::\d{2}(?:\.\d+)?)?$/.exec(tok);
    if (t) {
      h = Number(t[1]);
      mi = Number(t[2]);
      continue;
    }
    if (/^\d{4}$/.test(tok)) {
      y = Number(tok);
      continue;
    }
    if (/^\d{1,2}(st|nd|rd|th)?$/i.test(tok) && !d) {
      d = parseInt(tok, 10);
      continue;
    }
    // weekday names, zones and anything else are ignored
  }
  if (!y || !mo || !d || !validDate(y, mo, d)) return '';
  if (h === null) return `${y}-${pad(mo)}-${pad(d)}`;
  if (ampm === 'pm' && h < 12) h += 12;
  if (ampm === 'am' && h === 12) h = 0;
  if (h > 23 || mi > 59) return '';
  return fmtWall(y, mo, d, h, mi);
}

/**
 * Duration in whole seconds from "1:23:45", "83:45", "3300", "3300.5",
 * "PT55M" or "55 min"; 0 when unknown.
 */
export function parseDurationSec(raw) {
  const s = String(raw ?? '').trim();
  if (!s) return 0;
  let m = /^(\d+(?:\.\d+)?)$/.exec(s);
  if (m) return Math.round(Number(m[1]));
  m = /^(?:(\d+):)?(\d{1,2}):(\d{2})(?:\.\d+)?$/.exec(s);
  if (m) return Number(m[1] || 0) * 3600 + Number(m[2]) * 60 + Number(m[3]);
  m = /^PT(?:(\d+)H)?(?:(\d+)M)?(?:(\d+(?:\.\d+)?)S)?$/i.exec(s);
  if (m && (m[1] || m[2] || m[3])) return Math.round(Number(m[1] || 0) * 3600 + Number(m[2] || 0) * 60 + Number(m[3] || 0));
  m = /^(?:(\d+)\s*h(?:ours?|rs?)?)?\s*(?:(\d+)\s*m(?:in(?:utes?|s)?)?)?\s*(?:(\d+)\s*s(?:ec(?:onds?|s)?)?)?$/i.exec(s);
  if (m && (m[1] || m[2] || m[3])) return Number(m[1] || 0) * 3600 + Number(m[2] || 0) * 60 + Number(m[3] || 0);
  return 0;
}
