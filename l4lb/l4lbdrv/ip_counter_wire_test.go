package l4lbdrv

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"slices"
	"testing"
)

// These tests pin the wire format of SrcIPCount / PacketSizeDist /
// ISNLeastSignificantByteMap to the bytes the pre-A4-phase-E hand-
// written encoders produced. Stat consumers (pbstat.L4lbStat carries
// these as opaque `bytes` fields) speak the historical format and any
// drift would break inter-version compat.
//
// The reference encoders below are verbatim copies of the deleted
// implementations from ip_counter.go pre-E. They live in test code so
// the runtime path can keep using the brgen-generated readers/writers
// while the assertion still has an independent ground truth — there is
// no risk of a bug in the new encoder silently being baked into the
// golden via re-recording.

func refSrcIPCountToBytes(ips map[netip.Addr]map[ProtocolNumber]map[uint16]uint64) []byte {
	var chunk [][]byte
	for ip, protoMap := range ips {
		for proto, ports := range protoMap {
			for port, count := range ports {
				buf := []byte{byte(proto)}
				if ip.Is4() {
					buf = append(buf, byte(1))
				} else {
					buf = append(buf, byte(2))
				}
				buf = binary.BigEndian.AppendUint16(buf, port)
				if ip.Is4() {
					ip4 := ip.As4()
					buf = append(buf, ip4[0], ip4[1], ip4[2], ip4[3])
				} else {
					ip6 := ip.As16()
					buf = append(buf, ip6[0], ip6[1], ip6[2], ip6[3], ip6[4], ip6[5], ip6[6], ip6[7])
				}
				buf = binary.AppendUvarint(buf, count)
				chunk = append(chunk, buf)
			}
		}
	}
	slices.SortFunc(chunk, func(a, b []byte) int {
		if a[1] < b[1] {
			return -1
		} else if a[1] > b[1] {
			return 1
		}
		const prefixOffset = 4
		switch a[1] {
		case 1:
			for i := 0; i < 4; i++ {
				if a[prefixOffset+i] < b[prefixOffset+i] {
					return -1
				} else if a[prefixOffset+i] > b[prefixOffset+i] {
					return 1
				}
			}
		case 2:
			for i := 0; i < 8; i++ {
				if a[prefixOffset+i] < b[prefixOffset+i] {
					return -1
				} else if a[prefixOffset+i] > b[prefixOffset+i] {
					return 1
				}
			}
		}
		if a[0] < b[0] {
			return -1
		} else if a[0] > b[0] {
			return 1
		}
		portA := binary.BigEndian.Uint16(a[2:4])
		portB := binary.BigEndian.Uint16(b[2:4])
		if portA < portB {
			return -1
		} else if portA > portB {
			return 1
		}
		return 0
	})
	return slices.Concat(chunk...)
}

func refPacketSizeDistToBytes(sizes map[uint32]uint64) []byte {
	var chunk [][]byte
	for size, count := range sizes {
		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, size)
		buf = binary.AppendUvarint(buf, count)
		chunk = append(chunk, buf)
	}
	slices.SortFunc(chunk, func(a, b []byte) int {
		sizeA := binary.BigEndian.Uint32(a[0:4])
		sizeB := binary.BigEndian.Uint32(b[0:4])
		if sizeA < sizeB {
			return -1
		} else if sizeA > sizeB {
			return 1
		}
		return 0
	})
	return slices.Concat(chunk...)
}

func refISNToBytes(isnMap map[uint8]uint64) []byte {
	var chunk [][]byte
	for b, count := range isnMap {
		buf := []byte{b}
		buf = binary.AppendUvarint(buf, count)
		chunk = append(chunk, buf)
	}
	slices.SortFunc(chunk, func(a, b []byte) int {
		if a[0] < b[0] {
			return -1
		} else if a[0] > b[0] {
			return 1
		}
		return 0
	})
	return slices.Concat(chunk...)
}

func TestSrcIPCountWireCompat(t *testing.T) {
	ip4a := netip.MustParseAddr("10.0.0.1")
	ip4b := netip.MustParseAddr("192.168.1.5")
	ip6 := netip.MustParseAddr("2001:db8::1")
	cases := []map[netip.Addr]map[ProtocolNumber]map[uint16]uint64{
		// empty
		{},
		// single ipv4 entry
		{ip4a: {6: {80: 1}}},
		// multiple entries triggering all sort axes
		{
			ip4a: {6: {80: 100}, 17: {53: 200}},
			ip4b: {6: {80: 50, 443: 75}},
			ip6:  {6: {443: 300}, 17: {123: 500}},
		},
		// large counts spanning multi-byte LEB128
		{ip4a: {6: {80: 1 << 35}}},
	}
	for i, ips := range cases {
		got := SrcIPCountToBytes(ips)
		want := refSrcIPCountToBytes(ips)
		if !bytes.Equal(got, want) {
			t.Errorf("case %d: SrcIPCountToBytes mismatch\n  got:  %x\n  want: %x", i, got, want)
			continue
		}
		// round-trip
		decoded, err := SrcIPCountFromBytes(got)
		if err != nil {
			t.Errorf("case %d: SrcIPCountFromBytes failed: %v", i, err)
			continue
		}
		re := SrcIPCountToBytes(decoded.Map)
		if !bytes.Equal(re, got) {
			t.Errorf("case %d: round-trip mismatch\n  first:  %x\n  second: %x", i, got, re)
		}
	}
}

func TestPacketSizeDistWireCompat(t *testing.T) {
	cases := []map[uint32]uint64{
		{},
		{1500: 42},
		{64: 10, 1500: 100, 9000: 1, 1 << 20: 1 << 40},
	}
	for i, sizes := range cases {
		got := PacketSizeDistToBytes(sizes)
		want := refPacketSizeDistToBytes(sizes)
		if !bytes.Equal(got, want) {
			t.Errorf("case %d: PacketSizeDistToBytes mismatch\n  got:  %x\n  want: %x", i, got, want)
			continue
		}
		decoded, err := PacketSizeDistFromBytes(got)
		if err != nil {
			t.Errorf("case %d: PacketSizeDistFromBytes failed: %v", i, err)
			continue
		}
		re := PacketSizeDistToBytes(decoded.Map)
		if !bytes.Equal(re, got) {
			t.Errorf("case %d: round-trip mismatch\n  first:  %x\n  second: %x", i, got, re)
		}
	}
}

func TestISNLeastSignificantByteWireCompat(t *testing.T) {
	cases := []map[uint8]uint64{
		{},
		{0: 1, 255: 999},
		{0: 1, 1: 2, 2: 4, 128: 8, 255: 16, 100: 1 << 40},
	}
	for i, m := range cases {
		got := ISNLeastSignificantByteMapToBytes(m)
		want := refISNToBytes(m)
		if !bytes.Equal(got, want) {
			t.Errorf("case %d: ISNToBytes mismatch\n  got:  %x\n  want: %x", i, got, want)
			continue
		}
		decoded, err := ISNLeastSignificantByteMapFromBytes(got)
		if err != nil {
			t.Errorf("case %d: ISNFromBytes failed: %v", i, err)
			continue
		}
		re := ISNLeastSignificantByteMapToBytes(decoded.Map)
		if !bytes.Equal(re, got) {
			t.Errorf("case %d: round-trip mismatch\n  first:  %x\n  second: %x", i, got, re)
		}
	}
}
