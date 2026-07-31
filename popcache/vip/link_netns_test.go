//go:build linux

package vip

import (
	"io"
	"log/slog"
	"net/netip"
	"os"
	"testing"

	"github.com/vishvananda/netlink"
)

// TestUpdateRemoteCreatesIptun drives the real VIPManager.UpdateRemote against the
// kernel: it creates an IPIP tunnel per remote (the popcache IPIP-decap setup the
// l4lb<->popcache peering reconcile pushes) and removes the ones no longer present.
//
// Needs CAP_NET_ADMIN over a writable network namespace and the ipip module
// loaded; guarded so plain `go test ./...` skips it. Run with:
//
//	unshare -rn env KSCALE_NETNS_TEST=1 go test -run TestUpdateRemote ./popcache/vip/
func TestUpdateRemoteCreatesIptun(t *testing.T) {
	if os.Getenv("KSCALE_NETNS_TEST") != "1" {
		t.Skip("set KSCALE_NETNS_TEST=1 under `unshare -rn` (needs CAP_NET_ADMIN + ipip)")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// A veth stands in for the physical NIC the VIP manager binds the tunnels to.
	la := netlink.NewLinkAttrs()
	la.Name = "kphys"
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: la, PeerName: "kpeer"}); err != nil {
		t.Fatalf("create veth: %v", err)
	}
	phys, err := netlink.LinkByName("kphys")
	if err != nil {
		t.Fatalf("LinkByName: %v", err)
	}
	addr, _ := netlink.ParseAddr("10.0.0.1/24")
	if err := netlink.AddrAdd(phys, addr); err != nil {
		t.Fatalf("AddrAdd: %v", err)
	}
	if err := netlink.LinkSetUp(phys); err != nil {
		t.Fatalf("LinkSetUp: %v", err)
	}

	m, err := NewVIPManager()
	if err != nil {
		t.Fatalf("NewVIPManager: %v", err)
	}
	if err := m.BindToDevice("kphys", logger); err != nil {
		t.Fatalf("BindToDevice: %v", err)
	}

	r1 := RemoteInfo{Address: netip.MustParseAddr("10.0.0.11"), MacAddress: [6]byte{0xaa, 0, 0, 0, 0, 0x01}}
	r2 := RemoteInfo{Address: netip.MustParseAddr("10.0.0.12"), MacAddress: [6]byte{0xaa, 0, 0, 0, 0, 0x02}}

	if err := m.UpdateRemote([]RemoteInfo{r1, r2}, logger); err != nil {
		t.Fatalf("UpdateRemote(2): %v", err)
	}
	if n := countTunnels(t); n != 2 {
		t.Fatalf("after 2 remotes: %d ipip tunnels, want 2", n)
	}

	// Drop r2 from the desired set: its tunnel must be removed (the peering
	// reconcile converges on deletion).
	if err := m.UpdateRemote([]RemoteInfo{r1}, logger); err != nil {
		t.Fatalf("UpdateRemote(1): %v", err)
	}
	if n := countTunnels(t); n != 1 {
		t.Fatalf("after pruning r2: %d ipip tunnels, want 1", n)
	}
}

// countTunnels counts the IPIP tunnel links the manager created (excluding the
// module's fallback tunl0).
func countTunnels(t *testing.T) int {
	t.Helper()
	links, err := netlink.LinkList()
	if err != nil {
		t.Fatalf("LinkList: %v", err)
	}
	n := 0
	for _, l := range links {
		if _, ok := l.(*netlink.Iptun); ok && l.Attrs().Name != "tunl0" {
			n++
		}
	}
	return n
}
