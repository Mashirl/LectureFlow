// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"encoding/hex"
	"fmt"
	"math"
	"strings"
)

// Intel GPU / NPU utilisation (SPEC-v2 I10, lead ruling 16): Windows
// reports per-process, per-engine utilisation in the PDH counter
// "\GPU Engine(*)\Utilization Percentage" (the NPU is listed there too, as
// an adapter with a "Neural" engine). An instance is named
//
//	pid_{pid}_luid_0x{high}_0x{low}_phys_{n}_eng_{n}_engtype_{type}
//
// and the adapter is identified by its LUID, which the OpenVINO probe
// reports as DEVICE_LUID (the 8 bytes of the LUID struct as hex, low part
// first: "e220010000000000" is luid_0x00000000_0x000120E2). The device's
// utilisation is computed the way Task Manager does: the sum over processes
// per engine, then the busiest engine of the adapter. Everything here is
// pure; pdh_windows.go reads the counter.

// EngineCounter is the PDH counter path read for the device cards.
const EngineCounter = `\GPU Engine(*)\Utilization Percentage`

// EngineSample is one instance of EngineCounter.
type EngineSample struct {
	Instance string  // e.g. pid_1234_luid_0x00000000_0x000120E2_phys_0_eng_0_engtype_3D
	Value    float64 // percent
}

// ParseEngineInstance splits an instance name into the adapter key (as
// LUIDKey returns it: "0x00000000_0x000120E2", upper-case hex digits) and
// the engine ("phys_0_eng_0"). ok is false for names without a LUID.
func ParseEngineInstance(name string) (luid, engine string, ok bool) {
	i := strings.Index(name, "luid_")
	if i < 0 {
		return "", "", false
	}
	rest := name[i+len("luid_"):]
	// 0x{8}_0x{8}
	parts := strings.SplitN(rest, "_", 3)
	if len(parts) < 2 {
		return "", "", false
	}
	high, okH := hexWord(parts[0])
	low, okL := hexWord(parts[1])
	if !okH || !okL {
		return "", "", false
	}
	luid = fmt.Sprintf("0x%08X_0x%08X", high, low)
	if len(parts) == 3 {
		engine = parts[2]
		if j := strings.Index(engine, "_engtype_"); j >= 0 {
			engine = engine[:j]
		}
	}
	return luid, engine, true
}

// hexWord parses "0x0000D1F2" (any case).
func hexWord(s string) (uint32, bool) {
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		return 0, false
	}
	s = s[2:]
	if s == "" || len(s) > 8 {
		return 0, false
	}
	var v uint32
	for _, c := range s {
		var d uint32
		switch {
		case c >= '0' && c <= '9':
			d = uint32(c - '0')
		case c >= 'a' && c <= 'f':
			d = uint32(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = uint32(c-'A') + 10
		default:
			return 0, false
		}
		v = v<<4 | d
	}
	return v, true
}

// LUIDKey normalises a LUID to the key of ParseEngineInstance. It accepts
// the OpenVINO form (16 hex digits, the struct's bytes: LowPart then
// HighPart, each little-endian), the PDH form ("luid_0x…_0x…" or
// "0x…_0x…") and is case-insensitive. ok is false for anything else (an
// empty LUID: the probe did not report one).
func LUIDKey(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	if strings.Contains(s, "_") {
		if !strings.HasPrefix(strings.ToLower(s), "luid_") {
			s = "luid_" + s
		}
		luid, _, ok := ParseEngineInstance(s)
		return luid, ok
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 8 {
		return "", false
	}
	low := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
	high := uint32(b[4]) | uint32(b[5])<<8 | uint32(b[6])<<16 | uint32(b[7])<<24
	return fmt.Sprintf("0x%08X_0x%08X", high, low), true
}

// UtilByLUID is the utilisation per adapter (LUIDKey → percent 0..100,
// rounded): the sum of the processes' values per engine, the largest
// engine of the adapter. Instances without a LUID and negative or NaN
// values are ignored.
func UtilByLUID(samples []EngineSample) map[string]int {
	type key struct{ luid, engine string }
	sums := map[key]float64{}
	for _, s := range samples {
		if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) || s.Value < 0 {
			continue
		}
		luid, eng, ok := ParseEngineInstance(s.Instance)
		if !ok {
			continue
		}
		sums[key{luid, eng}] += s.Value
	}
	best := map[string]float64{}
	for k, v := range sums {
		if cur, ok := best[k.luid]; !ok || v > cur {
			best[k.luid] = v
		}
	}
	out := make(map[string]int, len(best))
	for l, v := range best {
		if v > 100 {
			v = 100
		}
		out[l] = int(math.Round(v))
	}
	return out
}

// engineReader reads EngineCounter (pdh_windows.go; elsewhere always an
// error). Read is called with sampleMu held. It returns nil while the rate
// counter is primed (the first collection: the values are unknown) and a
// non-nil, possibly empty, slice afterwards (an adapter without instances
// is idle).
type engineReader interface {
	Read() ([]EngineSample, error)
	Close()
}

// adapterMemReader is implemented by an engineReader that also reads
// AdapterMemCounter in the same collection (power management, task P4:
// the Intel cards' shared memory). SharedUsage returns the instances of
// the last Read (bytes), nil when unknown.
type adapterMemReader interface {
	SharedUsage() []EngineSample
}
