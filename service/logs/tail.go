package logs

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/on-keyday/kscale/agentserve"
	"github.com/on-keyday/kscale/internal/safe"
	"github.com/on-keyday/kscale/peer"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/kscale/rpc"
)

const (
	tailDefaultLines = 50
	tailMaxLines     = 200 // one logbuf ring's worth — more can't exist per source
	// tailPerSourceTimeout bounds one node's TailLogs call so a wedged node can't
	// stall the whole fan-in (the healthy sources still answer).
	tailPerSourceTimeout = 5 * time.Second
)

// Tail answers the bounded northbound log query: fan the node-side TailLogs out to
// the selected sources (selector "" or "*" = control plane + every dataplane node +
// every client agent; "cp" = the control plane alone; else a node CN / "<type>/*" /
// an agent), merge by timestamp, and return the newest `lines` formatted entries
// NEWEST FIRST — so a downstream size cap (the monitor loop truncates tool results
// head-first) drops the oldest lines, never the newest. The note discloses coverage
// honestly: what was shown, what each source retains, and any source that failed.
func (h *Handlers) Tail(ctx context.Context, req *pbaccess.ResourceLogsActionTailArgsDTO) (*pbaccess.ResourceLogsActionTailResponseDTO, error) {
	lines := int(req.Lines)
	if lines <= 0 {
		lines = tailDefaultLines
	}
	if lines > tailMaxLines {
		lines = tailMaxLines
	}
	nreq := &pb.DataplaneServiceTailLogsRequest{Level: req.Level, Contains: req.Contains, Lines: int32(lines)}

	type source struct {
		label string
		p     *peer.Peer // nil = the control plane's own logbuf
	}
	var sources []source
	switch sel := req.CommonName; sel {
	case "cp":
		sources = []source{{label: "cp"}}
	case "", "*":
		sources = []source{{label: "cp"}}
		if peers, err := h.Broker.ResolveAll("*"); err == nil {
			for _, p := range peers {
				sources = append(sources, source{label: nodeLabelOf(p.CommonName()), p: p})
			}
		}
		if h.ListAgents != nil {
			for _, p := range h.ListAgents() {
				sources = append(sources, source{label: nodeLabelOf(p.CommonName()), p: p})
			}
		}
	default:
		peers, err := h.Broker.ResolveAll(sel)
		if err != nil {
			if h.ResolveAgent != nil {
				if p, ok := h.ResolveAgent(sel); ok {
					peers, err = []*peer.Peer{p}, nil
				}
			}
			if err != nil {
				return nil, err
			}
		}
		for _, p := range peers {
			sources = append(sources, source{label: nodeLabelOf(p.CommonName()), p: p})
		}
	}

	type tagged struct {
		label string
		rec   *pb.LogRecord
	}
	var (
		mu     sync.Mutex
		merged []tagged
		failed []string
	)
	var wg sync.WaitGroup
	for _, src := range sources {
		wg.Add(1)
		go func(src source) {
			defer wg.Done()
			defer safe.Recover(h.Logger, "logs-tail-fanin")
			var (
				resp *pb.DataplaneServiceTailLogsResponse
				err  error
			)
			if src.p == nil {
				resp, err = agentserve.TailLogbuf(nreq)
			} else {
				cctx, cancel := context.WithTimeout(ctx, tailPerSourceTimeout)
				defer cancel()
				c := pb.NewDataplaneServiceClient(rpc.NewTrsfStreamSource(src.p.Streams(), h.Logger))
				resp, err = c.TailLogs(cctx, nreq)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				h.Logger.Error("logs tail: source failed", "source", src.label, "error", err)
				failed = append(failed, src.label)
				return
			}
			for _, rec := range resp.Records {
				merged = append(merged, tagged{label: src.label, rec: rec})
			}
		}(src)
	}
	wg.Wait()

	// Newest first overall, capped to lines.
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].rec.TimeUnixNano > merged[j].rec.TimeUnixNano })
	if len(merged) > lines {
		merged = merged[:lines]
	}
	entries := make([]string, 0, len(merged))
	for _, t := range merged {
		entries = append(entries, formatTailEntry(t.label, t.rec))
	}

	note := fmt.Sprintf("%d matching lines from %d sources, NEWEST FIRST (capped at %d; each source retains only its ~%d most recent lines — narrow with level/contains/common_name rather than raising lines)",
		len(entries), len(sources), lines, tailMaxLines)
	if len(failed) > 0 {
		sort.Strings(failed)
		note += "; sources unavailable: " + strings.Join(failed, ", ")
	}
	return &pbaccess.ResourceLogsActionTailResponseDTO{Note: note, Entries: entries}, nil
}

// formatTailEntry renders one record as a single line: source tag, RFC3339
// timestamp, level, message, then the structured attrs as key=value.
func formatTailEntry(label string, rec *pb.LogRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s %s %s", label, time.Unix(0, rec.TimeUnixNano).Format(time.RFC3339), rec.Level, rec.Message)
	for _, a := range rec.Attrs {
		fmt.Fprintf(&b, " %s=%s", a.Key, a.Value)
	}
	return b.String()
}
