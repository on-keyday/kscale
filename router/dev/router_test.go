package dev

import (
	"log/slog"
	"net/netip"
	"testing"

	pb "github.com/on-keyday/kscale/protobuf/proto"
)

func TestRouterDiff(t *testing.T) {
	type testCase struct {
		description string
		current     []*pb.AddrPortInfo
		desired     []*pb.AddrPortInfo
		adds        []string
		removes     []string
	}
	/*
		n - none
		e - exists
		current desire add remove
		n       n      n   n
		e       n      n   e
		n       e      e   n
		e       e      n   n
	*/
	cases := []testCase{
		{
			description: "no current, no desired",
			current:     []*pb.AddrPortInfo{},
			desired:     []*pb.AddrPortInfo{},
			adds:        []string{},
			removes:     []string{},
		},
		{
			description: "current exists, no desired",
			current: []*pb.AddrPortInfo{
				{
					Addr:  netip.AddrFrom4([4]byte{192, 168, 0, 1}),
					Ports: []*pb.PortInfo{{Port: 80, Protocol: "tcp"}},
				},
			},
			desired: []*pb.AddrPortInfo{},
			adds:    []string{},
			removes: []string{"no permit tcp any host 192.168.0.1 eq 80"},
		},
		{
			description: "no current, desired exists",
			current:     []*pb.AddrPortInfo{},
			desired: []*pb.AddrPortInfo{
				{
					Addr:  netip.AddrFrom4([4]byte{192, 168, 0, 1}),
					Ports: []*pb.PortInfo{{Port: 80, Protocol: "tcp"}},
				},
			},
			adds:    []string{"permit tcp any host 192.168.0.1 eq 80"},
			removes: []string{},
		},
		{
			description: "both current and desired exist, no change",
			current: []*pb.AddrPortInfo{
				{
					Addr:  netip.AddrFrom4([4]byte{192, 168, 0, 1}),
					Ports: []*pb.PortInfo{{Port: 80, Protocol: "tcp"}},
				},
			},
			desired: []*pb.AddrPortInfo{
				{
					Addr:  netip.AddrFrom4([4]byte{192, 168, 0, 1}),
					Ports: []*pb.PortInfo{{Port: 80, Protocol: "tcp"}},
				},
			},
			adds:    []string{},
			removes: []string{},
		},
		{
			description: "exists but different ports",
			current: []*pb.AddrPortInfo{
				{
					Addr:  netip.AddrFrom4([4]byte{192, 168, 0, 1}),
					Ports: []*pb.PortInfo{{Port: 80, Protocol: "tcp"}},
				},
			},
			desired: []*pb.AddrPortInfo{
				{
					Addr:  netip.AddrFrom4([4]byte{192, 168, 0, 1}),
					Ports: []*pb.PortInfo{{Port: 443, Protocol: "tcp"}},
				},
			},
			adds:    []string{"permit tcp any host 192.168.0.1 eq 443"},
			removes: []string{"no permit tcp any host 192.168.0.1 eq 80"},
		},
		{
			description: "exists but different protocols",
			current: []*pb.AddrPortInfo{
				{
					Addr:  netip.AddrFrom4([4]byte{192, 168, 0, 1}),
					Ports: []*pb.PortInfo{{Port: 53, Protocol: "udp"}},
				},
			},
			desired: []*pb.AddrPortInfo{
				{
					Addr:  netip.AddrFrom4([4]byte{192, 168, 0, 1}),
					Ports: []*pb.PortInfo{{Port: 53, Protocol: "tcp"}},
				},
			},
			adds:    []string{"permit tcp any host 192.168.0.1 eq 53"},
			removes: []string{"no permit udp any host 192.168.0.1 eq 53"},
		},
		{
			description: "address removed",
			current: []*pb.AddrPortInfo{
				{
					Addr:  netip.AddrFrom4([4]byte{192, 168, 0, 1}),
					Ports: []*pb.PortInfo{{Port: 80, Protocol: "tcp"}},
				},
			},
			desired: []*pb.AddrPortInfo{},
			adds:    []string{},
			removes: []string{"no permit tcp any host 192.168.0.1 eq 80"},
		},
		{
			// icmp entries (Port 0) render as "permit icmp any host X echo", not "eq 0" —
			// the router-side gate for the l4lb VIP ICMP echo reply (vip.icmp_echo).
			description: "icmp echo added",
			current: []*pb.AddrPortInfo{
				{
					Addr:  netip.AddrFrom4([4]byte{192, 168, 0, 1}),
					Ports: []*pb.PortInfo{{Port: 443, Protocol: "tcp"}},
				},
			},
			desired: []*pb.AddrPortInfo{
				{
					Addr:  netip.AddrFrom4([4]byte{192, 168, 0, 1}),
					Ports: []*pb.PortInfo{{Port: 443, Protocol: "tcp"}, {Port: 0, Protocol: "icmp"}},
				},
			},
			adds:    []string{"permit icmp any host 192.168.0.1 echo"},
			removes: []string{},
		},
		{
			description: "icmp echo removed",
			current: []*pb.AddrPortInfo{
				{
					Addr:  netip.AddrFrom4([4]byte{192, 168, 0, 1}),
					Ports: []*pb.PortInfo{{Port: 0, Protocol: "icmp"}},
				},
			},
			desired: []*pb.AddrPortInfo{
				{
					Addr:  netip.AddrFrom4([4]byte{192, 168, 0, 1}),
					Ports: []*pb.PortInfo{},
				},
			},
			adds:    []string{},
			removes: []string{"no permit icmp any host 192.168.0.1 echo"},
		},
		{
			description: "icmp echo unchanged",
			current: []*pb.AddrPortInfo{
				{
					Addr:  netip.AddrFrom4([4]byte{192, 168, 0, 1}),
					Ports: []*pb.PortInfo{{Port: 0, Protocol: "icmp"}},
				},
			},
			desired: []*pb.AddrPortInfo{
				{
					Addr:  netip.AddrFrom4([4]byte{192, 168, 0, 1}),
					Ports: []*pb.PortInfo{{Port: 0, Protocol: "icmp"}},
				},
			},
			adds:    []string{},
			removes: []string{},
		},
		{
			description: "multiple addresses and ports",
			current: []*pb.AddrPortInfo{
				{
					Addr:  netip.AddrFrom4([4]byte{192, 168, 0, 1}),
					Ports: []*pb.PortInfo{{Port: 80, Protocol: "tcp"}, {Port: 22, Protocol: "tcp"}},
				},
				{
					Addr:  netip.AddrFrom4([4]byte{192, 168, 0, 2}),
					Ports: []*pb.PortInfo{{Port: 53, Protocol: "udp"}},
				},
			},
			desired: []*pb.AddrPortInfo{
				{
					Addr:  netip.AddrFrom4([4]byte{192, 168, 0, 1}),
					Ports: []*pb.PortInfo{{Port: 22, Protocol: "tcp"}, {Port: 443, Protocol: "tcp"}},
				},
				{
					Addr:  netip.AddrFrom4([4]byte{192, 168, 0, 3}),
					Ports: []*pb.PortInfo{{Port: 80, Protocol: "tcp"}},
				},
			},
			adds: []string{
				"permit tcp any host 192.168.0.1 eq 443",
				"permit tcp any host 192.168.0.3 eq 80",
			},
			removes: []string{
				"no permit tcp any host 192.168.0.1 eq 80",
				"no permit udp any host 192.168.0.2 eq 53",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.description, func(t *testing.T) {
			client := &Client{}
			adds, removes := client.calculateDiff(tc.current, tc.desired)
			if len(adds) != len(tc.adds) {
				t.Errorf("expected adds length %d, got %d", len(tc.adds), len(adds))
			}
			for i, add := range adds {
				if add != tc.adds[i] {
					t.Errorf("expected add %s, got %s", tc.adds[i], add)
				}
			}
			if len(removes) != len(tc.removes) {
				t.Errorf("expected removes length %d, got %d", len(tc.removes), len(removes))
			}
			for i, remove := range removes {
				if remove != tc.removes[i] {
					t.Errorf("expected remove %s, got %s", tc.removes[i], remove)
				}
			}
		})
	}
}

func TestParseACLResponse(t *testing.T) {
	client := &Client{}
	// The icmp echo line must be parsed too — invisible lines are never removed and
	// get re-added on every sync. The trailing "(5 matches)" mirrors real IOS output.
	response := `permit tcp any host 192.168.0.1 eq 80
permit udp any host 192.168.0.2 eq 53
permit tcp any host 192.168.0.3 eq 22
permit icmp any host 192.168.0.4 echo (5 matches)
`
	ports, err := client.parseCurrentAclResponse(response, slog.Default())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []*pb.AddrPortInfo{
		{
			Addr: netip.AddrFrom4([4]byte{192, 168, 0, 1}),
			Ports: []*pb.PortInfo{
				{Port: 80, Protocol: "tcp"},
			},
		},
		{
			Addr: netip.AddrFrom4([4]byte{192, 168, 0, 2}),
			Ports: []*pb.PortInfo{
				{Port: 53, Protocol: "udp"},
			},
		},
		{
			Addr: netip.AddrFrom4([4]byte{192, 168, 0, 3}),
			Ports: []*pb.PortInfo{
				{Port: 22, Protocol: "tcp"},
			},
		},
		{
			Addr: netip.AddrFrom4([4]byte{192, 168, 0, 4}),
			Ports: []*pb.PortInfo{
				{Port: 0, Protocol: "icmp"},
			},
		},
	}
	if len(ports) != len(expected) {
		t.Fatalf("expected %d entries, got %d", len(expected), len(ports))
	}
	for i, portInfo := range ports {
		if portInfo.Addr != expected[i].Addr {
			t.Errorf("expected addr %s, got %s", expected[i].Addr, portInfo.Addr)
		}
		if len(portInfo.Ports) != len(expected[i].Ports) {
			t.Fatalf("expected %d ports for addr %s, got %d", len(expected[i].Ports), portInfo.Addr, len(portInfo.Ports))
		}
		for j, port := range portInfo.Ports {
			if *port != *expected[i].Ports[j] {
				t.Errorf("expected port %+v, got %+v", expected[i].Ports[j], port)
			}
		}
	}
}
