// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package resources

import (
	"fmt"
	"syscall"
	"unsafe"
)

// The PDH reader of EngineCounter (lead ruling 16), through pdh.dll like
// internal/api's diskfree_windows.go calls kernel32. The query stays open
// while the watcher samples: "Utilization Percentage" is a rate counter, so
// every Read collects once and formats the difference to the previous
// collection (the first Read after opening returns no samples).
//
// Power management (task P4): the same query also holds AdapterMemCounter
// (the Intel cards' shared memory, an instantaneous value, optional: a
// machine without the counter set keeps the utilisation), and
// system_windows.go opens a second query for the upper area's counters
// with the helpers below.

var (
	pdhDLL                          = syscall.NewLazyDLL("pdh.dll")
	procPdhOpenQueryW               = pdhDLL.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounterW       = pdhDLL.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData         = pdhDLL.NewProc("PdhCollectQueryData")
	procPdhGetFormattedCounterArray = pdhDLL.NewProc("PdhGetFormattedCounterArrayW")
	procPdhGetFormattedCounterValue = pdhDLL.NewProc("PdhGetFormattedCounterValue")
	procPdhCloseQuery               = pdhDLL.NewProc("PdhCloseQuery")
)

const (
	pdhFmtDouble   = 0x00000200
	pdhFmtNoCap100 = 0x00008000
	pdhMoreData    = 0x800007D2
	pdhNoData      = 0x800007D5
	pdhInvalidData = 0xC0000BC6

	pdhCStatusValidData = 0x00000000
	pdhCStatusNewData   = 0x00000001
)

// pdhItem is PDH_FMT_COUNTERVALUE_ITEM_W with a double value (64-bit
// layout: name pointer, CStatus, padding, the 8-byte union).
type pdhItem struct {
	Name    *uint16
	CStatus uint32
	_       uint32
	Value   float64
}

// pdhValue is PDH_FMT_COUNTERVALUE with a double value (CStatus, padding,
// the 8-byte union).
type pdhValue struct {
	CStatus uint32
	_       uint32
	Value   float64
}

// pdhOpenQuery opens a real-time query.
func pdhOpenQuery() (uintptr, error) {
	if err := pdhDLL.Load(); err != nil {
		return 0, err
	}
	var q uintptr
	if st, _, _ := procPdhOpenQueryW.Call(0, 0, uintptr(unsafe.Pointer(&q))); st != 0 {
		return 0, fmt.Errorf("PdhOpenQuery: 0x%08X", uint32(st))
	}
	return q, nil
}

// pdhAddCounter adds an English counter path to query q.
func pdhAddCounter(q uintptr, path string) (uintptr, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var c uintptr
	if st, _, _ := procPdhAddEnglishCounterW.Call(q, uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(&c))); st != 0 {
		return 0, fmt.Errorf("PdhAddEnglishCounter %s: 0x%08X", path, uint32(st))
	}
	return c, nil
}

// pdhArray formats the instances of a wildcard counter after a collection:
// ok false when the counter has no valid data now (PDH_INVALID_DATA), an
// empty slice when it has no instances.
func pdhArray(counter uintptr) (out []EngineSample, ok bool, err error) {
	var size, count uint32
	st, _, _ := procPdhGetFormattedCounterArray.Call(counter, pdhFmtDouble|pdhFmtNoCap100,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), 0)
	switch uint32(st) {
	case pdhMoreData:
	case 0, pdhNoData:
		return []EngineSample{}, true, nil
	case pdhInvalidData:
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("PdhGetFormattedCounterArray: 0x%08X", uint32(st))
	}
	itemSize := uint32(unsafe.Sizeof(pdhItem{}))
	buf := make([]pdhItem, size/itemSize+1)
	size = uint32(len(buf)) * itemSize
	st, _, _ = procPdhGetFormattedCounterArray.Call(counter, pdhFmtDouble|pdhFmtNoCap100,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&buf[0])))
	switch uint32(st) {
	case 0:
	case pdhNoData:
		return []EngineSample{}, true, nil
	case pdhInvalidData:
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("PdhGetFormattedCounterArray: 0x%08X", uint32(st))
	}
	if int(count) > len(buf) {
		count = uint32(len(buf))
	}
	out = make([]EngineSample, 0, count)
	for _, it := range buf[:count] {
		if it.CStatus != pdhCStatusValidData && it.CStatus != pdhCStatusNewData {
			continue
		}
		out = append(out, EngineSample{Instance: utf16PtrToString(it.Name), Value: it.Value})
	}
	return out, true, nil
}

// pdhSingle formats a single-instance counter after a collection; ok false
// when it has no valid value now (the first collection of a rate counter).
func pdhSingle(counter uintptr) (float64, bool) {
	var v pdhValue
	st, _, _ := procPdhGetFormattedCounterValue.Call(counter, pdhFmtDouble|pdhFmtNoCap100, 0, uintptr(unsafe.Pointer(&v)))
	if st != 0 || (v.CStatus != pdhCStatusValidData && v.CStatus != pdhCStatusNewData) {
		return 0, false
	}
	return v.Value, true
}

type pdhReader struct {
	query, counter uintptr
	mem            uintptr        // AdapterMemCounter; 0 when it could not be added
	shared         []EngineSample // the last reading of mem (nil: unknown)
	collected      bool           // one collection done (the next one yields rates)
}

func newEngineReader() (engineReader, error) {
	q, err := pdhOpenQuery()
	if err != nil {
		return nil, err
	}
	r := &pdhReader{query: q}
	if r.counter, err = pdhAddCounter(q, EngineCounter); err != nil {
		r.Close()
		return nil, err
	}
	r.mem, _ = pdhAddCounter(q, AdapterMemCounter) // optional
	return r, nil
}

// Read collects and returns the instances: nil while the rate counter is
// still being primed (the first collection), an empty slice when no
// process uses any adapter. The shared memory of the same collection is
// kept for SharedUsage.
func (r *pdhReader) Read() ([]EngineSample, error) {
	if r.query == 0 {
		return nil, fmt.Errorf("pdh query closed")
	}
	primed := r.collected
	r.collected = true
	r.shared = nil
	if st, _, _ := procPdhCollectQueryData.Call(r.query); st != 0 {
		if uint32(st) == pdhNoData && primed {
			return []EngineSample{}, nil
		}
		if uint32(st) == pdhNoData {
			return nil, nil
		}
		return nil, fmt.Errorf("PdhCollectQueryData: 0x%08X", uint32(st))
	}
	if r.mem != 0 {
		if ss, ok, err := pdhArray(r.mem); err == nil && ok {
			r.shared = ss
		}
	}
	if !primed {
		return nil, nil
	}
	ss, ok, err := pdhArray(r.counter)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return ss, nil
}

// SharedUsage implements adapterMemReader: the AdapterMemCounter instances
// of the last Read (nil when the counter is missing or had no data).
func (r *pdhReader) SharedUsage() []EngineSample { return r.shared }

func (r *pdhReader) Close() {
	if r.query != 0 {
		procPdhCloseQuery.Call(r.query)
		r.query, r.counter, r.mem = 0, 0, 0
	}
}

// utf16PtrToString reads a NUL-terminated UTF-16 string (the names live in
// the item buffer, after the items).
func utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	n := 0
	for ptr := unsafe.Pointer(p); *(*uint16)(ptr) != 0; ptr = unsafe.Add(ptr, 2) {
		n++
	}
	return syscall.UTF16ToString(unsafe.Slice(p, n))
}
