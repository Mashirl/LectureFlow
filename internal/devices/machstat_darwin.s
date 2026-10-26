// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build darwin

// Trampolines to libSystem's Mach host statistics (machstat_darwin.go);
// the same on arm64 and amd64.

#include "textflag.h"

TEXT libc_mach_host_self_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_mach_host_self(SB)
GLOBL	·libc_mach_host_self_trampoline_addr(SB), RODATA, $8
DATA	·libc_mach_host_self_trampoline_addr(SB)/8, $libc_mach_host_self_trampoline<>(SB)

TEXT libc_host_statistics_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_host_statistics(SB)
GLOBL	·libc_host_statistics_trampoline_addr(SB), RODATA, $8
DATA	·libc_host_statistics_trampoline_addr(SB)/8, $libc_host_statistics_trampoline<>(SB)

TEXT libc_host_statistics64_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_host_statistics64(SB)
GLOBL	·libc_host_statistics64_trampoline_addr(SB), RODATA, $8
DATA	·libc_host_statistics64_trampoline_addr(SB)/8, $libc_host_statistics64_trampoline<>(SB)
