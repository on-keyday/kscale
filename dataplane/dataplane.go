// Package dataplane is the thin kscale-native substrate shared by every
// dataplane agent (cmd/dpagent for l4lb, cmd/popcacheagent for popcache). It is
// NOT a revival of ksdk's heavy agent.Agent/AgentContext framework — it is a
// small layer over kscale's existing primitives (client/peer/rpc/proxy/stat)
// that commonizes what the two agents previously duplicated or were missing:
//
//   - one DataplaneServiceServer implementation (file plane + lifecycle + stats)
//     serving EVERY dataplane, so popcache gets the file methods for free;
//   - a single accept-loop + magic registry (RPCS -> RPC dispatch, HTTP ->
//     /metrics-over-transport when the dp exposes prometheus metrics);
//   - the resilience both agents lacked: a reconnect loop and a cert auto-renewal
//     goroutine.
//
// Per-dataplane behavior is delegated through the Hooks interface; the reference
// for WHAT this provides is ksdk's agent/client/{client.go,control.go}.
package dataplane

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/on-keyday/kscale/agentserve"
	"github.com/on-keyday/kscale/ca"
	"github.com/on-keyday/kscale/client"
	"github.com/on-keyday/kscale/consts"
	"github.com/on-keyday/kscale/internal/epclean"
	"github.com/on-keyday/kscale/internal/safe"
	"github.com/on-keyday/kscale/logbuf"
	"github.com/on-keyday/kscale/peer"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbstat "github.com/on-keyday/kscale/protobuf/proto/stat"
	"github.com/on-keyday/kscale/protobuf/wkt"
	"github.com/on-keyday/kscale/remoteexec"
	"github.com/on-keyday/kscale/rpc"
	"github.com/on-keyday/kscale/stat"
	"github.com/on-keyday/kscale/trsf/proxy"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/transport"
	"github.com/on-keyday/objtrsf/trsf"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spaolacci/murmur3"
)

// Hooks is the per-dataplane behavior the common substrate delegates to. l4lb's
// and popcache's controllers each implement it; the substrate owns everything
// else (enrollment, connect, serve, file plane, stats framing, reconnect, cert
// renewal).
type Hooks interface {
	DpType() string                                  // "l4lb" / "popcache"
	Start(ctx context.Context) error                 // StartDataplane
	Stop() error                                     // StopDataplane
	UpdateVip(vip netip.Addr, icmpEcho bool) error   // l4lb: XDP VIP (+ICMPv4 echo-reply mode); popcache: assign VIP to lo for IPIP decap; dns/router: real (icmpEcho ignored off-l4lb)
	SyncSecret(secret []byte) error                  // rotate the QUIC-LB shared secret
	BindInterface(iface string) error                // l4lb: stage the XDP interface; popcache: record serving iface
	UpdateMtu(mtu uint16) error                      // l4lb: re-sync MTU; popcache: no-op
	SetServerID(id uint32)                           // CP-assigned ServerID (QUIC-LB routing + reported as LbId)
	UpdateDestinations(dests []stat.DestEntry) error // l4lb: program the eBPF dest table; others: no-op
	UpdateRemote(remotes []stat.DestEntry) error     // popcache: IPIP decap tunnels to l4lb fronts; others: no-op
	Stats() []*pbstat.Stats                          // per-dp extra Stats entries for StreamStats (may be nil)
	PromMetrics() *stat.PromMetrics                  // for /metrics-over-transport (nil -> no /metrics)
}

// ResolveFileDir is the node-local file store Run serves the file plane from:
// cfg.FileDir, or <DataDir>/dp_files when unset. Agents that resolve names sent
// by the control plane (node_file save_as) use it to find the same directory.
func ResolveFileDir(cfg Config) string {
	if cfg.FileDir != "" {
		return cfg.FileDir
	}
	return filepath.Join(cfg.DataDir, "dp_files")
}

// FileReceiver is an optional Hooks capability: OnFileReceived is called after a
// SendFile (node_file / dp-file send) has written name into the file store, so a
// dp can act on a file that arrives after it was told to use it.
type FileReceiver interface {
	OnFileReceived(name string)
}

// Config parameterizes Run. Addr is the control-plane UDP address; DataDir holds
// the bootstrap token + saved cert; Node/App/Domain form the node's CommonName;
// FileDir is the node-local file store for the file plane; Hooks supplies the
// per-dp behavior; RegisterExtra (optional) registers additional RPC services on
// the per-connection manager (popcache uses it for PopcacheControlService +
// WasmService; l4lb passes nil).
type Config struct {
	Addr          string
	DataDir       string
	Node          string
	App           string
	Domain        string
	PingInterval  time.Duration
	FileDir       string
	Hooks         Hooks
	RegisterExtra func(mgr *rpc.RPCManager)
}

// Run is the substrate entry point. It enrolls (or reuses a saved cert), connects
// to the control plane, serves the common DataplaneService (plus any RegisterExtra
// services) over the peer, and reconnects on disconnect. A background goroutine
// renews the certificate every 30 minutes. Returns when ctx is cancelled.
func Run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	if cfg.Hooks == nil {
		return fmt.Errorf("dataplane: Hooks must be set")
	}
	if cfg.PingInterval == 0 {
		cfg.PingInterval = 15 * time.Second
	}

	ap, err := netip.ParseAddrPort(cfg.Addr)
	if err != nil {
		return fmt.Errorf("dataplane: bad addr %q: %w", cfg.Addr, err)
	}
	ep, err := transport.UDPEndpoint(logger, 0, objproto.EndpointModeClient)
	if err != nil {
		return fmt.Errorf("dataplane: endpoint: %w", err)
	}
	// Evict stale endpoint state periodically (ksdk's cleaner; dropped in the re-home).
	safe.Go(logger, "endpoint-cleaner", func() { epclean.Run(ctx, ep, logger) })
	// CN reverse-parses to FullName system.dp.<App>.<Node>, matching the authority
	// the control plane seeds + the per-app bootstrap token.
	cn := cfg.Node + "." + cfg.App + ".dp.system." + cfg.Domain

	fileDir := ResolveFileDir(cfg)
	if err := os.MkdirAll(fileDir, 0o755); err != nil {
		return fmt.Errorf("dataplane: file dir: %w", err)
	}

	savePath := filepath.Join(cfg.DataDir, "client."+cfg.App+".boot")
	tokenPath := filepath.Join(cfg.DataDir, "bootstrap."+cfg.App+".token")

	bh := &bootHolder{}
	// Cert auto-renewal: refresh the cert used on the next reconnect. Non-fatal.
	safe.Go(logger, "cert-renew", func() { renewCert(ctx, ep, ap, cn, cfg.App, savePath, bh, logger) })

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Fresh random connection id PER attempt — a fixed id (was 1) collides with the
		// prior attempt's connection that the endpoint hasn't torn down yet ("connection
		// already exists for udp:…-1"), which otherwise wedges the reconnect loop forever.
		cid, err := objproto.NewRandomConnectionID("udp", ap)
		if err != nil {
			logger.Error("dataplane: new connection id", "error", err)
			if !sleepOrDone(ctx, 5*time.Second) {
				return ctx.Err()
			}
			continue
		}

		boot, err := enroll(ctx, ep, cid, savePath, tokenPath, cn, cfg.App)
		if err != nil {
			logger.Error("dataplane: enroll", "error", err)
			if !sleepOrDone(ctx, 5*time.Second) {
				return ctx.Err()
			}
			continue
		}
		bh.set(boot)

		p, _, err := client.Connect(ctx, ep, cid, cfg.App, boot, cfg.PingInterval, logger)
		if err != nil {
			logger.Error("dataplane: connect", "error", err)
			if !sleepOrDone(ctx, 5*time.Second) {
				return ctx.Err()
			}
			continue
		}

		logger.Info("dataplane: connected; serving DataplaneService",
			"addr", cfg.Addr, "common_name", cn, "dp_type", cfg.Hooks.DpType())
		err = serve(ctx, p, cfg, cn, fileDir, logger)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logger.Error("dataplane: connection lost; reconnecting in 5s", "error", err)
		if !sleepOrDone(ctx, 5*time.Second) {
			return ctx.Err()
		}
	}
}

// serverIDFromCN derives a stable ServerID from the node's CommonName using the
// SAME hash ksdk used for ServerIDs (spaolacci/murmur3.Sum32) — do not swap the
// hash function. (ksdk hashed the dp connection ID CP-side and pushed it via a
// SetServerId control message; kscale derives it node-side from the stable CN so
// the value survives reconnects and needs no extra RPC. The 0 -> 1 guard keeps it
// non-zero, which stat/dest.go treats as "unassigned".)
func serverIDFromCN(cn string) uint32 {
	id := murmur3.Sum32([]byte(cn))
	if id == 0 {
		id = 1
	}
	return id
}

// serve registers the common DataplaneService (+ RegisterExtra) on a fresh RPC
// manager, wires the magic registry (RPCS -> RPC dispatch, HTTP -> metrics when
// the dp exposes prometheus), and runs the accept-loop until it errors.
func serve(ctx context.Context, p *peer.Peer, cfg Config, cn, fileDir string, logger *slog.Logger) error {
	// Serve on the peer's per-connection context: when the connection dies
	// (transport error, peer close, or liveness timeout) it is cancelled, so the
	// AcceptBidirectionalStream loop below returns and Run()'s reconnect loop
	// fires. The parent ctx still cancels this as a child, so process shutdown is
	// unaffected. Without this, a silently-dead CP connection wedged serve()
	// forever (the endpoint GC reaped the connection but never unblocked accept).
	ctx = p.Context()
	mgr := rpc.NewRPCManager()
	// Assign this node a stable ServerID derived from its CommonName (QUIC-LB
	// routing index). ksdk hashed the connection ID CP-side and pushed it; deriving
	// it from the stable CN here is self-consistent (the node's connid generator and
	// its reported LbId agree) and needs no extra RPC round-trip.
	cfg.Hooks.SetServerID(serverIDFromCN(cn))
	pb.RegisterDataplaneServiceServer(mgr, &dpService{hooks: cfg.Hooks, fileDir: fileDir, cn: cn, peer: p, logger: logger})
	if cfg.RegisterExtra != nil {
		cfg.RegisterExtra(mgr)
	}

	// Magic registry: every accepted stream is routed by its leading magic.
	handlers := map[string]func(stream trsf.BidirectionalStream){
		rpc.StreamMagicRPCS: func(stream trsf.BidirectionalStream) {
			mgr.HandleService(ctx, logger, stream)
		},
		// Remote shell: the control plane relays an authorized EXEC stream here
		// (it is the dp's only peer + already ran the remote_shell ABAC check).
		consts.StreamMagicEXEC: func(stream trsf.BidirectionalStream) {
			if err := remoteexec.ServeDp(ctx, stream, logger); err != nil {
				logger.Warn("exec stream", "error", err)
			}
		},
	}
	// /metrics-over-transport: only when the dp exposes prometheus metrics.
	if m := cfg.Hooks.PromMetrics(); m != nil {
		// Stamp the node identity as the `instance` label (the controllers don't
		// know their own CommonName; the substrate does). UpdateCdnAppSpecStat is
		// not driven for these dps, so this is not overwritten.
		m.CommonName = cn
		lis := serveMetricsOverTransport(ctx, m, cn, p, cfg.Hooks, logger)
		handlers[consts.StreamMagicHTTP] = func(stream trsf.BidirectionalStream) {
			if err := lis.Send(stream); err != nil {
				stream.CloseBoth()
			}
		}
	}

	for {
		stream, err := p.Streams().AcceptBidirectionalStream(ctx)
		if err != nil {
			return err
		}
		go func(stream trsf.BidirectionalStream) {
			defer safe.Recover(logger, "rpc-stream")
			magic, err := rpc.DecodeMagic(ctx, stream)
			if err != nil {
				return
			}
			if h, ok := handlers[magic]; ok {
				h(stream)
				return
			}
			stream.CloseBoth()
		}(stream)
	}
}

// bootHolder guards the latest BootstrapInfo shared between the reconnect loop
// (which sets it after enroll) and the renewal goroutine (which reads it to renew
// and sets the renewed result).
type bootHolder struct {
	mu   sync.Mutex
	boot *ca.BootstrapInfo
}

func (b *bootHolder) set(boot *ca.BootstrapInfo) {
	b.mu.Lock()
	b.boot = boot
	b.mu.Unlock()
}

func (b *bootHolder) get() *ca.BootstrapInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.boot
}

// renewCert mirrors ksdk's certUpdater: every 30 minutes it runs the update-cert
// protocol with the saved boot and re-saves the result to savePath, so the next
// reconnect picks up the renewed cert. A distinct connection id (2) keeps the
// renewal handshake off the main connection (1) on the shared endpoint. Errors
// are logged and non-fatal.
func renewCert(ctx context.Context, ep objproto.Endpoint, ap netip.AddrPort, cn, app, savePath string, bh *bootHolder, logger *slog.Logger) {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		boot := bh.get()
		if boot == nil {
			continue
		}
		// Fresh random id per renewal too — a fixed id (was 2) wedges renewal the same way
		// (its prior renewal connection lingering -> "already exists for …-2"), which is
		// what silently stopped cert renewal and let the certs expire.
		renewCID, err := objproto.NewRandomConnectionID("udp", ap)
		if err != nil {
			logger.Error("dataplane: cert renewal id (non-fatal)", "error", err)
			continue
		}
		newInfo, err := ca.UpdateCertProtocol(ctx, ep, renewCID, cn, app, boot)
		if err != nil {
			logger.Error("dataplane: cert renewal failed (non-fatal)", "error", err)
			continue
		}
		dumped, err := newInfo.DumpResult()
		if err != nil {
			logger.Error("dataplane: cert renewal dump failed (non-fatal)", "error", err)
			continue
		}
		if err := os.WriteFile(savePath, dumped, 0o600); err != nil {
			logger.Error("dataplane: cert renewal save failed (non-fatal)", "error", err)
			continue
		}
		bh.set(newInfo)
		logger.Info("dataplane: certificate renewed", "common_name", cn)
	}
}

// enroll reuses a saved cert if present, else bootstraps with the per-app token.
func enroll(ctx context.Context, ep objproto.Endpoint, cid objproto.ConnectionID, savePath, tokenPath, cn, app string) (*ca.BootstrapInfo, error) {
	var token []byte
	if _, err := os.Stat(savePath); os.IsNotExist(err) {
		token, err = os.ReadFile(tokenPath)
		if err != nil {
			return nil, fmt.Errorf("read %s bootstrap token: %w", app, err)
		}
	}
	return client.Enroll(ctx, ep, cid, cn, token, app, savePath)
}

func sleepOrDone(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// promCollector adapts *stat.PromMetrics (which only has Collect) into a full
// prometheus.Collector by adding an empty Describe — registering it as an
// "unchecked" collector (the shape ksdk's stat reporter used). The mutex serializes
// Collect against the host-metrics updater (updateHostMetrics) that mutates the same
// PromMetrics' host/network/temperature slices.
type promCollector struct {
	m  *stat.PromMetrics
	mu *sync.Mutex
}

func (promCollector) Describe(chan<- *prometheus.Desc) {}
func (c promCollector) Collect(ch chan<- prometheus.Metric) {
	if c.m == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m.Collect(ch)
}

// serveMetricsOverTransport serves GET /metrics over a transport-proxy listener,
// so the control plane scrapes prometheus through the encrypted peer connection —
// not a local TCP port. Returns the listener the accept-loop feeds StreamMagicHTTP
// streams into. It also (a) registers the standard process / Go-runtime collectors —
// stamped with this node's CommonName as the `instance` label, since metricsgw merges
// every node's exposition and unlabelled process_*/go_* would collide into duplicate
// samples — and (b) starts updateHostMetrics, which refreshes the host/network/
// temperature/RTT/app-status gauges the per-agent loops don't touch.
func serveMetricsOverTransport(ctx context.Context, m *stat.PromMetrics, cn string, p *peer.Peer, hooks Hooks, logger *slog.Logger) proxy.TransferProxyListener {
	var mu sync.Mutex
	reg := prometheus.NewRegistry()
	reg.MustRegister(promCollector{m: m, mu: &mu})
	// process_open_fds / process_*_bytes / go_goroutines ... — the dashboards' process
	// and Golang panels. WrapRegistererWith stamps instance=<CommonName> so these stay
	// distinct per node through metricsgw's merge.
	nodeReg := prometheus.WrapRegistererWith(prometheus.Labels{"instance": cn}, reg)
	nodeReg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	nodeReg.MustRegister(collectors.NewGoCollector())
	safe.Go(logger, "dp-host-metrics", func() {
		updateHostMetrics(m, &mu, p, hooks, logger)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				updateHostMetrics(m, &mu, p, hooks, logger)
			}
		}
	})
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	lis := proxy.NewTransferProxyListener(ctx)
	safe.Go(logger, "dp-metrics", func() {
		if err := (&http.Server{Handler: mux}).Serve(lis); err != nil && err != http.ErrServerClosed {
			logger.Error("dataplane: metrics server", "error", err)
		}
	})
	return lis
}

// updateHostMetrics samples this node's host telemetry (CPU/mem/disk/swap/uptime/load,
// per-NIC counters, temperatures), its measured RTT to the control plane, and its app
// lifecycle status, and folds them into the shared PromMetrics under mu (serialized with
// Collect). Per-subsystem errors are logged and skipped so one failing sensor never
// blanks the rest. The app-specific counters (popcache/l4lb/dns) are fed by each agent's
// own loop and left untouched here.
func updateHostMetrics(m *stat.PromMetrics, mu *sync.Mutex, p *peer.Peer, hooks Hooks, logger *slog.Logger) {
	spec, specErr := stat.GetHostSpecStat()
	realtime, rtErr := stat.GetHostRealtimeStat()
	phys, physErr := stat.GetHostPhysicalStat()
	netRT, netErr := stat.GetNetworkRealtimeStat()
	netSpec, nsErr := stat.GetNetworkStat()

	// App status from the agent's lifecycle Stat (Hooks.Stats carries it for agents that
	// report one; absent => stays at the zero value = Unknown).
	var appStatus float64
	for _, st := range hooks.Stats() {
		if st != nil && st.CdnAppRealtime != nil && st.CdnAppRealtime.Appstat != nil {
			appStatus = float64(*st.CdnAppRealtime.Appstat)
			break
		}
	}
	var rttMicros float64
	if p != nil {
		if rtt := p.RTT(); rtt > 0 {
			rttMicros = float64(rtt.Microseconds())
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if rtErr != nil {
		logger.Warn("dataplane: host realtime stat", "error", rtErr)
	} else {
		m.UpdateHostRealtimeStat(realtime)
	}
	if specErr != nil {
		logger.Warn("dataplane: host spec stat", "error", specErr)
	} else {
		m.UpdateHostSpecStat(spec)
	}
	if physErr != nil {
		logger.Warn("dataplane: host physical stat", "error", physErr)
	} else {
		m.UpdateHostPhysicalStat(phys)
	}
	if netErr != nil {
		logger.Warn("dataplane: network realtime stat", "error", netErr)
	} else {
		m.UpdateNetworkRealtimeStat(netRT)
	}
	if nsErr != nil {
		logger.Warn("dataplane: network spec stat", "error", nsErr)
	} else {
		m.UpdateNetworkSpecStat(netSpec)
	}
	m.AppStatus = appStatus
	m.RTT = rttMicros
}

// dpService is the single DataplaneServiceServer implementation shared by every
// dataplane: the node-local file plane (ListFiles/SendFile/RemoveFile/RemoveDir),
// lifecycle (Start/StopDataplane), the l4lb-style setters (UpdateVip/SyncSecret/
// BindInterface/UpdateMtu) — all delegated to Hooks — and the southbound
// StreamStats reporting path. SetLogStreaming is left Unimplemented (TODO).
type dpService struct {
	pb.UnimplementedDataplaneServiceServer
	hooks   Hooks
	fileDir string
	cn      string
	peer    *peer.Peer
	logger  *slog.Logger
}

var _ pb.DataplaneServiceServer = (*dpService)(nil)

// StreamStats streams a StatBatch every 2s until the control plane closes it. The
// base Stats carries node identity + host realtime (and the latest RTT if a Pong
// has been observed); the Hooks supply any per-dp extra Stats entries.
func (s *dpService) StreamStats(ctx context.Context, _ *wkt.Empty, stream *pb.DataplaneServiceStreamStatsServerStream) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			base := &pbstat.Stats{
				CommonName: s.cn,
				DpType:     s.hooks.DpType(),
			}
			// Realtime host metrics (CPU / mem / load + uptime). On error fall back to
			// uptime-only so the batch still flows.
			if hr, err := stat.GetMachineStat(); err != nil {
				s.logger.Warn("StreamStats: GetMachineStat", "error", err)
				base.HostRealtime = &pbstat.HostRealtimeStat{ServerUptime: int64(stat.ServerUptime())}
			} else {
				base.HostRealtime = hr
			}
			// Report this node's network identity (interface names, MAC/IP) so the
			// control plane can build l4lb destination entries from popcache nodes.
			if netSpec, err := stat.GetNetworkStat(); err != nil {
				s.logger.Warn("StreamStats: GetNetworkStat", "error", err)
			} else {
				base.NetworkSpec = netSpec.ToProto()
			}
			if s.peer != nil {
				if rtt := s.peer.RTT(); rtt > 0 {
					// conn_id is left for the control plane to stamp (it owns the
					// canonical CP-side transport ConnectionID); the dp only knows
					// its own measured RTT here.
					base.Connection = &pbstat.ConnectionStat{Rtt: int64(rtt)}
				}
			}
			stats := append([]*pbstat.Stats{base}, s.hooks.Stats()...)
			if err := stream.Send(&pbstat.StatBatch{Stats: stats}); err != nil {
				return err
			}
		}
	}
}

// StreamLogs subscribes to this node's structured log buffer and streams each
// record to the control plane until the CP closes the stream (the stream lifecycle
// is the on/off toggle — there is no separate enable flag). Records are captured as
// slog.Records, so their structure is preserved on the wire.
func (s *dpService) StreamLogs(ctx context.Context, _ *wkt.Empty, stream *pb.DataplaneServiceStreamLogsServerStream) error {
	ch, cancel := logbuf.Default.Subscribe()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case rec, ok := <-ch:
			if !ok {
				return nil
			}
			attrs := make([]*pb.LogAttr, 0, len(rec.Attrs))
			for _, a := range rec.Attrs {
				attrs = append(attrs, &pb.LogAttr{Key: a.Key, Value: a.Value})
			}
			if err := stream.Send(&pb.LogRecord{
				TimeUnixNano: rec.Time.UnixNano(),
				Level:        rec.Level.String(),
				Message:      rec.Message,
				Attrs:        attrs,
			}); err != nil {
				return err
			}
		}
	}
}

// TailLogs returns the newest matching records from this node's retained log ring,
// filtered here so only matches cross the wire (StreamLogs's bounded companion,
// backing the northbound logs tail action). One shared implementation for every
// agent kind — see agentserve.TailLogbuf.
func (s *dpService) TailLogs(ctx context.Context, req *pb.DataplaneServiceTailLogsRequest) (*pb.DataplaneServiceTailLogsResponse, error) {
	return agentserve.TailLogbuf(req)
}

// SetLogLevel switches this node's live log verbosity. The control plane calls it when
// an admin sets the log_level resource targeting this node; logbuf.LevelVar gates both
// the stderr output and the streamable buffer, so it takes effect immediately.
func (s *dpService) SetLogLevel(ctx context.Context, req *pb.DataplaneServiceSetLogLevelRequest) (*wkt.Empty, error) {
	lvl, err := logbuf.ParseLevel(req.Level)
	if err != nil {
		return nil, err
	}
	logbuf.SetLevel(lvl)
	return &wkt.Empty{}, nil
}

// GetLogLevel reports this node's current live log level.
func (s *dpService) GetLogLevel(ctx context.Context, _ *wkt.Empty) (*pb.DataplaneServiceGetLogLevelResponse, error) {
	return &pb.DataplaneServiceGetLogLevelResponse{Level: logbuf.GetLevel().String()}, nil
}

// balancerObjectSetter is the optional l4lb capability behind SetBalancerObject — the l4lb
// controller implements it; other dp kinds (popcache/dns/router) do not.
type balancerObjectSetter interface {
	SetBalancerObject(binPath, cryptoBin, pinDir string) error
}

// SetBalancerObject configures which eBPF objects an l4lb node loads (the l4lb_object
// resource). The object/crypto names are resolved against this node's file store (where
// node_file / dp-file SendFile lands them); pin_dir is used as-is. Errors on non-l4lb nodes.
func (s *dpService) SetBalancerObject(ctx context.Context, req *pb.DataplaneServiceSetBalancerObjectRequest) (*wkt.Empty, error) {
	setter, ok := s.hooks.(balancerObjectSetter)
	if !ok {
		return nil, fmt.Errorf("this node does not support balancer objects")
	}
	binPath := req.Object
	if binPath != "" {
		binPath = filepath.Join(s.fileDir, req.Object)
	}
	cryptoBin := req.CryptoObject
	if cryptoBin != "" {
		cryptoBin = filepath.Join(s.fileDir, req.CryptoObject)
	}
	if err := setter.SetBalancerObject(binPath, cryptoBin, req.PinDir); err != nil {
		return nil, err
	}
	return &wkt.Empty{}, nil
}

// GetTransferState returns a representative stub; real trsf state is a TODO.
// TODO(kscale): read the live trsf internal state from the transport.
func (s *dpService) GetTransferState(ctx context.Context, _ *wkt.Empty) (*pb.DataplaneServiceGetTransferStateResponse, error) {
	if s.peer == nil || s.peer.Streams() == nil {
		return nil, fmt.Errorf("dataplane: no transport streams available")
	}
	is := s.peer.Streams().GetInternalState()
	var sent []*pb.SentPacket
	for _, p := range is.SentPackets {
		sent = append(sent, &pb.SentPacket{
			PacketType: p.Kind,
			StreamId:   p.StreamID,
			SentAt:     p.SentTime,
			Size:       uint64(p.PacketSize),
			IsMtuProbe: p.IsMTUProbe,
		})
	}
	return &pb.DataplaneServiceGetTransferStateResponse{State: &pb.TransferInternalState{
		ActiveSendStreams:    uint64(is.ActiveSendStreams),
		ActiveReceiveStreams: uint64(is.ActiveReceiveStreams),
		CurrentMtu:           uint64(is.CurrentMTU),
		SendQueueLength:      uint64(is.SendQueueLength),
		ReceiveQueueLength:   uint64(is.ReceiveQueueLength),
		SendActionCount:      uint64(is.SendActionCount),
		UpdateWindowCount:    uint64(is.UpdateWindowCount),
		CancelStreamCount:    uint64(is.CancelStreamCount),
		BytesInFlight:        uint64(is.BytesInFlight),
		CongestionWindow:     uint64(is.CongestionWindow),
		SmoothedRtt:          is.SmoothedRTT,
		RttVariance:          is.RTTVariance,
		SentPackets:          sent,
	}}, nil
}

func (s *dpService) ListFiles(ctx context.Context, req *pb.DataplaneServiceListFilesRequest) (*pb.DataplaneServiceListFilesResponse, error) {
	dir := filepath.Join(s.fileDir, filepath.Clean("/"+req.Dir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return &pb.DataplaneServiceListFilesResponse{}, nil
		}
		return nil, err
	}
	resp := &pb.DataplaneServiceListFilesResponse{}
	for _, e := range entries {
		info, ierr := e.Info()
		fi := &pb.FileInfo{Name: e.Name()}
		if ierr == nil {
			fi.Size = info.Size()
			fi.ModifiedAt = info.ModTime()
			fi.Mode = info.Mode()
		}
		resp.Files = append(resp.Files, fi)
	}
	return resp, nil
}

func (s *dpService) SendFile(ctx context.Context, req *pb.DataplaneServiceSendFileRequest) (*wkt.Empty, error) {
	name := filepath.Base(req.SaveAs)
	if err := os.WriteFile(filepath.Join(s.fileDir, name), req.Content, 0o644); err != nil {
		return nil, err
	}
	s.logger.Info("dataplane: SendFile received", "name", name, "bytes", len(req.Content))
	if r, ok := s.hooks.(FileReceiver); ok {
		r.OnFileReceived(name)
	}
	return &wkt.Empty{}, nil
}

func (s *dpService) RemoveFile(ctx context.Context, req *pb.DataplaneServiceRemoveFileRequest) (*wkt.Empty, error) {
	if err := os.Remove(filepath.Join(s.fileDir, filepath.Base(req.File))); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return &wkt.Empty{}, nil
}

func (s *dpService) RemoveDir(ctx context.Context, req *pb.DataplaneServiceRemoveDirRequest) (*wkt.Empty, error) {
	dir := filepath.Join(s.fileDir, filepath.Clean("/"+req.Dir))
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	return &wkt.Empty{}, nil
}

func (s *dpService) StartDataplane(ctx context.Context, _ *wkt.Empty) (*wkt.Empty, error) {
	return &wkt.Empty{}, s.hooks.Start(ctx)
}

func (s *dpService) StopDataplane(ctx context.Context, _ *wkt.Empty) (*wkt.Empty, error) {
	return &wkt.Empty{}, s.hooks.Stop()
}

func (s *dpService) UpdateVip(ctx context.Context, req *pb.DataplaneServiceUpdateVipRequest) (*wkt.Empty, error) {
	vip, err := netip.ParseAddr(req.Vip)
	if err != nil {
		return nil, fmt.Errorf("dataplane: invalid vip %q: %w", req.Vip, err)
	}
	return &wkt.Empty{}, s.hooks.UpdateVip(vip, req.IcmpEcho)
}

func (s *dpService) UpdateDestinations(ctx context.Context, req *pb.DataplaneServiceUpdateDestinationsRequest) (*wkt.Empty, error) {
	dests := make([]stat.DestEntry, 0, len(req.Dests))
	for _, d := range req.Dests {
		ip, err := netip.ParseAddr(d.IpAddr)
		if err != nil {
			return nil, fmt.Errorf("dataplane: invalid dest ip %q: %w", d.IpAddr, err)
		}
		dests = append(dests, stat.DestEntry{
			ServerID:     d.ServerId,
			HardwareAddr: net.HardwareAddr(d.HardwareAddr),
			IPAddr:       ip,
		})
	}
	return &wkt.Empty{}, s.hooks.UpdateDestinations(dests)
}

func (s *dpService) UpdateRemote(ctx context.Context, req *pb.DataplaneServiceUpdateRemoteRequest) (*wkt.Empty, error) {
	remotes := make([]stat.DestEntry, 0, len(req.Remotes))
	for _, d := range req.Remotes {
		ip, err := netip.ParseAddr(d.IpAddr)
		if err != nil {
			return nil, fmt.Errorf("dataplane: invalid remote ip %q: %w", d.IpAddr, err)
		}
		remotes = append(remotes, stat.DestEntry{
			ServerID:     d.ServerId,
			HardwareAddr: net.HardwareAddr(d.HardwareAddr),
			IPAddr:       ip,
		})
	}
	return &wkt.Empty{}, s.hooks.UpdateRemote(remotes)
}

func (s *dpService) SyncSecret(ctx context.Context, req *pb.DataplaneServiceSyncSecretRequest) (*wkt.Empty, error) {
	return &wkt.Empty{}, s.hooks.SyncSecret(req.SharedSecret)
}

func (s *dpService) BindInterface(ctx context.Context, req *pb.DataplaneServiceBindInterfaceRequest) (*wkt.Empty, error) {
	return &wkt.Empty{}, s.hooks.BindInterface(req.Interface)
}

func (s *dpService) UpdateMtu(ctx context.Context, req *pb.DataplaneServiceUpdateMtuRequest) (*wkt.Empty, error) {
	var mtu int
	if _, err := fmt.Sscanf(req.Mtu, "%d", &mtu); err != nil {
		return nil, fmt.Errorf("dataplane: invalid mtu %q: %w", req.Mtu, err)
	}
	return &wkt.Empty{}, s.hooks.UpdateMtu(uint16(mtu))
}
