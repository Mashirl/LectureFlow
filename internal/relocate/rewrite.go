// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package relocate

import (
	"context"
	"database/sql"
	"fmt"
	"runtime"
	"strings"
)

// The database stores absolute paths (lecture.media_path, material.path,
// and paths inside JSON values, where a Windows backslash is doubled). A
// copied database therefore has the old data directory rewritten to the new
// one in every text value of every table.

// rewriter replaces one directory prefix by another in its three spellings:
// as is, JSON-escaped (\ → \\) and with forward slashes. A match must start
// at a path boundary (start of the value or a character that cannot be part
// of a path name) and end at one (end of the value, a separator, or such a
// character), so C:\data never matches inside C:\data2 or D:\x\C:\data.
type rewriter struct {
	olds, news []string
	fold       bool // ASCII case-insensitive (Windows paths)
}

// pathForms returns the spellings of p (deduplicated, longest first).
func pathForms(p string) []string {
	out := []string{p}
	add := func(s string) {
		for _, o := range out {
			if o == s {
				return
			}
		}
		out = append(out, s)
	}
	if strings.Contains(p, `\`) {
		add(strings.ReplaceAll(p, `\`, `\\`))
		add(strings.ReplaceAll(p, `\`, `/`))
	}
	return out
}

// newRewriter rewrites oldDir to newDir (both absolute and clean). On
// Windows the match ignores ASCII case (drive letters and NTFS names).
func newRewriter(oldDir, newDir string) *rewriter {
	return newRewriterFold(oldDir, newDir, runtime.GOOS == "windows")
}

func newRewriterFold(oldDir, newDir string, fold bool) *rewriter {
	olds := pathForms(oldDir)
	news := make([]string, len(olds))
	for i, o := range olds {
		switch {
		case i == 0:
			news[i] = newDir
		case strings.Contains(o, `\\`):
			news[i] = strings.ReplaceAll(newDir, `\`, `\\`)
		default:
			news[i] = strings.ReplaceAll(newDir, `\`, `/`)
		}
	}
	return &rewriter{olds: olds, news: news, fold: fold}
}

// pathChar reports whether b can continue a path name: a match next to one
// is not at a boundary. Separators are boundaries (a match may be followed
// by \ or / and preceded by nothing path-like).
func pathChar(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '.' || b == '_' || b == '-' || b == '~':
		return true
	case b >= 0x80: // part of a non-ASCII rune
		return true
	}
	return false
}

func lowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + 'a' - 'A'
	}
	return b
}

// hasAt reports whether s[i:] starts with form.
func (r *rewriter) hasAt(s string, i int, form string) bool {
	if len(s)-i < len(form) {
		return false
	}
	if !r.fold {
		return s[i:i+len(form)] == form
	}
	for k := 0; k < len(form); k++ {
		if lowerASCII(s[i+k]) != lowerASCII(form[k]) {
			return false
		}
	}
	return true
}

// matchAt returns the index of the form matching at i on boundaries, or -1.
func (r *rewriter) matchAt(s string, i int) int {
	if i > 0 && (pathChar(s[i-1]) || s[i-1] == '\\' || s[i-1] == '/') {
		return -1
	}
	for k, form := range r.olds {
		if !r.hasAt(s, i, form) {
			continue
		}
		end := i + len(form)
		if end == len(s) || !pathChar(s[end]) {
			return k
		}
	}
	return -1
}

// rewrite returns s with every boundary match replaced, and whether it changed.
func (r *rewriter) rewrite(s string) (string, bool) {
	if !r.mayContain(s) {
		return s, false
	}
	var b strings.Builder
	changed := false
	last := 0
	for i := 0; i < len(s); i++ {
		k := r.matchAt(s, i)
		if k < 0 {
			continue
		}
		b.WriteString(s[last:i])
		b.WriteString(r.news[k])
		i += len(r.olds[k]) - 1
		last = i + 1
		changed = true
	}
	if !changed {
		return s, false
	}
	b.WriteString(s[last:])
	return b.String(), true
}

// residual reports whether s still holds the old prefix on boundaries.
func (r *rewriter) residual(s string) bool {
	if !r.mayContain(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if r.matchAt(s, i) >= 0 {
			return true
		}
	}
	return false
}

// mayContain is a quick filter: some form occurs anywhere in s.
func (r *rewriter) mayContain(s string) bool {
	ls := s
	if r.fold {
		ls = strings.ToLower(s)
	}
	for _, f := range r.olds {
		if r.fold {
			f = strings.ToLower(f)
		}
		if strings.Contains(ls, f) {
			return true
		}
	}
	return false
}

// sqlFilter is a WHERE clause selecting the text values of column col that
// may contain a form (a superset; rewrite decides), with its arguments.
func (r *rewriter) sqlFilter(col string) (string, []any) {
	var parts []string
	var args []any
	for _, f := range r.olds {
		if r.fold {
			parts = append(parts, "instr(lower("+col+"), ?) > 0")
			args = append(args, strings.ToLower(f))
		} else {
			parts = append(parts, "instr("+col+", ?) > 0")
			args = append(args, f)
		}
	}
	return "typeof(" + col + ") = 'text' AND (" + strings.Join(parts, " OR ") + ")", args
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// userTables lists the tables of d (no sqlite_ internals), sorted.
func userTables(ctx context.Context, q queryer) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite\\_%' ESCAPE '\\' ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// columns lists the columns of table t. Every column is visited, whatever
// its declared type: only text values are read (typeof = 'text'), so this
// covers the TEXT columns and any text stored in a column of another
// affinity.
func columns(ctx context.Context, q queryer, t string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", t)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// rewriteDB rewrites every text value of every table of d in one
// transaction and returns the number of values changed.
func rewriteDB(ctx context.Context, d *sql.DB, r *rewriter) (int, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	tables, err := userTables(ctx, tx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range tables {
		cols, err := columns(ctx, tx, t)
		if err != nil {
			return n, err
		}
		for _, c := range cols {
			qc := quoteIdent(c)
			where, args := r.sqlFilter(qc)
			rows, err := tx.QueryContext(ctx, "SELECT rowid, "+qc+" FROM "+quoteIdent(t)+" WHERE "+where, args...)
			if err != nil {
				return n, fmt.Errorf("read %s.%s: %w", t, c, err)
			}
			type upd struct {
				id int64
				v  string
			}
			var ups []upd
			for rows.Next() {
				var id int64
				var v string
				if err := rows.Scan(&id, &v); err != nil {
					rows.Close()
					return n, err
				}
				if nv, ok := r.rewrite(v); ok {
					ups = append(ups, upd{id, nv})
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return n, err
			}
			for _, u := range ups {
				if _, err := tx.ExecContext(ctx, "UPDATE "+quoteIdent(t)+" SET "+qc+" = ? WHERE rowid = ?", u.v, u.id); err != nil {
					return n, fmt.Errorf("rewrite %s.%s: %w", t, c, err)
				}
				n++
			}
		}
	}
	return n, tx.Commit()
}

// findResidual returns "table.column" of the first text value that still
// holds the old prefix ("" = none).
func findResidual(ctx context.Context, d *sql.DB, r *rewriter) (string, error) {
	tables, err := userTables(ctx, d)
	if err != nil {
		return "", err
	}
	for _, t := range tables {
		cols, err := columns(ctx, d, t)
		if err != nil {
			return "", err
		}
		for _, c := range cols {
			qc := quoteIdent(c)
			where, args := r.sqlFilter(qc)
			rows, err := d.QueryContext(ctx, "SELECT "+qc+" FROM "+quoteIdent(t)+" WHERE "+where, args...)
			if err != nil {
				return "", err
			}
			found := false
			for rows.Next() {
				var v string
				if err := rows.Scan(&v); err != nil {
					rows.Close()
					return "", err
				}
				if r.residual(v) {
					found = true
					break
				}
			}
			rows.Close()
			if found {
				return t + "." + c, nil
			}
			if err := rows.Err(); err != nil {
				return "", err
			}
		}
	}
	return "", nil
}
