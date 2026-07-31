// Package dpbroker is the control plane's dataplane inventory: it tracks the
// connected dataplane agent peers grouped by dp type (l4lb/popcache/router/dns)
// and notifies a reconcile loop when a node connects. It is the minimal kscale
// re-home of ksdk's BrokerAgent (agent/dpmanage/dp.go) — enough for a reconcile
// loop to fan desired state out to nodes and react to new ones.
package dpbroker

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/on-keyday/kscale/peer"
)

type Broker struct {
	mu           sync.Mutex
	nodes        map[string][]*peer.Peer // dpType -> connected peers
	onConnect    []func(dpType string, p *peer.Peer)
	onDisconnect []func(dpType string, p *peer.Peer)
}

func New() *Broker {
	return &Broker{nodes: map[string][]*peer.Peer{}}
}

// OnConnect registers a callback fired when a dataplane node is added. Multiple
// callbacks may be registered (each reconcile loop + the stat collector registers
// its own); all fire in registration order.
func (b *Broker) OnConnect(f func(dpType string, p *peer.Peer)) {
	b.mu.Lock()
	b.onConnect = append(b.onConnect, f)
	b.mu.Unlock()
}

// OnDisconnect registers a callback fired when a node's connection closes and it
// is removed from the inventory (so consumers can drop stale per-node state).
func (b *Broker) OnDisconnect(f func(dpType string, p *peer.Peer)) {
	b.mu.Lock()
	b.onDisconnect = append(b.onDisconnect, f)
	b.mu.Unlock()
}

// Add registers a connected dataplane peer of dpType and notifies OnConnect. A
// reconnecting node supersedes its stale entry (any existing peer of the same
// CommonName is dropped first — the substrate reconnects, so without this the same
// node would accumulate duplicate, dead entries). A watcher goroutine removes the
// peer when its connection closes, so the inventory only ever holds live nodes.
func (b *Broker) Add(dpType string, p *peer.Peer) {
	cn := p.CommonName()
	b.mu.Lock()
	var superseded []*peer.Peer
	kept := b.nodes[dpType][:0:0]
	for _, q := range b.nodes[dpType] {
		if q.CommonName() == cn {
			superseded = append(superseded, q)
		} else {
			kept = append(kept, q)
		}
	}
	b.nodes[dpType] = append(kept, p)
	cbs := append([]func(string, *peer.Peer){}, b.onConnect...)
	b.mu.Unlock()
	// Tear down any superseded connection (same node reconnected before its old
	// connection's Done() fired). Closing it releases its resources and makes its
	// watcher goroutine exit promptly instead of lingering until keepalive finally
	// detects the drop. Best-effort, off-thread so Add never blocks on teardown.
	for _, q := range superseded {
		go q.Connection().Close()
	}
	for _, f := range cbs {
		f(dpType, p)
	}
	go func() {
		<-p.Connection().Done()
		b.Remove(dpType, p)
	}()
}

// Remove deletes a specific peer from the inventory (called when its connection
// closes) and fires OnDisconnect. A no-op if the peer was already superseded.
func (b *Broker) Remove(dpType string, p *peer.Peer) {
	b.mu.Lock()
	ps := b.nodes[dpType]
	kept := ps[:0:0]
	found := false
	for _, q := range ps {
		if q == p {
			found = true
			continue
		}
		kept = append(kept, q)
	}
	b.nodes[dpType] = kept
	var cbs []func(string, *peer.Peer)
	if found {
		cbs = append(cbs, b.onDisconnect...)
	}
	b.mu.Unlock()
	for _, f := range cbs {
		f(dpType, p)
	}
}

// List returns the connected peers of dpType, or — for "*" — every connected
// dataplane node regardless of kind (matching ksdk's dp_type="*" fan-out).
func (b *Broker) List(dpType string) []*peer.Peer {
	b.mu.Lock()
	defer b.mu.Unlock()
	if dpType == "*" {
		var all []*peer.Peer
		for _, ps := range b.nodes {
			all = append(all, ps...)
		}
		return all
	}
	out := make([]*peer.Peer, len(b.nodes[dpType]))
	copy(out, b.nodes[dpType])
	return out
}

// Matches reports whether a connecting node of connType is targeted by dpType
// ("*" matches any kind).
func Matches(dpType, connType string) bool {
	return dpType == "*" || dpType == connType
}

// NodeInfo identifies a connected dataplane node.
type NodeInfo struct {
	CommonName string
	DpType     string
	ConnID     string // full transport connection id, e.g. "udp:host:port-id"
	RemoteAddr string // just the peer's remote address (host:port)
}

// Nodes returns every connected node (for the DataplaneNode list / Connection
// queries).
func (b *Broker) Nodes() []NodeInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []NodeInfo
	for dpType, ps := range b.nodes {
		for _, p := range ps {
			cid := p.Connection().ConnectionID()
			out = append(out, NodeInfo{
				CommonName: p.CommonName(),
				DpType:     dpType,
				ConnID:     cid.String(),
				RemoteAddr: cid.Addr.String(),
			})
		}
	}
	return out
}

// Find returns the connected peer with the given certificate CommonName.
func (b *Broker) Find(commonName string) (*peer.Peer, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ps := range b.nodes {
		for _, p := range ps {
			if p.CommonName() == commonName {
				return p, true
			}
		}
	}
	return nil, false
}

// nodeLabel is the friendly node name embedded in a CommonName: its first label.
// CN "node1.l4lb.dp.system.kscale.local" -> "node1".
func nodeLabel(commonName string) string {
	if i := strings.IndexByte(commonName, '.'); i >= 0 {
		return commonName[:i]
	}
	return commonName
}

// MatchSpecificity scores how specifically `selector` targets peer p (of dpType):
//
//	3 = this exact node (full CommonName, "<dp_type>/<node>", or short node label)
//	2 = p's dp_type group ("<dp_type>/*")
//	1 = every node ("*")
//	0 = no match
//
// A per-node reconcile gives each node the value of its MOST specific matching
// selector, so a per-node override beats a group default beats "*".
func (b *Broker) MatchSpecificity(selector, dpType string, p *peer.Peer) int {
	switch selector {
	case "*":
		return 1
	case dpType + "/*":
		return 2
	}
	cn := p.CommonName()
	if selector == cn || selector == dpType+"/"+nodeLabel(cn) || selector == nodeLabel(cn) {
		return 3
	}
	return 0
}

// ForEachNode calls fn for every connected node with its dp type. fn runs outside
// the broker lock (a snapshot is taken first), so it may call back into the broker.
func (b *Broker) ForEachNode(fn func(dpType string, p *peer.Peer)) {
	b.mu.Lock()
	type typedPeer struct {
		dpType string
		p      *peer.Peer
	}
	var all []typedPeer
	for dpType, ps := range b.nodes {
		for _, p := range ps {
			all = append(all, typedPeer{dpType, p})
		}
	}
	b.mu.Unlock()
	for _, x := range all {
		fn(x.dpType, x.p)
	}
}

// Resolve maps a node selector to a connected peer so callers (and humans on the
// CLI) need not hand-type the full certificate CommonName. The selector may be:
//   - the full CommonName (exact match, backward compatible), or
//   - the short node name ("node1"), or
//   - the dp_type-qualified node name ("l4lb/node1").
//
// A bare node name that matches nodes of more than one dp_type is ambiguous and
// returns an error listing how to qualify it; no match lists the connected nodes.
func (b *Broker) Resolve(selector string) (*peer.Peer, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Exact CommonName first.
	for _, ps := range b.nodes {
		for _, p := range ps {
			if p.CommonName() == selector {
				return p, nil
			}
		}
	}
	// Short form: "<dp_type>/<node>" or "<node>".
	wantType, wantNode := "", selector
	if i := strings.IndexByte(selector, '/'); i >= 0 {
		wantType, wantNode = selector[:i], selector[i+1:]
	}
	var matches []*peer.Peer
	var candidates []string
	for dpType, ps := range b.nodes {
		for _, p := range ps {
			candidates = append(candidates, dpType+"/"+nodeLabel(p.CommonName()))
			if nodeLabel(p.CommonName()) == wantNode && (wantType == "" || wantType == dpType) {
				matches = append(matches, p)
			}
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		sort.Strings(candidates)
		return nil, fmt.Errorf("no connected node matches %q (connected: %s)", selector, strings.Join(candidates, ", "))
	default:
		return nil, fmt.Errorf("node selector %q is ambiguous across dp types; qualify it as <dp_type>/%s", selector, wantNode)
	}
}

// ResolveAll returns EVERY connected node matching selector: a single node (exact
// CommonName, "<dp_type>/<node>", or "<node>"), a whole dp-type group ("<dp_type>/*"),
// or all nodes ("*" or "*/*"). Unlike Resolve it never errors on multiple matches — it's
// for fan-out ops (start/stop, logs, scrape, transfer-state) where a wildcard means "do
// it to all of these". It errors only when nothing matches (listing the connected nodes).
func (b *Broker) ResolveAll(selector string) ([]*peer.Peer, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	all := selector == "*" || selector == "*/*"
	wantType, wantNode := "", selector
	if i := strings.IndexByte(selector, '/'); i >= 0 {
		wantType, wantNode = selector[:i], selector[i+1:]
	}
	var matches []*peer.Peer
	var candidates []string
	for dpType, ps := range b.nodes {
		for _, p := range ps {
			cn := p.CommonName()
			candidates = append(candidates, dpType+"/"+nodeLabel(cn))
			switch {
			case all:
				matches = append(matches, p)
			case wantNode == "*" && wantType == dpType: // "<dp_type>/*"
				matches = append(matches, p)
			case cn == selector || (nodeLabel(cn) == wantNode && (wantType == "" || wantType == dpType)):
				matches = append(matches, p)
			}
		}
	}
	if len(matches) == 0 {
		sort.Strings(candidates)
		return nil, fmt.Errorf("no connected node matches %q (connected: %s)", selector, strings.Join(candidates, ", "))
	}
	return matches, nil
}
