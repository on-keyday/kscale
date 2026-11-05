package l4lbdrv

import (
	"encoding/binary"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"syscall"
	"testing"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

// TestPortRouteValueLayout: the hand-marshaled value matches the packed C struct
// (destination_entry, then the action).
func TestPortRouteValueLayout(t *testing.T) {
	v := portRouteValue{
		dest:   DestinationEntry{IPAddr: netip.MustParseAddr("198.51.100.9"), HardwareAddr: net.HardwareAddr{1, 2, 3, 4, 5, 6}},
		action: PORT_ROUTE_ACTION_FORWARD,
	}
	b, err := v.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != PORT_ROUTE_VALUE_SIZE {
		t.Fatalf("len = %d, want %d", len(b), PORT_ROUTE_VALUE_SIZE)
	}
	if got := netip.AddrFrom4([4]byte(b[0:4])); got != v.dest.IPAddr {
		t.Fatalf("ip = %v", got)
	}
	if got := net.HardwareAddr(b[4:10]); got.String() != "01:02:03:04:05:06" {
		t.Fatalf("mac = %v", got)
	}
	if b[14] != PORT_ROUTE_ACTION_FORWARD || b[15]|b[16]|b[17] != 0 { // little-endian host
		t.Fatalf("action bytes = %v", b[14:])
	}
}

func TestPortRouteKey(t *testing.T) {
	k := portRouteKey(netip.MustParseAddr("192.0.2.10"), syscall.IPPROTO_TCP, 8443)
	want := netip.MustParseAddr("::ffff:192.0.2.10").As16()
	if k.Vip != want {
		t.Fatalf("vip = %v, want v4-mapped", k.Vip)
	}
	// network byte order in memory: 8443 = 0x20fb
	b := []byte{byte(k.Port), byte(k.Port >> 8)} // little-endian host view of the field
	if b[0] != 0x20 || b[1] != 0xfb || k.Proto != syscall.IPPROTO_TCP || k.Pad != 0 {
		t.Fatalf("key = %+v", k)
	}
}

func TestValidatePortRoutes(t *testing.T) {
	mac := net.HardwareAddr{1, 2, 3, 4, 5, 6}
	ok := PortRoute{Proto: syscall.IPPROTO_TCP, Port: 8443, Dest: DestinationEntry{IPAddr: netip.MustParseAddr("10.0.0.9"), HardwareAddr: mac}}
	if err := ValidatePortRoutes([]PortRoute{ok}); err != nil {
		t.Fatal(err)
	}
	bad := []PortRoute{
		{Proto: 1, Port: 1, Dest: ok.Dest},
		{Proto: syscall.IPPROTO_TCP, Port: 0, Dest: ok.Dest},
		{Proto: syscall.IPPROTO_TCP, Port: 1, Dest: DestinationEntry{IPAddr: netip.MustParseAddr("::1"), HardwareAddr: mac}},
		{Proto: syscall.IPPROTO_TCP, Port: 1, Dest: DestinationEntry{IPAddr: ok.Dest.IPAddr}},
	}
	for _, r := range bad {
		if err := ValidatePortRoutes([]PortRoute{r}); err == nil {
			t.Errorf("%+v accepted", r)
		}
	}
	if err := ValidatePortRoutes([]PortRoute{ok, ok}); err == nil {
		t.Error("duplicate accepted")
	}
}

// TestL4LBPortRoute runs the real XDP program (BPF_PROG_RUN): a routed
// (VIP, proto, port) is IPIP-encapsulated to the route's node, everything else
// keeps the consistent hash / QUIC-LB path. Gated like TestL4LB.
func TestL4LBPortRoute(t *testing.T) {
	if os.Getenv("KSCALE_BPF_TEST") != "1" {
		t.Skip("set KSCALE_BPF_TEST=1 on a BPF/XDP-capable host (needs CAP_BPF/CAP_SYS_ADMIN + `make -C l4lb/c`)")
	}
	vip4 := netip.MustParseAddr("192.0.2.10")
	vip6 := netip.MustParseAddr("2001:db8::10")
	lbMAC := net.HardwareAddr{0x00, 0x00, 0x5e, 0x00, 0x53, 0xfe}
	backend := DestinationEntry{IPAddr: netip.MustParseAddr("192.168.0.10"), HardwareAddr: net.HardwareAddr{0x00, 0x00, 0x5e, 0x00, 0x53, 0x10}}
	gpu := DestinationEntry{IPAddr: netip.MustParseAddr("192.168.0.20"), HardwareAddr: net.HardwareAddr{0x00, 0x00, 0x5e, 0x00, 0x53, 0x20}}

	cfg := &DynamicConfig{
		VIP:       vip4,
		VIPv6:     vip6,
		MTU:       1500,
		SharedKey: []byte("0123456789abcdef"),
		Dests: []DestinationEntry{
			{IPAddr: netip.MustParseAddr("192.168.0.254"), HardwareAddr: lbMAC},
			backend,
		},
	}
	lb, err := New(slog.Default(), &FixedConfig{BinPath: "../c/lb.o", CryptoBin: "../c/init_crypto.o", EBPFPinDir: "/sys/fs/bpf/"}, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer lb.Close()
	if err := lb.UpdatePortRoutes([]PortRoute{
		{Proto: syscall.IPPROTO_TCP, Port: 8443, Dest: gpu},
		{Proto: syscall.IPPROTO_UDP, Port: 9443, Dest: gpu},
	}); err != nil {
		t.Fatalf("UpdatePortRoutes: %v", err)
	}

	eth := func(typ layers.EthernetType) *layers.Ethernet {
		return &layers.Ethernet{SrcMAC: net.HardwareAddr{0x00, 0x00, 0x5e, 0x00, 0x53, 0xff}, DstMAC: lbMAC, EthernetType: typ}
	}
	v4 := func(dst netip.Addr, proto layers.IPProtocol) *layers.IPv4 {
		return &layers.IPv4{SrcIP: net.IP{10, 0, 0, 123}, DstIP: dst.AsSlice(), Version: 4, TTL: 64, Protocol: proto}
	}
	ser := func(t *testing.T, ls ...gopacket.SerializableLayer) []byte {
		t.Helper()
		buf := gopacket.NewSerializeBuffer()
		if err := gopacket.SerializeLayers(buf, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}, ls...); err != nil {
			t.Fatalf("serialize: %v", err)
		}
		return buf.Bytes()
	}
	tcp4 := func(t *testing.T, dst netip.Addr, port uint16) []byte {
		ip := v4(dst, layers.IPProtocolTCP)
		tcp := &layers.TCP{SrcPort: 40000, DstPort: layers.TCPPort(port), SYN: true, Seq: 1}
		tcp.SetNetworkLayerForChecksum(ip)
		return ser(t, eth(layers.EthernetTypeIPv4), ip, tcp)
	}
	udp4 := func(t *testing.T, dst netip.Addr, port uint16, payload []byte) []byte {
		ip := v4(dst, layers.IPProtocolUDP)
		udp := &layers.UDP{SrcPort: 40000, DstPort: layers.UDPPort(port)}
		udp.SetNetworkLayerForChecksum(ip)
		return ser(t, eth(layers.EthernetTypeIPv4), ip, udp, gopacket.Payload(payload))
	}
	tcp6 := func(t *testing.T, dst netip.Addr, port uint16) []byte {
		ip := &layers.IPv6{SrcIP: netip.MustParseAddr("2001:db8::123").AsSlice(), DstIP: dst.AsSlice(), Version: 6, HopLimit: 64, NextHeader: layers.IPProtocolTCP}
		tcp := &layers.TCP{SrcPort: 40000, DstPort: layers.TCPPort(port), SYN: true, Seq: 1}
		tcp.SetNetworkLayerForChecksum(ip)
		return ser(t, eth(layers.EthernetTypeIPv6), ip, tcp)
	}
	// run returns the verdict and, for XDP_TX, the outer IPIP destination.
	run := func(t *testing.T, pkt []byte) (uint32, netip.Addr, net.HardwareAddr) {
		t.Helper()
		ret, out, err := lb.bindings.LBMain.Test(pkt)
		if err != nil {
			t.Fatalf("Test: %v", err)
		}
		if ret != XDP_TX {
			return ret, netip.Addr{}, nil
		}
		p := gopacket.NewPacket(out, layers.LayerTypeEthernet, gopacket.Default)
		e, _ := p.Layer(layers.LayerTypeEthernet).(*layers.Ethernet)
		outer, _ := p.Layer(layers.LayerTypeIPv4).(*layers.IPv4)
		if e == nil || outer == nil || outer.Protocol != layers.IPProtocolIPv4 {
			t.Fatalf("XDP_TX output is not IPIP: %v", p)
		}
		// The encapsulated frame must hold exactly the outer IP packet: a frame
		// trimmed short (or left padded) is dropped by the receiver as invalid.
		if got, want := len(out), 14+int(outer.Length); got != want {
			t.Fatalf("encapsulated frame is %d bytes, outer IP total length says %d", got, want)
		}
		dst, _ := netip.AddrFromSlice(outer.DstIP)
		return ret, dst, e.DstMAC
	}
	expect := func(t *testing.T, name string, pkt []byte, want DestinationEntry) {
		t.Helper()
		ret, dst, mac := run(t, pkt)
		if ret != XDP_TX {
			t.Fatalf("%s: verdict %s, want XDP_TX", name, XdpRetValToString(ret))
		}
		if dst != want.IPAddr || mac.String() != want.HardwareAddr.String() {
			t.Fatalf("%s: encapsulated to %v / %v, want %v / %v", name, dst, mac, want.IPAddr, want.HardwareAddr)
		}
	}
	counters := func(t *testing.T) *StatCounters {
		t.Helper()
		c, err := lb.bindings.ReadStatCountersAggregate()
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	if err := lb.bindings.ResetStatCounters(); err != nil {
		t.Fatal(err)
	}
	expect(t, "tcp routed port", tcp4(t, vip4, 8443), gpu)
	expect(t, "tcp other port", tcp4(t, vip4, 443), backend)
	expect(t, "udp routed port, non-QUIC payload", udp4(t, vip4, 9443, []byte("objproto-hello")), gpu)
	expect(t, "udp routed port, empty payload", udp4(t, vip4, 9443, nil), gpu)
	// gopacket pads every frame to the 60-byte Ethernet minimum, as a NIC does on
	// the wire; a veth does not. Feed the unpadded form too: encapsulation must
	// keep exactly the IP packet, whether or not padding came with it.
	padded := udp4(t, vip4, 9443, []byte("hi"))
	unpadded := padded[:14+int(binary.BigEndian.Uint16(padded[16:18]))]
	if len(padded) != 60 || len(unpadded) >= 60 {
		t.Fatalf("fixture: padded %d bytes, unpadded %d", len(padded), len(unpadded))
	}
	expect(t, "udp routed port, padded short frame", padded, gpu)
	expect(t, "udp routed port, unpadded short frame (veth)", unpadded, gpu)
	expect(t, "tcp over the IPv6 VIP", tcp6(t, vip6, 8443), gpu)
	// tcp:8443 is routed, udp:8443 is not: it stays on the QUIC-LB path (a 1-byte
	// payload is too short for a QUIC header, so that path passes it up).
	if ret, dst, _ := run(t, udp4(t, vip4, 8443, []byte("x"))); ret == XDP_TX && dst == gpu.IPAddr {
		t.Fatal("udp:8443 followed the tcp:8443 route")
	}
	if got := counters(t).PortRouteForwardTotal; got != 6 {
		t.Fatalf("PortRouteForwardTotal = %d, want 6", got)
	}

	// The VIP moves: routes are re-keyed to the new VIP and the old key is gone.
	vip4b := netip.MustParseAddr("192.0.2.20")
	if err := lb.UpdateVIP(vip4b, false); err != nil {
		t.Fatal(err)
	}
	expect(t, "tcp routed port on the new VIP", tcp4(t, vip4b, 8443), gpu)
	if n := mapLen(t, lb); n != 4 { // 2 routes x (new v4 VIP + v6 VIP)
		t.Fatalf("port_route_map has %d entries after the VIP change, want 4", n)
	}

	// Removing the routes returns the port to the consistent hash.
	if err := lb.UpdatePortRoutes(nil); err != nil {
		t.Fatal(err)
	}
	expect(t, "tcp after the route is removed", tcp4(t, vip4b, 8443), backend)
	if n := mapLen(t, lb); n != 0 {
		t.Fatalf("port_route_map has %d entries after removing every route", n)
	}
}

func mapLen(t *testing.T, lb *L4LB) int {
	t.Helper()
	var k PortRouteKey
	var v []byte
	n := 0
	it := lb.bindings.PortRouteMap.Iterate()
	for it.Next(&k, &v) {
		n++
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	return n
}
