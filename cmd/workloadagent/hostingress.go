package main

import (
	"fmt"
	"log/slog"
	"net/netip"
	"sync"

	"github.com/on-keyday/kscale/popcache/vip"
	"github.com/on-keyday/kscale/stat"
)

// hostIngress (--host-ingress) lets VIP traffic that the l4lb pins to this node
// (the l4_route resource) reach processes on the HOST, the way popcache receives
// it on s1/s2: a kernel IPIP tunnel to each l4lb front decapsulates, and the VIP
// on lo makes the inner packet local. Replies leave directly from the VIP (DSR).
//
// Only for a workload node WITHOUT a popcache agent (e.g. the GPU node): where
// popcache runs, it already owns the VIP on lo and the tunnels, and VIPManager
// removes every other non-loopback address from lo when it sets the VIP, so two
// owners would fight. The pod-network datapath (netdp) keeps working alongside:
// its tc ingress takes the IPIP for pod ports first and passes the rest up to
// these tunnels.
//
// Which ports actually arrive is decided by the l4lb (only routed ports are sent
// here), but every host service bound to 0.0.0.0 is reachable on the VIP for
// those ports — bind what you route, and nothing else, to the VIP's ports.
type hostIngress struct {
	logger *slog.Logger
	mgr    *vip.VIPManager

	mu      sync.Mutex
	iface   string // the device the tunnels are bound to ("" until BindInterface succeeds)
	vip     netip.Addr
	remotes []vip.RemoteInfo
}

func newHostIngress(logger *slog.Logger) *hostIngress {
	mgr, _ := vip.NewVIPManager() // never errors
	return &hostIngress{logger: logger, mgr: mgr}
}

// bind arms the tunnels on iface, then applies remotes that arrived before it
// (the CP pushes interface, VIP and remotes in no fixed order).
func (h *hostIngress) bind(iface string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if iface == "" || iface == h.iface {
		return nil
	}
	if err := h.mgr.BindToDevice(iface, h.logger); err != nil {
		return fmt.Errorf("host ingress: bind %s: %w", iface, err)
	}
	h.iface = iface
	if len(h.remotes) > 0 {
		if err := h.mgr.UpdateRemote(h.remotes, h.logger); err != nil {
			return fmt.Errorf("host ingress: tunnels: %w", err)
		}
	}
	return nil
}

func (h *hostIngress) setVIP(v netip.Addr) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.mgr.UpdateVIP(netip.PrefixFrom(v, v.BitLen()), h.logger); err != nil {
		return fmt.Errorf("host ingress: VIP %s on lo: %w", v, err)
	}
	h.vip = v
	return nil
}

// setRemotes keeps one IPIP tunnel per l4lb front; applied at once when bound,
// else remembered for bind.
func (h *hostIngress) setRemotes(remotes []stat.DestEntry) error {
	infos := make([]vip.RemoteInfo, 0, len(remotes))
	for _, r := range remotes {
		if !r.IPAddr.Is4() || len(r.HardwareAddr) != 6 {
			continue
		}
		infos = append(infos, vip.RemoteInfo{Address: r.IPAddr, MacAddress: [6]byte(r.HardwareAddr)})
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.remotes = infos
	if h.iface == "" {
		return nil
	}
	if err := h.mgr.UpdateRemote(infos, h.logger); err != nil {
		return fmt.Errorf("host ingress: tunnels: %w", err)
	}
	return nil
}

// boundIface is the interface the node reports, so the CP can resolve an
// l4_route to this node's address ("" until bound).
func (h *hostIngress) boundIface() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.iface
}
