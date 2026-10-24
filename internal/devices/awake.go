// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

// Keeping the system awake while transcription runs (power management,
// v2-step20 freeze; lead addition after I12). I12 found that once the idle
// timeout put this machine into Modern Standby (S0, network off) the
// process kept running but the NVIDIA GPU dropped to P5 (about 32 W) and
// the CUDA RTF fell from 0.05–0.06 to 0.29–0.32. The scheduler therefore
// holds a power request while any pipeline asr job or self-test runs
// (sched.Options.KeepAwake) and releases it when the last one ends. No
// system setting is ever changed; the display may still turn off.
//
// FROZEN types and signatures (power-brief §2). Implementation (task P1,
// lead ruling 9.5): Windows PowerCreateRequest + PowerSetRequest
// (PowerRequestSystemRequired and PowerRequestExecutionRequired), which
// works per request handle, independent of the calling thread (unlike
// SetThreadExecutionState, which a goroutine could not keep on one
// thread); other systems: no-op. Never test it with SC_MONITORPOWER (this
// machine goes straight into disconnected standby); the unit tests replace
// the platform calls; the lead verifies it by letting the display time
// out and checking Kernel-Power event 506.

// KeepAwakeReason is the request's reason text (shown by powercfg
// /requests).
const KeepAwakeReason = "LectureFlow is transcribing a lecture"

// KeepAwake is one power request of the process.
type KeepAwake struct {
	reason    string
	on        bool
	h         uintptr // the request handle (PowerCreateRequest; 0 outside Windows)
	closed    bool
	reasonBuf []uint16 // the UTF-16 reason, kept alive with the handle
}

// The platform calls (awake_windows.go, awake_other.go); variables so the
// tests can record the sequence without touching the system.
var (
	awakeCreate = platformAwakeCreate // creates the request (not yet held)
	awakeSet    = platformAwakeSet    // holds it
	awakeClear  = platformAwakeClear  // releases it
	awakeClose  = platformAwakeClose  // closes the handle
)

// NewKeepAwake creates the request (not yet held). On Windows it is
// PowerCreateRequest with a simple reason string; an error is the call's.
// Other systems: never fails, does nothing.
func NewKeepAwake(reason string) (*KeepAwake, error) {
	k := &KeepAwake{reason: reason}
	h, buf, err := awakeCreate(reason)
	if err != nil {
		return nil, err
	}
	k.h, k.reasonBuf = h, buf
	return k, nil
}

// Set holds (on) or releases (off) the request; setting the current state
// again is a no-op. Not safe for concurrent use: the scheduler serialises
// its calls. Holding = PowerSetRequest(PowerRequestSystemRequired) and
// PowerSetRequest(PowerRequestExecutionRequired) (NOTES "决定（电源管理
// P1）"); releasing clears both. No system setting is changed; the display
// may still turn off.
func (k *KeepAwake) Set(on bool) error {
	if k == nil || k.closed || k.on == on {
		return nil
	}
	if on {
		if err := awakeSet(k.h); err != nil {
			return err
		}
	} else if err := awakeClear(k.h); err != nil {
		return err
	}
	k.on = on
	return nil
}

// On reports whether the request is held (tests).
func (k *KeepAwake) On() bool { return k != nil && k.on }

// Close releases the request for good (App.Close). Later Set calls do
// nothing.
func (k *KeepAwake) Close() error {
	if k == nil || k.closed {
		return nil
	}
	var err error
	if k.on {
		err = awakeClear(k.h)
		k.on = false
	}
	if cerr := awakeClose(k.h); err == nil {
		err = cerr
	}
	k.closed = true
	return err
}
