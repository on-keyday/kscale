package dpbroker

import (
	"sync"

	"github.com/on-keyday/kscale/peer"
)

// AgentRegistry tracks client-side agents (e.g. the monitor / nodewatch) that serve their
// own logs back over their peer. It is deliberately SEPARATE from the dataplane Broker:
// the reconcile loops fan config out to every Broker peer (List("*")), so a client agent
// must never land there — but the control plane still needs to address it to relay
// `logs stream <agent>` / `monitor_chat send`.
//
// A CommonName maps to a LIST of connections, oldest first, because same-CN connections
// overlap in normal operation: the agent's 30-minute cert renewal opens a short-lived
// authenticated connection with the agent's own CN, and an agent restart leaves the dead
// old connection registered until its keepalive expires. Remove drops exactly the closed
// connection (a renewal closing must not evict the live agent — the bug behind
// notes/bugs/bug_2026_07_07_agent_registry_cn_race.md), and Resolve returns the oldest entry:
// the long-lived agent connection predates any renewal connection riding its CN.
type AgentRegistry struct {
	mu   sync.Mutex
	byCN map[string][]*peer.Peer // oldest first
}

func NewAgentRegistry() *AgentRegistry { return &AgentRegistry{byCN: map[string][]*peer.Peer{}} }

// Add registers one connection of the agent cn.
func (a *AgentRegistry) Add(cn string, p *peer.Peer) {
	a.mu.Lock()
	a.byCN[cn] = append(a.byCN[cn], p)
	a.mu.Unlock()
}

// Remove drops exactly p from cn's connections (no-op if already gone). Never removes
// by CN alone: other live connections may share it.
func (a *AgentRegistry) Remove(cn string, p *peer.Peer) {
	a.mu.Lock()
	defer a.mu.Unlock()
	peers := a.byCN[cn]
	for i, q := range peers {
		if q == p {
			peers = append(peers[:i:i], peers[i+1:]...)
			break
		}
	}
	if len(peers) == 0 {
		delete(a.byCN, cn)
	} else {
		a.byCN[cn] = peers
	}
}

// List returns every registered agent (one per CommonName, its oldest connection) —
// e.g. for fanning a fleet-wide op like log_level to the monitor agents alongside the
// dataplane nodes.
func (a *AgentRegistry) List() []*peer.Peer {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*peer.Peer, 0, len(a.byCN))
	for _, peers := range a.byCN {
		out = append(out, peers[0])
	}
	return out
}

// Resolve matches an agent by full CommonName or short node label (the CommonName up to
// the first dot — e.g. "monitor" for "monitor.manager.ca.admin.<domain>") and returns
// its oldest connection.
func (a *AgentRegistry) Resolve(selector string) (*peer.Peer, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for cn, peers := range a.byCN {
		if cn == selector || nodeLabel(cn) == selector {
			return peers[0], true
		}
	}
	return nil, false
}
