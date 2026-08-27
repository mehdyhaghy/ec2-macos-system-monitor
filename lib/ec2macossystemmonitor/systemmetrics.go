package ec2macossystemmonitor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	"golang.org/x/sys/unix"
)

// Metric tags namespace each payload on the serial device. The receiving end of
// the serial relay must recognize a tag before the data reaches CloudWatch, so
// these values are part of the wire contract and must not be renamed.
const (
	// TagCPUUtilization carries total CPU utilization as a percentage.
	TagCPUUtilization = "cpuutil"
	// TagDiskFree carries per-volume free space.
	TagDiskFree = "diskfree"
	// TagMemoryUtilization carries physical memory use and macOS memory pressure.
	TagMemoryUtilization = "memutil"
	// TagSwapUtilization carries swap use.
	TagSwapUtilization = "swaputil"
	// TagThermal carries the system thermal pressure state.
	TagThermal = "thermal"
	// TagHostHealth carries uptime and unexpected-reboot evidence.
	TagHostHealth = "hosthealth"
)

// dataVolumePath is the writable APFS data volume on macOS 10.15 and later.
//
// "/" is not measured. It is a sealed read-only system snapshot sharing an APFS
// container with the data volume, so it reports the same free bytes but a used
// percentage that does not track the volume that actually fills.
const dataVolumePath = "/System/Volumes/Data"

// instanceVolumePrefixes are the volume names used for the internal SSD
// attached to EC2 Mac metal instances. Apple Silicon instance families expose
// the internal disk as /Volumes/InternalDisk and its provisioned partitions as
// /Volumes/ephemeral0 and similar. Other entries under /Volumes, such as
// mounted disk images or network shares, are ignored on purpose to keep the
// CloudWatch dimension cardinality bounded and predictable.
var instanceVolumePrefixes = []string{"ephemeral", "InternalDisk"}

// diagnosticReportsPath holds macOS panic reports. A launchd daemon running as
// root can normally read this directory, but Tahoe tightened Full Disk Access
// for daemons, so every read here must degrade gracefully rather than fail the
// collection cycle.
const diagnosticReportsPath = "/Library/Logs/DiagnosticReports"

// vmPressureSysctl exposes the macOS memory pressure level. This sysctl is
// present on Apple Silicon under Tahoe and is the supported way to read
// pressure without spawning the memory_pressure binary.
const vmPressureSysctl = "kern.memorystatus_vm_pressure_level"

// macOS memory pressure levels as reported by vmPressureSysctl. The kernel uses
// a sparse bitmask rather than sequential values, so 3 is not a valid level.
const (
	memoryPressureNormal   = 1
	memoryPressureWarning  = 2
	memoryPressureCritical = 4
)

// VolumeUsage describes free space for a single mounted volume. FreePercent is
// the value to alarm on, and Mount becomes the CloudWatch dimension.
type VolumeUsage struct {
	Mount       string  `json:"mount"`
	FreePercent float64 `json:"free_pct"`
	FreeBytes   uint64  `json:"free_bytes"`
	TotalBytes  uint64  `json:"total_bytes"`
}

// MemoryUsage describes physical memory use plus the macOS pressure level.
// PressureLevel is more actionable than UsedPercent on macOS, because the
// kernel reports high memory use as normal while it still has compressible
// pages available.
type MemoryUsage struct {
	UsedPercent   float64 `json:"used_pct"`
	AvailBytes    uint64  `json:"avail_bytes"`
	TotalBytes    uint64  `json:"total_bytes"`
	PressureLevel uint32  `json:"pressure_level"`
	PressureLabel string  `json:"pressure_label"`
}

// SwapUsage describes swap use. Sustained swap growth on a build fleet is an
// early indicator of memory exhaustion that CPU utilization does not surface.
type SwapUsage struct {
	UsedPercent float64 `json:"used_pct"`
	UsedBytes   uint64  `json:"used_bytes"`
	FreeBytes   uint64  `json:"free_bytes"`
	TotalBytes  uint64  `json:"total_bytes"`
}

// HostHealth describes uptime and evidence of unexpected reboots. Uptime alone
// detects a reboot because the counter resets, which makes this payload useful
// even when PanicReadable is false.
type HostHealth struct {
	UptimeSeconds uint64 `json:"uptime_seconds"`
	BootTime      uint64 `json:"boot_time"`
	PanicCount    int    `json:"panic_count"`
	PanicReadable bool   `json:"panic_readable"`
}

// macOS thermal pressure levels as published by thermald, matching the
// TARGET_OS_OSX branch of kOSThermalPressureLevel in
// /usr/include/libkern/OSThermalNotification.h. Verified against the macOS
// Tahoe SDK. Note that the non-macOS branch of that enum uses a different,
// sparse numbering, so these values must not be reused for other platforms.
const (
	thermalNominal   = 0
	thermalModerate  = 1
	thermalHeavy     = 2
	thermalTrapping  = 3
	thermalSleeping  = 4
	thermalThrottled = thermalHeavy
)

// ThermalPressure describes the system thermal pressure state.
//
// Throttling is the field to alarm on. It is true from the heavy level upward,
// which is the point at which the SoC is actually reducing clocks rather than
// merely running warm. Reporting the boundary here rather than leaving it to
// the consumer keeps the alarm definition from drifting away from the enum.
type ThermalPressure struct {
	Level      uint64 `json:"level"`
	LevelLabel string `json:"level_label"`
	Throttling bool   `json:"throttling"`
}

// thermalLabel maps a macOS thermal pressure level to a readable label.
func thermalLabel(level uint64) string {
	switch level {
	case thermalNominal:
		return "nominal"
	case thermalModerate:
		return "moderate"
	case thermalHeavy:
		return "heavy"
	case thermalTrapping:
		return "trapping"
	case thermalSleeping:
		return "sleeping"
	default:
		return "unknown"
	}
}

// ThermalState gathers the current system thermal pressure.
//
// This reads the thermal pressure level that thermald publishes to the Darwin
// notification system, which is the same source powermetrics reports and is
// readable without root.
//
// The notification name is the SDK constant
// kOSThermalNotificationPressureLevelName, exported with an availability of
// macOS 10.10, so the Intel mac1.metal generation and every Apple Silicon
// generation resolve the same public symbol with the same level enum.
func ThermalState() (pressure ThermalPressure, err error) {
	level, err := readThermalPressureLevel()
	if err != nil {
		return ThermalPressure{}, err
	}

	return ThermalPressure{
		Level:      level,
		LevelLabel: thermalLabel(level),
		Throttling: level >= thermalThrottled,
	}, nil
}

// pressureLabel maps a macOS memory pressure level to a readable label.
func pressureLabel(level uint32) string {
	switch level {
	case memoryPressureNormal:
		return "normal"
	case memoryPressureWarning:
		return "warning"
	case memoryPressureCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// isMonitoredVolume reports whether a mount point should be measured.
//
// The data volume is always measured. Entries under /Volumes are measured only
// when they match a known EC2 Mac internal disk name, which excludes mounted
// installers and disk images that would otherwise churn the dimension set.
func isMonitoredVolume(mountPoint string) bool {
	if mountPoint == dataVolumePath {
		return true
	}

	name, found := strings.CutPrefix(mountPoint, "/Volumes/")
	if !found {
		return false
	}

	for _, prefix := range instanceVolumePrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}

	return false
}

// isReadOnly reports whether a partition is mounted read-only. The sealed
// system volume on Tahoe is read-only and is skipped by this check even if it
// somehow passes the mount point filter.
func isReadOnly(opts []string) bool {
	for _, opt := range opts {
		if opt == "ro" {
			return true
		}
	}

	return false
}

// DiskUsage gathers free space for the volumes that can fill on an EC2 Mac
// instance. It returns at least one entry on success.
//
// A failure to stat a single volume is not fatal: a volume can be unmounted
// between enumeration and measurement, and losing one volume is preferable to
// losing the whole collection cycle. An error is returned only when no volume
// could be measured at all.
func DiskUsage() (usage []VolumeUsage, err error) {
	partitions, err := disk.Partitions(false)
	if err != nil {
		return nil, fmt.Errorf("ec2macossystemmonitor: error while listing partitions: %w", err)
	}

	for _, partition := range partitions {
		if !isMonitoredVolume(partition.Mountpoint) || isReadOnly(partition.Opts) {
			continue
		}

		stat, statErr := disk.Usage(partition.Mountpoint)
		if statErr != nil || stat == nil || stat.Total == 0 {
			continue
		}

		usage = append(usage, VolumeUsage{
			Mount:       partition.Mountpoint,
			FreePercent: 100 - stat.UsedPercent,
			FreeBytes:   stat.Free,
			TotalBytes:  stat.Total,
		})
	}

	if len(usage) == 0 {
		return nil, fmt.Errorf("ec2macossystemmonitor: no monitored volumes could be measured")
	}

	return usage, nil
}

// MemoryUtilization gathers physical memory use and the macOS memory pressure
// level.
//
// A missing pressure sysctl is not fatal. The used percentage still has value
// on its own, so the pressure level falls back to an unknown label rather than
// failing the whole metric.
func MemoryUtilization() (usage MemoryUsage, err error) {
	virtual, err := mem.VirtualMemory()
	if err != nil {
		return MemoryUsage{}, fmt.Errorf("ec2macossystemmonitor: error while getting memory stats: %w", err)
	}

	usage = MemoryUsage{
		UsedPercent: virtual.UsedPercent,
		AvailBytes:  virtual.Available,
		TotalBytes:  virtual.Total,
	}

	level, sysctlErr := syscall.SysctlUint32(vmPressureSysctl)
	if sysctlErr != nil {
		usage.PressureLabel = pressureLabel(0)
		return usage, nil
	}

	usage.PressureLevel = level
	usage.PressureLabel = pressureLabel(level)

	return usage, nil
}

// SwapUtilization gathers swap use from the kernel. On Apple Silicon this reads
// the vm.swapusage sysctl, which remains available under Tahoe.
func SwapUtilization() (usage SwapUsage, err error) {
	swap, err := mem.SwapMemory()
	if err != nil {
		return SwapUsage{}, fmt.Errorf("ec2macossystemmonitor: error while getting swap stats: %w", err)
	}

	return SwapUsage{
		UsedPercent: swap.UsedPercent,
		UsedBytes:   swap.Used,
		FreeBytes:   swap.Free,
		TotalBytes:  swap.Total,
	}, nil
}

// panicReportCount counts macOS panic reports on disk.
//
// The boolean return reports whether the directory could be read at all, which
// distinguishes a genuinely healthy host with zero panics from a host where the
// daemon lacks the access needed to look.
func panicReportCount() (count int, readable bool) {
	entries, err := os.ReadDir(diagnosticReportsPath)
	if err != nil {
		return 0, false
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(entry.Name()), ".panic") {
			count++
		}
	}

	return count, true
}

// bootTimeSysctl reports the kernel boot time. Reading it directly avoids
// pulling in gopsutil's host package, which transitively compiles in its
// process and net packages for information this agent does not report.
const bootTimeSysctl = "kern.boottime"

// hostBootTime returns the kernel boot time.
func hostBootTime() (bootTime time.Time, err error) {
	tv, err := unix.SysctlTimeval(bootTimeSysctl)
	if err != nil {
		return time.Time{}, fmt.Errorf("ec2macossystemmonitor: error while reading %s: %w", bootTimeSysctl, err)
	}

	return time.Unix(tv.Sec, int64(tv.Usec)*int64(time.Microsecond)), nil
}

// HostHealthStatus gathers uptime and unexpected reboot evidence.
func HostHealthStatus() (health HostHealth, err error) {
	bootTime, err := hostBootTime()
	if err != nil {
		return HostHealth{}, err
	}

	// Clamp rather than report a negative duration. A boot time in the future
	// means the clock was stepped backwards, which is not something this metric
	// can describe, and an unsigned field cannot carry it.
	uptime := time.Since(bootTime)
	if uptime < 0 {
		uptime = 0
	}

	count, readable := panicReportCount()

	return HostHealth{
		UptimeSeconds: uint64(uptime.Seconds()),
		BootTime:      uint64(bootTime.Unix()),
		PanicCount:    count,
		PanicReadable: readable,
	}, nil
}

// encodeMetric marshals a metric payload to the compact JSON string carried in
// the data field of a serial payload.
func encodeMetric(v interface{}) (string, error) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("ec2macossystemmonitor: error while encoding metric: %w", err)
	}

	return string(encoded), nil
}

// SystemMetric pairs a wire tag with its encoded payload.
type SystemMetric struct {
	Tag  string
	Data string
	// Compress requests zlib compression of the payload on the wire.
	//
	// Only diskfree sets this. It is the one payload whose length grows with
	// the host, since it carries an entry per volume. The fixed-size payloads
	// are small enough that the base64 encoding applied to compressed data
	// cancels out the saving.
	Compress bool
}

// CollectSystemMetrics gathers the interval metrics and returns those that
// succeeded along with the errors for those that did not.
//
// Collection is deliberately partial-tolerant. A single unavailable metric,
// such as thermal state on a host where the framework call is unsupported,
// must not suppress the disk metric that exists to prevent a kernel panic.
func CollectSystemMetrics() (metrics []SystemMetric, errs []error) {
	if usage, err := DiskUsage(); err != nil {
		errs = append(errs, err)
	} else if data, encErr := encodeMetric(usage); encErr != nil {
		errs = append(errs, encErr)
	} else {
		metrics = append(metrics, SystemMetric{Tag: TagDiskFree, Data: data, Compress: true})
	}

	if usage, err := MemoryUtilization(); err != nil {
		errs = append(errs, err)
	} else if data, encErr := encodeMetric(usage); encErr != nil {
		errs = append(errs, encErr)
	} else {
		metrics = append(metrics, SystemMetric{Tag: TagMemoryUtilization, Data: data})
	}

	if usage, err := SwapUtilization(); err != nil {
		errs = append(errs, err)
	} else if data, encErr := encodeMetric(usage); encErr != nil {
		errs = append(errs, encErr)
	} else {
		metrics = append(metrics, SystemMetric{Tag: TagSwapUtilization, Data: data})
	}

	if state, err := ThermalState(); err != nil {
		errs = append(errs, err)
	} else if data, encErr := encodeMetric(state); encErr != nil {
		errs = append(errs, encErr)
	} else {
		metrics = append(metrics, SystemMetric{Tag: TagThermal, Data: data})
	}

	if health, err := HostHealthStatus(); err != nil {
		errs = append(errs, err)
	} else if data, encErr := encodeMetric(health); encErr != nil {
		errs = append(errs, encErr)
	} else {
		metrics = append(metrics, SystemMetric{Tag: TagHostHealth, Data: data})
	}

	return metrics, errs
}
