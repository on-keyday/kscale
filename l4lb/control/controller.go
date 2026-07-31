// Package control is the kscale-native l4lb dataplane controller — the
// framework-free re-home of ksdk's l4lbControllerAgent (agent/control). It keeps
// the original's stateful stage-or-apply lifecycle, just without the agent.Agent
// (Run/AgentContext) framework: the dpagent calls these methods directly from
// its DataplaneService handlers.
//
// The original pattern, preserved here: every operation runs under a lock and,
// while the eBPF instance is not yet started, stages the value into the dynamic
// config; once StartDataplane has created the instance, the same operation is
// applied live. StartDataplane builds the driver from the staged config;
// StopDataplane tears it down.
package control

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"sync"

	"github.com/on-keyday/kscale/consts"

	"github.com/on-keyday/kscale/l4lb/l4lbdrv"
	pbstat "github.com/on-keyday/kscale/protobuf/proto/stat"
	"github.com/on-keyday/kscale/stat"
)

// Driver is the l4lb dataplane's programming surface. l4lbdrv.L4LB satisfies it
// (the real eBPF driver); StubDriver stands in on hosts without XDP.
type Driver interface {
	UpdateVIP(vip netip.Addr, icmpEcho bool) error
	UpdateDestinations(dests l4lbdrv.DestinationEntries) error
	SyncSecret(secret []byte, logger *slog.Logger) error
	Sync() error
	Close() error
}

var _ Driver = (*l4lbdrv.L4LB)(nil)

// statSource is the eBPF stat-counter surface of the live driver. The real
// *l4lbdrv.L4LB satisfies it; StubDriver does not (no kernel maps to read), so a
// type-assert on the staged driver cleanly distinguishes "running on XDP host"
// from "stub / not started" without widening the Driver contract.
type statSource interface {
	GetCounters() (*l4lbdrv.StatCounters, error)
	GetSrcIPCounters() (*l4lbdrv.SrcIPCounts, error)
	GetPacketSizeCounters(thresholds []uint32) (*l4lbdrv.PacketSizeDist, error)
	GetISNLsbDistribution(thresholds []uint8) (*l4lbdrv.ISNLeastSignificantByteMap, error)
}

var _ statSource = (*l4lbdrv.L4LB)(nil)

// packetSizeBuckets / isnLsbBuckets are the histogram thresholds the eBPF stat
// maps are aggregated into — copied verbatim from ksdk's sendEBPFStats so the
// prometheus series line up with the data plane's kernel-side bucketing.
var packetSizeBuckets = []uint32{64, 128, 256, 512, 1024, 1280, 1460, 1500, 1518, 9000}

var isnLsbBuckets = []uint8{
	0x00, 0x10, 0x20, 0x30, 0x40, 0x50, 0x60, 0x70, 0x80,
	0x90, 0xA0, 0xB0, 0xC0, 0xD0, 0xE0, 0xF0, 0xFF,
}

// DriverFactory builds the driver from the staged config (called by Start).
type DriverFactory func(fix *l4lbdrv.FixedConfig, dyn *l4lbdrv.DynamicConfig) (Driver, error)

// L4lbdrvFactory builds the real eBPF driver (needs an XDP host + eBPF objects).
func L4lbdrvFactory(logger *slog.Logger) DriverFactory {
	return func(fix *l4lbdrv.FixedConfig, dyn *l4lbdrv.DynamicConfig) (Driver, error) {
		return l4lbdrv.New(logger, fix, dyn)
	}
}

// StubFactory builds a logging stub driver (demos / non-XDP hosts).
func StubFactory(logger *slog.Logger) DriverFactory {
	return func(_ *l4lbdrv.FixedConfig, _ *l4lbdrv.DynamicConfig) (Driver, error) {
		return &StubDriver{logger: logger}, nil
	}
}

// Controller holds the l4lb instance (nil until Start) and the staged config,
// guarding all access with a lock — mirroring l4lbControllerAgent.
type Controller struct {
	mu      sync.Mutex
	drv     Driver
	fix     *l4lbdrv.FixedConfig
	dyn     *l4lbdrv.DynamicConfig
	factory DriverFactory
	logger  *slog.Logger
	// appliedVips is the set of VIPs this node has been told to apply — the
	// observed state reported back to the control plane (StreamStats) so the
	// reconcile loop can reflect it as the Vip resource's status. (l4lbdrv holds a
	// single VIP; this is the node's view of "what I was asked to serve".)
	appliedVips map[string]struct{}
	// promMetrics is the live prometheus surface served at /metrics-over-transport;
	// lastStat is the previous eBPF snapshot, kept so the StreamStats loop only
	// re-emits the L4Lb stat entry on change (mirroring ksdk's delta reporting).
	promMetrics *stat.PromMetrics
	lastStat    stat.L4lbStat
	// serverID is this node's CP-assigned ServerID, reported back as LbId so the
	// peering identity is symmetric with popcache backends.
	serverID uint32
	// lc tracks the authoritative app lifecycle (Initialized/Running/Stopped/Error),
	// reported as the first Stats entry's AppStatus.
	lc *stat.AppLifecycle
}

// SetServerID records the CP-assigned ServerID (reported back as LbId).
func (c *Controller) SetServerID(id uint32) {
	c.mu.Lock()
	c.serverID = id
	c.mu.Unlock()
}

func (c *Controller) UpdateRemote(_ []stat.DestEntry) error { return nil } // popcache-only

// UpdateDestinations stages (before start) or applies (after) the eBPF dest table
// — the popcache backends the CP computed from reported stats (stat/dest.go).
func (c *Controller) UpdateDestinations(dests []stat.DestEntry) error {
	entries := make(l4lbdrv.DestinationEntries, len(dests))
	for i, d := range dests {
		entries[i] = l4lbdrv.DestinationEntry{IPAddr: d.IPAddr, HardwareAddr: d.HardwareAddr, ServerID: d.ServerID}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.drv == nil {
		c.dyn.Dests = entries
		return nil
	}
	return c.drv.UpdateDestinations(entries)
}

// New returns a Controller staged with the original's defaults (MTU 1500, fixed
// routing salt) and the given driver factory. logger is used for live SyncSecret
// programming (may be nil).
func New(fix *l4lbdrv.FixedConfig, factory DriverFactory, logger *slog.Logger) *Controller {
	return &Controller{
		fix:         fix,
		dyn:         &l4lbdrv.DynamicConfig{MTU: 1500, RoutingRandom: [4]byte{0xa5, 0xa5, 0xa5, 0xa5}},
		factory:     factory,
		logger:      logger,
		appliedVips: map[string]struct{}{},
		promMetrics: &stat.PromMetrics{},
		lc:          stat.NewAppLifecycle(),
	}
}

// DpType identifies this dataplane to the common substrate (dataplane.Hooks).
func (c *Controller) DpType() string { return "l4lb" }

// Stats returns the per-dp extra Stats entries reported via StreamStats: the
// observed VIP set as the Vip resource's status, plus the eBPF stat counters when
// the live driver is running on an XDP host. The substrate supplies node identity
// / host realtime on a separate base entry.
func (c *Controller) Stats() []*pbstat.Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	var entries []*pbstat.Stats
	// Observed VIP set -> Vip resource status; plus peering identity (bound
	// interface / LbId / AppStatus) once the driver is running, mirroring popcache.
	vips := make([]string, 0, len(c.appliedVips))
	for v := range c.appliedVips {
		vips = append(vips, v)
	}
	sort.Strings(vips)
	running := c.drv != nil && c.dyn.InterfaceName != "" && c.serverID != 0
	if len(vips) > 0 || running {
		app := &pbstat.CdnAppRealtimeStat{}
		if len(vips) > 0 {
			app.Vip = &pbstat.StringList{Items: vips}
		}
		if running {
			id := c.serverID
			appstat := uint32(consts.AppStatusRunning)
			app.BoundIfaces = &pbstat.StringList{Items: []string{c.dyn.InterfaceName}}
			app.LbId = &id
			app.Appstat = &appstat
		}
		entries = append(entries, &pbstat.Stats{CdnAppRealtime: app})
	}
	// eBPF counters -> L4Lb stat (delta) + live prometheus update.
	if l4 := c.sampleL4lbStatLocked(); l4 != nil {
		entries = append(entries, &pbstat.Stats{L4Lb: l4})
	}
	return append([]*pbstat.Stats{c.lc.Stat()}, entries...)
}

// sampleL4lbStatLocked polls the eBPF stat maps (only when the live driver exposes
// them — i.e. started on an XDP host), updates the prometheus surface, and returns
// the L4Lb stat proto when the snapshot changed since the last sample (nil on the
// stub driver, before start, or when unchanged). Mirrors ksdk's
// l4lbControllerAgent.sendEBPFStats, but pull-driven by the substrate's StreamStats
// loop rather than a self-running ticker. Caller must hold c.mu.
func (c *Controller) sampleL4lbStatLocked() *pbstat.L4LbStat {
	src, ok := c.drv.(statSource)
	if !ok {
		return nil // stub driver / not started: no eBPF maps to read
	}
	counters, err := src.GetCounters()
	if err != nil {
		c.logf("get eBPF counters", err)
		return nil
	}
	ips, err := src.GetSrcIPCounters()
	if err != nil {
		c.logf("get source IP counters", err)
		return nil
	}
	pkts, err := src.GetPacketSizeCounters(packetSizeBuckets)
	if err != nil {
		c.logf("get packet size counters", err)
		return nil
	}
	dist, err := src.GetISNLsbDistribution(isnLsbBuckets)
	if err != nil {
		c.logf("get ISN LSB distribution", err)
		return nil
	}
	newStat := stat.L4lbStat{
		EbpfStats:                           *counters,
		SourceIPPacketCount:                 *ips,
		PacketSizeDistribution:              *pkts,
		ISNLeastSignificantByteDistribution: *dist,
	}
	c.promMetrics.L4lbEbpfEnabled = true
	if newStat.Equal(&c.lastStat) {
		return nil
	}
	c.promMetrics.UpdateL4lbStat(&newStat)
	c.lastStat = newStat
	return newStat.ToProto()
}

func (c *Controller) logf(msg string, err error) {
	if c.logger != nil {
		c.logger.Error("l4lb stat: "+msg, "error", err)
	}
}

// PromMetrics returns the live prometheus surface (eBPF counters once the data
// plane is running on an XDP host) served at /metrics-over-transport.
func (c *Controller) PromMetrics() *stat.PromMetrics { return c.promMetrics }

// Vips returns the observed VIP set (sorted) for status reporting.
func (c *Controller) Vips() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.appliedVips))
	for v := range c.appliedVips {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// Start builds the eBPF instance from the staged config. The context argument
// satisfies dataplane.Hooks (StartDataplane); l4lb start is synchronous so it is
// unused.
func (c *Controller) Start(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.drv != nil {
		return fmt.Errorf("l4lb: already started")
	}
	d, err := c.factory(c.fix, c.dyn)
	if err != nil {
		c.lc.SetError()
		return fmt.Errorf("l4lb: start: %w", err)
	}
	c.drv = d
	c.lc.SetRunning()
	return nil
}

// SetBalancerObject configures which eBPF objects this l4lb node loads (the l4lb_object
// resource). The paths are staged into the fixed config and take effect at the next
// StartDataplane; if the dataplane is already running, the driver is reloaded live.
func (c *Controller) SetBalancerObject(binPath, cryptoBin, pinDir string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fix.BinPath = binPath
	c.fix.CryptoBin = cryptoBin
	c.fix.EBPFPinDir = pinDir
	if c.drv == nil {
		return nil // staged; applied at the next Start
	}
	if err := c.drv.Close(); err != nil {
		c.logger.Warn("l4lb: close on balancer-object reload", "error", err)
	}
	c.drv = nil
	d, err := c.factory(c.fix, c.dyn)
	if err != nil {
		c.lc.SetError()
		return fmt.Errorf("l4lb: reload balancer object: %w", err)
	}
	c.drv = d
	c.lc.SetRunning()
	return nil
}

// Stop tears the eBPF instance down.
func (c *Controller) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.drv == nil {
		return fmt.Errorf("l4lb: not started")
	}
	err := c.drv.Close()
	c.drv = nil
	c.lc.SetStopped()
	return err
}

// UpdateVip stages (before start) or applies (after) a VIP, choosing v4/v6.
// icmpEcho toggles in-XDP ICMPv4 echo reply for VIP-destined pings (a node-wide
// data-plane flag; the last applied VIP's value wins on the single-VIP config).
func (c *Controller) UpdateVip(vip netip.Addr, icmpEcho bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.appliedVips[vip.String()] = struct{}{} // observed state for status reporting
	c.dyn.IcmpEcho = icmpEcho
	if c.drv == nil {
		if vip.Is4() {
			c.dyn.VIP = vip
		} else {
			c.dyn.VIPv6 = vip
		}
		return nil
	}
	return c.drv.UpdateVIP(vip, icmpEcho)
}

// SyncSecret stages or applies the QUIC shared secret.
func (c *Controller) SyncSecret(secret []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.drv == nil {
		c.dyn.SharedKey = secret
		return nil
	}
	return c.drv.SyncSecret(secret, c.logger)
}

// BindInterface stages the interface to attach XDP to (only valid before start).
func (c *Controller) BindInterface(iface string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.drv != nil {
		// Already started: the reconcile loop keeps pushing the desired interface, so
		// a redundant re-bind to the same value is a no-op; only a *different* one is
		// an error (the XDP attach can't be moved live).
		if iface == c.dyn.InterfaceName {
			return nil
		}
		return fmt.Errorf("l4lb: cannot rebind interface %q->%q after start", c.dyn.InterfaceName, iface)
	}
	c.dyn.InterfaceName = iface
	return nil
}

// UpdateMtu stages the MTU and re-syncs the running instance.
func (c *Controller) UpdateMtu(mtu uint16) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dyn.MTU = mtu
	if c.drv == nil {
		return nil
	}
	return c.drv.Sync()
}

// StubDriver logs instead of programming the kernel.
type StubDriver struct{ logger *slog.Logger }

func (s *StubDriver) UpdateVIP(vip netip.Addr, icmpEcho bool) error {
	s.logger.Info("l4lb stub: UpdateVIP", "vip", vip)
	return nil
}
func (s *StubDriver) UpdateDestinations(dests l4lbdrv.DestinationEntries) error {
	s.logger.Info("l4lb stub: UpdateDestinations", "count", len(dests), "dests", dests)
	return nil
}
func (s *StubDriver) SyncSecret(secret []byte, _ *slog.Logger) error {
	s.logger.Info("l4lb stub: SyncSecret", "bytes", len(secret))
	return nil
}
func (s *StubDriver) Sync() error { return nil }
func (s *StubDriver) Close() error {
	s.logger.Info("l4lb stub: Close")
	return nil
}
