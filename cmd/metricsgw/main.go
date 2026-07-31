// Command metricsgw is the kscale metrics gateway: an authenticated transport
// client that re-exposes the whole fleet's prometheus metrics on one plain HTTP
// /metrics for an external prometheus. On each scrape it discovers the dataplane
// nodes (DataplaneNodeService.List) and pulls each node's /metrics — plus the
// control plane's own — over the ENCRYPTED transport via StatsService.ScrapeMetrics,
// then merges the expositions (deduping HELP/TYPE). The single plaintext surface is
// this gateway, meant to sit next to prometheus (loopback); the control plane and
// dataplane nodes expose no plain metrics port.
//
// ksdk did this with cmd/httpproxy's reverse-connection tunnel (a docker-NAT hack);
// kscale needs only the clean reverse-proxy-over-transport core, which the existing
// StatsService.ScrapeMetrics already is — the gateway just fans it out + merges.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/on-keyday/kscale/agentserve"
	"github.com/on-keyday/kscale/ca"
	"github.com/on-keyday/kscale/client"
	"github.com/on-keyday/kscale/internal/demo"
	"github.com/on-keyday/kscale/logbuf"
	"github.com/on-keyday/kscale/peer"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/objtrsf/objproto"
)

// selfMetricsTarget mirrors service/stats.SelfMetricsTarget: the ScrapeMetrics
// common_name that returns the CP's own metrics rather than a node's.
const selfMetricsTarget = "controlplane"

func main() {
	logger := logbuf.NewStderrLogger(os.Stderr, slog.LevelInfo)
	fs := flag.NewFlagSet("metricsgw", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:9443", "control plane address: udp:host:port-id / ws:host:port-id / wss:... / bare host:port (=udp). ws/wss lets it reach an SSH-tunnelled CP from outside the lab")
	dataDir := fs.String("data", "/tmp/kscale-ca", "dir holding bootstrap token / saved cert")
	role := fs.String("role", "admin", "identity role (must be allowed stats scrape-metrics + node list)")
	listen := fs.String("listen", ":9091", "plain HTTP /metrics listen address (for prometheus)")
	domainFlag := fs.String("ca-domain", demo.Domain, "CA domain — must match the control plane's --ca-domain")
	commonNameFlag := fs.String("common-name", "", "cert CommonName to enroll as (default <role>.manager.ca.admin.<ca-domain>)")
	renewInterval := fs.Duration("renew-interval", client.DefaultCertRenewInterval, "how often to renew the enrolled cert (update-cert; the bootstrap token is single-use, so without renewal the cert eventually expires and can't be re-enrolled)")
	_ = fs.Parse(os.Args[1:])
	demo.Domain = *domainFlag

	// client.Endpoint parses the transport out of the address (udp / ws / wss, or a
	// bare host:port = udp) — the same ws-aware path the cli/katui use, so the gateway
	// can run next to prometheus OUTSIDE the lab and reach a non-routable-UDP CP over an
	// SSH-forwarded ws port (e.g. --addr ws:127.0.0.1:9444-*), not just intra-lab UDP.
	cid, ep, err := client.Endpoint(logger, *addr)
	if err != nil {
		logger.Error("bad addr", "error", err)
		os.Exit(1)
	}
	// connCtx is the lifetime of the persistent CP connection — process-scoped, NOT tied to
	// any single /metrics request. peer.WrapAcceptedConn binds the connection's read/ping
	// goroutines to the context it's given, so using a per-request context here would tear
	// the connection down the instant each scrape's HTTP request completes (the next scrape
	// then finds a dead cached connection → 503, reconnects → 200, dies again — a 200/503
	// flap that riddles the dashboards with gaps). Long-lived daemons (cmd/nodewatch) pass a
	// process-scoped context for exactly this reason.
	gw := &gateway{ep: ep, cid: cid, dataDir: *dataDir, role: *role, commonName: *commonNameFlag, logger: logger, holder: client.NewBootHolder(nil), renewInterval: *renewInterval, connCtx: context.Background()}

	// Establish the CP connection BEFORE listening, retrying until it succeeds. metricsgw
	// otherwise connects lazily inside handleMetrics, but the very first connect does the
	// single-use bootstrap enroll plus a cold fleet fan-out, which takes longer than
	// prometheus's scrape_timeout (< scrape_interval, pinned small so the ported dashboards'
	// rate() windows work). Each scrape then cancelled that first connect before it finished —
	// consuming the one-shot token without ever saving the cert — so metricsgw wedged in an
	// "invalid bootstrap token" retry loop and never came up. Doing (and confirming) the first
	// connect here means the token is spent exactly once, off the scrape path, and prometheus
	// only ever reaches a ready gateway: the first scrape hits a warm persistent connection
	// (~1s fan-out) and every later reconnect reuses the saved cert (no token, fast).
	for attempt := 1; ; attempt++ {
		if _, _, err := gw.clients(); err != nil {
			logger.Warn("metricsgw: waiting for control plane connection", "attempt", attempt, "error", err)
			time.Sleep(3 * time.Second)
			continue
		}
		logger.Info("metrics gateway connected to control plane", "cp", *addr, "attempts", attempt)
		break
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", gw.handleMetrics)
	logger.Info("metrics gateway serving", "listen", *listen, "cp", *addr)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		logger.Error("listen", "error", err)
		os.Exit(1)
	}
}

type gateway struct {
	ep         objproto.Endpoint
	cid        objproto.ConnectionID
	dataDir    string
	role       string
	commonName string
	logger     *slog.Logger
	connCtx    context.Context // process-lifetime context for the persistent CP connection

	mu            sync.Mutex
	p             *peer.Peer
	stats         pb.StatsServiceClient
	nodes         pb.DataplaneNodeServiceClient
	holder        *client.BootHolder // latest cert, shared with the renewal loop
	renewOnce     sync.Once          // starts RenewCertLoop after the first enroll
	renewInterval time.Duration
}

// clients returns live typed clients, (re)connecting to the control plane if the
// current connection is absent.
func (g *gateway) clients() (pb.StatsServiceClient, pb.DataplaneNodeServiceClient, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stats == nil {
		// Fresh random connection id per (re)connect — g.cid (from client.Endpoint) is only
		// a transport/addr template. Reusing one fixed id wedges reconnects: the CP still
		// holds the prior connection for that id until keepalive expires, so the new one
		// fails with "connection already exists for handshake". (The dataplane substrate
		// uses a fresh id per reconnect for the same reason; the renewal loop already does.)
		cid, err := objproto.NewRandomConnectionID(g.cid.Transport, g.cid.Addr)
		if err != nil {
			return nil, nil, fmt.Errorf("connection id: %w", err)
		}
		boot := g.holder.Get()
		if boot == nil {
			// First connect: enroll, then start cert auto-renewal once. Later reconnects
			// reuse holder.Get(), which the renewal loop keeps fresh — without it the
			// enrolled cert would expire (~the mint's expires_period) and, since the
			// bootstrap token is single-use, metricsgw could never re-enroll.
			b, cn, savePath, err := enroll(g.connCtx, g.ep, cid, g.dataDir, g.role, g.commonName)
			if err != nil {
				return nil, nil, fmt.Errorf("enroll: %w", err)
			}
			boot = b
			g.holder.Set(boot)
			g.renewOnce.Do(func() {
				go client.RenewCertLoop(context.Background(), g.ep, g.cid, cn, g.role, savePath, g.renewInterval, g.holder, g.logger)
			})
		}
		p, src, err := client.Connect(g.connCtx, g.ep, cid, g.role, boot, demo.PingInterval, g.logger)
		if err != nil {
			return nil, nil, fmt.Errorf("connect: %w", err)
		}
		// Answer the control plane's log RPCs (StreamLogs / TailLogs / log levels)
		// over this peer while it lives, like every other client agent. Without
		// this, metricsgw was a registered agent that never dispatched incoming
		// RPCs, so every CP-side logs fan-out (tail, set-level "all") dangled into
		// it until the per-source timeout — observed as `logs tail` always burning
		// the full 5s with "sources unavailable: metricsgw.manager". Serve pins
		// itself to the peer's connection context, so it ends with the connection.
		go agentserve.Serve(g.connCtx, p, g.logger)
		g.p = p
		g.stats = pb.NewStatsServiceClient(src)
		g.nodes = pb.NewDataplaneNodeServiceClient(src)
	}
	return g.stats, g.nodes, nil
}

// reset drops the connection so the next scrape reconnects (after an RPC error).
func (g *gateway) reset() {
	g.mu.Lock()
	if g.p != nil {
		g.p.Connection().Close()
	}
	g.p, g.stats, g.nodes = nil, nil, nil
	g.mu.Unlock()
}

func (g *gateway) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// The persistent connection is established under g.connCtx (process lifetime). The
	// request ctx below is used only for the per-scrape RPC deadlines (nodes.List /
	// ScrapeMetrics), so a slow/cancelled scrape bounds those calls without killing the
	// shared connection.
	stats, nodes, err := g.clients()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	nl, err := nodes.List(ctx, &pbaccess.ResourceDataplaneNodeActionListArgsDTO{})
	if err != nil {
		g.reset()
		http.Error(w, "node list: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	var parts []string
	// CP's own metrics (drift etc.) first, then every dataplane node.
	targets := []string{selfMetricsTarget}
	for _, n := range nl.Items {
		targets = append(targets, n.CommonName)
	}
	for _, cn := range targets {
		resp, err := stats.ScrapeMetrics(ctx, &pbaccess.ResourceStatsActionScrapeMetricsArgsDTO{CommonName: cn})
		if err != nil {
			g.logger.Warn("metricsgw: scrape failed", "target", cn, "error", err)
			continue
		}
		if resp.Metrics != "" {
			parts = append(parts, resp.Metrics)
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	io.WriteString(w, mergeExposition(parts))
}

// mergeExposition concatenates per-target exposition text, keeping each metric's
// "# HELP"/"# TYPE" line only once (duplicates across targets are a parse error in
// prometheus). Sample lines carry distinct instance/node labels so they don't
// collide.
func mergeExposition(parts []string) string {
	var b strings.Builder
	seenHelp, seenType := map[string]bool{}, map[string]bool{}
	for _, p := range parts {
		for _, line := range strings.Split(p, "\n") {
			switch {
			case strings.HasPrefix(line, "# HELP "):
				if name := metricName(line, "# HELP "); seenHelp[name] {
					continue
				} else {
					seenHelp[name] = true
				}
			case strings.HasPrefix(line, "# TYPE "):
				if name := metricName(line, "# TYPE "); seenType[name] {
					continue
				} else {
					seenType[name] = true
				}
			case line == "":
				continue
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func metricName(line, prefix string) string {
	rest := strings.TrimPrefix(line, prefix)
	if i := strings.IndexByte(rest, ' '); i >= 0 {
		return rest[:i]
	}
	return rest
}

// enroll mirrors cmd/cli: resolve the identity's CN + bootstrap token (read only when no
// cached cert exists) and delegate to client.Enroll. Also returns the resolved CN and
// the cert save path so the caller can drive cert auto-renewal (client.RenewCertLoop).
func enroll(ctx context.Context, ep objproto.Endpoint, cid objproto.ConnectionID, dataDir, role, commonName string) (boot *ca.BootstrapInfo, cn, savePath string, err error) {
	savePath = client.ClientCertPath(dataDir, role)
	var token []byte
	if _, statErr := os.Stat(savePath); os.IsNotExist(statErr) {
		t, readErr := os.ReadFile(filepath.Join(dataDir, "bootstrap."+role+".token"))
		if readErr != nil {
			return nil, "", "", fmt.Errorf("read %s bootstrap token: %w", role, readErr)
		}
		token = t
	}
	cn = commonName
	if cn == "" {
		cn = demo.ClientCN(role)
	}
	boot, err = client.Enroll(ctx, ep, cid, cn, token, role, savePath)
	return boot, cn, savePath, err
}
