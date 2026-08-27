package ec2macossystemmonitor

import (
	"encoding/json"
	"runtime"
	"testing"
)

func TestIsMonitoredVolume(t *testing.T) {
	tests := []struct {
		name  string
		mount string
		want  bool
	}{
		{name: "Data volume", mount: dataVolumePath, want: true},
		{name: "Ephemeral zero", mount: "/Volumes/ephemeral0", want: true},
		{name: "Ephemeral one", mount: "/Volumes/ephemeral1", want: true},
		{name: "Internal disk", mount: "/Volumes/InternalDisk", want: true},
		// "/" is the sealed read-only system snapshot on modern macOS. Its
		// capacity percentage does not reflect the volume that actually fills,
		// so it must not be measured.
		{name: "Sealed system volume", mount: "/", want: false},
		{name: "Preboot volume", mount: "/System/Volumes/Preboot", want: false},
		{name: "VM volume", mount: "/System/Volumes/VM", want: false},
		{name: "Mounted disk image", mount: "/Volumes/Some Installer", want: false},
		{name: "Network share", mount: "/Volumes/share", want: false},
		{name: "Empty", mount: "", want: false},
		// A volume whose name merely contains a prefix must not match, since
		// matching is anchored at the start of the volume name.
		{name: "Prefix not at start", mount: "/Volumes/not-ephemeral0", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isMonitoredVolume(tt.mount); got != tt.want {
				t.Errorf("isMonitoredVolume(%q) = %v, want %v", tt.mount, got, tt.want)
			}
		})
	}
}

func TestIsReadOnly(t *testing.T) {
	tests := []struct {
		name string
		opts []string
		want bool
	}{
		{name: "Read only", opts: []string{"ro"}, want: true},
		{name: "Read write", opts: []string{"rw"}, want: false},
		{name: "Read only with others", opts: []string{"nobrowse", "ro", "noowners"}, want: true},
		{name: "No options", opts: nil, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isReadOnly(tt.opts); got != tt.want {
				t.Errorf("isReadOnly(%v) = %v, want %v", tt.opts, got, tt.want)
			}
		})
	}
}

func TestPressureLabel(t *testing.T) {
	tests := []struct {
		level uint32
		want  string
	}{
		{level: memoryPressureNormal, want: "normal"},
		{level: memoryPressureWarning, want: "warning"},
		{level: memoryPressureCritical, want: "critical"},
		// The kernel uses a sparse bitmask, so 3 is not a defined level and
		// must not be silently reported as one of the known labels.
		{level: 3, want: "unknown"},
		{level: 0, want: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := pressureLabel(tt.level); got != tt.want {
				t.Errorf("pressureLabel(%d) = %q, want %q", tt.level, got, tt.want)
			}
		})
	}
}

func TestThermalLabel(t *testing.T) {
	tests := []struct {
		level uint64
		want  string
	}{
		{level: thermalNominal, want: "nominal"},
		{level: thermalModerate, want: "moderate"},
		{level: thermalHeavy, want: "heavy"},
		{level: thermalTrapping, want: "trapping"},
		{level: thermalSleeping, want: "sleeping"},
		{level: 99, want: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := thermalLabel(tt.level); got != tt.want {
				t.Errorf("thermalLabel(%d) = %q, want %q", tt.level, got, tt.want)
			}
		})
	}
}

// TestThermalThrottlingBoundary pins the level at which the metric reports
// throttling. Moving this boundary changes alarm behavior on every instance, so
// it is asserted rather than left implicit.
func TestThermalThrottlingBoundary(t *testing.T) {
	tests := []struct {
		name  string
		level uint64
		want  bool
	}{
		{name: "Nominal is not throttling", level: thermalNominal, want: false},
		{name: "Moderate is not throttling", level: thermalModerate, want: false},
		{name: "Heavy is throttling", level: thermalHeavy, want: true},
		{name: "Trapping is throttling", level: thermalTrapping, want: true},
		{name: "Sleeping is throttling", level: thermalSleeping, want: true},
		// An unrecognized level fails toward alerting rather than reporting a
		// healthy state for telemetry we do not understand.
		{name: "Unknown level is throttling", level: 99, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.level >= thermalThrottled; got != tt.want {
				t.Errorf("level %d throttling = %v, want %v", tt.level, got, tt.want)
			}
		})
	}
}

func TestEncodeMetric(t *testing.T) {
	usage := SwapUsage{UsedPercent: 94.8, UsedBytes: 9157, FreeBytes: 505, TotalBytes: 9662}

	encoded, err := encodeMetric(usage)
	if err != nil {
		t.Fatalf("encodeMetric() error = %v", err)
	}

	var decoded SwapUsage
	if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
		t.Fatalf("payload is not valid json: %v", err)
	}

	if decoded != usage {
		t.Errorf("round trip = %+v, want %+v", decoded, usage)
	}
}

// TestMetricTagsAreDistinct guards the wire contract. The receiving end of the
// serial relay dispatches on tag, so a duplicate tag would silently overwrite
// another metric.
func TestMetricTagsAreDistinct(t *testing.T) {
	tags := []string{
		TagCPUUtilization,
		TagDiskFree,
		TagMemoryUtilization,
		TagSwapUtilization,
		TagThermal,
		TagHostHealth,
	}

	seen := make(map[string]bool, len(tags))
	for _, tag := range tags {
		if tag == "" {
			t.Error("found an empty metric tag")
		}
		if seen[tag] {
			t.Errorf("duplicate metric tag %q", tag)
		}
		seen[tag] = true
	}
}

func TestDiskUsage(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("volume layout is darwin specific")
	}

	usage, err := DiskUsage()
	if err != nil {
		t.Fatalf("DiskUsage() error = %v", err)
	}
	if len(usage) == 0 {
		t.Fatal("DiskUsage() returned no volumes")
	}

	for _, volume := range usage {
		if volume.Mount == "/" {
			t.Errorf("measured the sealed system volume %q", volume.Mount)
		}
		if volume.TotalBytes == 0 {
			t.Errorf("volume %q reported zero total bytes", volume.Mount)
		}
		if volume.FreePercent < 0 || volume.FreePercent > 100 {
			t.Errorf("volume %q free percent %v is out of range", volume.Mount, volume.FreePercent)
		}
	}
}

func TestMemoryUtilization(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("memory pressure sysctl is darwin specific")
	}

	usage, err := MemoryUtilization()
	if err != nil {
		t.Fatalf("MemoryUtilization() error = %v", err)
	}
	if usage.TotalBytes == 0 {
		t.Error("total memory is zero")
	}
	if usage.UsedPercent < 0 || usage.UsedPercent > 100 {
		t.Errorf("used percent %v is out of range", usage.UsedPercent)
	}
	if usage.PressureLabel == "" {
		t.Error("pressure label is empty")
	}
}

func TestSwapUtilization(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("swap accounting is darwin specific")
	}

	// Swap can legitimately be zero on a freshly booted instance, so only the
	// call itself and the percentage range are asserted.
	usage, err := SwapUtilization()
	if err != nil {
		t.Fatalf("SwapUtilization() error = %v", err)
	}
	if usage.UsedPercent < 0 || usage.UsedPercent > 100 {
		t.Errorf("used percent %v is out of range", usage.UsedPercent)
	}
}

func TestThermalState(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("thermal pressure notification is darwin specific")
	}

	pressure, err := ThermalState()
	if err != nil {
		t.Fatalf("ThermalState() error = %v", err)
	}
	if pressure.LevelLabel == "unknown" {
		t.Errorf("thermal level %d did not map to a known label", pressure.Level)
	}
	if pressure.Throttling != (pressure.Level >= thermalThrottled) {
		t.Errorf("throttling %v is inconsistent with level %d", pressure.Throttling, pressure.Level)
	}
}

func TestHostHealthStatus(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("uptime source is darwin specific")
	}

	health, err := HostHealthStatus()
	if err != nil {
		t.Fatalf("HostHealthStatus() error = %v", err)
	}
	if health.UptimeSeconds == 0 {
		t.Error("uptime is zero")
	}
	if health.BootTime == 0 {
		t.Error("boot time is zero")
	}
	if health.PanicCount < 0 {
		t.Errorf("panic count %d is negative", health.PanicCount)
	}
}

// TestCollectSystemMetrics asserts the partial-tolerance contract: the
// collector must return whatever succeeded rather than failing as a unit.
func TestCollectSystemMetrics(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("collectors are darwin specific")
	}

	metrics, errs := CollectSystemMetrics()
	if len(metrics) == 0 {
		t.Fatalf("no metrics collected, errors = %v", errs)
	}

	for _, metric := range metrics {
		if metric.Tag == "" {
			t.Error("collected a metric with an empty tag")
		}
		if !json.Valid([]byte(metric.Data)) {
			t.Errorf("metric %q payload is not valid json: %s", metric.Tag, metric.Data)
		}
		// Every metric must survive the wire encoding it will actually be sent
		// with, otherwise the failure only appears in production.
		if _, err := BuildMessage(metric.Tag, metric.Data, false); err != nil {
			t.Errorf("metric %q could not be built into a message: %v", metric.Tag, err)
		}
	}
}
