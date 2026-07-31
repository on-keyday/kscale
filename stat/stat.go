package stat

import (
	"fmt"
	"net"
	"net/netip"
	"time"
)

// startTime records process start; ServerUptime reports elapsed time since.
// Salvaged minimal base from ksdk stat/stat.go (the agent/trsf-coupled remainder
// was intentionally not brought over).
var startTime time.Time

func init() {
	startTime = time.Now()
}

// ServerUptime returns the duration since process start.
func ServerUptime() time.Duration {
	return time.Since(startTime)
}

// humanSize formats a byte count into a human-readable string. Salvaged from
// ksdk stat/machine.go; the generated stat_metrics.go references it via the
// `humanSize($src)` string_format directives in stat_metrics.json. Kept here
// (rather than importing all of machine.go) since it is self-contained.
func humanSize(size uint64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	if size < KB {
		return fmt.Sprintf("%d B", size)
	} else if size < MB {
		return fmt.Sprintf("%.1f KB (%d B)", float64(size)/KB, size)
	} else if size < GB {
		return fmt.Sprintf("%.1f MB (%d B)", float64(size)/MB, size)
	}
	return fmt.Sprintf("%.1f GB (%d B)", float64(size)/GB, size)
}

// GetNetworkStat collects this host's per-interface network identity (names,
// MTUs, flags, MAC and IP addresses) into a NetworkSpecStat. Salvaged verbatim
// from ksdk stat/stat.go: the dataplane substrate reports it in every stat
// batch, and the control plane builds l4lb destination entries (MAC/IP/ServerID)
// from the popcache nodes' reported NetworkSpec (see stat/dest.go).
func GetNetworkStat() (*NetworkSpecStat, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("failed to read network interfaces: %w", err)
	}
	specs := make([]InterfaceSpecStat, 0, len(ifaces))
	for _, iface := range ifaces {
		spec := InterfaceSpecStat{
			Name:  iface.Name,
			MTU:   uint32(iface.MTU),
			Mac:   iface.HardwareAddr,
			Flags: iface.Flags,
		}
		ipAddrs, err := iface.Addrs()
		if err != nil {
			specs = append(specs, spec)
			continue
		}
		for _, addr := range ipAddrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			prefix, ok := netip.AddrFromSlice(ipNet.IP)
			if !ok {
				continue
			}
			prefix = prefix.Unmap() // convert IPv4-mapped IPv6 to IPv4
			ones, _ := ipNet.Mask.Size()
			spec.IPs = append(spec.IPs, netip.PrefixFrom(prefix, ones))
		}
		specs = append(specs, spec)
	}
	return &NetworkSpecStat{Interfaces: specs}, nil
}
