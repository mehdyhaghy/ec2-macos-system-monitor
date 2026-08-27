//go:build darwin && cgo

package ec2macossystemmonitor

/*
#include <libkern/OSThermalNotification.h>
#include <notify.h>
#include <stdint.h>

// read_thermal_pressure fetches the current thermal pressure level that
// thermald publishes to the Darwin notification system.
//
// The notification name comes from kOSThermalNotificationPressureLevelName
// rather than a hardcoded string. That symbol is exported by the SDK with an
// availability of macOS 10.10, which is what makes this approach portable
// across every EC2 Mac instance family: the Intel mac1.metal generation and
// every Apple Silicon generation resolve the same public constant, so there is
// no per-chip interface to maintain.
//
// notify_register_check gives a cheap polling token rather than a delivery
// callback, which suits an interval collector: there is no run loop to service
// and no notification backlog to drain. The token is registered and cancelled
// per call so the collector holds no kernel state between intervals.
//
// The notify_* symbols live in libSystem, so no additional linker flag is
// required beyond the default C runtime.
static uint32_t read_thermal_pressure(uint64_t *out_level) {
	int token = 0;

	uint32_t status = notify_register_check(kOSThermalNotificationPressureLevelName, &token);
	if (status != NOTIFY_STATUS_OK) {
		return status;
	}

	status = notify_get_state(token, out_level);

	// Always release the token, even when the read failed, so a persistently
	// failing notification cannot leak registrations over the life of the
	// daemon.
	notify_cancel(token);

	return status;
}
*/
import "C"

import (
	"fmt"
)

// readThermalPressureLevel returns the raw macOS thermal pressure level.
//
// A non-zero status from the notification layer is reported as an error rather
// than silently coerced to nominal, because reporting a healthy thermal state
// for a host whose thermal telemetry is unavailable would hide exactly the
// condition this metric exists to surface.
func readThermalPressureLevel() (level uint64, err error) {
	var raw C.uint64_t

	status := C.read_thermal_pressure(&raw)
	if status != C.NOTIFY_STATUS_OK {
		return 0, fmt.Errorf("ec2macossystemmonitor: could not read thermal pressure level, notify status %d", int(status))
	}

	return uint64(raw), nil
}
