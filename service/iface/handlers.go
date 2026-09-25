// Package iface holds the hand-written business behind InterfaceService — a
// declarative l4lb resource: the XDP host interface each l4lb node attaches its
// balancer to. The desired interface set lives in a control-plane store shared
// with the reconcile loop, which pushes it to l4lb nodes via
// DataplaneService.BindInterface. (Named "iface" because "interface" is a Go
// keyword; the resource/service are still called interface.)
package iface

import (
	"context"
	"sort"
	"sync"

	"github.com/on-keyday/kscale/dpbroker"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
)

// Store is the desired per-node interface binding (node -> interface), notifying
// the reconcile loop on change. Per-node because each node binds its own NIC
// (node1 -> eth0, node2 -> enp3s0); the reconcile pushes each node only its value.
type Store struct {
	mu     sync.Mutex
	byNode map[string]string // node selector -> interface
	notify func()
}

func NewStore() *Store { return &Store{byNode: map[string]string{}} }

func (s *Store) OnChange(f func()) { s.notify = f }

func (s *Store) changed() {
	if s.notify != nil {
		s.notify()
	}
}

// Desired returns one DTO per node (Node + Interface); the reconcile reads the
// node selector + value off each. AppliedOn is left for the List handler to fill.
func (s *Store) Desired() []*pbaccess.ResourceInterfaceActionGetResponseDTO {
	s.mu.Lock()
	defer s.mu.Unlock()
	nodes := make([]string, 0, len(s.byNode))
	for n := range s.byNode {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	out := make([]*pbaccess.ResourceInterfaceActionGetResponseDTO, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, &pbaccess.ResourceInterfaceActionGetResponseDTO{Node: n, Interface: s.byNode[n]})
	}
	return out
}

func (s *Store) apply(node, iface string) {
	s.mu.Lock()
	s.byNode[node] = iface
	s.mu.Unlock()
	s.changed()
}

func (s *Store) delete(node string) {
	s.mu.Lock()
	delete(s.byNode, node)
	s.mu.Unlock()
	s.changed()
}

// DriftLister adapts the per-node store to reconcile.WatchDrift, whose Desired()
// []string is the flat set of desired interface VALUES (across all nodes) — drift
// is a node reporting an interface bound that no node desires.
type DriftLister struct{ Store *Store }

func (d DriftLister) Desired() []string {
	d.Store.mu.Lock()
	defer d.Store.mu.Unlock()
	seen := map[string]bool{}
	out := make([]string, 0, len(d.Store.byNode))
	for _, v := range d.Store.byNode {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// Observer reports the observed status of an interface: the nodes that actually
// bound it (from their reported CdnAppRealtime.BoundIfaces). Optional (nil -> no
// status). Generated as reconcile.InterfaceObserver. Mirrors the vip pattern.
type Observer interface {
	AppliedNodes(iface string) []string
	ObservedSets() map[string][]string // node -> bound interfaces it reports
}

// NodeLister gives each connected node's dp_type (satisfied by *dpbroker.Broker).
type NodeLister interface {
	Nodes() []dpbroker.NodeInfo
}

type Handlers struct {
	pb.UnimplementedInterfaceServiceServer
	Store    *Store
	Observer Observer
	// Broker, if set, lets List attribute each node's bound interface to the ONE
	// declaration that targets it (see attributeAppliedOn). Without it applied_on
	// falls back to "every node binding an interface of this name".
	Broker NodeLister
	// ConfirmPush, set by the control plane, returns the resource's current per-node
	// reconcile push error (nil if all good). Called right after notify() so apply
	// reports a failed push synchronously instead of a silent OK. Optional.
	ConfirmPush func() error
}

func (h *Handlers) appliedOn(iface string) []string {
	if h.Observer == nil {
		return nil
	}
	return h.Observer.AppliedNodes(iface)
}

var _ pb.InterfaceServiceServer = (*Handlers)(nil)

func (h *Handlers) Apply(ctx context.Context, req *pbaccess.ResourceInterfaceActionApplyArgsDTO) (*pbaccess.ResourceInterfaceActionApplyResponseDTO, error) {
	h.Store.apply(req.Node, req.Interface)
	resp := &pbaccess.ResourceInterfaceActionApplyResponseDTO{Node: req.Node, Interface: req.Interface}
	if h.ConfirmPush != nil {
		return resp, h.ConfirmPush()
	}
	return resp, nil
}

func (h *Handlers) List(ctx context.Context, _ *pbaccess.ResourceInterfaceActionListArgsDTO) (*pbaccess.ResourceInterfaceActionListResponseDTO, error) {
	items := h.Store.Desired()
	if h.Observer != nil && h.Broker != nil {
		dpTypes := map[string]string{}
		for _, n := range h.Broker.Nodes() {
			dpTypes[n.CommonName] = n.DpType
		}
		attributeAppliedOn(items, h.Observer.ObservedSets(), dpTypes)
	} else {
		for _, it := range items {
			it.AppliedOn = h.appliedOn(it.Interface)
		}
	}
	return &pbaccess.ResourceInterfaceActionListResponseDTO{Items: items}, nil
}

// attributeAppliedOn fills each declaration's AppliedOn with the nodes it is the
// most specific match for (the same rule the per-node reconcile uses to pick which
// value to push) AND that report binding its interface. Matching by interface name
// alone listed a node under every declaration of the same NIC name — the popcache
// nodes showed up on the workload entries because both bind enp2s0f1.
func attributeAppliedOn(items []*pbaccess.ResourceInterfaceActionGetResponseDTO, observed map[string][]string, dpTypes map[string]string) {
	for _, it := range items {
		it.AppliedOn = nil
	}
	nodes := make([]string, 0, len(observed))
	for cn := range observed {
		nodes = append(nodes, cn)
	}
	sort.Strings(nodes)
	for _, cn := range nodes {
		var best *pbaccess.ResourceInterfaceActionGetResponseDTO
		bestScore := 0
		for _, it := range items { // sorted by selector: ties go to the first
			if s := dpbroker.MatchSelector(it.Node, dpTypes[cn], cn); s > bestScore {
				best, bestScore = it, s
			}
		}
		if best == nil {
			continue
		}
		for _, bound := range observed[cn] {
			if bound == best.Interface {
				best.AppliedOn = append(best.AppliedOn, cn)
				break
			}
		}
	}
}

func (h *Handlers) Delete(ctx context.Context, req *pbaccess.ResourceInterfaceActionDeleteArgsDTO) (*pbaccess.ResourceInterfaceActionDeleteResponseDTO, error) {
	h.Store.delete(req.Node)
	return &pbaccess.ResourceInterfaceActionDeleteResponseDTO{Node: req.Node}, nil
}
