// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sysprobe

// macOS readings through the system's own commands (no cgo): sysctl,
// vm_stat, ps, system_profiler and ioreg. The parsers are plain functions
// so every platform's tests run them on recorded output.

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// parseVMStat returns the available memory (free + inactive + speculative
// + purgeable pages) in MiB from vm_stat's output.
func parseVMStat(out string) (int, bool) {
	page := int64(4096)
	if m := regexp.MustCompile(`page size of (\d+) bytes`).FindStringSubmatch(out); m != nil {
		if v, err := strconv.ParseInt(m[1], 10, 64); err == nil && v > 0 {
			page = v
		}
	}
	vals := map[string]int64{}
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		v = strings.TrimSuffix(strings.TrimSpace(v), ".")
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			continue
		}
		vals[strings.TrimSpace(k)] = n
	}
	free, ok := vals["Pages free"]
	if !ok {
		return 0, false
	}
	pages := free + vals["Pages inactive"] + vals["Pages speculative"] + vals["Pages purgeable"]
	return int(pages * page >> 20), true
}

// parseAppleGPU reads the chip name from "system_profiler -json
// SPDisplaysDataType".
func parseAppleGPU(out string) (string, bool) {
	var v struct {
		Displays []struct {
			Model  string `json:"sppci_model"`
			Vendor string `json:"spdisplays_vendor"`
		} `json:"SPDisplaysDataType"`
	}
	if json.Unmarshal([]byte(out), &v) != nil {
		return "", false
	}
	for _, d := range v.Displays {
		name := strings.TrimSpace(d.Model)
		if strings.HasPrefix(name, "Apple") {
			return cleanName(name), true
		}
	}
	return "", false
}

var deviceUtilRe = regexp.MustCompile(`"Device Utilization %"\s*=\s*(\d+)`)

// parseIORegUtil reads "Device Utilization %" of the first accelerator
// from "ioreg -r -d 1 -w 0 -c IOAccelerator".
func parseIORegUtil(out string) (int, bool) {
	m := deviceUtilRe.FindStringSubmatch(out)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 0 || n > 100 {
		return 0, false
	}
	return n, true
}

// parsePSCPU sums "ps -A -o %cpu=" and divides by the CPUs (percent of the
// whole machine, 0–100).
func parsePSCPU(out string, cpus int) (int, bool) {
	if cpus <= 0 {
		return 0, false
	}
	var sum float64
	n := 0
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(strings.ReplaceAll(l, ",", "."))
		if l == "" {
			continue
		}
		v, err := strconv.ParseFloat(l, 64)
		if err != nil {
			continue
		}
		sum += v
		n++
	}
	if n == 0 {
		return 0, false
	}
	pct := int(sum/float64(cpus) + 0.5)
	return min(max(pct, 0), 100), true
}

var nameCleanRe = regexp.MustCompile(`[^A-Za-z0-9 ._()/+-]`)

// cleanName keeps the characters remote.GPUNamePattern allows (≤ 64).
func cleanName(s string) string {
	s = strings.TrimSpace(nameCleanRe.ReplaceAllString(s, ""))
	if len(s) > 64 {
		s = strings.TrimSpace(s[:64])
	}
	return s
}
