// Package control is the kscale-native popcache dataplane controller — the
// framework-free re-home of ksdk's serverAgent control surface (popcache/server).
// It owns the popcache node's running HTTP cache Server and applies live config
// (TLS paths, listen ports, origin, qlog) by delegating to the Server's exported
// methods; the popcacheagent calls these controller methods directly from its
// PopcacheControlService handlers instead of routing through the agent framework.
package control

import (
	"context"
	"log/slog"
	"net/netip"
	"os"
	"sync"

	"github.com/on-keyday/kscale/consts"
	"github.com/on-keyday/kscale/popcache/server"
	pbstat "github.com/on-keyday/kscale/protobuf/proto/stat"
	"github.com/on-keyday/kscale/stat"
)

// Controller owns the popcache HTTP cache Server and applies config to it live.
type Controller struct {
	srv    *server.Server
	logger *slog.Logger

	// Peering identity reported via StreamStats so the control plane can build
	// l4lb destination entries for this backend (stat/dest.go membership gate:
	// non-zero serverID + bound interface + running).
	mu         sync.Mutex
	serverID   uint32
	boundIface string
	started    bool

	// lc tracks the authoritative app lifecycle, reported as the first Stats entry.
	lc *stat.AppLifecycle
}

// New builds the popcache Server (ports/origin are then set via the setters
// before Run). fileDir is the node-local file root — shared with the dataplane
// substrate's file plane so WasmService.Register reads the same files DpFile.send
// writes; "" falls back to the working directory.
func New(logger *slog.Logger, fileDir string) (*Controller, error) {
	if fileDir == "" {
		fileDir = "."
	}
	root, err := os.OpenRoot(fileDir)
	if err != nil {
		return nil, err
	}
	srv, err := server.NewServer(server.Config{
		NodeID:             "popcache",
		InsecureListenAddr: ":80",
		SecureListenAddr:   ":443",
		HTTP3ListenAddr:    ":443",
		OriginURL:          "http://127.0.0.1:8080",
		FileDir:            root,
	}, logger)
	if err != nil {
		return nil, err
	}
	return &Controller{srv: srv, logger: logger, lc: stat.NewAppLifecycle()}, nil
}

// Server exposes the underlying HTTP cache Server (e.g. for the agent to register
// its WasmService on the RPC manager).
func (c *Controller) Server() *server.Server { return c.srv }

// Run starts the HTTP / HTTPS / HTTP3 listeners and the metrics goroutine.
func (c *Controller) Run(ctx context.Context) error { return c.srv.Start(ctx) }

// DpType identifies this dataplane to the common substrate (dataplane.Hooks).
func (c *Controller) DpType() string { return "popcache" }

// Start satisfies dataplane.Hooks (StartDataplane) — popcache's "dataplane" is
// the HTTP cache server; bring up its listeners.
// Start is idempotent: a Start while already running (e.g. the CP's node_run
// reconcile re-issuing it after a CP restart, before this node's stats reached
// the fresh CP) is a no-op rather than an "already running" error.
func (c *Controller) Start(ctx context.Context) error {
	c.mu.Lock()
	started := c.started
	c.mu.Unlock()
	if started {
		return nil
	}
	if err := c.srv.Start(ctx); err != nil {
		c.lc.SetError()
		return err
	}
	c.mu.Lock()
	c.started = true
	c.mu.Unlock()
	c.lc.SetRunning()
	return nil
}

// Stop tears down the running listeners. Returns error to satisfy dataplane.Hooks.
func (c *Controller) Stop() error {
	c.srv.Stop()
	c.mu.Lock()
	c.started = false
	c.mu.Unlock()
	c.lc.SetStopped()
	return nil
}

// UpdateVip is a no-op: the node's VIP on lo (and the IPIP decap tunnels) belong
// to netagent (dp_type net), so the internet-facing cache holds no CAP_NET_ADMIN.
// See notes/ai/2026_11_18_netagent_design.md.
func (c *Controller) UpdateVip(netip.Addr, bool) error { return nil }

// UpdateMtu is l4lb-specific (XDP MTU re-sync) and has no meaning for popcache; it
// is a no-op so the shared DataplaneService surface is uniform.
func (c *Controller) UpdateMtu(_ uint16) error                    { return nil }
func (c *Controller) UpdateDestinations(_ []stat.DestEntry) error { return nil } // l4lb-only

// BindInterface records popcache's serving interface — l4lb encapsulates to this
// interface's MAC/IP (reported via NetworkSpec), so the CP needs to know which
// interface this node is bound to. (For l4lb the same interface drives XDP.) The
// decap side of that interface is netagent's.
func (c *Controller) BindInterface(iface string) error {
	c.mu.Lock()
	c.boundIface = iface
	c.mu.Unlock()
	return nil
}

// UpdateRemote is a no-op: the l4lb fronts are peered to netagent, which owns
// the IPIP decap tunnels on this node.
func (c *Controller) UpdateRemote([]stat.DestEntry) error { return nil }

// Stats returns the per-dp extra Stats entries reported via StreamStats: the
// HTTP cache server's own metrics (hits/misses, origin) plus — when running and
// bound — a CdnAppRealtime entry carrying this backend's peering identity
// (serverID/LbId, bound interface, AppStatus) for the CP's dest reconcile. The
// substrate supplies node identity / host realtime / NetworkSpec on a base entry.
func (c *Controller) Stats() []*pbstat.Stats {
	var entries []*pbstat.Stats
	if popStat, popSpec := c.Metrics(); popStat != nil || popSpec != nil {
		entries = append(entries, &pbstat.Stats{Popcache: popStat, PopcacheSpec: popSpec})
	}
	c.mu.Lock()
	// The peering identity (BoundIfaces/LbId for the CP's dest reconcile), once
	// ready. No Vip: the VIP is netagent's, so vip.applied_on lists the net node.
	if c.started && c.boundIface != "" && c.serverID != 0 {
		id := c.serverID
		appstat := uint32(consts.AppStatusRunning)
		entries = append(entries, &pbstat.Stats{CdnAppRealtime: &pbstat.CdnAppRealtimeStat{
			BoundIfaces: &pbstat.StringList{Items: []string{c.boundIface}},
			LbId:        &id,
			Appstat:     &appstat,
		}})
	}
	c.mu.Unlock()
	return append([]*pbstat.Stats{c.lc.Stat()}, entries...)
}

func (c *Controller) SetCertPath(p string)     { c.srv.SetCertPath(p) }
func (c *Controller) SetKeyPath(p string)      { c.srv.SetKeyPath(p) }
func (c *Controller) SetHttpPort(p uint16)     { c.srv.SetHttpPort(p) }
func (c *Controller) SetHttpsPort(p uint16)    { c.srv.SetHttpsPort(p) }
func (c *Controller) SetHttp3Port(p uint16)    { c.srv.SetHttp3Port(p) }
func (c *Controller) SetOrigin(o string) error { return c.srv.SetOrigin(o) }
func (c *Controller) SetQlogEnabled(e bool)    { c.srv.SetQlogEnabled(e) }

// SetWasmComputeBudgets sets the node-wide default/ceiling for per-module wasm
// compute budgets (0 = built-in default).
func (c *Controller) SetWasmComputeBudgets(defaultMs, maxMs uint32) {
	c.srv.SetWasmComputeBudgets(defaultMs, maxMs)
}

// ReloadTlsCert re-reads the cert/key from disk and swaps the live certificate.
func (c *Controller) ReloadTlsCert() error { return c.srv.ReloadTlsCert() }

// SyncSecret rotates the QUIC-LB shared secret.
func (c *Controller) SyncSecret(secret []byte) error { return c.srv.SyncSecret(secret) }

// SetServerID updates the server ID used by the connection-ID generator and
// records it for the peering-identity report (LbId).
func (c *Controller) SetServerID(id uint32) {
	c.mu.Lock()
	c.serverID = id
	c.mu.Unlock()
	c.srv.SetServerID(id)
}

// Metrics returns the latest popcache stat snapshots for streaming over StreamStats.
func (c *Controller) Metrics() (*pbstat.PopcacheStat, *pbstat.PopcacheSpecStat) {
	return c.srv.Metrics()
}

// PromMetrics returns the live prometheus metrics for serving at /metrics.
func (c *Controller) PromMetrics() *stat.PromMetrics { return c.srv.PromMetrics() }
