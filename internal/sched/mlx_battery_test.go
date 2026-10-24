// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"testing"
	"time"

	"lectureflow/internal/devices"
	"lectureflow/internal/model"
)

// SPEC-v3 V5 (v5-integration-b1): on battery power mlx:gpu keeps running
// jobs but one at a time, whatever its max_parallel; back on AC its slots
// return.
func TestMLXBatteryOneSlot(t *testing.T) {
	mlx := devRow(model.DeviceMLXGPU, 250, 2)
	mlx.Model, mlx.Precision = "large-v3-turbo", "fp16"
	de := newDevEnv(t, []model.ASRDevice{mlx}, nil)
	de.power.set(onBattery)
	l1, l2 := de.lectureOn(autoOn(mlx)), de.lectureOn(autoOn(mlx))
	de.prepDone(l1, l2)
	de.start()
	de.waitRunningOn(l1, model.DeviceMLXGPU)
	// The policy's wait for the busy single device of a laptop: its one
	// slot is full (with max_parallel 2 the job would run).
	de.stillQueued(l2, model.WaitPowerQueue(model.DeviceMLXGPU))
	if s := de.status(model.DeviceMLXGPU); s.SlotsUsed != 1 || s.SlotsMax != 1 || s.BatteryHold {
		t.Fatalf("on battery: status %+v", s)
	}

	// The adapter is plugged in: the second slot comes back.
	de.power.set(onACLaptop)
	de.s.Kick()
	de.waitRunningOn(l2, model.DeviceMLXGPU)
	if s := de.status(model.DeviceMLXGPU); s.SlotsUsed != 2 || s.SlotsMax != 2 {
		t.Fatalf("on AC: status %+v", s)
	}
	de.asr.release(l1.ID)
	de.asr.release(l2.ID)
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)
}

// deviceSlots reads the power state without s.mu, and with it held (the
// policy input is built under s.mu): no deadlock, and only mlx:gpu is
// limited.
func TestDeviceSlotsBatteryOnlyMLX(t *testing.T) {
	mlx := devRow(model.DeviceMLXGPU, 250, 3)
	igpu := devRow(model.DeviceOVGPU, 200, 2)
	de := newDevEnv(t, []model.ASRDevice{mlx, igpu}, nil)
	de.power.set(onBattery)
	de.s.readPower()
	done := make(chan [2]int, 1)
	go func() {
		de.s.mu.Lock()
		defer de.s.mu.Unlock()
		done <- [2]int{de.s.deviceSlots(mlx), de.s.deviceSlots(igpu)}
	}()
	select {
	case got := <-done:
		if got != [2]int{1, 2} {
			t.Fatalf("slots on battery (mlx, igpu) = %v, want [1 2]", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deviceSlots blocked with s.mu held")
	}
	// A desktop (no battery) or an unknown power state: max_parallel.
	for _, p := range []devices.Power{onAC, {}, onACLaptop} {
		de.power.set(p)
		de.s.readPower()
		if n := de.s.deviceSlots(mlx); n != 3 {
			t.Fatalf("power %+v: mlx slots %d, want 3", p, n)
		}
	}
}
