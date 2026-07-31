// Package stats holds the control plane's per-node stat cache and the business
// behind StatsService. The cache is fed by the southbound StreamStats reporting
// path (a collector opens StreamStats on each node and stores the latest batch);
// get/watch read the cache, and transfer_state is a direct GetTransferState pull
// kept for debugging.
package stats

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/on-keyday/kscale/dpbroker"
	"github.com/on-keyday/kscale/peer"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	pbstat "github.com/on-keyday/kscale/protobuf/proto/stat"
	"github.com/on-keyday/kscale/protobuf/wkt"
	"github.com/on-keyday/kscale/rpc"
	"github.com/on-keyday/kscale/trsf/proxy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

// Cache holds the latest reported StatBatch per node (by common name).
type Cache struct {
	mu     sync.Mutex
	latest map[string]*pbstat.StatBatch
}

func NewCache() *Cache { return &Cache{latest: map[string]*pbstat.StatBatch{}} }

func (c *Cache) Set(commonName string, b *pbstat.StatBatch) {
	c.mu.Lock()
	c.latest[commonName] = b
	c.mu.Unlock()
}

// Delete drops a node's cached stats (called when it disconnects so the cache
// does not accumulate dead nodes).
func (c *Cache) Delete(commonName string) {
	c.mu.Lock()
	delete(c.latest, commonName)
	c.mu.Unlock()
}

// Get returns one node's latest batch (empty if none reported yet).
func (c *Cache) Get(commonName string) *pbstat.StatBatch {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b := c.latest[commonName]; b != nil {
		return b
	}
	return &pbstat.StatBatch{}
}

// All merges every node's latest batch into one (sorted by node for stability).
func (c *Cache) All() *pbstat.StatBatch {
	c.mu.Lock()
	defer c.mu.Unlock()
	names := make([]string, 0, len(c.latest))
	for n := range c.latest {
		names = append(names, n)
	}
	sort.Strings(names)
	out := &pbstat.StatBatch{}
	for _, n := range names {
		if b := c.latest[n]; b != nil {
			out.Stats = append(out.Stats, b.Stats...)
		}
	}
	return out
}

type Handlers struct {
	pb.UnimplementedStatsServiceServer
	Cache  *Cache
	Broker *dpbroker.Broker
	Logger *slog.Logger
	// SelfGatherer, if set, is the control plane's OWN prometheus registry (drift
	// gauge etc.). ScrapeMetrics returns it for the sentinel common_name
	// "controlplane", so the metrics gateway can pull CP metrics over the same
	// authenticated transport — the CP needs no plain metrics port.
	SelfGatherer prometheus.Gatherer
}

// SelfMetricsTarget is the ScrapeMetrics common_name that returns the CP's own
// metrics (rather than a dataplane node's).
const SelfMetricsTarget = "controlplane"

var _ pb.StatsServiceServer = (*Handlers)(nil)

func (h *Handlers) snapshot(commonName string) *pbstat.StatBatch {
	if commonName == "" || commonName == "*" {
		return h.Cache.All()
	}
	return h.Cache.Get(commonName)
}

func (h *Handlers) Get(ctx context.Context, req *pbaccess.ResourceStatsActionGetArgsDTO) (*pbstat.StatBatch, error) {
	return h.snapshot(req.CommonName), nil
}

func (h *Handlers) Watch(ctx context.Context, req *pbaccess.ResourceStatsActionWatchArgsDTO, stream *pb.StatsServiceWatchServerStream) error {
	interval := req.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := stream.Send(h.snapshot(req.CommonName)); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// ScrapeMetrics dials the node's prometheus /metrics over the encrypted transport
// (a StreamMagicHTTP proxy stream routed to the node's metrics listener) and
// returns the exposition text — the control-plane side of /metrics-over-transport.
func (h *Handlers) ScrapeMetrics(ctx context.Context, req *pbaccess.ResourceStatsActionScrapeMetricsArgsDTO) (*pbaccess.ResourceStatsActionScrapeMetricsResponseDTO, error) {
	if req.CommonName == SelfMetricsTarget {
		text, err := gatherToText(h.SelfGatherer)
		if err != nil {
			return nil, err
		}
		return &pbaccess.ResourceStatsActionScrapeMetricsResponseDTO{Metrics: text}, nil
	}
	peers, err := h.Broker.ResolveAll(req.CommonName)
	if err != nil {
		return nil, err
	}
	multi := len(peers) > 1
	var out strings.Builder
	for _, p := range peers {
		text, err := scrapeOverTransport(ctx, p)
		if err != nil {
			h.Logger.Error("scrape-metrics", "node", p.CommonName(), "error", err)
			continue
		}
		if multi { // label each node's block so a fan-out scrape is readable
			fmt.Fprintf(&out, "# node: %s\n", p.CommonName())
		}
		out.WriteString(text)
		if multi && !strings.HasSuffix(text, "\n") {
			out.WriteByte('\n')
		}
	}
	return &pbaccess.ResourceStatsActionScrapeMetricsResponseDTO{Metrics: out.String()}, nil
}

// scrapeOverTransport dials one node's /metrics over the encrypted transport (the
// StreamMagicHTTP proxy) and returns its exposition text.
func scrapeOverTransport(ctx context.Context, p *peer.Peer) (string, error) {
	dialer := proxy.NewDialerWrapper(ctx, p.Streams())
	conn, err := dialer.DialContext(ctx, "tcp", "/metrics")
	if err != nil {
		return "", fmt.Errorf("dial metrics: %w", err)
	}
	defer conn.Close()
	httpReq, err := http.NewRequestWithContext(ctx, "GET", "http://node/metrics", nil)
	if err != nil {
		return "", err
	}
	if err := httpReq.Write(conn); err != nil {
		return "", fmt.Errorf("write metrics request: %w", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), httpReq)
	if err != nil {
		return "", fmt.Errorf("read metrics response: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// gatherToText renders a prometheus Gatherer to text-format exposition.
func gatherToText(g prometheus.Gatherer) (string, error) {
	if g == nil {
		return "", nil
	}
	mfs, err := g.Gather()
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			return "", err
		}
	}
	return buf.String(), nil
}

func (h *Handlers) TransferState(ctx context.Context, req *pbaccess.ResourceStatsActionTransferStateArgsDTO) (*pbaccess.ResourceStatsActionTransferStateResponseDTO, error) {
	peers, err := h.Broker.ResolveAll(req.CommonName)
	if err != nil {
		return nil, err
	}
	var out strings.Builder
	for _, p := range peers {
		fmt.Fprintf(&out, "=== %s ===\n", p.CommonName())
		c := pb.NewDataplaneServiceClient(rpc.NewTrsfStreamSource(p.Streams(), h.Logger))
		resp, err := c.GetTransferState(ctx, &wkt.Empty{})
		if err != nil {
			fmt.Fprintf(&out, "error: %v\n", err)
			continue
		}
		fmt.Fprintf(&out, "%+v\n", resp.State)
	}
	return &pbaccess.ResourceStatsActionTransferStateResponseDTO{Result: out.String()}, nil
}
