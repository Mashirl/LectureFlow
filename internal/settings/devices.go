// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import (
	"context"
	"strings"

	"lectureflow/internal/model"
)

// SPEC-v2 I6/I8 (phase 3b freeze, .claude/v3b-brief.md). FROZEN by the
// lead: DeviceSource and SetDeviceSource. Bodies: task B, which also
//   - makes CurrentSnapshots write the asr snapshot from the DeviceSource
//     when one is set (CurrentASRSnapshot; ok false: the fallback auto
//     snapshot FallbackASRSnapshot, 3b Gate round 1 item 2; no source:
//     the legacy snapshot of the ASR fields, exactly as before);
//   - adds the defaults of the new ASR fields to DefaultASR / fillASR
//     (device_mode auto, multi_device true, battery_aware true,
//     mem_reserve_mb 2048) and their validation to SetASR
//     (CodeDeviceModeInvalid: not auto and no such device row;
//     CodeMemReserveInvalid: 0..65536).

// DeviceSource is the device registry as the settings see it
// (devices.Registry implements it).
type DeviceSource interface {
	// CurrentASRSnapshot is the asr snapshot a launch writes now; ok
	// false = no device to name (the launch writes FallbackASRSnapshot).
	CurrentASRSnapshot(a model.ASRSettings) (model.ASRSnapshot, bool)
	// Get returns a device row (validation of device_mode).
	Get(id string) (model.ASRDevice, bool)
}

// SetDeviceSource installs the device registry (cmd, after
// Registry.MigrateLegacy). nil removes it.
func (s *Service) SetDeviceSource(d DeviceSource) {
	s.mu.Lock()
	s.devices = d
	s.mu.Unlock()
}

// deviceSource returns the installed registry (nil when none).
func (s *Service) deviceSource() DeviceSource {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.devices
}

// CurrentASR is the raw asr snapshot a launch writes now: the asr entry of
// CurrentSnapshots, also available when an LLM role is not configured (the
// scheduler's A4.3 fallback, sched.currentSnapshots, uses it instead of
// building the legacy snapshot itself). With a DeviceSource it is the
// registry's snapshot (I6: device_id, backend, model, precision, language,
// auto); when the registry has none (ok false: no enabled row, or no row
// for the device mode) an auto snapshot of the fallback (3b Gate round 1,
// item 2: FallbackASRSnapshot), so the scheduler binds a device when the
// job starts — never a fixed cuda:0 made of the legacy fields. Without a
// DeviceSource the legacy snapshot of the ASR fields, byte for byte as
// before phase 3b.
func (s *Service) CurrentASR() string {
	a := s.ASR()
	// The registry is called outside s.mu (it has its own lock and never
	// calls back into the settings).
	if d := s.deviceSource(); d != nil {
		if snap, ok := d.CurrentASRSnapshot(a); ok {
			return model.MarshalSnapshot(snap)
		}
		return model.MarshalSnapshot(FallbackASRSnapshot(a.Language))
	}
	return model.MarshalSnapshot(model.ASRSnapshot{
		Model: a.Model, ComputeType: a.ComputeType, Device: a.Device, Language: a.Language,
	})
}

// FallbackASRSnapshot is the launch snapshot while no device can be named:
// auto mode on fw:cpu with the fallback model and precision of
// faster-whisper (devices.Fallback, small / auto; the devices package
// checks they are the same) and the settings' language. Its device fields
// are rewritten when the job starts (state.BindASRDevice).
func FallbackASRSnapshot(language string) model.ASRSnapshot {
	return model.ASRSnapshot{DeviceID: model.DeviceFWCPU, Backend: model.BackendFasterWhisper, Model: "small",
		Precision: model.PrecisionAuto, Language: language, Auto: true}
}

// deviceRow is the device check of SetASR: a row of the installed registry,
// or (no registry yet) a row of the database.
func (s *Service) deviceRow(ctx context.Context, id string) (model.ASRDevice, bool) {
	if d := s.deviceSource(); d != nil {
		return d.Get(id)
	}
	row, err := s.st.GetASRDevice(ctx, id)
	return row, err == nil // db.ErrNotFound or a read error: not a usable choice
}

// deviceLabel is the device param of the manual_devices codes: the row's
// label, the id as sent when there is no row (or no label).
func deviceLabel(row model.ASRDevice, ok bool, id string) string {
	if ok && strings.TrimSpace(row.Label) != "" {
		return row.Label
	}
	return id
}

// checkDeviceMode is the device part of SetASR (power-brief §3.2), checked
// on the request before model.NormalizeASRDeviceMode: auto; manual with at
// least one device (blank entries ignored), none a CPU device
// (CodeManualDevicesCPU) and every one an existing row
// (CodeManualDevicesUnknown), in the order sent; or a phase-3b single
// device id (an older client): an existing row (else CodeDeviceModeInvalid
// as in 3b) that is not a CPU device (CodeManualDevicesCPU), stored as
// manual [id]. mode is a.DeviceMode trimmed, "" already replaced by auto.
func (s *Service) checkDeviceMode(ctx context.Context, mode string, list []string) error {
	cpu := func(label string) error {
		return userErr(ErrInvalid, CodeManualDevicesCPU, map[string]any{"device": label},
			"手动指定不能选择 CPU 转写设备「%s」", label)
	}
	switch mode {
	case model.ASRDeviceModeAuto:
		return nil
	case model.ASRDeviceModeManual:
		n := 0
		for _, id := range list {
			if id = strings.TrimSpace(id); id == "" {
				continue
			}
			n++
			row, ok := s.deviceRow(ctx, id)
			label := deviceLabel(row, ok, id)
			if model.DeviceKind(id) == model.DeviceKindCPU {
				return cpu(label)
			}
			if !ok {
				return userErr(ErrInvalid, CodeManualDevicesUnknown, map[string]any{"device": label},
					"转写设备「%s」不存在，请从列表中选择", label)
			}
		}
		if n == 0 {
			return userErr(ErrInvalid, CodeManualDevicesEmpty, nil, "手动指定转写设备时至少要选择一台设备")
		}
		return nil
	}
	row, ok := s.deviceRow(ctx, mode)
	if !ok {
		return userErr(ErrInvalid, CodeDeviceModeInvalid, map[string]any{"value": mode},
			"转写设备「%s」不存在，请选择“自动”或列表中的设备", mode)
	}
	if model.DeviceKind(mode) == model.DeviceKindCPU {
		return cpu(deviceLabel(row, ok, mode))
	}
	return nil
}
