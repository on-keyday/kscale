package dpbroker

import (
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/kscale/access"
	"github.com/on-keyday/kscale/peer"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// fakeConn is a minimal objproto.Connection: only Done()/Close()/ConnectionID() are
// exercised by the broker; the embedded nil interface panics if anything else is
// touched (it is not). Close() fires Done() so a disconnect can be simulated.
type fakeConn struct {
	objproto.Connection
	mu     sync.Mutex
	done   chan struct{}
	closed bool
}

func (c *fakeConn) ConnectionID() objproto.ConnectionID {
	return objproto.NewConnectionID("test", netip.AddrPort{}, 0)
}
func (c *fakeConn) Done() <-chan struct{} { return c.done }
func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.done)
	}
	return nil
}
func (c *fakeConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

type fakeClient struct{ conn *fakeConn }

func (c fakeClient) Connection() objproto.Connection { return c.conn }
func (c fakeClient) Streams() trsf.Multiplexer       { return nil }

// testPeer builds a *peer.Peer whose CommonName() is cn (via the real authority
// chain) and whose connection can be closed to simulate a disconnect.
func testPeer(cn string) (*peer.Peer, *fakeConn) {
	fc := &fakeConn{done: make(chan struct{})}
	policy := access.NewContextCollector().CollectUser(
		access.NewUserContext(access.NewRootAuthority(cn), nil))
	return peer.NewPeer(fakeClient{fc}, policy), fc
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", msg)
}

// TestAddRemoveOnDisconnect: a node is dropped from the inventory when its
// connection closes (not just accumulated forever), firing OnDisconnect.
func TestAddRemoveOnDisconnect(t *testing.T) {
	b := New()
	var disc []string
	var dmu sync.Mutex
	b.OnDisconnect(func(_ string, p *peer.Peer) {
		dmu.Lock()
		disc = append(disc, p.CommonName())
		dmu.Unlock()
	})

	pA, cA := testPeer("nodeA")
	pB, _ := testPeer("nodeB")
	b.Add("l4lb", pA)
	b.Add("popcache", pB)
	if got := len(b.Nodes()); got != 2 {
		t.Fatalf("nodes = %d, want 2", got)
	}

	cA.Close() // simulate nodeA disconnect
	waitFor(t, func() bool { _, ok := b.Find("nodeA"); return !ok }, "nodeA removed")
	if got := len(b.Nodes()); got != 1 {
		t.Fatalf("after nodeA disconnect nodes = %d, want 1", got)
	}
	if _, ok := b.Find("nodeB"); !ok {
		t.Fatal("nodeB should remain")
	}
	waitFor(t, func() bool {
		dmu.Lock()
		defer dmu.Unlock()
		return len(disc) == 1 && disc[0] == "nodeA"
	}, "OnDisconnect(nodeA) fired")
}

// TestResolve covers the friendly node selector: full CommonName, short node
// name, dp_type-qualified name, ambiguity, and no-match.
func TestResolve(t *testing.T) {
	b := New()
	pL1, _ := testPeer("node1.l4lb.dp.system.kscale.local")
	pP1, _ := testPeer("node1.popcache.dp.system.kscale.local")
	pL2, _ := testPeer("node2.l4lb.dp.system.kscale.local")
	b.Add("l4lb", pL1)
	b.Add("popcache", pP1)
	b.Add("l4lb", pL2)

	// Full CommonName (backward compatible).
	if p, err := b.Resolve("node1.l4lb.dp.system.kscale.local"); err != nil || p != pL1 {
		t.Fatalf("exact CN: got (%p, %v), want %p", p, err, pL1)
	}
	// Unique short node name (node2 only exists as l4lb).
	if p, err := b.Resolve("node2"); err != nil || p != pL2 {
		t.Fatalf("short name node2: got (%p, %v), want %p", p, err, pL2)
	}
	// Ambiguous short name (node1 is both l4lb and popcache).
	if _, err := b.Resolve("node1"); err == nil {
		t.Fatal("node1 should be ambiguous across dp types")
	}
	// dp_type-qualified disambiguation.
	if p, err := b.Resolve("popcache/node1"); err != nil || p != pP1 {
		t.Fatalf("popcache/node1: got (%p, %v), want %p", p, err, pP1)
	}
	if p, err := b.Resolve("l4lb/node1"); err != nil || p != pL1 {
		t.Fatalf("l4lb/node1: got (%p, %v), want %p", p, err, pL1)
	}
	// No match.
	if _, err := b.Resolve("ghost"); err == nil {
		t.Fatal("ghost should not resolve")
	}
}

func TestResolveAll(t *testing.T) {
	b := New()
	pL1, _ := testPeer("node1.l4lb.dp.system.kscale.local")
	pP1, _ := testPeer("node1.popcache.dp.system.kscale.local")
	pL2, _ := testPeer("node2.l4lb.dp.system.kscale.local")
	b.Add("l4lb", pL1)
	b.Add("popcache", pP1)
	b.Add("l4lb", pL2)

	count := func(sel string) int {
		ps, err := b.ResolveAll(sel)
		if err != nil {
			return -1
		}
		return len(ps)
	}
	if n := count("*"); n != 3 {
		t.Fatalf(`"*" = %d, want 3 (all)`, n)
	}
	if n := count("*/*"); n != 3 {
		t.Fatalf(`"*/*" = %d, want 3 (all)`, n)
	}
	if n := count("l4lb/*"); n != 2 {
		t.Fatalf(`"l4lb/*" = %d, want 2`, n)
	}
	if n := count("popcache/*"); n != 1 {
		t.Fatalf(`"popcache/*" = %d, want 1`, n)
	}
	// Unlike Resolve, a bare name matching multiple dp types fans out (no ambiguity error).
	if n := count("node1"); n != 2 {
		t.Fatalf(`"node1" = %d, want 2 (both dp types)`, n)
	}
	if n := count("l4lb/node1"); n != 1 {
		t.Fatalf(`"l4lb/node1" = %d, want 1`, n)
	}
	if _, err := b.ResolveAll("ghost"); err == nil {
		t.Fatal("ghost should error (no match)")
	}
}

// TestReconnectSupersedes is the exact scenario raised: the same CommonName
// reconnects (a new connection) BEFORE the old connection's Done() has fired. The
// new peer must supersede the old — no duplicate, Find returns the new one — the
// old connection is torn down, and the old peer's (eventual) late Done() must NOT
// remove the live new peer.
func TestReconnectSupersedes(t *testing.T) {
	b := New()

	pOld, cOld := testPeer("node1")
	b.Add("l4lb", pOld) // old still "present" — its Done() has NOT fired

	pNew, cNew := testPeer("node1")
	b.Add("l4lb", pNew) // reconnect, same CommonName, before old disconnected

	// Dedup is synchronous under the lock: exactly one node1, and it is the NEW peer.
	if got := len(b.List("l4lb")); got != 1 {
		t.Fatalf("after reconnect list = %d, want 1 (dedup by CommonName)", got)
	}
	if p, ok := b.Find("node1"); !ok || p != pNew {
		t.Fatalf("Find(node1) = (%p, %v), want the new peer %p", p, ok, pNew)
	}

	// The superseded old connection is closed (releasing it + ending its watcher).
	waitFor(t, cOld.isClosed, "old connection torn down on supersede")

	// The old peer's now-fired Done() runs Remove(pOld) — pointer-based, so it is a
	// no-op and must not evict the live new peer.
	waitFor(t, func() bool {
		p, ok := b.Find("node1")
		return ok && p == pNew && len(b.List("l4lb")) == 1
	}, "new peer survives the old peer's late disconnect")

	if cNew.isClosed() {
		t.Fatal("new (live) connection must NOT have been closed")
	}
}

// TestMatchSpecificity covers the per-node selector precedence: exact node (3) >
// dp_type group (2) > all (1) > no match (0).
func TestMatchSpecificity(t *testing.T) {
	b := New()
	p, _ := testPeer("node1.l4lb.dp.system.kscale.local")
	b.Add("l4lb", p)
	cases := []struct {
		sel  string
		want int
	}{
		{"*", 1},
		{"l4lb/*", 2},
		{"node1.l4lb.dp.system.kscale.local", 3}, // full CommonName
		{"l4lb/node1", 3},                        // dp_type/node
		{"node1", 3},                             // short label
		{"popcache/*", 0},                        // wrong dp_type group
		{"l4lb/node2", 0},                        // wrong node
		{"node2", 0},                             // wrong short label
	}
	for _, c := range cases {
		if got := b.MatchSpecificity(c.sel, "l4lb", p); got != c.want {
			t.Errorf("MatchSpecificity(%q) = %d, want %d", c.sel, got, c.want)
		}
	}
}
