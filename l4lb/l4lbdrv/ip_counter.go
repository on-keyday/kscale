package l4lbdrv

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/on-keyday/kscale/l4lb/l4lbdrv/wire"
)

type SrcIPCounts struct {
	Map map[netip.Addr]map[ProtocolNumber]map[uint16]uint64
	Sum uint64
}

func (s *SrcIPCounts) String() string {
	var sb strings.Builder
	for ip, protoMap := range s.Map {
		for proto, ports := range protoMap {
			for port, count := range ports {
				sb.WriteString(fmt.Sprintf("%s/%s:%d: %d ", ip.String(), proto, port, count))
			}
		}
	}
	return sb.String()
}

type PacketSizeDist struct {
	Map       map[uint32]uint64
	Count     uint64             // count
	Histogram map[float64]uint64 // for prometheus histogram
	Sum       uint64             // bytes
}

func (p *PacketSizeDist) String() string {
	var sb strings.Builder
	for size, count := range p.Map {
		sb.WriteString(fmt.Sprintf("%d: %d ", size, count))
	}
	return sb.String()
}

// SrcIPCountToBytes / SrcIPCountFromBytes carry source-ip × protocol ×
// dest-port × count entries inside pbstat.L4lbStat.src_ips (which is a
// `bytes` field). Wire schema lives in l4lb/l4lbdrv/wire/counter.bgn
// (format SrcIPCount); entries are sorted by family → prefix → protocol
// → dest_port so the resulting bytes are deterministic for diffing /
// equality. ipv6 keeps only the first 8 bytes (= /64 prefix), matching
// the pre-A4-phase-E hand-written encoder this replaced.
func SrcIPCountToBytes(ips map[netip.Addr]map[ProtocolNumber]map[uint16]uint64) []byte {
	entries := make([]wire.SrcIPCountEntry, 0)
	for ip, protoMap := range ips {
		for proto, ports := range protoMap {
			for port, count := range ports {
				entry := wire.SrcIPCountEntry{
					Protocol: uint8(proto),
					DestPort: port,
				}
				if ip.Is4() {
					entry.Family = wire.Family_Ipv4
					entry.SetPrefix4(ip.As4())
				} else {
					entry.Family = wire.Family_Ipv6
					ip6 := ip.As16()
					var p6 [8]uint8
					copy(p6[:], ip6[:8])
					entry.SetPrefix6(p6)
				}
				entry.Count.Value = count
				entries = append(entries, entry)
			}
		}
	}
	slices.SortFunc(entries, func(a, b wire.SrcIPCountEntry) int {
		if a.Family != b.Family {
			if a.Family < b.Family {
				return -1
			}
			return 1
		}
		switch a.Family {
		case wire.Family_Ipv4:
			if c := compareBytes(a.Prefix4()[:], b.Prefix4()[:]); c != 0 {
				return c
			}
		case wire.Family_Ipv6:
			if c := compareBytes(a.Prefix6()[:], b.Prefix6()[:]); c != 0 {
				return c
			}
		}
		if a.Protocol != b.Protocol {
			if a.Protocol < b.Protocol {
				return -1
			}
			return 1
		}
		if a.DestPort != b.DestPort {
			if a.DestPort < b.DestPort {
				return -1
			}
			return 1
		}
		return 0
	})
	out, err := (&wire.SrcIPCount{Entries: entries}).Append(nil)
	if err != nil {
		// Append only fails on encode-side validation; for SrcIPCount it
		// has no length-bounded fields, so this branch is unreachable.
		// Returning nil keeps the legacy "best effort" semantics of the
		// hand-written predecessor.
		return nil
	}
	return out
}

func compareBytes(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

func SrcIPCountFromBytes(data []byte) (*SrcIPCounts, error) {
	var pkt wire.SrcIPCount
	if err := pkt.DecodeExact(data); err != nil {
		return nil, fmt.Errorf("decode src ip count: %w", err)
	}
	result := make(map[netip.Addr]map[ProtocolNumber]map[uint16]uint64)
	sum := uint64(0)
	for _, e := range pkt.Entries {
		var ip netip.Addr
		switch e.Family {
		case wire.Family_Ipv4:
			p4 := e.Prefix4()
			if p4 == nil {
				return nil, fmt.Errorf("ipv4 entry missing prefix4")
			}
			ip = netip.AddrFrom4(*p4)
		case wire.Family_Ipv6:
			p6 := e.Prefix6()
			if p6 == nil {
				return nil, fmt.Errorf("ipv6 entry missing prefix6")
			}
			var b16 [16]byte
			copy(b16[:], p6[:])
			ip = netip.AddrFrom16(b16)
		default:
			return nil, fmt.Errorf("unknown family: %d", e.Family)
		}
		protocol := ProtocolNumber(e.Protocol)
		if _, ok := result[ip]; !ok {
			result[ip] = make(map[ProtocolNumber]map[uint16]uint64)
		}
		if _, ok := result[ip][protocol]; !ok {
			result[ip][protocol] = make(map[uint16]uint64)
		}
		result[ip][protocol][e.DestPort] += e.Count.Value
		sum += e.Count.Value
	}
	return &SrcIPCounts{Map: result, Sum: sum}, nil
}

// PacketSizeDistToBytes / PacketSizeDistFromBytes serialize a packet-
// size histogram carried inside pbstat.L4lbStat.packet_sizes. Wire
// schema: l4lb/l4lbdrv/wire/counter.bgn (format PacketSizeDist).
// Entries are sorted by size for deterministic output.
func PacketSizeDistToBytes(sizes map[uint32]uint64) []byte {
	entries := make([]wire.PacketSizeEntry, 0, len(sizes))
	for size, count := range sizes {
		var e wire.PacketSizeEntry
		e.Size = size
		e.Count.Value = count
		entries = append(entries, e)
	}
	slices.SortFunc(entries, func(a, b wire.PacketSizeEntry) int {
		if a.Size < b.Size {
			return -1
		}
		if a.Size > b.Size {
			return 1
		}
		return 0
	})
	out, err := (&wire.PacketSizeDist{Entries: entries}).Append(nil)
	if err != nil {
		return nil
	}
	return out
}

func PacketSizeDistFromBytes(data []byte) (*PacketSizeDist, error) {
	var pkt wire.PacketSizeDist
	if err := pkt.DecodeExact(data); err != nil {
		return nil, fmt.Errorf("decode packet size dist: %w", err)
	}
	result := make(map[uint32]uint64)
	count := uint64(0)
	sum := uint64(0)
	for _, e := range pkt.Entries {
		result[e.Size] += e.Count.Value
		count += e.Count.Value
		sum += uint64(e.Size) * e.Count.Value
	}
	return &PacketSizeDist{Map: result, Sum: sum, Count: count}, nil
}

type ISNLeastSignificantByteMap struct {
	Map       map[uint8]uint64
	Count     uint64             // count
	Histogram map[float64]uint64 // for prometheus histogram
	Sum       uint64             // bytes
}

func (i *ISNLeastSignificantByteMap) String() string {
	var sb strings.Builder
	for b, count := range i.Map {
		sb.WriteString(fmt.Sprintf("%d: %d ", b, count))
	}
	return sb.String()
}

// ISNLeastSignificantByteMapToBytes / FromBytes serialize an ISN-LSB
// frequency map carried inside pbstat.L4lbStat.isn_least_significant_
// bytes. Wire schema: l4lb/l4lbdrv/wire/counter.bgn (format
// ISNLeastSignificantByteMap). Entries are sorted by byte for
// deterministic output.
func ISNLeastSignificantByteMapToBytes(isnMap map[uint8]uint64) []byte {
	entries := make([]wire.ISNEntry, 0, len(isnMap))
	for b, count := range isnMap {
		var e wire.ISNEntry
		e.Byte = b
		e.Count.Value = count
		entries = append(entries, e)
	}
	slices.SortFunc(entries, func(a, b wire.ISNEntry) int {
		if a.Byte < b.Byte {
			return -1
		}
		if a.Byte > b.Byte {
			return 1
		}
		return 0
	})
	out, err := (&wire.ISNLeastSignificantByteMap{Entries: entries}).Append(nil)
	if err != nil {
		return nil
	}
	return out
}

func ISNLeastSignificantByteMapFromBytes(data []byte) (*ISNLeastSignificantByteMap, error) {
	var pkt wire.ISNLeastSignificantByteMap
	if err := pkt.DecodeExact(data); err != nil {
		return nil, fmt.Errorf("decode ISN LSB map: %w", err)
	}
	result := &ISNLeastSignificantByteMap{
		Map: make(map[uint8]uint64),
	}
	for _, e := range pkt.Entries {
		result.Map[e.Byte] += e.Count.Value
		result.Count += e.Count.Value
		result.Sum += uint64(e.Byte) * e.Count.Value
	}
	return result, nil
}
