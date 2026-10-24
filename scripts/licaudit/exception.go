// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The additional permission under AGPL-3.0 section 7 (v3 agpl-exception,
// user decision 2026-10-25) lets LectureFlow be combined with the
// GPL-incompatible libraries that the component manager downloads on the
// user's computer. It lists those libraries one by one, by PyPI package
// name. A package accepted as "user-fetched" that the permission does not
// name would be combined with LectureFlow without permission, so it fails.
const (
	exceptionFile      = "LICENSE-EXCEPTION.md"
	exceptionLibraries = "### The Libraries" // the list in the authoritative English text
)

// auditException runs after auditPython (it needs the user-fetched packages).
func auditException(opt Options, r *Report, _ *allowlist) error {
	seen := map[string]bool{}
	var pkgs []Component
	for _, c := range r.Components {
		if c.Kind == KindPython && c.UserFetched && !seen[strings.ToLower(c.Name)] {
			seen[strings.ToLower(c.Name)] = true
			pkgs = append(pkgs, c)
		}
	}
	if len(pkgs) == 0 {
		return nil
	}
	sort.Slice(pkgs, func(i, j int) bool { return strings.ToLower(pkgs[i].Name) < strings.ToLower(pkgs[j].Name) })
	b, err := os.ReadFile(filepath.Join(opt.Root, exceptionFile))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	list, problem := exceptionList(string(b), err == nil)
	for _, c := range pkgs {
		switch {
		case problem != "":
			r.fail("exception", c.Name, c.Version, c.License, fmt.Sprintf("accepted as user-fetched, but %s; the additional permission under AGPL-3.0 section 7 must name every user-fetched package", problem))
		case !strings.Contains(list, "`"+strings.ToLower(c.Name)+"`"):
			r.fail("exception", c.Name, c.Version, c.License, fmt.Sprintf("accepted as user-fetched, but %s does not name it (`%s`) under %q; LectureFlow would be combined with it without the additional permission under AGPL-3.0 section 7", exceptionFile, c.Name, exceptionLibraries))
		}
	}
	return nil
}

// exceptionList returns the lower-cased "### The Libraries" section of the
// exception text, or why it cannot be read.
func exceptionList(text string, exists bool) (string, string) {
	if !exists {
		return "", exceptionFile + " does not exist"
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	i := strings.Index(text, "\n"+exceptionLibraries+"\n")
	if i < 0 {
		return "", fmt.Sprintf("%s has no %q section", exceptionFile, exceptionLibraries)
	}
	rest := text[i+len(exceptionLibraries)+2:]
	if j := strings.Index(rest, "\n#"); j >= 0 {
		rest = rest[:j]
	}
	return strings.ToLower(rest), ""
}
