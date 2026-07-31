// Package vip holds the hand-written business behind VipService — a declarative
// l4lb resource carved from dp_management's update_vip. Its desired-state Store
// is shared with the reconcile loop (M3): apply/delete mutate the desired VIP
// set and notify the loop, which pushes it to the l4lb dataplane nodes via
// DataplaneService.UpdateVip. Kept separate from the generated gate
// (service.VipGated).
package vip

import (
	"context"
	"sort"
	"sync"

	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
)

// Store is the control-plane desired state: the desired VIP set, each carrying
// its icmp_echo flag (answer VIP-destined ICMPv4 echo requests in the l4lb data
// plane). It notifies a reconcile loop on every change.
type Store struct {
	mu     sync.Mutex
	vips   map[string]bool // vip -> icmp_echo
	notify func()
}

func NewStore() *Store { return &Store{vips: map[string]bool{}} }

// OnChange registers the reconcile trigger, invoked after any mutation.
func (s *Store) OnChange(f func()) { s.notify = f }

func (s *Store) changed() {
	if s.notify != nil {
		s.notify()
	}
}

// Desired returns the current desired VIP set (sorted by VIP, for determinism)
// as the multi-field reconcile DTOs (vip + icmp_echo).
func (s *Store) Desired() []*pbaccess.ResourceVipActionGetResponseDTO {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.vips))
	for v := range s.vips {
		names = append(names, v)
	}
	sort.Strings(names)
	out := make([]*pbaccess.ResourceVipActionGetResponseDTO, 0, len(names))
	for _, v := range names {
		out = append(out, &pbaccess.ResourceVipActionGetResponseDTO{Vip: v, IcmpEcho: s.vips[v]})
	}
	return out
}

// DriftLister adapts the Store to reconcile.WatchDrift, whose Desired() []string
// is the flat set of desired VIP addresses — drift is a node reporting a VIP no
// node desires. icmp_echo is not observable in the reported set, so it is not
// part of drift.
type DriftLister struct{ Store *Store }

func (d DriftLister) Desired() []string {
	d.Store.mu.Lock()
	defer d.Store.mu.Unlock()
	out := make([]string, 0, len(d.Store.vips))
	for v := range d.Store.vips {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// icmpEcho reports the desired icmp_echo for a VIP (false if absent).
func (s *Store) icmpEcho(vip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vips[vip]
}

func (s *Store) apply(vip string, icmpEcho bool) {
	s.mu.Lock()
	s.vips[vip] = icmpEcho
	s.mu.Unlock()
	s.changed()
}

func (s *Store) delete(vip string) {
	s.mu.Lock()
	delete(s.vips, vip)
	s.mu.Unlock()
	s.changed()
}

// Observer reports the observed/actual state behind a VIP: the nodes whose
// reported state includes it. Implemented over the stat cache (the dataplane
// reports its actual VIP set), it closes the reconcile loop — get/list show not
// just the desired VIP but where it is actually applied.
type Observer interface {
	AppliedNodes(vip string) []string
}

// Handlers is the VipService business over the shared Store. Observer is optional
// (nil → no status reflected).
type Handlers struct {
	pb.UnimplementedVipServiceServer
	Store    *Store
	Observer Observer
	// ConfirmPush, set by the control plane, returns the resource's current per-node
	// reconcile push error (nil if all good). Called right after notify() so apply
	// reports a failed push synchronously instead of a silent OK. Optional.
	ConfirmPush func() error
}

var _ pb.VipServiceServer = (*Handlers)(nil)

func (h *Handlers) appliedOn(vip string) []string {
	if h.Observer == nil {
		return nil
	}
	return h.Observer.AppliedNodes(vip)
}

func (h *Handlers) Apply(ctx context.Context, req *pbaccess.ResourceVipActionApplyArgsDTO) (*pbaccess.ResourceVipActionApplyResponseDTO, error) {
	h.Store.apply(req.Vip, req.IcmpEcho)
	resp := &pbaccess.ResourceVipActionApplyResponseDTO{Vip: req.Vip, IcmpEcho: req.IcmpEcho}
	if h.ConfirmPush != nil {
		return resp, h.ConfirmPush()
	}
	return resp, nil
}

func (h *Handlers) Get(ctx context.Context, req *pbaccess.ResourceVipActionGetArgsDTO) (*pbaccess.ResourceVipActionGetResponseDTO, error) {
	return &pbaccess.ResourceVipActionGetResponseDTO{Vip: req.Vip, IcmpEcho: h.Store.icmpEcho(req.Vip), AppliedOn: h.appliedOn(req.Vip)}, nil
}

func (h *Handlers) List(ctx context.Context, _ *pbaccess.ResourceVipActionListArgsDTO) (*pbaccess.ResourceVipActionListResponseDTO, error) {
	desired := h.Store.Desired()
	items := make([]*pbaccess.ResourceVipActionGetResponseDTO, 0, len(desired))
	for _, d := range desired {
		items = append(items, &pbaccess.ResourceVipActionGetResponseDTO{Vip: d.Vip, IcmpEcho: d.IcmpEcho, AppliedOn: h.appliedOn(d.Vip)})
	}
	return &pbaccess.ResourceVipActionListResponseDTO{Items: items}, nil
}

func (h *Handlers) Delete(ctx context.Context, req *pbaccess.ResourceVipActionDeleteArgsDTO) (*pbaccess.ResourceVipActionDeleteResponseDTO, error) {
	h.Store.delete(req.Vip)
	return &pbaccess.ResourceVipActionDeleteResponseDTO{Vip: req.Vip}, nil
}
