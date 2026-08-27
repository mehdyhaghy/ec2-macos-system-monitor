# Amazon EC2 System Monitor for macOS


## Overview
Amazon EC2 System Monitor for macOS is a small agent that runs on every [mac1.metal](https://aws.amazon.com/ec2/instance-types/mac/)
instance to provide on-instance metrics in CloudWatch. Currently the primary use case for this agent is to send CPU utilization
metrics. This uses a serial connection attached via the [AWS Nitro System](https://aws.amazon.com/ec2/nitro/) 
and is forwarded to CloudWatch for the instance automatically. 

## Metrics

| Tag | Interval | Contents |
|---|---|---|
| `cpuutil` | 60s | Total CPU utilization as a percentage |
| `diskfree` | 300s | Per-volume free space and capacity |
| `memutil` | 300s | Physical memory use and macOS memory pressure level |
| `swaputil` | 300s | Swap use |
| `thermal` | 300s | Thermal pressure level and a throttling flag |
| `hosthealth` | 300s | Uptime, boot time and panic report count |

`cpuutil` is a bare decimal string. The remaining tags carry a JSON object, or a
JSON array in the case of `diskfree`, which reports one entry per volume.

Collection is partial-tolerant: a metric that cannot be gathered is logged and
skipped so that a single unavailable source does not suppress the others.

### Volumes measured by `diskfree`

On macOS 10.15 and later, `/` is a sealed read-only APFS system snapshot that
shares an APFS container with the writable data volume. The two report identical
free bytes but very different used percentages, so `/` is misleading as a
capacity signal: on a macOS Tahoe host measured during development, `/` reported
15% used while the data volume that actually fills reported 84% used.

`diskfree` therefore measures `/System/Volumes/Data` plus any volume under
`/Volumes` whose name begins with `ephemeral` or `InternalDisk`, which are the
names used for the internal SSD on EC2 Mac instances. Read-only volumes and
other entries under `/Volumes`, such as mounted disk images, are skipped to keep
the CloudWatch dimension cardinality bounded.

### Alarming on `diskfree`

Five minutes is loose for free space, because a runaway build can consume the
remaining headroom on a small root volume inside a single interval. Alarm with
margin, in the range of 20 to 25 percent free rather than 10, so that one missed
interval does not become a kernel panic.

### Thermal pressure source

`thermal` reads the level that `thermald` publishes to the Darwin notification
system, using the `kOSThermalNotificationPressureLevelName` constant from
`<libkern/OSThermalNotification.h>`. That symbol is available from macOS 10.10,
so the same code path works on Intel and on every Apple Silicon generation. The
level is reported as `nominal`, `moderate`, `heavy`, `trapping` or `sleeping`,
and `throttling` is true from `heavy` upward, which is the point at which clocks
are actually being reduced.

This source was chosen over the alternatives for portability. SMC temperature
keys differ per chip generation. `thermal levels` and `pmset -g thermlog` are
Intel era interfaces that report nothing on Apple Silicon.
`ProcessInfo.thermalState` is portable but collapses `moderate` and `heavy` into
a single `fair` value on Apple Silicon, which cannot distinguish a warm instance
from a throttling one.

Reading this level requires cgo, which the build already enables. With cgo
disabled the agent still builds and reports every other metric.

## Usage
The agent is installed and enabled by default for all AMIs vended by AWS. It logs to 
`/var/log/amazon/ec2/system-monitoring.log` and can be updated via [Homebrew](https://github.com/aws/homebrew-aws). 

### Managing the monitor with `setup-ec2monitoring`
The package includes a shell script for enabling, disabling, and listing the current status of the agent 
according to `launchd`. 

### Viewing the agent status
To view the status of the agent:
```bash
sudo setup-ec2monitoring list
```

### Enabling the agent
To enable/install ec2-macos-system-monitor:
```bash
sudo setup-ec2monitoring enable
```
This must be run if updating to a new version to ensure it is scheduled to run again. 

### Disabling the agent
To disable ec2-macos-system-monitor:
```bash
sudo setup-ec2monitoring disable
```

## Design
The Amazon EC2 System Monitor for macOS uses multiple goroutines to manage two primary mechanisms:
1. The serial relay takes data from a UNIX domain socket and writes the data in a payload via a basic wire protocol.
2. Runs a ticker that reads CPU utilization and sends the CPU usage percentage to the UNIX domain socket.

This design allows for multiple different processes to write to the serial device while allowing one process to
always have the device open for writing. 

### Wire protocol
The wire protocol's primary purpose is to ensure the payload is complete by wrapping the payload in a checksum.
There is a tag which is used as a namespace to ensure the reader knows what type of data is being written. The data
itself which, in this case is typically the CPU utilization as a percentage of total usage the second field. Finally, a 
boolean is set specifying if the data should be compressed before sending. A checksum is then computed on this payload 
and included along with the payload. This payload with checksum allows the receiver to ensure that all the data was 
correctly received as well as inform if the data should be decompressed before parsing.

## Security

See [CONTRIBUTING](CONTRIBUTING.md#security-issue-notifications) for more information.

## License

This project is licensed under the [Apache License, version 2.0](https://www.apache.org/licenses/LICENSE-2.0).

