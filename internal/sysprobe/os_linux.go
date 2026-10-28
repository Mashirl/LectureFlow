// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build linux

package sysprobe

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func memInfo(context.Context) (total, avail int, ok bool) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	return parseMeminfo(string(b))
}

type cpuState struct {
	prev cpuTimes
	have bool
}

func (c *cpuState) percent(context.Context) int {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return -1
	}
	cur, ok := parseProcStat(string(b))
	if !ok {
		return -1
	}
	pct := -1
	if c.have {
		pct = cpuPercent(c.prev, cur)
	}
	c.prev, c.have = cur, true
	return pct
}

func processTable(context.Context) (map[int]procEntry, bool) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, false
	}
	tab := map[int]procEntry{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "status"))
		if err != nil {
			continue
		}
		if ppid, rss, ok := parseProcStatus(string(b)); ok {
			tab[pid] = procEntry{ppid: ppid, rssKB: rss}
		}
	}
	return tab, true
}

func osInfo(context.Context) (osName, cpu string) {
	osName = "Linux"
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		osName += " " + strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "model name" {
				cpu = strings.TrimSpace(v)
				break
			}
		}
	}
	return osName, cpu
}

func appleGPU(context.Context) (string, bool)  { return "", false }
func appleGPUUtil(context.Context) (int, bool) { return 0, false }
