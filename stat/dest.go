package stat

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"

	"github.com/on-keyday/kscale/consts"
	pbstat "github.com/on-keyday/kscale/protobuf/proto/stat"
)

// DestEntry is one l4lb backend: a popcache node's serving interface MAC/IP plus
// its assigned ServerID (QUIC-LB routing + eBPF dest index). Salvaged from ksdk
// stat/dest.go; framework-free (depends only on the stat types). In kscale this
// is driven control-plane-side from the stat cache, where ksdk drove it on the
// l4lb agent from relayed peer metrics.
type DestEntry struct {
	ServerID     uint32
	HardwareAddr net.HardwareAddr
	IPAddr       netip.Addr
}

// GetDestEntry builds a DestEntry for the named interface out of a NetworkSpecStat
// (find the per-interface InterfaceSpecStat and read its MAC + first IP).
func GetDestEntry(serverID uint32, iface string, netStat *NetworkSpecStat) (DestEntry, error) {
	var dest DestEntry
	dest.ServerID = serverID
	index := slices.IndexFunc(netStat.Interfaces, func(i InterfaceSpecStat) bool { return i.Name == iface })
	if index == -1 {
		return dest, fmt.Errorf("unknown interface: %s", iface)
	}
	spec := netStat.Interfaces[index]
	if len(spec.IPs) == 0 {
		return dest, fmt.Errorf("no IP addresses for interface: %s", iface)
	}
	dest.HardwareAddr = spec.Mac
	dest.IPAddr = spec.IPs[0].Addr()
	return dest, nil
}

// ErrNoBoundInterface marks a node that is not (yet) a usable backend — no bound
// interface, or not running. Such nodes are skipped, not errors.
var ErrNoBoundInterface = errors.New("no bound interface")

// GetDestEntryFromStat extracts a backend DestEntry from one node's reported
// Stats: it must report a non-zero ServerID (LbId), a bound interface, and
// AppStatus=Running — the membership gate for becoming an l4lb destination.
func GetDestEntryFromStat(data *pbstat.Stats) (DestEntry, error) {
	var dest DestEntry
	if data == nil {
		return dest, fmt.Errorf("nil stats")
	}
	var netSpec NetworkSpecStat
	if data.NetworkSpec != nil {
		netSpec.FromProto(data.NetworkSpec)
	}
	var app CdnAppRealtimeStat
	if data.CdnAppRealtime != nil {
		app.FromProto(data.CdnAppRealtime)
	} else {
		return dest, fmt.Errorf("missing cdn app realtime")
	}
	// A uint32 scalar cannot encode "unset" in proto, so treat zero LbId as missing.
	if app.LoadBalancerID == 0 {
		return dest, fmt.Errorf("missing load balancer ID")
	}
	if len(app.BoundInterfaces) == 0 {
		return dest, ErrNoBoundInterface
	}
	if app.AppStatus != consts.AppStatusRunning {
		return dest, ErrNoBoundInterface
	}
	return GetDestEntry(app.LoadBalancerID, app.BoundInterfaces[0], &netSpec)
}

// ActiveDestEntriesFromStats builds the full destination set: self first, then
// every reported node that passes the membership gate (skipping not-yet-ready).
func ActiveDestEntriesFromStats(self DestEntry, dataList []*pbstat.Stats) ([]DestEntry, error) {
	dests := []DestEntry{self}
	for _, data := range dataList {
		entry, err := GetDestEntryFromStat(data)
		if err != nil {
			if err == ErrNoBoundInterface {
				continue
			}
			return nil, fmt.Errorf("failed to get dest entry from stat: %w", err)
		}
		dests = append(dests, entry)
	}
	return dests, nil
}

// DestManager holds the last-pushed destination set and only fires onUpdate when
// it changes (diff-based idempotent reconcile).
type DestManager struct {
	CurrentInterface string
	PrevInterface    string
	DestList         []DestEntry
}

func (m *DestManager) UpdateFromStats(self DestEntry, metrics []*pbstat.Stats, onUpdate func(newEntries []DestEntry) error) error {
	entries, err := ActiveDestEntriesFromStats(self, metrics)
	if err != nil {
		return fmt.Errorf("failed to get dest entries from stats: %w", err)
	}
	return m.UpdateIfDiffer(entries, onUpdate)
}

// UpdateIfDiffer fires onUpdate (and records the new set) only if dests differs
// from the last pushed set on MAC, IP, or ServerID.
func (m *DestManager) UpdateIfDiffer(dests []DestEntry, onUpdate func(newEntries []DestEntry) error) error {
	differ := len(dests) != len(m.DestList)
	if !differ {
		for i, entry := range dests {
			if entry.HardwareAddr.String() != m.DestList[i].HardwareAddr.String() ||
				entry.IPAddr != m.DestList[i].IPAddr ||
				entry.ServerID != m.DestList[i].ServerID {
				differ = true
				break
			}
		}
	}
	if !differ {
		return nil
	}
	if err := onUpdate(dests); err != nil {
		return err
	}
	m.DestList = dests
	return nil
}
