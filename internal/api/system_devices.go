// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"os"
	"strings"

	"lectureflow/internal/devices"
	"lectureflow/internal/model"
)

// The OpenVINO checks of the diagnostics page (SPEC-v2 I10): .venv-ov
// exists, OpenVINO imports, the devices it found, the NPU driver. All of
// them are at most warn (I10: "只有 OpenVINO 相关项失败时为 warn（不影响 CUDA
// 转写）", lead ruling 13). They are derived on every GET /api/system from
// the registry's last detection (the worker's --probe) and a stat of
// python_openvino, so they never start a process of their own and always
// agree with the OpenVINO table next to them.

const (
	checkOpenVINOVenv    = "openvino_venv"
	checkOpenVINO        = "openvino"
	checkOpenVINODevices = "openvino_devices"
	checkNPUDriver       = "npu_driver"
)

// openVINOChecks returns the four checks (none without the registry, and
// none on a machine without an Intel GPU or NPU and without an OpenVINO
// environment, mac-opti M3: openVINOVisible). In desktop mode a missing
// environment is the asr-openvino component that is not installed.
func (s *Server) openVINOChecks() []SystemCheck {
	if s.dev == nil {
		return nil
	}
	det := s.dev.LastDetection()
	python := det.Python
	if python == "" && s.cfg != nil {
		python = s.cfg.PythonOpenVINO
	}
	if !s.openVINOVisible(det, python) {
		return nil
	}
	venvHint := s.devHint("check.hint_openvino_venv", codeHintInstallOpenVINO)
	venv := SystemCheck{Name: checkOpenVINOVenv, Status: statusOK}
	venvOK := false
	if fi, err := os.Stat(python); python != "" && err == nil && !fi.IsDir() {
		venvOK = true
		venv.setDetail("check.openvino_venv_ok", map[string]any{"path": python})
	} else {
		venv.Status = statusWarn
		if s.desktopDiag() || python == "" { // "": no component, nothing configured (3.1.0)
			venv.setDetail(codeOpenVINONotInstalled, nil)
		} else {
			venv.setDetail("check.openvino_venv_missing", map[string]any{"path": python})
		}
		venv.setHint(venvHint, nil)
	}
	imp := SystemCheck{Name: checkOpenVINO, Status: statusWarn}
	devs := SystemCheck{Name: checkOpenVINODevices, Status: statusWarn}
	npu := SystemCheck{Name: checkNPUDriver, Status: statusWarn}
	notChecked := func(cs ...*SystemCheck) {
		for _, c := range cs {
			c.setDetail("check.api_skipped", nil) // "未检查"
		}
	}
	pending := func(cs ...*SystemCheck) {
		for _, c := range cs {
			c.setDetail(checkPending, nil)
		}
	}
	switch {
	case det.OV != nil:
		imp.Status = statusOK
		imp.setDetail("check.import_ok", map[string]any{"module": "openvino"})
		if len(det.OV.Devices) == 0 {
			devs.setDetail("check.openvino_no_devices", nil)
		} else {
			devs.Status = statusOK
			names := make([]string, 0, len(det.OV.Devices))
			for _, d := range det.OV.Devices {
				n := d.FullName
				if n == "" {
					n = d.Name
				} else if d.Name != "" {
					n += " (" + d.Name + ")"
				}
				names = append(names, n)
			}
			devs.setDetail("check.openvino_devices", map[string]any{"devices": strings.Join(names, ", ")})
		}
		npuCheck(&npu, det.OV)
	case !venvOK:
		notChecked(&imp, &devs, &npu)
	case det.OVErr != nil:
		errText := errParam(*det.OVErr)
		switch det.OVErr.Code {
		case devices.CodeOVImportFailed:
			imp.setDetail("check.import_failed", map[string]any{"module": "openvino", "error": errText})
			imp.setHint(venvHint, nil)
		case devices.CodeOVPythonMissing:
			imp.setDetail("check.run_failed", map[string]any{"program": python, "error": errText})
			imp.setHint(venvHint, nil)
		default: // devices.CodeProbeFailed: timeout, unreadable output
			imp.setDetail("check.run_failed", map[string]any{"program": "asr_openvino.py --probe", "error": errText})
		}
		imp.DetailInfo.Detail = det.OVErr.Detail
		notChecked(&devs, &npu)
	case det.At.IsZero():
		pending(&imp, &devs, &npu) // the first detection has not finished
	default:
		notChecked(&imp, &devs, &npu)
	}
	return []SystemCheck{venv, imp, devs, npu}
}

// npuCheck rates the NPU driver from the probe: listed → ok with its
// driver version; listed among the ignored devices with an error (reading
// its properties failed) → the driver does not work; absent → no NPU.
func npuCheck(c *SystemCheck, p *devices.OVProbe) {
	for _, d := range p.Devices {
		if d.Kind == model.DeviceKindNPU {
			c.Status = statusOK
			v := d.DriverVersion
			if v == "" {
				v = "--"
			}
			c.setDetail("check.npu_driver_ok", map[string]any{"version": v})
			return
		}
	}
	for _, d := range p.Ignored {
		if strings.HasPrefix(strings.ToUpper(d.Name), "NPU") {
			msg := d.Error
			if msg == "" {
				msg = d.Reason
			}
			c.setDetail("check.npu_driver_failed", map[string]any{"error": msg})
			c.setHint("check.hint_npu_driver", nil)
			return
		}
	}
	c.setDetail("check.npu_missing", nil)
	c.setHint("check.hint_npu_driver", nil)
}

// errParam is the error text of a devices.* code (its error param, else
// its detail).
func errParam(e model.ErrorInfo) string {
	if v, ok := e.Params["error"].(string); ok && v != "" {
		return v
	}
	if v, ok := e.Params["path"].(string); ok && v != "" && e.Detail == "" {
		return v
	}
	if e.Detail != "" {
		return e.Detail
	}
	return e.Code
}
