package stat

import (
	"fmt"
	"runtime"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	psnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/sensors"
)

// This file ports ksdk's richer host/network collectors (ksdk/stat/stat.go) into
// kscale. They return the stat-package Go types that PromMetrics.Update*Stat consume,
// so a dataplane node can populate its /metrics with full host telemetry — CPU, memory,
// disk, swap, uptime, load, per-NIC counters and temperatures. (machine.go's
// GetMachineStat returns the proto form for the southbound StreamStats path and only
// covers cpu/mem/load/uptime; these cover the rest the dashboards expect.)

// GetHostSpecStat collects static host capability stats (CPU count, total memory/disk,
// OS / platform / kernel). Ported from ksdk's GetMachineData.
func GetHostSpecStat() (*HostSpecStat, error) {
	cpuCount, err := cpu.Counts(true)
	if err != nil {
		return nil, fmt.Errorf("cpu count: %w", err)
	}
	vmem, err := mem.VirtualMemory()
	if err != nil {
		return nil, fmt.Errorf("virtual memory: %w", err)
	}
	du, err := disk.Usage("/")
	if err != nil {
		return nil, fmt.Errorf("disk usage: %w", err)
	}
	// Platform / kernel are best-effort (unavailable in some sandboxes) — don't fail the
	// whole spec sample over them.
	kernel, _ := host.KernelVersion()
	osName, platform, platformVer, _ := host.PlatformInformation()
	return &HostSpecStat{
		CpuCount:      uint32(cpuCount),
		MemoryTotal:   vmem.Total,
		DiskTotal:     du.Total,
		KernelVersion: kernel,
		Os:            osName,
		Platform:      platform,
		PlatformVer:   platformVer,
		Arch:          runtime.GOARCH,
	}, nil
}

// GetHostRealtimeStat collects realtime host stats (per-core CPU usage, memory/disk
// usage, swap %, host + server uptime, load average). Ported from ksdk's GetMachineStat.
// CPU is sampled over a 1s window so the reading is an independent measurement (not tied
// to gopsutil's package-global last-sample, which machine.go's non-blocking sampler uses
// for the StreamStats path).
func GetHostRealtimeStat() (*HostRealtimeStat, error) {
	out := &HostRealtimeStat{ServerUptime: ServerUptime()}
	if up, err := host.Uptime(); err == nil {
		out.HostUptime = up
	}
	cpus, err := cpu.Percent(time.Second, true)
	if err != nil {
		return nil, fmt.Errorf("cpu percent: %w", err)
	}
	out.CpuUsages = cpus
	vmem, err := mem.VirtualMemory()
	if err != nil {
		return nil, fmt.Errorf("virtual memory: %w", err)
	}
	out.MemoryUsage = vmem.Used
	if du, err := disk.Usage("/"); err == nil {
		out.DiskUsage = du.Used
	}
	if sw, err := mem.SwapMemory(); err == nil {
		out.DiskSwap = sw.UsedPercent
	}
	if la, err := load.Avg(); err == nil {
		out.LoadAvg = [3]float64{la.Load1, la.Load5, la.Load15}
	}
	return out, nil
}

// GetHostPhysicalStat collects temperature sensor readings. Ported from ksdk's
// GetHostPhysicalStat. Sensor warnings (a partial read on some platforms) are tolerated;
// a host with no sensors (e.g. a container) returns empty slices, not an error.
func GetHostPhysicalStat() (*HostPhysicalStat, error) {
	temps, err := sensors.SensorsTemperatures()
	if err != nil {
		if _, ok := err.(*sensors.Warnings); !ok {
			return nil, fmt.Errorf("sensors temperatures: %w", err)
		}
	}
	out := &HostPhysicalStat{}
	for _, t := range temps {
		out.TemperatureCelsius = append(out.TemperatureCelsius, t.Temperature)
		out.TemperatureSensorKeys = append(out.TemperatureSensorKeys, t.SensorKey)
		out.TemperatureCelsiusHigh = append(out.TemperatureCelsiusHigh, t.High)
		out.TemperatureCelsiusCritical = append(out.TemperatureCelsiusCritical, t.Critical)
	}
	return out, nil
}

// GetNetworkRealtimeStat collects per-interface byte/packet/error/drop counters. Ported
// from ksdk's GetNetworkData. The slice order matches gopsutil's IOCounters; the
// prometheus collector labels each by the interface at the same index in NetworkSpecStat
// (kept consistent with ksdk's reporting).
func GetNetworkRealtimeStat() (*NetworkRealtimeStat, error) {
	ios, err := psnet.IOCounters(true)
	if err != nil {
		return nil, fmt.Errorf("net io counters: %w", err)
	}
	out := &NetworkRealtimeStat{}
	for _, io := range ios {
		out.RxBytes = append(out.RxBytes, io.BytesRecv)
		out.TxBytes = append(out.TxBytes, io.BytesSent)
		out.RxPackets = append(out.RxPackets, io.PacketsRecv)
		out.TxPackets = append(out.TxPackets, io.PacketsSent)
		out.RxErrors = append(out.RxErrors, io.Errin)
		out.TxErrors = append(out.TxErrors, io.Errout)
		out.RxDrops = append(out.RxDrops, io.Dropin)
		out.TxDrops = append(out.TxDrops, io.Dropout)
	}
	return out, nil
}
