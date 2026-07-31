// Package reconcile — node_run converges each declared node's desired run-state
// (expected_node.desired_run) onto its dataplane via Start/StopDataplane. This is
// what turns `node start`/`node stop` from one-shot imperative calls into desired
// state: a node that reconnects (agent restart, CP restart with the persisted
// store) is started again without an operator, and a node whose start fails is
// retried with bounded exponential backoff — then HELD after repeated failures,
// so a deterministically-broken node (bad config, missing interface) is not
// hammered forever. Custom (not generated) because the push is stateful: it is
// gated on the node's OBSERVED app_status (Start/Stop are not idempotent on the
// agent side), tracks per-node backoff/hold, and honors the same "don't start a
// misconfigured node" gate the imperative path had.
package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/on-keyday/kscale/consts"
	"github.com/on-keyday/kscale/internal/safe"
	"github.com/on-keyday/kscale/peer"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/kscale/protobuf/wkt"
	"github.com/on-keyday/kscale/rpc"
)

// NodeRunResource is the reconcile.Status key node_run records under — it shows
// up in dataplane_node.reconcile_errors as "node_run: <error>" while a node's
// run-state fails to converge (backoff or hold).
const NodeRunResource = "node_run"

// Desired-run values (expected_node.desired_run). "" means unmanaged: the node
// is left exactly as `node start`/`node stop` never having been run.
const (
	RunStateRunning = "running"
	RunStateStopped = "stopped"
)

// Failure-handling knobs. Transient start failures (boot ordering: interface not
// yet up, dependency not yet connected) self-heal within the backoff; after
// nodeRunMaxAttempts consecutive failures the node is held — no more pushes —
// until the operator re-runs `node start` (generation bump), the desired state
// changes, or the agent reconnects (fresh process, cause likely gone).
var (
	nodeRunBaseBackoff = 5 * time.Second
	nodeRunMaxBackoff  = 5 * time.Minute
	nodeRunMaxAttempts = 5
	// nodeRunSettle is how long after a successful push the observed app_status
	// is trusted to still be catching up (stats stream every ~2s). Without it,
	// the next evaluation would see the stale pre-push status and push again —
	// and Start/StopDataplane error on a redundant call.
	nodeRunSettle = 10 * time.Second
)

// NodeRunDesired is the desired-state source (the generated expected_node store).
type NodeRunDesired interface {
	Desired() []*pbaccess.ResourceExpectedNodeActionGetResponseDTO
	OnChange(func())
}

// nodeRunBroker is the broker surface node_run needs (satisfied by
// *dpbroker.Broker; an interface so tests can fake connectivity).
type nodeRunBroker interface {
	Find(commonName string) (*peer.Peer, bool)
	OnConnect(func(dpType string, p *peer.Peer))
	OnDisconnect(func(dpType string, p *peer.Peer))
}

// nodeRunState is one node's convergence bookkeeping.
type nodeRunState struct {
	gen       uint64    // last seen run_generation; a bump resets backoff/hold
	attempts  int       // consecutive failed pushes
	nextTry   time.Time // no push before this (backoff)
	held      bool      // gave up after nodeRunMaxAttempts; no pushes until reset
	forceOnce bool      // skip the reconcile-errors gate for the next push (node start --force)
	pushedAt  time.Time // last successful push (settle window)
}

// NodeRunController converges desired run-state; construct via NodeRun.
type NodeRunController struct {
	store     NodeRunDesired
	broker    nodeRunBroker
	appStatus func(commonName string) consts.AppStatus
	status    *Status
	logger    *slog.Logger

	// push performs the actual southbound call; swapped by tests.
	push func(ctx context.Context, p *peer.Peer, start bool) error

	mu    sync.Mutex
	nodes map[string]*nodeRunState
}

// NodeRun wires the run-state reconcile: evaluate on desired change (inline, so
// `node start` gets synchronous feedback), on (re)connect (fresh agent — reset
// its backoff/hold and converge it), and on a tick (drives backoff retries and
// settle-window expiry). appStatus reports a node's observed lifecycle state
// from the stat cache (Unknown when it has not reported yet).
func NodeRun(ctx context.Context, store NodeRunDesired, broker nodeRunBroker, appStatus func(string) consts.AppStatus, status *Status, logger *slog.Logger, tick time.Duration) *NodeRunController {
	c := &NodeRunController{
		store:     store,
		broker:    broker,
		appStatus: appStatus,
		status:    status,
		logger:    logger,
		nodes:     map[string]*nodeRunState{},
	}
	c.push = func(ctx context.Context, p *peer.Peer, start bool) error {
		client := pb.NewDataplaneServiceClient(rpc.NewTrsfStreamSource(p.Streams(), logger))
		var err error
		if start {
			_, err = client.StartDataplane(ctx, &wkt.Empty{})
		} else {
			_, err = client.StopDataplane(ctx, &wkt.Empty{})
		}
		return err
	}
	store.OnChange(func() { c.Evaluate(ctx) })
	broker.OnConnect(func(_ string, p *peer.Peer) {
		// A (re)connected agent is a fresh process (lifecycle back to
		// Initialized): whatever failed before likely no longer applies.
		c.reset(p.CommonName())
		c.Evaluate(ctx)
	})
	broker.OnDisconnect(func(_ string, p *peer.Peer) { c.forget(p.CommonName()) })
	safe.Go(logger, "reconcile:node_run", func() {
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.Evaluate(ctx)
			}
		}
	})
	return c
}

// ForceNext makes cn's next push skip the reconcile-errors gate (the declarative
// carry-over of `node start --force`). Consumed by the push it enables.
func (c *NodeRunController) ForceNext(cn string) {
	c.mu.Lock()
	c.state(cn).forceOnce = true
	c.mu.Unlock()
}

func (c *NodeRunController) reset(cn string) {
	c.mu.Lock()
	*c.state(cn) = nodeRunState{}
	c.mu.Unlock()
}

func (c *NodeRunController) forget(cn string) {
	c.mu.Lock()
	delete(c.nodes, cn)
	c.mu.Unlock()
}

// state returns cn's bookkeeping, creating it. Callers hold c.mu.
func (c *NodeRunController) state(cn string) *nodeRunState {
	st := c.nodes[cn]
	if st == nil {
		st = &nodeRunState{}
		c.nodes[cn] = st
	}
	return st
}

// Evaluate walks the desired set once and pushes whatever is due. Level-triggered
// and serialized (one evaluation at a time); safe to call from any trigger.
func (c *NodeRunController) Evaluate(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for _, d := range c.store.Desired() {
		if d.DesiredRun == "" {
			continue // unmanaged: never touched
		}
		cn := d.CommonName
		st := c.state(cn)
		if d.RunGeneration != st.gen {
			// New generation = the operator (re)declared intent: clear any hold
			// and backoff so this declaration gets a fresh set of attempts.
			forceOnce := st.forceOnce
			*st = nodeRunState{gen: d.RunGeneration, forceOnce: forceOnce}
		}
		p, connected := c.broker.Find(cn)
		if !connected {
			continue // nothing to push; OnConnect converges it on arrival
		}
		obs := c.appStatus(cn)
		if obs == consts.AppStatusUnknown {
			continue // connected but not yet reported — don't act blind
		}
		switch d.DesiredRun {
		case RunStateRunning:
			if obs == consts.AppStatusRunning {
				c.converged(st, cn, now)
				continue
			}
			// Initialized / Stopped / Error (= a failed start; nothing running).
			if c.pushDue(st, now) {
				if rerrs := c.status.ErrorsForNodeExcept(cn, NodeRunResource); len(rerrs) > 0 && !st.forceOnce {
					// Same gate the imperative start had: don't start a node whose
					// declarative config failed to push. Not a failed attempt — it
					// retries every tick and unblocks the moment the config heals.
					c.status.Record(NodeRunResource, cn, now, fmt.Errorf("start blocked by reconcile errors (pass force=true to override): %s", strings.Join(rerrs, "; ")))
					continue
				}
				c.pushLocked(ctx, st, p, cn, true, now)
			}
		case RunStateStopped:
			if obs != consts.AppStatusRunning {
				// Stopped, Initialized (never started), or Error (start failed —
				// nothing is running): stopped is already satisfied.
				c.converged(st, cn, now)
				continue
			}
			if c.pushDue(st, now) {
				c.pushLocked(ctx, st, p, cn, false, now)
			}
		default:
			c.status.Record(NodeRunResource, cn, now, fmt.Errorf("unknown desired_run %q (want %q or %q)", d.DesiredRun, RunStateRunning, RunStateStopped))
		}
	}
}

// converged records success and clears failure bookkeeping.
func (c *NodeRunController) converged(st *nodeRunState, cn string, now time.Time) {
	if st.attempts != 0 || st.held {
		c.logger.Info("reconcile node_run: converged", "node", cn)
	}
	st.attempts = 0
	st.held = false
	st.forceOnce = false
	c.status.Record(NodeRunResource, cn, now, nil)
}

// pushDue reports whether a push may go out now (not held, past backoff, and
// past the settle window of the previous successful push).
func (c *NodeRunController) pushDue(st *nodeRunState, now time.Time) bool {
	if st.held || now.Before(st.nextTry) {
		return false
	}
	if !st.pushedAt.IsZero() && now.Sub(st.pushedAt) < nodeRunSettle {
		return false // stats have not caught up with the last push yet
	}
	return true
}

// pushLocked performs one Start/StopDataplane and updates backoff/hold state.
// Callers hold c.mu — the southbound call runs under it, serializing pushes
// (same inline-push model as the other reconcile loops).
func (c *NodeRunController) pushLocked(ctx context.Context, st *nodeRunState, p *peer.Peer, cn string, start bool, now time.Time) {
	verb := "stop"
	if start {
		verb = "start"
	}
	err := c.push(ctx, p, start)
	if err == nil {
		st.attempts = 0
		st.held = false
		st.forceOnce = false
		st.pushedAt = now
		c.status.Record(NodeRunResource, cn, now, nil)
		c.logger.Info("reconcile node_run: pushed", "node", cn, "op", verb)
		return
	}
	st.attempts++
	if st.attempts >= nodeRunMaxAttempts {
		st.held = true
		c.status.Record(NodeRunResource, cn, now, fmt.Errorf("%s held after %d failed attempts (last: %v) — re-run `node %s`, fix the node, or wait for it to reconnect", verb, st.attempts, err, verb))
		c.logger.Error("reconcile node_run: holding after repeated failures", "node", cn, "op", verb, "attempts", st.attempts, "error", err)
		return
	}
	backoff := nodeRunBaseBackoff << (st.attempts - 1)
	if backoff > nodeRunMaxBackoff {
		backoff = nodeRunMaxBackoff
	}
	st.nextTry = now.Add(backoff)
	c.status.Record(NodeRunResource, cn, now, fmt.Errorf("%s failed (attempt %d/%d, retry in %s): %v", verb, st.attempts, nodeRunMaxAttempts, backoff, err))
	c.logger.Warn("reconcile node_run: push failed", "node", cn, "op", verb, "attempt", st.attempts, "backoff", backoff, "error", err)
}
