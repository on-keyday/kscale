// Package control is the kscale-native router dataplane controller — the
// framework-free re-home of ksdk's routerAgent (router/agent.go). It owns the
// router device Client (scrapligo SSH/CLI to a Cisco IOS-XE box) and applies the
// VIP it has been told to serve; the routeragent calls these methods directly
// from its RouterService handlers instead of routing through the agent framework.
//
// The router "dataplane" is a managed network device, so Start/Stop are no-ops
// (matching ksdk's dummy StartDataplane/StopDataplane) — the device is configured
// on demand via the RouterService RPCs (ListOpenPort / DesireOpenPort), not by a
// long-running local process.
package control

import (
	"context"
	"log/slog"
	"net/netip"
	"sort"
	"sync"

	pbstat "github.com/on-keyday/kscale/protobuf/proto/stat"
	"github.com/on-keyday/kscale/router/dev"
	"github.com/on-keyday/kscale/stat"
)

// Controller owns the router device Client and tracks the VIP it serves (reported
// back as the Vip resource's status, mirroring the other dataplanes).
type Controller struct {
	client *dev.Client
	logger *slog.Logger
	mu     sync.Mutex
	// appliedVips is the observed VIP set reported via StreamStats so the reconcile
	// loop can reflect it as the Vip resource's status.
	appliedVips map[string]struct{}
	// lc tracks the authoritative app lifecycle, reported as the first Stats entry.
	lc *stat.AppLifecycle
	// promMetrics is the node's prometheus surface. The router has no app-specific
	// counters, but a non-nil PromMetrics is what makes the substrate register the
	// /metrics-over-transport handler (and drive host telemetry into it); returning
	// nil left the router with no /metrics endpoint at all, so the control plane's
	// ScrapeMetrics dial closed with "unexpected EOF" and the node never scraped.
	promMetrics *stat.PromMetrics
}

// New builds the controller with an unconfigured device Client; host/credentials
// are set live via the RouterService setters before the device is touched.
func New(logger *slog.Logger) *Controller {
	return &Controller{
		client:      dev.NewClient("", "", ""),
		logger:      logger,
		appliedVips: map[string]struct{}{},
		lc:          stat.NewAppLifecycle(),
		promMetrics: &stat.PromMetrics{},
	}
}

// Client exposes the underlying device Client (the routeragent registers its
// RouterService against it).
func (c *Controller) Client() *dev.Client { return c.client }

// DpType identifies this dataplane to the common substrate (dataplane.Hooks).
func (c *Controller) DpType() string { return "router" }

// Start / Stop are no-ops: the router device is configured on demand via the
// RouterService RPCs, there is no local dataplane process to bring up or tear down
// (ksdk's StartDataplane/StopDataplane were dummies).
func (c *Controller) Start(_ context.Context) error {
	c.lc.SetRunning()
	return nil
}
func (c *Controller) Stop() error {
	c.lc.SetStopped()
	return nil
}

// UpdateVip records the VIP and pushes it to the device Client (used as the host
// in the ACL permit entries the RouterService syncs).
func (c *Controller) UpdateVip(vip netip.Addr, _ bool) error { // icmpEcho: l4lb-only
	c.mu.Lock()
	c.appliedVips[vip.String()] = struct{}{}
	c.mu.Unlock()
	c.client.SetVIP([]netip.Addr{vip})
	return nil
}

// SyncSecret / BindInterface / UpdateMtu are l4lb-specific and have no meaning for
// router; they are no-ops so the shared DataplaneService surface stays uniform.
func (c *Controller) SyncSecret(_ []byte) error                   { return nil }
func (c *Controller) BindInterface(_ string) error                { return nil }
func (c *Controller) UpdateMtu(_ uint16) error                    { return nil }
func (c *Controller) UpdateDestinations(_ []stat.DestEntry) error { return nil } // l4lb-only
func (c *Controller) UpdateRemote(_ []stat.DestEntry) error       { return nil } // popcache-only
func (c *Controller) SetServerID(_ uint32)                        {}             // ServerID is meaningless for this dp

// Stats reports the observed VIP set as the Vip resource's status. The router has
// no request metrics of its own.
func (c *Controller) Stats() []*pbstat.Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.appliedVips) == 0 {
		return []*pbstat.Stats{c.lc.Stat()}
	}
	vips := make([]string, 0, len(c.appliedVips))
	for v := range c.appliedVips {
		vips = append(vips, v)
	}
	sort.Strings(vips)
	return []*pbstat.Stats{
		c.lc.Stat(),
		{CdnAppRealtime: &pbstat.CdnAppRealtimeStat{Vip: &pbstat.StringList{Items: vips}}},
	}
}

// PromMetrics returns the router's prometheus surface. It carries no app-specific
// counters (the router has none), but the shared substrate populates it with host
// telemetry (CPU / mem / disk / network / temperature / uptime) + process/Go metrics
// and serves it at /metrics-over-transport — so the router node scrapes like the rest.
func (c *Controller) PromMetrics() *stat.PromMetrics { return c.promMetrics }
