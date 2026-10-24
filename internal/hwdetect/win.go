// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package hwdetect

// Windows probes and their parsers. The file has no build constraint: the
// parsers are pure and the assembly takes an injected Runner, so both are
// tested on every platform.

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// nvidiaSMIArgs is the nvidia-smi query (V3.4): one CSV line per GPU.
var nvidiaSMIArgs = []string{"--query-gpu=name,memory.total,driver_version", "--format=csv,noheader,nounits"}

// psScript gathers every CIM/registry probe in one PowerShell start (each
// PowerShell start costs about a second). Every probe is in its own try so
// a failure leaves only its own key empty. Output is UTF-8 JSON.
const psScript = `$ErrorActionPreference = 'SilentlyContinue'
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$r = [ordered]@{}
try { $r.video = @(Get-CimInstance Win32_VideoController | Select-Object Name, AdapterCompatibility, AdapterRAM, PNPDeviceID, DriverVersion) } catch {}
try { $r.vram = @(Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}\0*' | Select-Object DriverDesc, 'HardwareInformation.qwMemorySize') } catch {}
try { $r.pnp = @(Get-CimInstance Win32_PnPEntity -Filter "PNPClass = 'ComputeAccelerator' OR Name LIKE '%AI Boost%' OR Name LIKE '%NPU%'" | Select-Object Name, PNPClass, Manufacturer) } catch {}
try { $r.cpu = @(Get-CimInstance Win32_Processor | Select-Object Name) } catch {}
try { $r.cs = @(Get-CimInstance Win32_ComputerSystem | Select-Object TotalPhysicalMemory) } catch {}
try { $r.battery = @(Get-CimInstance Win32_Battery | Select-Object Name) } catch {}
$r | ConvertTo-Json -Depth 4 -Compress`

var powershellArgs = []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", psScript}

func detectWindows(ctx context.Context, hw *HW, o Options) {
	var (
		wg     sync.WaitGroup
		smiOut []byte
		smiErr error
		psOut  []byte
		psErr  error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		smiOut, smiErr = o.run(ctx, o.Timeout, "nvidia-smi", nvidiaSMIArgs...)
	}()
	go func() {
		defer wg.Done()
		psOut, psErr = o.run(ctx, powershellTimeoutFactor*o.Timeout, "powershell.exe", powershellArgs...)
	}()
	wg.Wait()

	var ps WinProbe
	if psErr == nil {
		ps, _ = ParseWinProbe(psOut)
	}
	if smiErr == nil {
		hw.NVIDIA = ParseNvidiaSMI(string(smiOut))
	}
	if len(hw.NVIDIA) == 0 {
		hw.NVIDIA = ps.NVIDIAFallback()
	}
	hw.IntelGPUName = ps.IntelGPUName()
	hw.IntelGPU = hw.IntelGPUName != ""
	hw.IntelNPUName = ps.IntelNPUName()
	hw.IntelNPU = hw.IntelNPUName != ""
	hw.CPU = ps.CPUName()
	hw.RAMMB = ps.RAMMB()
	hw.HasBattery = len(ps.Battery) > 0
}

// ParseNvidiaSMI parses `nvidia-smi --query-gpu=name,memory.total,
// driver_version --format=csv,noheader,nounits`: one "name, MiB, driver"
// line per GPU (nvidia-smi lists NVIDIA GPUs only). Lines whose memory
// field is neither a number nor "[N/A]" are skipped; "[N/A]" is kept as 0
// (unknown).
func ParseNvidiaSMI(out string) []GPU {
	var gpus []GPU
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		if line == "" {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < 3 {
			continue
		}
		// The name never contains a comma in practice; join defensively.
		n := len(f)
		name := strings.TrimSpace(strings.Join(f[:n-2], ","))
		memField := strings.TrimSpace(f[n-2])
		mem, err := strconv.ParseFloat(memField, 64)
		if err != nil {
			if !strings.Contains(memField, "N/A") {
				continue // not a result line (e.g. an error message with commas)
			}
			mem = 0 // unknown
		}
		if name == "" {
			continue
		}
		drv := strings.TrimSpace(f[n-1])
		if strings.Contains(drv, "N/A") {
			drv = ""
		}
		gpus = append(gpus, GPU{Name: name, VRAMMB: int(mem), Driver: drv, Source: "nvidia-smi"})
	}
	return gpus
}

// WinProbe is the decoded output of psScript.
type WinProbe struct {
	Video   []WinVideo
	VRAM    []WinVRAM
	PnP     []WinPnP
	CPU     []string
	RAMB    uint64
	Battery []string
}

// WinVideo is one Win32_VideoController.
type WinVideo struct {
	Name, AdapterCompatibility, PNPDeviceID, DriverVersion string
	AdapterRAM                                             uint64
}

// WinVRAM is one display-class registry key: the driver's own 64-bit memory
// size (Win32_VideoController.AdapterRAM is 32-bit and wraps above 4 GB).
type WinVRAM struct {
	DriverDesc string
	Bytes      uint64 // 0 = not reported
}

// WinPnP is one Win32_PnPEntity candidate for an NPU.
type WinPnP struct {
	Name, PNPClass, Manufacturer string
}

// ParseWinProbe decodes psScript's JSON. Missing or malformed keys stay
// empty; the error is set only when the output is not a JSON object.
func ParseWinProbe(out []byte) (WinProbe, error) {
	var p WinProbe
	out = bytes.TrimPrefix(bytes.TrimSpace(out), []byte("\ufeff"))
	if i := bytes.IndexByte(out, '{'); i > 0 {
		out = out[i:]
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		return p, err
	}
	for _, o := range objects(raw["video"]) {
		p.Video = append(p.Video, WinVideo{
			Name:                 str(o["Name"]),
			AdapterCompatibility: str(o["AdapterCompatibility"]),
			PNPDeviceID:          str(o["PNPDeviceID"]),
			DriverVersion:        str(o["DriverVersion"]),
			AdapterRAM:           num(o["AdapterRAM"]),
		})
	}
	for _, o := range objects(raw["vram"]) {
		p.VRAM = append(p.VRAM, WinVRAM{DriverDesc: str(o["DriverDesc"]), Bytes: num(o["HardwareInformation.qwMemorySize"])})
	}
	for _, o := range objects(raw["pnp"]) {
		p.PnP = append(p.PnP, WinPnP{Name: str(o["Name"]), PNPClass: str(o["PNPClass"]), Manufacturer: str(o["Manufacturer"])})
	}
	for _, o := range objects(raw["cpu"]) {
		if n := strings.TrimSpace(str(o["Name"])); n != "" {
			p.CPU = append(p.CPU, n)
		}
	}
	for _, o := range objects(raw["cs"]) {
		if b := num(o["TotalPhysicalMemory"]); b > 0 {
			p.RAMB = b
		}
	}
	for _, o := range objects(raw["battery"]) {
		p.Battery = append(p.Battery, str(o["Name"]))
	}
	return p, nil
}

// objects accepts a JSON array of objects or a single object (PowerShell
// unwraps one-element arrays in some versions).
func objects(r json.RawMessage) []map[string]json.RawMessage {
	r = bytes.TrimSpace(r)
	if len(r) == 0 {
		return nil
	}
	var list []map[string]json.RawMessage
	if r[0] == '[' {
		var items []json.RawMessage
		if json.Unmarshal(r, &items) != nil {
			return nil
		}
		for _, it := range items {
			var m map[string]json.RawMessage
			if json.Unmarshal(it, &m) == nil && m != nil {
				list = append(list, m)
			}
		}
		return list
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(r, &m) == nil && m != nil {
		list = append(list, m)
	}
	return list
}

func str(r json.RawMessage) string {
	var s string
	if json.Unmarshal(r, &s) != nil {
		return ""
	}
	return s
}

// num reads a non-negative integer that may be a JSON number, a numeric
// string, or a little-endian byte array (a REG_BINARY registry value).
func num(r json.RawMessage) uint64 {
	r = bytes.TrimSpace(r)
	if len(r) == 0 || string(r) == "null" {
		return 0
	}
	var f float64
	if json.Unmarshal(r, &f) == nil {
		if f < 0 {
			return 0
		}
		return uint64(f)
	}
	var s string
	if json.Unmarshal(r, &s) == nil {
		v, _ := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
		return v
	}
	var bs []int
	if json.Unmarshal(r, &bs) == nil && len(bs) > 0 && len(bs) <= 8 {
		var v uint64
		for i := len(bs) - 1; i >= 0; i-- {
			v = v<<8 | uint64(bs[i]&0xff)
		}
		return v
	}
	return 0
}

func (v WinVideo) isNVIDIA() bool {
	return containsFold(v.AdapterCompatibility, "NVIDIA") || containsFold(v.Name, "NVIDIA") ||
		containsFold(v.PNPDeviceID, "VEN_10DE")
}

func (v WinVideo) isIntel() bool {
	return containsFold(v.AdapterCompatibility, "Intel") || containsFold(v.PNPDeviceID, "VEN_8086") ||
		strings.HasPrefix(strings.ToUpper(strings.TrimSpace(v.Name)), "INTEL")
}

// NVIDIAFallback lists the NVIDIA display adapters of the system device
// list (used when nvidia-smi is missing or fails). The memory size comes
// from the driver's registry key when it reports one, otherwise it is
// unknown (0): AdapterRAM is a 32-bit value and is not trusted.
func (p WinProbe) NVIDIAFallback() []GPU {
	var gpus []GPU
	for _, v := range p.Video {
		if !v.isNVIDIA() || strings.TrimSpace(v.Name) == "" {
			continue
		}
		g := GPU{Name: strings.TrimSpace(v.Name), Driver: NvidiaDriverFromWindows(v.DriverVersion), Source: "wmi"}
		for _, r := range p.VRAM {
			if r.Bytes > 0 && strings.EqualFold(strings.TrimSpace(r.DriverDesc), g.Name) {
				g.VRAMMB = int(r.Bytes / bytesPerMB)
				break
			}
		}
		gpus = append(gpus, g)
	}
	return gpus
}

// IntelGPUName is the first Intel display adapter, "" when there is none.
func (p WinProbe) IntelGPUName() string {
	for _, v := range p.Video {
		if v.isIntel() && strings.TrimSpace(v.Name) != "" {
			return strings.TrimSpace(v.Name)
		}
	}
	return ""
}

var npuWord = regexp.MustCompile(`(?i)\bNPU\b`)

// IntelNPUName is the first Intel neural processor: PnP class
// ComputeAccelerator, or a name containing "AI Boost" or the word "NPU"
// (a plain substring match would also hit "Input"), made by Intel.
func (p WinProbe) IntelNPUName() string {
	for _, d := range p.PnP {
		name := strings.TrimSpace(d.Name)
		if name == "" {
			continue
		}
		npu := strings.EqualFold(d.PNPClass, "ComputeAccelerator") || containsFold(name, "AI Boost") || npuWord.MatchString(name)
		intel := containsFold(d.Manufacturer, "Intel") || containsFold(name, "Intel") || containsFold(name, "AI Boost")
		if npu && intel {
			return name
		}
	}
	return ""
}

// CPUName is the first processor's name.
func (p WinProbe) CPUName() string {
	if len(p.CPU) == 0 {
		return ""
	}
	return strings.Join(strings.Fields(p.CPU[0]), " ")
}

// RAMMB is the total physical memory in MiB, 0 = unknown.
func (p WinProbe) RAMMB() int { return int(p.RAMB / bytesPerMB) }

// NvidiaDriverFromWindows converts a Windows driver version of an NVIDIA
// driver ("32.0.15.9200") to NVIDIA's own numbering ("592.00"): the last
// five digits of the last two parts. Anything else gives "".
func NvidiaDriverFromWindows(v string) string {
	parts := strings.Split(strings.TrimSpace(v), ".")
	if len(parts) != 4 {
		return ""
	}
	digits := parts[2] + parts[3]
	for _, c := range digits {
		if c < '0' || c > '9' {
			return ""
		}
	}
	if len(digits) < 5 {
		return ""
	}
	d := digits[len(digits)-5:]
	major := strings.TrimLeft(d[:3], "0")
	if major == "" {
		major = "0"
	}
	return major + "." + d[3:]
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToUpper(s), strings.ToUpper(sub))
}
