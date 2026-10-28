// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sysprobe

// Linux readings from /proc (plain parsers; the platform file reads the
// files).

import (
	"strconv"
	"strings"
)

// parseMeminfo returns MemTotal and MemAvailable in MiB.
func parseMeminfo(s string) (total, avail int, ok bool) {
	var t, a int64 = -1, -1
	for _, l := range strings.Split(s, "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			t = v
		case "MemAvailable:":
			a = v
		}
	}
	if t < 0 || a < 0 {
		return 0, 0, false
	}
	return int(t >> 10), int(a >> 10), true
}

// cpuTimes are the jiffies of /proc/stat's "cpu" line: busy and total.
type cpuTimes struct{ busy, total uint64 }

// parseProcStat reads the aggregate cpu line.
func parseProcStat(s string) (cpuTimes, bool) {
	for _, l := range strings.Split(s, "\n") {
		f := strings.Fields(l)
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var vals []uint64
		for _, x := range f[1:] {
			v, err := strconv.ParseUint(x, 10, 64)
			if err != nil {
				return cpuTimes{}, false
			}
			vals = append(vals, v)
		}
		var t cpuTimes
		for i, v := range vals {
			if i >= 8 { // guest and guest_nice are already in user / nice
				break
			}
			t.total += v
			if i != 3 && i != 4 { // idle, iowait
				t.busy += v
			}
		}
		return t, true
	}
	return cpuTimes{}, false
}

// cpuPercent is the busy share between two readings (0–100, -1 unknown).
func cpuPercent(prev, cur cpuTimes) int {
	if cur.total <= prev.total || cur.busy < prev.busy {
		return -1
	}
	pct := int(float64(cur.busy-prev.busy)*100/float64(cur.total-prev.total) + 0.5)
	return min(max(pct, 0), 100)
}

// parseProcStatus returns VmRSS of /proc/<pid>/status in KiB and the ppid.
func parseProcStatus(s string) (ppid int, rssKB int64, ok bool) {
	ppid = -1
	for _, l := range strings.Split(s, "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "PPid:":
			if v, err := strconv.Atoi(f[1]); err == nil {
				ppid = v
			}
		case "VmRSS:":
			if v, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				rssKB = v
			}
		}
	}
	return ppid, rssKB, ppid >= 0
}
