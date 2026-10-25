// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build darwin

package devices

import (
	"errors"
	"reflect"
	"testing"
)

func TestReadPowerDarwinMock(t *testing.T) {
	orig := pmsetBattCmd
	defer func() { pmsetBattCmd = orig }()

	pmsetBattCmd = func() ([]byte, error) {
		return nil, errors.New("exec error")
	}
	if got := readPower(); !reflect.DeepEqual(got, Power{}) {
		t.Fatalf("expected empty power on error, got %+v", got)
	}

	pmsetBattCmd = func() ([]byte, error) {
		return []byte("Now drawing from 'Battery Power'\n -InternalBattery-0\t65%; discharging; 4:00 remaining present: true"), nil
	}
	want := Power{Known: true, OnBattery: true, HasBattery: true, BatteryPct: pct(65)}
	if got := readPower(); !reflect.DeepEqual(got, want) {
		t.Fatalf("mocked readPower got %+v, want %+v", got, want)
	}
}
