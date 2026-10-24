// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package api

import (
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32                          = syscall.NewLazyDLL("kernel32.dll")
	procGetUserDefaultLocaleName      = kernel32.NewProc("GetUserDefaultLocaleName")
	procGetDynamicTimeZoneInformation = kernel32.NewProc("GetDynamicTimeZoneInformation")
)

// systemRegion returns "cn" when Windows is set up for mainland China: the
// user locale zh-CN (zh-Hans-CN), or the time zone "China Standard Time"
// unless the locale names Hong Kong, Macao or Taiwan (that zone also
// covers Hong Kong). Only a suggestion for the download sources.
func systemRegion() string {
	return regionFrom(userLocaleName(), timeZoneKeyName())
}

func userLocaleName() string {
	if procGetUserDefaultLocaleName.Find() != nil {
		return ""
	}
	buf := make([]uint16, 85) // LOCALE_NAME_MAX_LENGTH
	n, _, _ := procGetUserDefaultLocaleName.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf)
}

// dynamicTimeZoneInformation is DYNAMIC_TIME_ZONE_INFORMATION.
type dynamicTimeZoneInformation struct {
	Bias                        int32
	StandardName                [32]uint16
	StandardDate                [8]uint16 // SYSTEMTIME
	StandardBias                int32
	DaylightName                [32]uint16
	DaylightDate                [8]uint16
	DaylightBias                int32
	TimeZoneKeyName             [128]uint16
	DynamicDaylightTimeDisabled uint8
}

func timeZoneKeyName() string {
	if procGetDynamicTimeZoneInformation.Find() != nil {
		return ""
	}
	var tz dynamicTimeZoneInformation
	r, _, _ := procGetDynamicTimeZoneInformation.Call(uintptr(unsafe.Pointer(&tz)))
	if r == 0xFFFFFFFF { // TIME_ZONE_ID_INVALID
		return ""
	}
	return syscall.UTF16ToString(tz.TimeZoneKeyName[:])
}

// regionFrom decides from a locale name and a Windows time zone key.
func regionFrom(locale, tzKey string) string {
	l := strings.ToLower(locale)
	if i := strings.IndexAny(l, ".@"); i >= 0 {
		l = l[:i]
	}
	l = strings.ReplaceAll(l, "_", "-")
	switch {
	case l == "zh-cn" || l == "zh-hans-cn":
		return "cn"
	case strings.HasSuffix(l, "-hk") || strings.HasSuffix(l, "-mo") || strings.HasSuffix(l, "-tw"):
		return ""
	case tzKey == "China Standard Time":
		return "cn"
	}
	return ""
}
