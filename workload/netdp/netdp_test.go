package netdp

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"testing"

	"github.com/cilium/ebpf"
)

// These tests drive the real programs through BPF_PROG_TEST_RUN: craft a frame,
// run it, check the verdict and every rewritten byte + checksum. Loading needs
// CAP_BPF/CAP_NET_ADMIN, so they skip when unprivileged (run the compiled test
// binary in a privileged container: see e2e/compose/phase6_workload_pod.sh).

const objPath = "c/netdp.o"

const (
	tcActOK       = 0
	tcActRedirect = 7
)

var (
	lbIP     = netip.MustParseAddr("10.5.0.3")
	nodeIP   = netip.MustParseAddr("10.5.0.4")
	clientIP = netip.MustParseAddr("10.5.0.5")
	vip      = netip.MustParseAddr("192.0.2.10")
	podIP    = netip.MustParseAddr("10.200.0.2")
	podMAC   = net.HardwareAddr{0x02, 0x01, 0x23, 0x45, 0x67, 0x89}
	hostMAC  = net.HardwareAddr{0x0a, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
	nicMAC   = net.HardwareAddr{0x0a, 0x11, 0x22, 0x33, 0x44, 0x55}
)

func loadForTest(t *testing.T) *Datapath {
	t.Helper()
	if _, err := os.Stat(objPath); err != nil {
		t.Skipf("%s not built (make -C workload/netdp/c)", objPath)
	}
	// State first (as the agent may learn it before any object arrives), then load:
	// LoadObject must fill the maps from it.
	d := New(nil)
	d.cfg = config{VIP: vip.As4(), NICIfindex: 42}
	d.lbSrcs = map[[4]byte]bool{lbIP.As4(): true}
	dest := podDest{Ifindex: 7, PodIP: podIP.As4()}
	copy(dest.PodMAC[:], podMAC)
	copy(dest.HostMAC[:], hostMAC)
	d.in = map[portKey]podDest{{Proto: ipprotoTCP, Port: be16(8080)}: dest}
	d.out = map[outKey]bool{{PodIP: podIP.As4(), Proto: ipprotoTCP, Port: be16(8080)}: true}
	if err := d.LoadObject(objPath); err != nil {
		if errors.Is(err, os.ErrPermission) {
			t.Skipf("needs CAP_BPF: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func csum(b []byte, initial uint32) uint16 {
	sum := initial
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

func ipHeader(src, dst netip.Addr, proto byte, payloadLen int) []byte {
	h := make([]byte, 20)
	h[0] = 0x45
	binary.BigEndian.PutUint16(h[2:], uint16(20+payloadLen))
	h[8] = 64
	h[9] = proto
	s, d := src.As4(), dst.As4()
	copy(h[12:], s[:])
	copy(h[16:], d[:])
	binary.BigEndian.PutUint16(h[10:], csum(h, 0))
	return h
}

func pseudoSum(src, dst netip.Addr, l4len int) uint32 {
	s, d := src.As4(), dst.As4()
	var sum uint32
	for _, b := range [][]byte{s[:], d[:]} {
		sum += uint32(binary.BigEndian.Uint16(b[0:])) + uint32(binary.BigEndian.Uint16(b[2:]))
	}
	return sum + 6 + uint32(l4len)
}

func tcpSegment(src, dst netip.Addr, sport, dport uint16, payload []byte) []byte {
	t := make([]byte, 20+len(payload))
	binary.BigEndian.PutUint16(t[0:], sport)
	binary.BigEndian.PutUint16(t[2:], dport)
	t[12] = 5 << 4
	t[13] = 0x02 // SYN
	binary.BigEndian.PutUint16(t[14:], 65535)
	copy(t[20:], payload)
	binary.BigEndian.PutUint16(t[16:], csum(t, pseudoSum(src, dst, len(t))))
	return t
}

func eth(dst, src net.HardwareAddr) []byte {
	e := make([]byte, 14)
	copy(e[0:], dst)
	copy(e[6:], src)
	binary.BigEndian.PutUint16(e[12:], 0x0800)
	return e
}

// ipipFrame is what l4lb puts on the wire: outer IPv4 (lb -> node, proto 4)
// around the client's TCP segment to VIP:dport.
func ipipFrame(outerSrc netip.Addr, dport uint16) []byte {
	seg := tcpSegment(clientIP, vip, 40000, dport, []byte("hello"))
	inner := append(ipHeader(clientIP, vip, 6, len(seg)), seg...)
	outer := append(ipHeader(outerSrc, nodeIP, 4, len(inner)), inner...)
	// padded past the ETH_ZLEN minimum a real NIC would deliver
	return append(eth(nicMAC, hostMAC), outer...)
}

func run(t *testing.T, prog *ebpf.Program, in []byte) (uint32, []byte) {
	t.Helper()
	out := make([]byte, len(in)+64)
	ret, err := prog.Run(&ebpf.RunOptions{Data: in, DataOut: out})
	if err != nil {
		t.Fatalf("test run: %v", err)
	}
	return ret, out
}

func verifyIPv4AndTCP(t *testing.T, pkt []byte, wantSrc, wantDst netip.Addr) {
	t.Helper()
	ip := pkt[14:34]
	if got := netip.AddrFrom4([4]byte(ip[12:16])); got != wantSrc {
		t.Errorf("ip src = %v, want %v", got, wantSrc)
	}
	if got := netip.AddrFrom4([4]byte(ip[16:20])); got != wantDst {
		t.Errorf("ip dst = %v, want %v", got, wantDst)
	}
	if c := csum(ip, 0); c != 0 {
		t.Errorf("ipv4 checksum invalid (residue %#x)", c)
	}
	totalLen := int(binary.BigEndian.Uint16(ip[2:]))
	seg := pkt[34 : 14+totalLen]
	if c := csum(seg, pseudoSum(wantSrc, wantDst, len(seg))); c != 0 {
		t.Errorf("tcp checksum invalid (residue %#x)", c)
	}
}

func counter(t *testing.T, d *Datapath, name string) uint64 {
	t.Helper()
	c, err := d.Counters()
	if err != nil {
		t.Fatal(err)
	}
	return c[name]
}

func TestIngressDecapsAndSteers(t *testing.T) {
	d := loadForTest(t)
	in := ipipFrame(lbIP, 8080)
	ret, out := run(t, d.coll.Programs["netdp_ingress"], in)
	if ret != tcActRedirect {
		t.Fatalf("verdict %d, want TC_ACT_REDIRECT", ret)
	}
	pkt := out[:len(in)-20]
	if got := net.HardwareAddr(pkt[0:6]); got.String() != podMAC.String() {
		t.Errorf("eth dst = %v, want pod MAC %v", got, podMAC)
	}
	if got := net.HardwareAddr(pkt[6:12]); got.String() != hostMAC.String() {
		t.Errorf("eth src = %v, want host veth MAC %v", got, hostMAC)
	}
	if pkt[14+9] != 6 {
		t.Fatalf("outer header not stripped: protocol %d at the IP slot", pkt[14+9])
	}
	verifyIPv4AndTCP(t, pkt, clientIP, podIP)
	if string(pkt[len(pkt)-5:]) != "hello" {
		t.Errorf("payload corrupted: %q", pkt[len(pkt)-5:])
	}
	if n := counter(t, d, "in_steered"); n != 1 {
		t.Errorf("in_steered = %d", n)
	}
}

func TestIngressLeavesForeignIPIPAlone(t *testing.T) {
	d := loadForTest(t)
	in := ipipFrame(netip.MustParseAddr("10.5.0.99"), 8080) // not an l4lb front
	ret, out := run(t, d.coll.Programs["netdp_ingress"], in)
	if ret != tcActOK {
		t.Fatalf("verdict %d, want TC_ACT_OK", ret)
	}
	if string(out[:len(in)]) != string(in) {
		t.Error("packet modified although it was passed through")
	}
	if n := counter(t, d, "in_not_lb_src"); n != 1 {
		t.Errorf("in_not_lb_src = %d", n)
	}
}

func TestIngressPassesUnownedPort(t *testing.T) {
	d := loadForTest(t)
	ret, _ := run(t, d.coll.Programs["netdp_ingress"], ipipFrame(lbIP, 80)) // popcache's port
	if ret != tcActOK {
		t.Fatalf("verdict %d, want TC_ACT_OK (left to popcache's kernel decap)", ret)
	}
	if n := counter(t, d, "in_no_port"); n != 1 {
		t.Errorf("in_no_port = %d", n)
	}
}

func TestEgressSNATsReply(t *testing.T) {
	d := loadForTest(t)
	seg := tcpSegment(podIP, clientIP, 8080, 40000, []byte("world"))
	in := append(eth(hostMAC, podMAC), append(ipHeader(podIP, clientIP, 6, len(seg)), seg...)...)
	ret, out := run(t, d.coll.Programs["netdp_egress"], in)
	if ret != tcActRedirect {
		t.Fatalf("verdict %d, want TC_ACT_REDIRECT", ret)
	}
	verifyIPv4AndTCP(t, out[:len(in)], vip, clientIP)
	if n := counter(t, d, "out_snat"); n != 1 {
		t.Errorf("out_snat = %d", n)
	}
}

func TestEgressPassesOtherSourcePorts(t *testing.T) {
	d := loadForTest(t)
	seg := tcpSegment(podIP, clientIP, 5555, 40000, nil) // not a declared port
	in := append(eth(hostMAC, podMAC), append(ipHeader(podIP, clientIP, 6, len(seg)), seg...)...)
	if ret, _ := run(t, d.coll.Programs["netdp_egress"], in); ret != tcActOK {
		t.Fatalf("verdict %d, want TC_ACT_OK (egress outside the declared ports is not ours)", ret)
	}
}

func TestReloadRefillsMapsAndResetsCounters(t *testing.T) {
	d := loadForTest(t)
	if ret, _ := run(t, d.coll.Programs["netdp_ingress"], ipipFrame(lbIP, 8080)); ret != tcActRedirect {
		t.Fatalf("before reload: verdict %d", ret)
	}
	first := d.coll
	if err := d.LoadObject(objPath); err != nil {
		t.Fatal(err)
	}
	if d.coll == first {
		t.Fatal("LoadObject did not replace the collection")
	}
	if ok, obj := d.Loaded(); !ok || obj != objPath {
		t.Fatalf("Loaded() = %v, %q", ok, obj)
	}
	// The new object's maps came from the kept state: steering still works.
	ret, out := run(t, d.coll.Programs["netdp_ingress"], ipipFrame(lbIP, 8080))
	if ret != tcActRedirect {
		t.Fatalf("after reload: verdict %d, want TC_ACT_REDIRECT", ret)
	}
	verifyIPv4AndTCP(t, out[:len(ipipFrame(lbIP, 8080))-20], clientIP, podIP)
	if n := counter(t, d, "in_steered"); n != 1 {
		t.Errorf("in_steered after reload = %d, want 1 (counters restart with the object)", n)
	}
}

func TestStateBeforeLoadIsKept(t *testing.T) {
	d := New(nil)
	if err := d.SetVIP(vip); err != nil {
		t.Fatal(err)
	}
	if err := d.SetLBSources([]netip.Addr{lbIP}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.Loaded(); ok {
		t.Fatal("nothing loaded yet")
	}
	c, err := d.Counters()
	if err != nil || c["in_steered"] != 0 {
		t.Fatalf("counters without an object = %v, %v", c, err)
	}
	if d.cfg.VIP != vip.As4() || !d.lbSrcs[lbIP.As4()] {
		t.Fatal("state set before load was not recorded")
	}
}
