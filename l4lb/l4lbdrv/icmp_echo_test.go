package l4lbdrv

import (
	"log/slog"
	"net/netip"
	"os"
	"testing"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

// TestL4LBICMPEcho exercises the VIP-destined ICMPv4 echo path through the real
// XDP program via BPF_PROG_RUN: with IcmpEcho on, an echo request to the VIP is
// answered (XDP_TX, type flipped to reply, icmp_echo_reply_total++); with it
// off, it is dropped (XDP_DROP, icmp_dropped_total++). Gated like TestL4LB —
// needs a BPF/XDP host with `make -C l4lb/c`.
func TestL4LBICMPEcho(t *testing.T) {
	if os.Getenv("KSCALE_BPF_TEST") != "1" {
		t.Skip("set KSCALE_BPF_TEST=1 on a BPF/XDP-capable host (needs CAP_BPF/CAP_SYS_ADMIN + `make -C l4lb/c`)")
	}
	vip4 := netip.MustParseAddr("192.0.2.10")
	lbMAC := []byte{0x00, 0x00, 0x5e, 0x00, 0x53, 0xfe}

	build := func(t *testing.T, icmpEcho bool) *L4LB {
		cfg := &DynamicConfig{
			VIP:       vip4,
			IcmpEcho:  icmpEcho,
			SharedKey: []byte("0123456789abcdef"),
			Dests: []DestinationEntry{
				{IPAddr: netip.MustParseAddr("192.168.0.254"), HardwareAddr: lbMAC},
				{IPAddr: netip.MustParseAddr("192.168.0.10"), HardwareAddr: []byte{0x00, 0x00, 0x5e, 0x00, 0x53, 0x10}},
			},
		}
		lb, err := New(slog.Default(), &FixedConfig{BinPath: "../c/lb.o", CryptoBin: "../c/init_crypto.o", EBPFPinDir: "/sys/fs/bpf/"}, cfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return lb
	}

	// echo request to the VIP.
	eth := &layers.Ethernet{SrcMAC: []byte{0x00, 0x00, 0x5e, 0x00, 0x53, 0xff}, DstMAC: lbMAC, EthernetType: layers.EthernetTypeIPv4}
	ip4 := &layers.IPv4{SrcIP: netip.MustParseAddr("10.0.0.123").AsSlice(), DstIP: vip4.AsSlice(), Version: 4, TTL: 64, Protocol: layers.IPProtocolICMPv4}
	icmp := &layers.ICMPv4{TypeCode: layers.CreateICMPv4TypeCode(layers.ICMPv4TypeEchoRequest, 0), Id: 0x1234, Seq: 1}
	buf := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buf, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}, eth, ip4, icmp, gopacket.Payload([]byte("ping-payload"))); err != nil {
		t.Fatalf("serialize: %v", err)
	}

	t.Run("on: reply", func(t *testing.T) {
		lb := build(t, true)
		defer lb.Close()
		if err := lb.bindings.ResetStatCounters(); err != nil {
			t.Fatalf("ResetStatCounters: %v", err)
		}
		retval, out, err := lb.bindings.LBMain.Test(buf.Bytes())
		if err != nil {
			t.Fatalf("Test: %v", err)
		}
		if retval != XDP_TX {
			t.Fatalf("retval = %s, want XDP_TX", XdpRetValToString(retval))
		}
		pkt := gopacket.NewPacket(out, layers.LayerTypeEthernet, gopacket.Default)
		il := pkt.Layer(layers.LayerTypeICMPv4)
		if il == nil {
			t.Fatalf("no ICMP layer in reply")
		}
		if got := il.(*layers.ICMPv4).TypeCode.Type(); got != layers.ICMPv4TypeEchoReply {
			t.Fatalf("reply ICMP type = %d, want echo reply (%d)", got, layers.ICMPv4TypeEchoReply)
		}
		cnt, _ := lb.bindings.ReadStatCountersAggregate()
		if cnt.IcmpEchoReplyTotal != 1 {
			t.Fatalf("IcmpEchoReplyTotal = %d, want 1", cnt.IcmpEchoReplyTotal)
		}
	})

	t.Run("off: drop", func(t *testing.T) {
		lb := build(t, false)
		defer lb.Close()
		if err := lb.bindings.ResetStatCounters(); err != nil {
			t.Fatalf("ResetStatCounters: %v", err)
		}
		retval, _, err := lb.bindings.LBMain.Test(buf.Bytes())
		if err != nil {
			t.Fatalf("Test: %v", err)
		}
		if retval != XDP_DROP {
			t.Fatalf("retval = %s, want XDP_DROP", XdpRetValToString(retval))
		}
		cnt, _ := lb.bindings.ReadStatCountersAggregate()
		if cnt.IcmpDroppedTotal != 1 {
			t.Fatalf("IcmpDroppedTotal = %d, want 1", cnt.IcmpDroppedTotal)
		}
	})
}
