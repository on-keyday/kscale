// Package logs holds the hand-written business behind LogsService: stream
// structured logs from a dataplane node (relayed over the node's DataplaneService
// StreamLogs) or — when common_name is empty — the control plane's own logs (read
// straight from this process's logbuf). The streaming lifecycle is the on/off
// toggle: the stream stays open until the admin disconnects or the node drops.
package logs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/on-keyday/kscale/dpbroker"
	"github.com/on-keyday/kscale/internal/safe"
	"github.com/on-keyday/kscale/logbuf"
	"github.com/on-keyday/kscale/peer"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/kscale/protobuf/wkt"
	"github.com/on-keyday/kscale/rpc"
)

type Handlers struct {
	pb.UnimplementedLogsServiceServer
	Broker *dpbroker.Broker
	Logger *slog.Logger
	// ResolveAgent resolves a client-side agent (e.g. the monitor / nodewatch) that
	// serves its own logs, for selectors that aren't dataplane nodes. Optional.
	ResolveAgent func(selector string) (*peer.Peer, bool)
	// ListAgents enumerates every client agent, for fanning set/get-level to the
	// monitor agents on a node="all" selector. Optional.
	ListAgents func() []*peer.Peer
}

var _ pb.LogsServiceServer = (*Handlers)(nil)

func (h *Handlers) Stream(ctx context.Context, req *pbaccess.ResourceLogsActionStreamArgsDTO, stream *pb.LogsServiceStreamServerStream) error {
	if req.CommonName == "" {
		return streamLocal(ctx, stream)
	}
	peers, err := h.Broker.ResolveAll(req.CommonName)
	if err != nil {
		// Not a dataplane node — maybe a client agent (the monitor) serving its own logs.
		if h.ResolveAgent != nil {
			if p, ok := h.ResolveAgent(req.CommonName); ok {
				return relayOne(ctx, p, "", stream, h.Logger)
			}
		}
		return err
	}
	if len(peers) == 1 {
		return relayOne(ctx, peers[0], "", stream, h.Logger)
	}
	// Fan-out: merge every matched node's StreamLogs into this one stream in
	// TIMESTAMP order (see merge.go — each node replays a backlog burst first,
	// so arrival order interleaves whole histories; the k-way merge with a
	// watermark re-serializes them). Records are tagged with a "node" attr so
	// the admin can tell sources apart. A node dropping just finishes its merge
	// slot; the stream closes when every node's stream has ended and the buffer
	// is drained (or the admin disconnects / cancels).
	type mergeEvent struct {
		id  int
		rec *pb.LogRecord // nil = this source ended
	}
	events := make(chan mergeEvent, 256)
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for i, p := range peers {
		go func(id int, p *peer.Peer) {
			defer safe.Recover(h.Logger, "logs-fanout")
			label := nodeLabelOf(p.CommonName())
			// The end-of-source event must reach the merge loop even on panic,
			// or the merge would wait on this slot forever.
			defer func() {
				select {
				case events <- mergeEvent{id: id}:
				case <-streamCtx.Done():
				}
			}()
			c := pb.NewDataplaneServiceClient(rpc.NewTrsfStreamSource(p.Streams(), h.Logger))
			src, err := c.StreamLogs(streamCtx, &wkt.Empty{})
			if err != nil {
				h.Logger.Error("logs fan-out: open node stream", "node", p.CommonName(), "error", err)
				return
			}
			for {
				rec, err := src.Recv(streamCtx)
				if err != nil {
					return // node ended / ctx cancelled
				}
				rec.Attrs = append(rec.Attrs, &pb.LogAttr{Key: "node", Value: label})
				select {
				case events <- mergeEvent{id: id, rec: rec}:
				case <-streamCtx.Done():
					return
				}
			}
		}(i, p)
	}
	mb := newMergeBuffer(len(peers), streamWatermark)
	// The ticker drives watermark expiry (an idle source stops blocking after
	// streamWatermark); events drive everything else. Coarse on purpose —
	// eligibility is re-checked after every event anyway.
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e := <-events:
			if e.rec == nil {
				mb.finish(e.id)
			} else {
				mb.push(e.id, e.rec, time.Now())
			}
		case <-tick.C:
		}
		for {
			rec := mb.pop(time.Now())
			if rec == nil {
				break
			}
			if err := stream.Send(rec); err != nil {
				return err // admin disconnected
			}
		}
		if mb.drained() {
			return nil // every node stream ended and everything was emitted
		}
	}
}

// relayOne pipes a single node's StreamLogs to the admin (a direct pass-through — the
// node's records are already pb.LogRecord). If label != "", each record is tagged with a
// "node" attr (used by the fan-out path; empty for a single explicit node).
func relayOne(ctx context.Context, p *peer.Peer, label string, stream *pb.LogsServiceStreamServerStream, logger *slog.Logger) error {
	c := pb.NewDataplaneServiceClient(rpc.NewTrsfStreamSource(p.Streams(), logger))
	src, err := c.StreamLogs(ctx, &wkt.Empty{})
	if err != nil {
		return fmt.Errorf("open node log stream: %w", err)
	}
	for {
		rec, err := src.Recv(ctx)
		if err != nil {
			return err // node stream ended / node gone
		}
		if label != "" {
			rec.Attrs = append(rec.Attrs, &pb.LogAttr{Key: "node", Value: label})
		}
		if err := stream.Send(rec); err != nil {
			return err // admin disconnected
		}
	}
}

// nodeLabelOf is the short node name (the CommonName up to the first dot).
// nodeLabelOf returns "<node>.<dp_type>" (the first two CN segments), e.g.
// "s3.l4lb.dp.system.kscale.local" -> "s3.l4lb", so co-located nodes that share a host
// (s3.l4lb / s3.dns / s3.router) are distinguishable in the fan-out's "node" tag.
func nodeLabelOf(cn string) string {
	parts := strings.SplitN(cn, ".", 3)
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return cn
}

// streamLocal streams this (control-plane) process's own structured logs from its
// logbuf, converting each captured record to the wire form.
func streamLocal(ctx context.Context, stream *pb.LogsServiceStreamServerStream) error {
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
