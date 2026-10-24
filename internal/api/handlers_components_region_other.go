// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package api

import (
	"os"
	"path/filepath"
	"strings"
)

// mainlandZones are the IANA zones of mainland China.
var mainlandZones = map[string]bool{
	"Asia/Shanghai": true, "Asia/Chongqing": true, "Asia/Chungking": true, "Asia/Harbin": true,
	"Asia/Urumqi": true, "Asia/Kashgar": true, "PRC": true,
}

// systemRegion returns "cn" when the system is set up for mainland China:
// the locale (LC_ALL, LC_MESSAGES, LANG) zh_CN, or a mainland time zone (TZ
// or the /etc/localtime link) unless the locale names Hong Kong, Macao or
// Taiwan. Only a suggestion for the download sources.
func systemRegion() string {
	locale := ""
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(k); v != "" {
			locale = v
			break
		}
	}
	tz := strings.TrimPrefix(os.Getenv("TZ"), ":")
	if tz == "" {
		if link, err := os.Readlink("/etc/localtime"); err == nil {
			if i := strings.Index(link, "zoneinfo/"); i >= 0 {
				tz = link[i+len("zoneinfo/"):]
			}
		}
	}
	return regionFrom(locale, filepath.ToSlash(tz))
}

// regionFrom decides from a POSIX locale and an IANA time zone.
func regionFrom(locale, tz string) string {
	l := strings.ToLower(locale)
	if i := strings.IndexAny(l, ".@"); i >= 0 {
		l = l[:i]
	}
	l = strings.ReplaceAll(l, "_", "-")
	switch {
	case l == "zh-cn" || l == "zh-hans-cn":
		return "cn"
	case strings.HasSuffix(l, "-hk") || strings.HasSuffix(l, "-mo") || strings.HasSuffix(l, "-tw"):
		return ""
	case mainlandZones[tz]:
		return "cn"
	}
	return ""
}
