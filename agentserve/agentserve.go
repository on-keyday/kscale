// Package agentserve lets a client-side agent (nodewatch, metricsgw, …) serve its own
// logs back over its existing peer connection, so the control plane can relay them via
// `logs stream <agent>` exactly like a dataplane node — without the agent becoming a
// dataplane node (it stays a pure client, just additionally answering StreamLogs).
//
// It mirrors the dataplane substrate's serve loop: register a DataplaneService whose only
// real method is StreamLogs (from this process's logbuf), then accept incoming RPC
// streams on the peer and dispatch them. The control plane only ever calls StreamLogs on
// a client agent; every other DataplaneService method is the Unimplemented default.
package agentserve

import (
	"context"
	"log/slog"

	"github.com/on-keyday/kscale/logbuf"
	"github.com/on-keyday/kscale/peer"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	"github.com/on-keyday/kscale/protobuf/wkt"
	"github.com/on-keyday/kscale/rpc"
	"github.com/on-keyday/objtrsf/trsf"
)

type logServer struct {
	pb.UnimplementedDataplaneServiceServer
}

// TailLogs answers the bounded log query from this agent's logbuf ring, exactly
// like a dataplane node (see TailLogbuf).
func (*logServer) TailLogs(ctx context.Context, req *pb.DataplaneServiceTailLogsRequest) (*pb.DataplaneServiceTailLogsResponse, error) {
	return TailLogbuf(req)
}

// TailLogbuf runs a TailLogs request against this process's logbuf ring — the one
// implementation every agent kind shares (client agents here, dataplane nodes via
// the dataplane substrate delegating to it): newest matching records, oldest first,
// filtered before they cross the wire.
func TailLogbuf(req *pb.DataplaneServiceTailLogsRequest) (*pb.DataplaneServiceTailLogsResponse, error) {
	hasLevel := req.Level != ""
	var min slog.Level
	if hasLevel {
		var err error
		if min, err = logbuf.ParseLevel(req.Level); err != nil {
			return nil, err
		}
	}
	recs := logbuf.Default.Tail(min, hasLevel, req.Contains, int(req.Lines))
	out := make([]*pb.LogRecord, 0, len(recs))
	for _, rec := range recs {
		attrs := make([]*pb.LogAttr, 0, len(rec.Attrs))
		for _, a := range rec.Attrs {
			attrs = append(attrs, &pb.LogAttr{Key: a.Key, Value: a.Value})
		}
		out = append(out, &pb.LogRecord{
			TimeUnixNano: rec.Time.UnixNano(),
			Level:        rec.Level.String(),
			Message:      rec.Message,
			Attrs:        attrs,
		})
	}
	return &pb.DataplaneServiceTailLogsResponse{Records: out}, nil
}

// SetLogLevel switches this client agent's live log level — the same DataplaneService RPC
// the control plane uses for dataplane nodes, so a monitor/nodewatch agent is reachable by
// the log_level resource exactly like a node.
func (*logServer) SetLogLevel(ctx context.Context, req *pb.DataplaneServiceSetLogLevelRequest) (*wkt.Empty, error) {
	lvl, err := logbuf.ParseLevel(req.Level)
	if err != nil {
		return nil, err
	}
	logbuf.SetLevel(lvl)
	return &wkt.Empty{}, nil
}

// GetLogLevel reports this client agent's current live log level.
func (*logServer) GetLogLevel(ctx context.Context, _ *wkt.Empty) (*pb.DataplaneServiceGetLogLevelResponse, error) {
	return &pb.DataplaneServiceGetLogLevelResponse{Level: logbuf.GetLevel().String()}, nil
}

// StreamLogs streams this process's structured logs from its logbuf (same wire form as a
// dataplane node, so the control plane relays it unchanged).
func (*logServer) StreamLogs(ctx context.Context, _ *wkt.Empty, stream *pb.DataplaneServiceStreamLogsServerStream) error {
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

// Serve registers the logs-only DataplaneService on p and dispatches incoming RPC streams
// to it until ctx is cancelled or the peer drops. Run it in a goroutine right after
// client.Connect (the peer is bidirectional: the agent keeps calling the control plane
// over its StreamSource while this answers the control plane's StreamLogs).
//
// register lets an agent expose additional services on the same peer (e.g. nodewatch's
// MonitorChatService): each is called with the manager before serving starts.
func Serve(ctx context.Context, p *peer.Peer, logger *slog.Logger, register ...func(rpc.Registry)) {
	// Serve on the peer's per-connection context (mirrors dataplane's serve()): when
	// the connection dies — transport error, peer close, or liveness timeout — it is
	// cancelled, so AcceptBidirectionalStream unblocks and Serve returns, letting the
	// caller's reconnect loop fire. With the caller's process context instead, a dead
	// connection wedged Serve (and nodewatch's reconnect) forever: the liveness loop
	// tore the connection down but nothing ever unblocked accept.
	ctx = p.Context()
	mgr := rpc.NewRPCManager()
	pb.RegisterDataplaneServiceServer(mgr, &logServer{})
	for _, r := range register {
		r(mgr)
	}
	for {
		stream, err := p.Streams().AcceptBidirectionalStream(ctx)
		if err != nil {
			return // peer dropped / ctx cancelled
		}
		go func(stream trsf.BidirectionalStream) {
			magic, err := rpc.DecodeMagic(ctx, stream)
			if err != nil {
				return
			}
			if magic == rpc.StreamMagicRPCS {
				mgr.HandleService(ctx, logger, stream)
				return
			}
			stream.CloseBoth()
		}(stream)
	}
}
