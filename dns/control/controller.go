// Package control is the kscale-native dns dataplane controller — the
// framework-free re-home of ksdk's dnsAgent (dns/agent.go). It owns the running
// authoritative DNS server (built-in miekg or the Cloudflare API backend), applies
// live config (port/domain/mail/API token/zone/ACME tokens) by delegating to the
// server's exported methods, and surfaces the server's request metrics. The
// dnsagent calls these controller methods directly from its DnsControlService
// handlers instead of routing through the agent framework; the metrics/zone
// reporting that ksdk's sendMetrics ran on a ticker is folded into Stats(),
// pull-driven by the dataplane substrate's StreamStats loop.
package control

import (
	"context"
	"log/slog"
	"net/netip"
	"sort"
	"sync"

	"github.com/on-keyday/kscale/dns/dnsmetrics"
	"github.com/on-keyday/kscale/dns/server"
	pbstat "github.com/on-keyday/kscale/protobuf/proto/stat"
	"github.com/on-keyday/kscale/stat"
)

// Controller owns the DNS server and applies config to it live. The dns request
// counters live in `dns` (the server increments them via SetMetricsCounter);
// `zones` is the managed-zone spec, guarded by the same metrics lock as ksdk did.
type Controller struct {
	srv         server.Server
	logger      *slog.Logger
	dns         dnsmetrics.DNSMetricsWithLock
	zones       stat.DnsSpecStat
	promMetrics *stat.PromMetrics
	lastStat    stat.DnsStat
	lastSpec    stat.DnsSpecStat
	// appliedVips is the VIP set UpdateVip applied, reported at CdnAppRealtime.Vip
	// so the CP's vip.applied_on shows this node (same as the other dataplanes).
	mu          sync.Mutex
	appliedVips map[string]struct{}
	// lc tracks the authoritative app lifecycle, reported as the first Stats entry.
	lc *stat.AppLifecycle
}

// New wires the controller to a DNS server backend (built-in or Cloudflare) and
// hooks the server's request counters into this controller's metrics so Stats()
// can report them.
func New(logger *slog.Logger, srv server.Server) *Controller {
	c := &Controller{srv: srv, logger: logger, promMetrics: &stat.PromMetrics{DnsEnabled: true}, appliedVips: map[string]struct{}{}, lc: stat.NewAppLifecycle()}
	srv.SetMetricsCounter(&c.dns)
	return c
}

// Server exposes the underlying DNS server.
func (c *Controller) Server() server.Server { return c.srv }

// DpType identifies this dataplane to the common substrate (dataplane.Hooks).
func (c *Controller) DpType() string { return "dns" }

// Start brings up the DNS listener (built-in server); Cloudflare's Start reconciles
// records via the API.
func (c *Controller) Start(ctx context.Context) error {
	if err := c.srv.Start(ctx); err != nil {
		c.lc.SetError()
		return err
	}
	c.lc.SetRunning()
	return nil
}

// Stop tears the server down. Returns error to satisfy dataplane.Hooks.
func (c *Controller) Stop() error {
	err := c.srv.Stop()
	c.lc.SetStopped()
	return err
}

// UpdateVip sets the A-record VIP (IPv4 only, as in ksdk's dnsAgent update-vip)
// and records it for the CdnAppRealtime.Vip status report.
func (c *Controller) UpdateVip(vip netip.Addr, _ bool) error { // icmpEcho: l4lb-only
	if err := c.srv.SetV4VIP([]netip.Addr{vip}); err != nil {
		return err
	}
	c.mu.Lock()
	c.appliedVips[vip.String()] = struct{}{}
	c.mu.Unlock()
	return nil
}

// SyncSecret / BindInterface / UpdateMtu are l4lb-specific and have no meaning for
// dns; they are no-ops so the shared DataplaneService surface stays uniform.
func (c *Controller) SyncSecret(_ []byte) error                   { return nil }
func (c *Controller) BindInterface(_ string) error                { return nil }
func (c *Controller) UpdateMtu(_ uint16) error                    { return nil }
func (c *Controller) UpdateDestinations(_ []stat.DestEntry) error { return nil } // l4lb-only
func (c *Controller) UpdateRemote(_ []stat.DestEntry) error       { return nil } // popcache-only
func (c *Controller) SetServerID(_ uint32)                        {}             // ServerID is meaningless for this dp

// Stats returns the per-dp extra Stats entries reported via StreamStats: the DNS
// request counters (Dns) and the managed-zone spec (DnsSpec), each delta-reported
// (emitted only when changed) and folded into the live prometheus surface —
// mirroring ksdk's dnsAgent.sendMetrics, but pull-driven by the substrate.
func (c *Controller) Stats() []*pbstat.Stats {
	var counters dnsmetrics.DNSMetrics
	var zones stat.DnsSpecStat
	c.dns.WithLock(func(m *dnsmetrics.DNSMetrics) {
		counters = *m
		zones = c.zones
	})
	ps := stat.DnsStat{DnsStats: counters}

	var entries []*pbstat.Stats
	if !ps.Equal(&c.lastStat) {
		c.promMetrics.UpdateDnsStat(&ps)
		entries = append(entries, &pbstat.Stats{Dns: ps.ToProto()})
		c.lastStat = ps
	}
	if !zones.Equal(&c.lastSpec) {
		entries = append(entries, &pbstat.Stats{DnsSpec: zones.ToProto()})
		c.lastSpec = zones
	}
	// Report the applied VIP set (vip.applied_on source) every snapshot, like the
	// other dataplanes — the CP's VipObserver reads the latest batch, so this entry
	// must not be delta-gated.
	c.mu.Lock()
	if len(c.appliedVips) > 0 {
		vips := make([]string, 0, len(c.appliedVips))
		for v := range c.appliedVips {
			vips = append(vips, v)
		}
		sort.Strings(vips)
		entries = append(entries, &pbstat.Stats{CdnAppRealtime: &pbstat.CdnAppRealtimeStat{Vip: &pbstat.StringList{Items: vips}}})
	}
	c.mu.Unlock()
	return append([]*pbstat.Stats{c.lc.Stat()}, entries...)
}

// PromMetrics returns the live prometheus surface served at /metrics-over-transport.
func (c *Controller) PromMetrics() *stat.PromMetrics { return c.promMetrics }

// setZones records the managed-zone list under the metrics lock (ksdk parity).
func (c *Controller) setZones(zones []string) {
	c.dns.WithLock(func(*dnsmetrics.DNSMetrics) {
		c.zones.ManagedDNSZones = zones
	})
}

// --- live config, called from the dnsagent's DnsControlService handlers ---

// SetPort sets the listen port (built-in server only) and records it in metrics.
func (c *Controller) SetPort(port uint16) error {
	if err := c.srv.SetPort(port); err != nil {
		return err
	}
	c.dns.SetDnsPort(port)
	return nil
}

// SetDomain sets the authoritative domain and records it as the managed zone.
func (c *Controller) SetDomain(domain string) error {
	if err := c.srv.SetDomain(domain); err != nil {
		return err
	}
	c.setZones([]string{domain})
	return nil
}

func (c *Controller) SetMailAddress(mail string) error { return c.srv.SetMailAddress(mail) }
func (c *Controller) SetAPIToken(token string) error   { return c.srv.SetAPIToken(token) }
func (c *Controller) SetZoneID(zoneID string) error    { return c.srv.SetZoneID(zoneID) }
func (c *Controller) ResetAcmeToken(domain string) error {
	return c.srv.ResetAcmeToken(domain)
}
func (c *Controller) SetAcmeToken(domain, token string) error {
	return c.srv.SetAcmeToken(domain, token)
}
func (c *Controller) ClearAcmeToken(domain, token string) error {
	return c.srv.ClearAcmeToken(domain, token)
}
