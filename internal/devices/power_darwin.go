// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build darwin

package devices

import (
	"context"
	"os/exec"
	"sync"
	"time"
)

// macOS: the power state from pmset (PowerFromPmset). ReadPower runs on
// every scheduler scan, so a reading is reused for powerCacheTTL; two pmset
// runs take about 10 ms together. A failure or a timeout reads as unknown
// (treated as AC power) and is retried after the same interval. The
// available memory is read through Mach (memory_darwin.go, macmem.go).

const (
	pmsetPath     = "/usr/bin/pmset"
	powerCacheTTL = 3 * time.Second
	pmsetTimeout  = 2 * time.Second
)

var powerCache struct {
	sync.Mutex
	at time.Time
	p  Power
}

func readPower() Power {
	powerCache.Lock()
	defer powerCache.Unlock()
	if !powerCache.at.IsZero() && time.Since(powerCache.at) < powerCacheTTL {
		return powerCache.p
	}
	powerCache.p = readPmset()
	powerCache.at = time.Now()
	return powerCache.p
}

func readPmset() Power {
	batt, err := pmset("-g", "batt")
	if err != nil {
		return Power{}
	}
	settings, _ := pmset("-g") // only Low Power Mode; a failure leaves it off
	return PowerFromPmset(batt, settings)
}

func pmset(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pmsetTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, pmsetPath, args...).Output()
	return string(out), err
}

// availableMemoryMB: the unified memory not in use, the measure the
// resource bar shows (memory_darwin.go).
func availableMemoryMB() (int, error) { return availableMemoryMBDarwin() }
