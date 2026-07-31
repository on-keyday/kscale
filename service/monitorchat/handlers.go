// Package monitorchat holds the hand-written business behind MonitorChatService:
// relay an admin's chat turn to the monitor agent's own MonitorChatService (served
// over its peer via agentserve, like its StreamLogs) and pipe the step events back.
// The LLM, tools and session history all live on the agent; the control plane only
// authorizes (the generated gate) and forwards — both legs speak the same generated
// service, so the relay is a direct pass-through.
package monitorchat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/on-keyday/kscale/peer"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/kscale/rpc"
)

type Handlers struct {
	pb.UnimplementedMonitorChatServiceServer
	Logger *slog.Logger
	// ResolveAgent resolves a client agent by CommonName or short label (the same
	// hook logs relaying uses — the monitor is not a dataplane node).
	ResolveAgent func(selector string) (*peer.Peer, bool)
	// ListAgents enumerates the connected client agents, for defaulting when the
	// request names no agent.
	ListAgents func() []*peer.Peer
}

var _ pb.MonitorChatServiceServer = (*Handlers)(nil)

// Send relays one chat turn to the monitor agent and pipes its ChatEvents back to
// the admin until the agent closes the turn's stream.
func (h *Handlers) Send(ctx context.Context, req *pbaccess.ResourceMonitorChatActionSendArgsDTO, stream *pb.MonitorChatServiceSendServerStream) error {
	p, err := h.target(req.Agent)
	if err != nil {
		return err
	}
	c := pb.NewMonitorChatServiceClient(rpc.NewTrsfStreamSource(p.Streams(), h.Logger))
	src, err := c.Send(ctx, req)
	if err != nil {
		return fmt.Errorf("open monitor chat stream: %w", err)
	}
	return relayEvents(ctx, src, stream)
}

// Resume relays a client-side tool result back into a suspended turn on the monitor
// agent and pipes the continued ChatEvents back to the admin.
func (h *Handlers) Resume(ctx context.Context, req *pbaccess.ResourceMonitorChatActionResumeArgsDTO, stream *pb.MonitorChatServiceResumeServerStream) error {
	p, err := h.target(req.Agent)
	if err != nil {
		return err
	}
	c := pb.NewMonitorChatServiceClient(rpc.NewTrsfStreamSource(p.Streams(), h.Logger))
	src, err := c.Resume(ctx, req)
	if err != nil {
		return fmt.Errorf("open monitor chat resume stream: %w", err)
	}
	return relayEvents(ctx, src, stream)
}

// relayEvents pipes a monitor agent's ChatEvent stream to the admin until the agent
// closes it (io.EOF) or either side errors. Send and Resume share it.
func relayEvents[S interface {
	Recv(context.Context) (*pb.ChatEvent, error)
}, D interface {
	Send(*pb.ChatEvent) error
}](ctx context.Context, src S, dst D) error {
	for {
		ev, err := src.Recv(ctx)
		if errors.Is(err, io.EOF) {
			return nil // turn (or sub-turn) complete
		}
		if err != nil {
			return fmt.Errorf("monitor agent chat: %w", err)
		}
		if err := dst.Send(ev); err != nil {
			return err // admin disconnected
		}
	}
}

// target picks the monitor agent peer: by selector when given; the default is the
// conventional "monitor" label (metricsgw shares the agent registry, so "the only
// agent" is not a safe default), then the only connected agent as a fallback.
// Ambiguity and absence are both errors that name the fix.
func (h *Handlers) target(selector string) (*peer.Peer, error) {
	if selector != "" {
		if p, ok := h.ResolveAgent(selector); ok {
			return p, nil
		}
		return nil, fmt.Errorf("monitor agent %q is not connected", selector)
	}
	if p, ok := h.ResolveAgent("monitor"); ok {
		return p, nil
	}
	agents := h.ListAgents()
	switch len(agents) {
	case 0:
		return nil, errors.New("no monitor agent is connected")
	case 1:
		return agents[0], nil
	default:
		names := make([]string, 0, len(agents))
		for _, p := range agents {
			names = append(names, p.CommonName())
		}
		return nil, fmt.Errorf("multiple monitor agents connected (%s): specify one via agent", strings.Join(names, ", "))
	}
}
