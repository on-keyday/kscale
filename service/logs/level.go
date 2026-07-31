package logs

import (
	"context"
	"fmt"

	"github.com/on-keyday/kscale/logbuf"
	"github.com/on-keyday/kscale/peer"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/kscale/protobuf/wkt"
	"github.com/on-keyday/kscale/rpc"
)

// levelTargets resolves the node selector to the southbound peers to act on (dataplane
// nodes + monitor agents) and whether the control plane itself is included:
//   - "cp"/"controlplane": the CP only (in-process).
//   - "all": CP + every dataplane node + every monitor agent.
//   - else: a node CN / "<type>/*" / "*" (dataplane), also matched against the agents.
func (h *Handlers) levelTargets(sel string) (peers []*peer.Peer, includeCP bool) {
	switch sel {
	case "cp", "controlplane":
		return nil, true
	case "all":
		if dp, err := h.Broker.ResolveAll("*"); err == nil {
			peers = append(peers, dp...)
		}
		if h.ListAgents != nil {
			peers = append(peers, h.ListAgents()...)
		}
		return peers, true
	default:
		if dp, err := h.Broker.ResolveAll(sel); err == nil {
			peers = append(peers, dp...)
		}
		if h.ResolveAgent != nil {
			if a, ok := h.ResolveAgent(sel); ok {
				peers = append(peers, a)
			}
		}
		return peers, false
	}
}

// SetLevel switches the live log level of the targets named by `node` (see levelTargets).
// The CP sets its own level in-process; dataplane nodes and monitor agents are set over
// DataplaneService.SetLogLevel. Returns one "<target> -> <level>" (or "-> ERROR ...") line.
func (h *Handlers) SetLevel(ctx context.Context, req *pbaccess.ResourceLogsActionSetLevelArgsDTO) (*pbaccess.ResourceLogsActionSetLevelResponseDTO, error) {
	lvl, err := logbuf.ParseLevel(req.Level)
	if err != nil {
		return nil, err
	}
	name := lvl.String()
	peers, includeCP := h.levelTargets(req.Node)
	var applied []string
	if includeCP {
		logbuf.SetLevel(lvl)
		applied = append(applied, "cp -> "+name)
	}
	for _, p := range peers {
		c := pb.NewDataplaneServiceClient(rpc.NewTrsfStreamSource(p.Streams(), h.Logger))
		if _, err := c.SetLogLevel(ctx, &pb.DataplaneServiceSetLogLevelRequest{Level: name}); err != nil {
			applied = append(applied, fmt.Sprintf("%s -> ERROR %v", p.CommonName(), err))
			continue
		}
		applied = append(applied, fmt.Sprintf("%s -> %s", p.CommonName(), name))
	}
	if len(applied) == 0 {
		return nil, fmt.Errorf("no target matched node %q (try: cp, *, all, a node CN, or a <type>/* group)", req.Node)
	}
	return &pbaccess.ResourceLogsActionSetLevelResponseDTO{Applied: applied}, nil
}

// GetLevel reports the current live log level of the targets named by `node`.
func (h *Handlers) GetLevel(ctx context.Context, req *pbaccess.ResourceLogsActionGetLevelArgsDTO) (*pbaccess.ResourceLogsActionGetLevelResponseDTO, error) {
	peers, includeCP := h.levelTargets(req.Node)
	var levels []string
	if includeCP {
		levels = append(levels, "cp -> "+logbuf.GetLevel().String())
	}
	for _, p := range peers {
		c := pb.NewDataplaneServiceClient(rpc.NewTrsfStreamSource(p.Streams(), h.Logger))
		resp, err := c.GetLogLevel(ctx, &wkt.Empty{})
		if err != nil {
			levels = append(levels, fmt.Sprintf("%s -> ERROR %v", p.CommonName(), err))
			continue
		}
		levels = append(levels, fmt.Sprintf("%s -> %s", p.CommonName(), resp.Level))
	}
	if len(levels) == 0 {
		return nil, fmt.Errorf("no target matched node %q (try: cp, *, all, a node CN, or a <type>/* group)", req.Node)
	}
	return &pbaccess.ResourceLogsActionGetLevelResponseDTO{Levels: levels}, nil
}
