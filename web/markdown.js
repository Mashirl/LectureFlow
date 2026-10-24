// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// A small Markdown renderer for the SYLLABUS.md preview (v1.1 A8.1 step 3,
// A8.2 course-page editor). It builds DOM nodes and only ever sets text
// through text nodes — never innerHTML — so HTML in the source (from the
// model or the user) is shown as text, not executed. The one exception is
// <br>, which LLMs often put in table cells: it becomes a <br> element.
//
// Supported: ATX headings, paragraphs, bullet and numbered lists (nested by
// indentation, tolerant of the 2/3/4-space mixes LLMs write), GFM pipe
// tables with alignment (outer pipes optional), blockquotes, fenced code,
// horizontal rules; inline **bold**, *italic* / _italic_, ~~strike~~,
// `code`, backslash escapes, common HTML entities, [links](url "title"),
// <autolinks> and bare http(s) URLs. Only http, https and mailto links are
// made clickable; anything else is shown as plain text. Images show their
// alt text (nothing is loaded from elsewhere).

const LIST_RE = /^( *)([-*+]|\d{1,9}[.)])(?: +(.*))?$/;
const HEADING_RE = /^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$/;
const HR_RE = /^ {0,3}([-*_])(?:[ \t]*\1){2,}[ \t]*$/;
const FENCE_RE = /^ {0,3}(`{3,}|~{3,})(.*)$/;
const QUOTE_RE = /^ {0,3}> ?(.*)$/;
const TABLE_DELIM_RE = /^ *\|? *:?-+:? *(?:\| *:?-+:? *)*\|? *$/;
const PUNCT = '\\`*_{}[]()#+-.!|~<>"\'&';
const ENTITIES = {
  amp: '&', lt: '<', gt: '>', quot: '"', apos: "'", nbsp: '\u00a0', copy: '©', reg: '®', trade: '™',
  ndash: '–', mdash: '—', hellip: '…', lsquo: '‘', rsquo: '’', ldquo: '“', rdquo: '”', laquo: '«', raquo: '»',
  middot: '·', bull: '•', times: '×', divide: '÷', deg: '°', plusmn: '±', le: '≤', ge: '≥', ne: '≠', rarr: '→', larr: '←',
};

function el(tag, cls) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  return e;
}

// renderMarkdown(src) → DocumentFragment
export function renderMarkdown(src) {
  const lines = String(src || '')
    .replace(/\r\n?/g, '\n')
    .replace(/\t/g, '    ')
    .split('\n');
  const frag = document.createDocumentFragment();
  blocks(lines, frag);
  return frag;
}

function isBlank(line) {
  return !line || !line.trim();
}

function indentOf(line) {
  return line.length - line.trimStart().length;
}

// A GFM table starts with a header row and a delimiter row that both have
// pipes and the same number of cells.
function isTableStart(lines, i) {
  if (i + 1 >= lines.length) return false;
  const head = lines[i];
  const delim = lines[i + 1];
  if (!head.includes('|') || !delim.includes('|') || !delim.includes('-') || !TABLE_DELIM_RE.test(delim)) return false;
  return splitRow(head).length === splitRow(delim).length;
}

// Starts a block that ends a table (GFM: "the beginning of another block").
function startsOtherBlock(line) {
  return HEADING_RE.test(line) || HR_RE.test(line) || FENCE_RE.test(line) || QUOTE_RE.test(line) || LIST_RE.test(line);
}

// Starts a block that may interrupt a paragraph. As in CommonMark, a
// numbered list only does when it starts at 1 ("2026. Year" goes on as
// text), and an empty list item never does.
function interrupts(lines, i) {
  const line = lines[i];
  if (HEADING_RE.test(line) || HR_RE.test(line) || FENCE_RE.test(line) || QUOTE_RE.test(line) || isTableStart(lines, i)) return true;
  const m = LIST_RE.exec(line);
  return !!m && !!m[3] && (!/\d/.test(m[2]) || parseInt(m[2], 10) === 1);
}

function blocks(lines, out) {
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (isBlank(line)) {
      i++;
      continue;
    }
    let m;
    if ((m = FENCE_RE.exec(line))) {
      const fence = m[1];
      const closeRe = new RegExp(`^ {0,3}${fence[0] === '`' ? '`' : '~'}{${fence.length},}[ \\t]*$`);
      const body = [];
      i++;
      while (i < lines.length && !closeRe.test(lines[i])) {
        body.push(lines[i]);
        i++;
      }
      i++; // closing fence (or end of input)
      const pre = el('pre', 'md-pre');
      const code = el('code');
      code.textContent = body.join('\n');
      pre.append(code);
      out.append(pre);
      continue;
    }
    if (HR_RE.test(line)) {
      // before lists: "- - -" is a rule, not a list item (CommonMark)
      out.append(el('hr'));
      i++;
      continue;
    }
    if ((m = HEADING_RE.exec(line))) {
      const hd = el('h' + m[1].length);
      inline(m[2] || '', hd);
      out.append(hd);
      i++;
      continue;
    }
    if (isTableStart(lines, i)) {
      i = table(lines, i, out);
      continue;
    }
    if (QUOTE_RE.test(line)) {
      const inner = [];
      while (i < lines.length && !isBlank(lines[i]) && (m = QUOTE_RE.exec(lines[i]))) {
        inner.push(m[1]);
        i++;
      }
      const bq = el('blockquote');
      blocks(inner, bq);
      out.append(bq);
      continue;
    }
    if (LIST_RE.test(line)) {
      i = list(lines, i, out);
      continue;
    }
    // paragraph
    const para = [];
    while (i < lines.length && !isBlank(lines[i]) && (para.length === 0 || !interrupts(lines, i))) {
      para.push(lines[i].trimStart());
      i++;
    }
    const p = el('p');
    inlineLines(para, p);
    out.append(p);
  }
}

// Paragraph lines: a soft break is a line break inside the paragraph only
// when the line ends with two spaces or a backslash (CommonMark hard break).
function inlineLines(lines, parent) {
  lines.forEach((line, k) => {
    const last = k === lines.length - 1;
    let text = line;
    let hard = false;
    if (!last && / {2,}$/.test(text)) hard = true;
    else if (!last && /\\$/.test(text)) {
      text = text.slice(0, -1);
      hard = true;
    }
    inline(text.trimEnd(), parent);
    if (!last) parent.append(hard ? el('br') : document.createTextNode(' '));
  });
}

// list(lines, i, out, parentInd) parses a list whose items are indented
// more than parentInd (the parent item's marker, -1 at the top level).
// LLMs mix 2-, 3- and 4-space nesting, so the rule is simpler than
// CommonMark's: an item indented at least 2 columns past the current
// item's marker opens (or continues) its sub-list; anything less, but
// still past the parent's marker, is a sibling. Returns the next line.
function list(lines, i, out, parentInd = -1) {
  const first = LIST_RE.exec(lines[i]);
  const ordered = /\d/.test(first[2]);
  const node = el(ordered ? 'ol' : 'ul');
  if (ordered) {
    const start = parseInt(first[2], 10);
    if (start !== 1) node.setAttribute('start', String(start));
  }
  let li = null;
  let cur = -1; // marker indentation of the current item
  let paras = 0; // paragraphs already in the current item
  let buf = [];
  // The item's first paragraph goes straight into the <li> (tight list);
  // later ones (after a blank line) are <p> elements.
  const flush = () => {
    if (li && buf.length) {
      const target = paras ? el('p') : document.createDocumentFragment();
      inlineLines(buf, target);
      li.append(target);
      paras++;
    }
    buf = [];
  };
  while (i < lines.length) {
    const line = lines[i];
    if (isBlank(line)) {
      // The list goes on after blank lines when the next line still belongs to it.
      let j = i + 1;
      while (j < lines.length && isBlank(lines[j])) j++;
      if (j >= lines.length || !li || HR_RE.test(lines[j])) break;
      const ind = indentOf(lines[j]);
      const goesOn = LIST_RE.test(lines[j]) ? ind > parentInd && (ind >= cur + 2 || sameKind(lines[j], ordered)) : ind >= cur + 2;
      if (!goesOn) break;
      flush();
      i = j;
      continue;
    }
    const m = LIST_RE.exec(line);
    const ind = indentOf(line);
    if (m) {
      if (HR_RE.test(line) || ind <= parentInd) break; // a rule, or an item of an outer list
      if (li && ind >= cur + 2) {
        flush();
        i = list(lines, i, li, cur);
        continue;
      }
      if (!sameKind(line, ordered)) break;
      flush();
      li = el('li');
      node.append(li);
      cur = ind;
      paras = 0;
      buf = m[3] ? [m[3].trimStart()] : [];
      i++;
      continue;
    }
    // continuation: indented under the item, or a lazy line of its paragraph
    if (li && (ind >= cur + 2 || (buf.length && !interrupts(lines, i)))) {
      buf.push(line.trimStart());
      i++;
      continue;
    }
    break;
  }
  flush();
  out.append(node);
  return i;
}

function sameKind(line, ordered) {
  const m = LIST_RE.exec(line);
  return !!m && /\d/.test(m[2]) === ordered;
}

// The end of the code span whose opening backtick run starts at i (the
// index after the closing run), or -1 when the run is not closed.
function codeEnd(text, i) {
  let n = 1;
  while (text[i + n] === '`') n++;
  const ticks = '`'.repeat(n);
  let j = i + n;
  for (;;) {
    j = text.indexOf(ticks, j);
    if (j < 0) return -1;
    let run = n;
    while (text[j + run] === '`') run++;
    if (run === n) return j + n;
    j += run;
  }
}

// Table cells: outer pipes are optional, \| is a literal pipe, and a pipe
// inside a (closed) code span does not split the cell.
function splitRow(line) {
  let s = line.trim();
  if (s.startsWith('|')) s = s.slice(1);
  if (s.endsWith('|') && !s.endsWith('\\|')) s = s.slice(0, -1);
  const cells = [];
  let cur = '';
  for (let k = 0; k < s.length; k++) {
    const c = s[k];
    if (c === '\\' && s[k + 1] === '|') {
      cur += '\\|'; // unescaped by inline()
      k++;
    } else if (c === '`') {
      const e = codeEnd(s, k);
      if (e > 0) {
        cur += s.slice(k, e).replace(/\\\|/g, '|'); // GFM: \| is a pipe in code too
        k = e - 1;
      } else {
        cur += c;
      }
    } else if (c === '|') {
      cells.push(cur.trim());
      cur = '';
    } else {
      cur += c;
    }
  }
  cells.push(cur.trim());
  return cells;
}

function table(lines, i, out) {
  const head = splitRow(lines[i]);
  const aligns = splitRow(lines[i + 1]).map((d) => {
    const l = d.startsWith(':');
    const r = d.endsWith(':');
    return l && r ? 'center' : r ? 'right' : l ? 'left' : '';
  });
  i += 2;
  const wrap = el('div', 'md-table-wrap');
  const tbl = el('table');
  const thead = el('thead');
  const tr = el('tr');
  head.forEach((c, k) => tr.append(cell('th', c, aligns[k])));
  thead.append(tr);
  tbl.append(thead);
  const tbody = el('tbody');
  while (i < lines.length && !isBlank(lines[i]) && lines[i].includes('|') && !startsOtherBlock(lines[i])) {
    const cells = splitRow(lines[i]);
    const row = el('tr');
    for (let k = 0; k < head.length; k++) row.append(cell('td', cells[k] || '', aligns[k]));
    tbody.append(row);
    i++;
  }
  tbl.append(tbody);
  wrap.append(tbl);
  out.append(wrap);
  return i;
}

function cell(tag, text, align) {
  const c = el(tag);
  if (align) c.style.textAlign = align;
  inline(text, c);
  return c;
}

function safeHref(url) {
  const u = url.trim().replace(/\\([!-/:-@[-`{-~])/g, '$1');
  return /^(https?:\/\/|mailto:)\S+$/i.test(u) ? u : null;
}

function anchor(href) {
  const a = el('a');
  a.setAttribute('href', href);
  a.target = '_blank';
  a.rel = 'noopener noreferrer';
  return a;
}

const isWordChar = (c) => !!c && /[\p{L}\p{N}]/u.test(c);

// Finds the closing delimiter for an emphasis run starting at i. Code spans
// and escaped characters in between are skipped, so `a*b` inside *…* does
// not end the emphasis.
function closing(text, i, delim) {
  let j = i + delim.length;
  if (j >= text.length || /\s/.test(text[j])) return -1;
  while (j < text.length) {
    const c = text[j];
    if (c === '\\') {
      j += 2;
      continue;
    }
    if (c === '`') {
      const e = codeEnd(text, j);
      if (e > 0) {
        j = e;
        continue;
      }
    }
    if (text.startsWith(delim, j) && j > i + delim.length && !/\s/.test(text[j - 1])) {
      // a single * or _ must not be half of a double one
      if (delim.length === 1 && text[j + 1] === delim) {
        j += 2;
        continue;
      }
      if (delim === '_' && isWordChar(text[j + 1])) {
        j++;
        continue;
      }
      return j;
    }
    j++;
  }
  return -1;
}

// parseLink(text, i) reads [label](destination "title") with text[i] === '['.
// Brackets in the label may nest; the destination may contain balanced
// parentheses or be written as <…>. Returns {label, url, end} or null.
function parseLink(text, i) {
  let depth = 0;
  let j = i;
  for (; j < text.length; j++) {
    const c = text[j];
    if (c === '\\') {
      j++;
      continue;
    }
    if (c === '`') {
      const e = codeEnd(text, j);
      if (e > 0) {
        j = e - 1;
        continue;
      }
    }
    if (c === '[') depth++;
    else if (c === ']' && --depth === 0) break;
  }
  if (j >= text.length || text[j + 1] !== '(') return null;
  const label = text.slice(i + 1, j);
  let k = j + 2;
  while (text[k] === ' ') k++;
  let url;
  if (text[k] === '<') {
    const e = text.indexOf('>', k);
    if (e < 0) return null;
    url = text.slice(k + 1, e);
    k = e + 1;
  } else {
    const s = k;
    let par = 0;
    for (; k < text.length; k++) {
      const c = text[k];
      if (c === '\\' && k + 1 < text.length) {
        k++;
        continue;
      }
      if (/\s/.test(c)) break;
      if (c === '(') par++;
      else if (c === ')') {
        if (par === 0) break;
        par--;
      }
    }
    url = text.slice(s, k);
  }
  while (text[k] === ' ') k++;
  const q = text[k];
  if (q === '"' || q === "'" || (q === '(' && k > j + 2)) {
    const e = text.indexOf(q === '(' ? ')' : q, k + 1);
    if (e < 0) return null;
    k = e + 1;
    while (text[k] === ' ') k++;
  }
  if (text[k] !== ')') return null;
  return { label, url, end: k + 1 };
}

// A bare http(s) URL at i (GFM autolink extension): trailing punctuation
// and an unbalanced closing parenthesis are not part of it.
function bareURL(text, i) {
  const m = /^https?:\/\/[^\s<>`]+/i.exec(text.slice(i));
  if (!m) return null;
  let url = m[0];
  const count = (s, ch) => s.split(ch).length - 1;
  for (;;) {
    const last = url[url.length - 1];
    if (/[?!.,:;*_~'"]/.test(last)) url = url.slice(0, -1);
    else if (last === ')' && count(url, '(') < count(url, ')')) url = url.slice(0, -1);
    else break;
  }
  return /^https?:\/\/[^/?#]/i.test(url) ? url : null;
}

// An HTML entity at i: {ch, len}, or null (then '&' is a plain character).
function entity(text, i) {
  const m = /^&(?:#(\d{1,7})|#[xX]([0-9a-fA-F]{1,6})|([A-Za-z][A-Za-z0-9]{1,31}));/.exec(text.slice(i, i + 40));
  if (!m) return null;
  let ch;
  if (m[3]) ch = ENTITIES[m[3]];
  else {
    const cp = m[1] ? parseInt(m[1], 10) : parseInt(m[2], 16);
    ch = cp > 0 && cp <= 0x10ffff && !(cp >= 0xd800 && cp <= 0xdfff) ? String.fromCodePoint(cp) : '\ufffd';
  }
  return ch == null ? null : { ch, len: m[0].length };
}

// inline(text, parent, inLink): appends the inline nodes of text to parent.
// Inside a link label no further links are made.
function inline(text, parent, inLink = false) {
  let buf = '';
  const flush = () => {
    if (buf) parent.append(document.createTextNode(buf));
    buf = '';
  };
  let i = 0;
  while (i < text.length) {
    const c = text[i];
    if (c === '\\' && i + 1 < text.length && PUNCT.includes(text[i + 1])) {
      buf += text[i + 1];
      i += 2;
      continue;
    }
    if (c === '`') {
      const e = codeEnd(text, i);
      let n = 1;
      while (text[i + n] === '`') n++;
      if (e > 0) {
        flush();
        const code = el('code');
        code.textContent = text.slice(i + n, e - n).replace(/^ (.*) $/, '$1');
        parent.append(code);
        i = e;
        continue;
      }
      buf += text.slice(i, i + n);
      i += n;
      continue;
    }
    if (c === '<') {
      const br = /^<br\s*\/?>/i.exec(text.slice(i, i + 8));
      if (br) {
        flush();
        parent.append(el('br'));
        i += br[0].length;
        continue;
      }
      const auto = /^<((?:https?:\/\/|mailto:)[^\s<>]+)>/i.exec(text.slice(i));
      const href = auto && !inLink && safeHref(auto[1]);
      if (href) {
        flush();
        const a = anchor(href);
        a.append(document.createTextNode(auto[1]));
        parent.append(a);
        i += auto[0].length;
        continue;
      }
    }
    if (c === '&') {
      const ent = entity(text, i);
      if (ent) {
        buf += ent.ch;
        i += ent.len;
        continue;
      }
    }
    if (c === '!' && text[i + 1] === '[') {
      const lk = parseLink(text, i + 1);
      if (lk) {
        flush();
        inline(lk.label, parent, true); // image: its alt text only
        i = lk.end;
        continue;
      }
    }
    if (c === '[') {
      const lk = parseLink(text, i);
      if (lk) {
        flush();
        const href = !inLink && safeHref(lk.url);
        if (href) {
          const a = anchor(href);
          inline(lk.label, a, true);
          parent.append(a);
        } else {
          inline(lk.label, parent, inLink);
        }
        i = lk.end;
        continue;
      }
    }
    if ((c === 'h' || c === 'H') && !inLink && !isWordChar(text[i - 1])) {
      const url = bareURL(text, i);
      if (url) {
        flush();
        const a = anchor(url);
        a.append(document.createTextNode(url));
        parent.append(a);
        i += url.length;
        continue;
      }
    }
    const two = text.slice(i, i + 2);
    if (two === '**' || two === '__' || two === '~~') {
      const j = two === '__' && isWordChar(text[i - 1]) ? -1 : closing(text, i, two);
      if (j > 0) {
        flush();
        const node = el(two === '~~' ? 'del' : 'strong');
        inline(text.slice(i + 2, j), node, inLink);
        parent.append(node);
        i = j + 2;
        continue;
      }
      buf += two;
      i += 2;
      continue;
    }
    if (c === '*' || c === '_') {
      const j = c === '_' && isWordChar(text[i - 1]) ? -1 : closing(text, i, c);
      if (j > 0) {
        flush();
        const em = el('em');
        inline(text.slice(i + 1, j), em, inLink);
        parent.append(em);
        i = j + 1;
        continue;
      }
    }
    buf += c;
    i++;
  }
  flush();
}
