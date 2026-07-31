package dpbroker

import (
	"testing"

	"github.com/on-keyday/kscale/peer"
)

// TestAgentRegistrySameCNOverlap reproduces the 2026-07-07 registry race: two
// connections share one CN (agent restart / cert renewal), the older one closes,
// and its cleanup must NOT evict the live newer connection.
func TestAgentRegistrySameCNOverlap(t *testing.T) {
	const cn = "monitor.manager.ca.admin.kscale.local"
	a := NewAgentRegistry()
	oldConn, newConn := &peer.Peer{}, &peer.Peer{}

	a.Add(cn, oldConn)
	a.Add(cn, newConn)
	a.Remove(cn, oldConn) // the old connection's keepalive expires after the new one joined

	got, ok := a.Resolve("monitor")
	if !ok || got != newConn {
		t.Fatalf("Resolve after old-connection cleanup = (%p, %v), want the live connection %p", got, ok, newConn)
	}
	if l := a.List(); len(l) != 1 || l[0] != newConn {
		t.Fatalf("List = %v, want just the live connection", l)
	}
}

// TestAgentRegistryRenewalDoesNotShadow: a short-lived same-CN connection (cert
// renewal) joins while the agent is live; the agent (oldest) keeps being resolved,
// and the renewal's removal leaves it untouched.
func TestAgentRegistryRenewalDoesNotShadow(t *testing.T) {
	const cn = "monitor.manager.ca.admin.kscale.local"
	a := NewAgentRegistry()
	agent, renewal := &peer.Peer{}, &peer.Peer{}

	a.Add(cn, agent)
	a.Add(cn, renewal)
	if got, _ := a.Resolve(cn); got != agent {
		t.Fatalf("Resolve during renewal overlap = %p, want the long-lived agent %p", got, agent)
	}
	a.Remove(cn, renewal)
	if got, ok := a.Resolve("monitor"); !ok || got != agent {
		t.Fatalf("Resolve after renewal close = (%p, %v), want the agent %p", got, ok, agent)
	}
}

// TestAgentRegistryRemoveLast: removing an agent's only connection empties its CN
// (Resolve misses, List omits it).
func TestAgentRegistryRemoveLast(t *testing.T) {
	const cn = "metricsgw.manager.ca.admin.kscale.local"
	a := NewAgentRegistry()
	p := &peer.Peer{}
	a.Add(cn, p)
	a.Remove(cn, p)
	if _, ok := a.Resolve("metricsgw"); ok {
		t.Fatal("Resolve should miss after the only connection is removed")
	}
	if l := a.List(); len(l) != 0 {
		t.Fatalf("List = %v, want empty", l)
	}
}
