// Command netagent is the node's network agent (dp_type "net"): the one owner, per
// node, of the kernel state that lets l4lb-forwarded VIP traffic arrive — the VIP
// on lo, an IPIP decap tunnel to each l4lb front, and rp_filter on those devices.
//
// It exists so the processes that face the internet hold no CAP_NET_ADMIN:
// popcache serves the decapped traffic but no longer configures the network, and
// a host-ingress node (the GPU node) needs no workload agent for it either. Only
// netagent holds CAP_NET_ADMIN, and it opens no port. Design:
// notes/ai/2026_11_18_netagent_design.md.
//
// The control plane drives it through the common DataplaneService like any dp:
// UpdateVip (vip resource, dp_type "*"), BindInterface (interface resource, "*")
// and UpdateRemote (peering l4lb -> net). They arrive in no fixed order, so the
// fronts are remembered until the interface is bound. The kernel objects outlive
// the agent: if it stops, decap keeps working for the fronts it last saw.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"sync"

	"github.com/on-keyday/kscale/dataplane"
	"github.com/on-keyday/kscale/internal/sigctx"
	"github.com/on-keyday/kscale/logbuf"
	"github.com/on-keyday/kscale/netmgr"
	pbstat "github.com/on-keyday/kscale/protobuf/proto/stat"
	"github.com/on-keyday/kscale/stat"
)

// domain is the CA domain; default "kscale.local", overridden by --ca-domain at startup.
// It MUST match the control plane's --ca-domain (it forms this node's cert CommonName).
var domain = "kscale.local"

// netHooks implements dataplane.Hooks over a netmgr.VIPManager.
type netHooks struct {
	logger      *slog.Logger
	mgr         *netmgr.VIPManager
	lc          *stat.AppLifecycle
	promMetrics *stat.PromMetrics

	mu      sync.Mutex
	iface   string // the device the tunnels are bound to ("" until BindInterface succeeds)
	vip     netip.Addr
	remotes []netmgr.RemoteInfo
}

func newNetHooks(logger *slog.Logger) *netHooks {
	mgr, _ := netmgr.NewVIPManager() // never errors (no netlink until BindToDevice)
	lc := stat.NewAppLifecycle()
	// No start gate: the agent only applies what the CP pushes, so it is
	// operational from boot (as the workload agent). Without this Stats reports
	// no app status and the CP shows "Unknown".
	lc.SetRunning()
	return &netHooks{logger: logger, mgr: mgr, lc: lc, promMetrics: &stat.PromMetrics{}}
}

func (h *netHooks) DpType() string                            { return "net" }
func (h *netHooks) Start(context.Context) error               { h.lc.SetRunning(); return nil }
func (h *netHooks) Stop() error                               { h.lc.SetStopped(); return nil }
func (h *netHooks) SyncSecret([]byte) error                   { return nil }
func (h *netHooks) UpdateMtu(uint16) error                    { return nil }
func (h *netHooks) SetServerID(uint32)                        {}
func (h *netHooks) UpdateDestinations([]stat.DestEntry) error { return nil }

// PromMetrics is non-nil so the substrate serves /metrics (host/process/go
// telemetry); nil would leave the CP's ScrapeMetrics on a stream that closes.
func (h *netHooks) PromMetrics() *stat.PromMetrics { return h.promMetrics }

// UpdateVip puts the VIP on lo, so the decapped inner packets are local.
func (h *netHooks) UpdateVip(v netip.Addr, _ bool) error { // icmpEcho: l4lb-only
	if !v.IsValid() {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.mgr.UpdateVIP(netip.PrefixFrom(v, v.BitLen()), h.logger); err != nil {
		return fmt.Errorf("net: VIP %s on lo: %w", v, err)
	}
	h.vip = v
	return nil
}

// BindInterface arms the tunnels on iface, then applies the fronts that arrived
// before it.
func (h *netHooks) BindInterface(iface string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if iface == "" || iface == h.iface {
		return nil
	}
	if err := h.mgr.BindToDevice(iface, h.logger); err != nil {
		return fmt.Errorf("net: bind %s: %w", iface, err)
	}
	h.iface = iface
	if len(h.remotes) > 0 {
		if err := h.mgr.UpdateRemote(h.remotes, h.logger); err != nil {
			return fmt.Errorf("net: tunnels: %w", err)
		}
	}
	return nil
}

// UpdateRemote keeps one IPIP tunnel per l4lb front (peering l4lb -> net):
// applied at once when bound, else remembered for BindInterface.
func (h *netHooks) UpdateRemote(remotes []stat.DestEntry) error {
	infos := make([]netmgr.RemoteInfo, 0, len(remotes))
	for _, r := range remotes {
		if !r.IPAddr.Is4() || len(r.HardwareAddr) != 6 {
			continue
		}
		infos = append(infos, netmgr.RemoteInfo{Address: r.IPAddr, MacAddress: [6]byte(r.HardwareAddr)})
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.remotes = infos
	if h.iface == "" {
		return nil
	}
	if err := h.mgr.UpdateRemote(infos, h.logger); err != nil {
		return fmt.Errorf("net: tunnels: %w", err)
	}
	return nil
}

// Stats reports, in the lifecycle entry (readers take the first CdnAppRealtime),
// the bound interface — so the interface resource's applied_on shows this node
// and an l4_route can resolve to its address — and the VIP actually on lo, for
// the vip resource's applied_on.
func (h *netHooks) Stats() []*pbstat.Stats {
	life := h.lc.Stat()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.iface != "" {
		life.CdnAppRealtime.BoundIfaces = &pbstat.StringList{Items: []string{h.iface}}
	}
	if h.vip.IsValid() {
		life.CdnAppRealtime.Vip = &pbstat.StringList{Items: []string{h.vip.String()}}
	}
	return []*pbstat.Stats{life}
}

func main() {
	logger := logbuf.NewStderrLogger(os.Stderr, slog.LevelInfo)
	fs := flag.NewFlagSet("netagent", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:9443", "control-plane UDP address")
	dataDir := fs.String("data", "/tmp/kscale-ca", "dir holding the net bootstrap token / saved cert")
	node := fs.String("node", "node1", "this node's name")
	fileDir := fs.String("file-dir", "", "node-local file store (default <data>/dp_files)")
	domainFlag := fs.String("ca-domain", domain, "CA domain — must match the control plane's --ca-domain")
	_ = fs.Parse(os.Args[1:])
	domain = *domainFlag

	ctx, stop := sigctx.Context()
	defer stop()

	if err := dataplane.Run(ctx, dataplane.Config{
		Addr:    *addr,
		DataDir: *dataDir,
		Node:    *node,
		App:     "net",
		Domain:  domain,
		FileDir: *fileDir,
		Hooks:   newNetHooks(logger),
	}, logger); err != nil && ctx.Err() == nil && !errors.Is(err, context.Canceled) {
		logger.Error("netagent exited", "error", err)
		os.Exit(1)
	}
}
