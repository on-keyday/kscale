package l4route

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/on-keyday/kscale/dpbroker"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/kscale/service/openport"
)

// OpenPorts is the open_port desired state (the router ACL), read by Diff.
type OpenPorts interface {
	Desired() []*pbaccess.ResourceOpenPortActionGetResponseDTO
}

// Service composes the generated store with apply-time validation and the Diff
// action. The control plane registers it in place of the bare store; the inner
// *Handlers is still used directly by the reconcile loop, persistence and
// ConfirmPush.
type Service struct {
	*Handlers
	Broker    *dpbroker.Broker
	Cache     StatSource
	OpenPorts OpenPorts
	// PushError reports the last SetPortRoutes push failures (reconcile status).
	PushError func() error
	Logger    *slog.Logger
}

var _ pb.L4RouteServiceServer = (*Service)(nil)

// Apply refuses a malformed declaration, or one whose proto:port another route
// already owns, before it is stored.
func (s *Service) Apply(ctx context.Context, req *pbaccess.ResourceL4RouteActionApplyArgsDTO) (*pbaccess.ResourceL4RouteActionApplyResponseDTO, error) {
	if err := Validate(req.Name, req.Port, req.Node); err != nil {
		return nil, fmt.Errorf("l4_route %q: %w", req.Name, err)
	}
	next := []*pbaccess.ResourceL4RouteActionGetResponseDTO{{Name: req.Name, Port: req.Port, Node: req.Node}}
	for _, it := range s.Handlers.Desired() {
		if it.Name != req.Name {
			next = append(next, it)
		}
	}
	if err := ValidateSet(next); err != nil {
		return nil, err
	}
	resp, err := s.Handlers.Apply(ctx, req)
	if err != nil {
		return resp, err
	}
	// Stored either way (the node may join later); say so when it cannot be
	// programmed yet, instead of a silent OK.
	if _, rerr := Resolve(next[0], s.Broker, s.Cache); rerr != nil {
		return resp, fmt.Errorf("l4_route %q stored, but not programmed yet: %w (see l4-route diff)", req.Name, rerr)
	}
	return resp, nil
}

// Diff reports what keeps a declared route from working, one line per problem:
//
//	<name> <proto:port>: node unresolved: <why>          (no connected node / no address: not programmed)
//	<name> <proto:port>: not opened by open_port          (the router ACL drops it before the l4lb)
//	l4lb push: <error>                                    (the last SetPortRoutes push failed)
//
// Empty means every route resolves, its port is open, and the l4lbs took the set.
// Read-only.
func (s *Service) Diff(ctx context.Context, _ *pbaccess.ResourceL4RouteActionDiffArgsDTO) (*pbaccess.ResourceL4RouteActionDiffResponseDTO, error) {
	open := map[string]bool{}
	if s.OpenPorts != nil {
		for _, acl := range s.OpenPorts.Desired() {
			ports, err := openport.ParsePorts(acl.Ports)
			if err != nil {
				continue
			}
			for _, p := range ports {
				open[fmt.Sprintf("%s:%d", p.Protocol, p.Port)] = true
			}
		}
	}
	var entries []string
	for _, it := range s.Handlers.Desired() {
		proto, port, err := ParsePort(it.Port)
		if err != nil {
			entries = append(entries, fmt.Sprintf("%s %s: %v", it.Name, it.Port, err))
			continue
		}
		key := fmt.Sprintf("%s:%d", proto, port)
		if _, err := Resolve(it, s.Broker, s.Cache); err != nil {
			entries = append(entries, fmt.Sprintf("%s %s: node unresolved: %v", it.Name, key, err))
		}
		if !open[key] {
			entries = append(entries, fmt.Sprintf("%s %s: not opened by open_port", it.Name, key))
		}
	}
	sort.Strings(entries)
	if s.PushError != nil {
		if err := s.PushError(); err != nil {
			entries = append(entries, fmt.Sprintf("l4lb push: %v", err))
		}
	}
	return &pbaccess.ResourceL4RouteActionDiffResponseDTO{Entries: entries}, nil
}
