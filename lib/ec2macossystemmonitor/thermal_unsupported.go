//go:build !darwin || !cgo

package ec2macossystemmonitor

import (
	"fmt"
)

// readThermalPressureLevel is unavailable without cgo on darwin.
//
// The thermal pressure level is only reachable through the Darwin notification
// system, which requires a C call. The build sets CGO_ENABLED=1, so this path
// exists to keep vet, lint, and non-darwin tooling working rather than to serve
// production. CollectSystemMetrics treats the resulting error as non-fatal, so
// the remaining metrics are unaffected.
func readThermalPressureLevel() (level uint64, err error) {
	return 0, fmt.Errorf("ec2macossystemmonitor: thermal pressure requires a cgo-enabled darwin build")
}
