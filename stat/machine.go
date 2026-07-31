package stat

import (
	"fmt"

	pbstat "github.com/on-keyday/kscale/protobuf/proto/stat"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
)

// GetMachineStat samples this host's realtime CPU / memory / load and returns them as the
// proto HostRealtimeStat (ported from ksdk's GetMachineStat). CPU is sampled non-blocking
// — usage since the previous call — so it fits a periodic reporting loop without stalling
// it; the first sample after process start reads ~0. ServerUptime is the dp's own uptime.
func GetMachineStat() (*pbstat.HostRealtimeStat, error) {
	out := &pbstat.HostRealtimeStat{ServerUptime: int64(ServerUptime())}

	cpus, err := cpu.Percent(0, true)
	if err != nil {
		return nil, fmt.Errorf("cpu usage: %w", err)
	}
	out.CpuUsages = cpus

	vmem, err := mem.VirtualMemory()
	if err != nil {
		return nil, fmt.Errorf("virtual memory: %w", err)
	}
	out.MemoryUsage = vmem.Used

	// LoadAvg is best-effort (unsupported on some platforms) — don't fail the whole sample.
	if la, err := load.Avg(); err == nil {
		out.LoadAvg = []float64{la.Load1, la.Load5, la.Load15}
	}
	return out, nil
}
