package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/kscale/consts"
	"github.com/on-keyday/kscale/peer"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
)

type fakeRunStore struct {
	items []*pbaccess.ResourceExpectedNodeActionGetResponseDTO
}

func (f *fakeRunStore) Desired() []*pbaccess.ResourceExpectedNodeActionGetResponseDTO {
	return f.items
}
func (f *fakeRunStore) OnChange(func()) {}

type fakeRunBroker struct {
	connected map[string]bool
}

func (f *fakeRunBroker) Find(cn string) (*peer.Peer, bool)     { return nil, f.connected[cn] }
func (f *fakeRunBroker) OnConnect(func(string, *peer.Peer))    {}
func (f *fakeRunBroker) OnDisconnect(func(string, *peer.Peer)) {}

type pushCall struct {
	start bool
}

// newTestController builds a controller with a recording push and zero backoff
// (so consecutive Evaluate calls exercise the attempt counter directly).
func newTestController(t *testing.T, store *fakeRunStore, broker *fakeRunBroker, appStatus map[string]consts.AppStatus, pushErr *error) (*NodeRunController, *[]pushCall, *Status) {
	t.Helper()
	saveBase, saveSettle := nodeRunBaseBackoff, nodeRunSettle
	nodeRunBaseBackoff, nodeRunSettle = 0, 0
	t.Cleanup(func() { nodeRunBaseBackoff, nodeRunSettle = saveBase, saveSettle })

	status := NewStatus()
	c := &NodeRunController{
		store:  store,
		broker: broker,
		appStatus: func(cn string) consts.AppStatus {
			return appStatus[cn]
		},
		status: status,
		logger: slog.Default(),
		nodes:  map[string]*nodeRunState{},
	}
	var calls []pushCall
	c.push = func(_ context.Context, _ *peer.Peer, start bool) error {
		calls = append(calls, pushCall{start: start})
		if pushErr != nil {
			return *pushErr
		}
		return nil
	}
	return c, &calls, status
}

func desired(cn, run string, gen uint64) *pbaccess.ResourceExpectedNodeActionGetResponseDTO {
	return &pbaccess.ResourceExpectedNodeActionGetResponseDTO{CommonName: cn, DesiredRun: run, RunGeneration: gen}
}

// TestNodeRunStartsInitializedNode: the core promise — desired running + observed
// Initialized (fresh agent) gets exactly one StartDataplane, and the settle
// window stops an immediate duplicate while stats lag (Start is not idempotent).
func TestNodeRunStartsInitializedNode(t *testing.T) {
	store := &fakeRunStore{items: []*pbaccess.ResourceExpectedNodeActionGetResponseDTO{desired("s1", RunStateRunning, 1)}}
	broker := &fakeRunBroker{connected: map[string]bool{"s1": true}}
	obs := map[string]consts.AppStatus{"s1": consts.AppStatusInitialized}
	c, calls, _ := newTestController(t, store, broker, obs, nil)
	nodeRunSettle = time.Minute // real settle: the 2nd Evaluate must not re-push

	c.Evaluate(context.Background())
	if len(*calls) != 1 || !(*calls)[0].start {
		t.Fatalf("calls = %+v, want one start", *calls)
	}
	c.Evaluate(context.Background()) // stats still say Initialized — settle window holds
	if len(*calls) != 1 {
		t.Fatalf("re-pushed during settle window: %+v", *calls)
	}
	obs["s1"] = consts.AppStatusRunning // stats caught up
	c.Evaluate(context.Background())
	if len(*calls) != 1 {
		t.Fatalf("pushed a running node: %+v", *calls)
	}
}

// TestNodeRunLeavesUnmanagedAndDisconnectedAlone: desired_run "" never touches a
// node; a disconnected declared node gets nothing pushed (converged on connect).
func TestNodeRunLeavesUnmanagedAndDisconnectedAlone(t *testing.T) {
	store := &fakeRunStore{items: []*pbaccess.ResourceExpectedNodeActionGetResponseDTO{
		desired("unmanaged", "", 0),
		desired("offline", RunStateRunning, 1),
	}}
	broker := &fakeRunBroker{connected: map[string]bool{"unmanaged": true}}
	obs := map[string]consts.AppStatus{"unmanaged": consts.AppStatusInitialized}
	c, calls, _ := newTestController(t, store, broker, obs, nil)

	c.Evaluate(context.Background())
	if len(*calls) != 0 {
		t.Fatalf("pushed to unmanaged/disconnected node: %+v", *calls)
	}
}

// TestNodeRunUnknownStatusIsNotActedOn: a connected node that has not reported
// stats yet is never pushed blind (it might already be running; Start errors on
// a redundant call).
func TestNodeRunUnknownStatusIsNotActedOn(t *testing.T) {
	store := &fakeRunStore{items: []*pbaccess.ResourceExpectedNodeActionGetResponseDTO{desired("s1", RunStateRunning, 1)}}
	broker := &fakeRunBroker{connected: map[string]bool{"s1": true}}
	c, calls, _ := newTestController(t, store, broker, map[string]consts.AppStatus{}, nil)

	c.Evaluate(context.Background())
	if len(*calls) != 0 {
		t.Fatalf("pushed with Unknown status: %+v", *calls)
	}
}

// TestNodeRunStoppedSemantics: desired stopped is satisfied by Stopped,
// Initialized AND Error (Error = a failed start, nothing is running — the agreed
// design: never poke an errored node to make it "more stopped"); only an
// actually Running node gets StopDataplane.
func TestNodeRunStoppedSemantics(t *testing.T) {
	for _, satisfied := range []consts.AppStatus{consts.AppStatusStopped, consts.AppStatusInitialized, consts.AppStatusError} {
		store := &fakeRunStore{items: []*pbaccess.ResourceExpectedNodeActionGetResponseDTO{desired("s1", RunStateStopped, 1)}}
		broker := &fakeRunBroker{connected: map[string]bool{"s1": true}}
		c, calls, _ := newTestController(t, store, broker, map[string]consts.AppStatus{"s1": satisfied}, nil)
		c.Evaluate(context.Background())
		if len(*calls) != 0 {
			t.Fatalf("status %v: pushed %+v, want none", satisfied, *calls)
		}
	}
	store := &fakeRunStore{items: []*pbaccess.ResourceExpectedNodeActionGetResponseDTO{desired("s1", RunStateStopped, 1)}}
	broker := &fakeRunBroker{connected: map[string]bool{"s1": true}}
	c, calls, _ := newTestController(t, store, broker, map[string]consts.AppStatus{"s1": consts.AppStatusRunning}, nil)
	c.Evaluate(context.Background())
	if len(*calls) != 1 || (*calls)[0].start {
		t.Fatalf("calls = %+v, want one stop", *calls)
	}
}

// TestNodeRunBackoffThenHold: repeated start failures increment attempts and
// end in a hold — no further pushes, with the hold visible in Status. Error
// observed status keeps being retried (transient failures self-heal) until the
// attempt budget runs out.
func TestNodeRunBackoffThenHold(t *testing.T) {
	store := &fakeRunStore{items: []*pbaccess.ResourceExpectedNodeActionGetResponseDTO{desired("s1", RunStateRunning, 1)}}
	broker := &fakeRunBroker{connected: map[string]bool{"s1": true}}
	obs := map[string]consts.AppStatus{"s1": consts.AppStatusError}
	pushErr := errors.New("xdp attach failed")
	c, calls, status := newTestController(t, store, broker, obs, &pushErr)

	for i := 0; i < nodeRunMaxAttempts+3; i++ {
		c.Evaluate(context.Background())
	}
	if len(*calls) != nodeRunMaxAttempts {
		t.Fatalf("pushes = %d, want exactly %d then hold", len(*calls), nodeRunMaxAttempts)
	}
	if err := status.NodeError(NodeRunResource, "s1"); err == nil || !strings.Contains(err.Error(), "held after") {
		t.Fatalf("status = %v, want a held error", err)
	}

	// Generation bump (operator re-runs `node start`) clears the hold and retries.
	store.items[0] = desired("s1", RunStateRunning, 2)
	c.Evaluate(context.Background())
	if len(*calls) != nodeRunMaxAttempts+1 {
		t.Fatalf("generation bump did not retry: pushes = %d", len(*calls))
	}

	// Reconnect (fresh agent process) also clears the hold.
	for i := 0; i < nodeRunMaxAttempts; i++ {
		c.Evaluate(context.Background())
	}
	before := len(*calls)
	c.reset("s1")
	c.Evaluate(context.Background())
	if len(*calls) != before+1 {
		t.Fatalf("reset did not clear the hold: pushes = %d, want %d", len(*calls), before+1)
	}
}

// TestNodeRunGateAndForce: a node with OTHER resources' reconcile errors is not
// started (same protection the imperative path had) but is not counted as a
// failed attempt; ForceNext lets the next push through once. node_run's own
// prior error must NOT gate (its backoff handles that).
func TestNodeRunGateAndForce(t *testing.T) {
	store := &fakeRunStore{items: []*pbaccess.ResourceExpectedNodeActionGetResponseDTO{desired("s1", RunStateRunning, 1)}}
	broker := &fakeRunBroker{connected: map[string]bool{"s1": true}}
	obs := map[string]consts.AppStatus{"s1": consts.AppStatusInitialized}
	c, calls, status := newTestController(t, store, broker, obs, nil)

	status.Record("vip", "s1", time.Now(), errors.New("push failed"))
	c.Evaluate(context.Background())
	if len(*calls) != 0 {
		t.Fatalf("started a misconfigured node: %+v", *calls)
	}
	if err := status.NodeError(NodeRunResource, "s1"); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("status = %v, want a blocked message", err)
	}

	c.ForceNext("s1")
	c.Evaluate(context.Background())
	if len(*calls) != 1 || !(*calls)[0].start {
		t.Fatalf("force did not push: %+v", *calls)
	}

	// The config error healing unblocks without force.
	c2, calls2, status2 := newTestController(t, store, broker, obs, nil)
	status2.Record("vip", "s1", time.Now(), errors.New("push failed"))
	c2.Evaluate(context.Background())
	status2.Record("vip", "s1", time.Now(), nil)
	c2.Evaluate(context.Background())
	if len(*calls2) != 1 {
		t.Fatalf("healed config did not unblock: %+v", *calls2)
	}
}

// TestNodeRunUnknownDesiredValue: a bogus desired_run never pushes and surfaces
// as a status error instead of being silently ignored.
func TestNodeRunUnknownDesiredValue(t *testing.T) {
	store := &fakeRunStore{items: []*pbaccess.ResourceExpectedNodeActionGetResponseDTO{desired("s1", "runnning", 1)}}
	broker := &fakeRunBroker{connected: map[string]bool{"s1": true}}
	c, calls, status := newTestController(t, store, broker, map[string]consts.AppStatus{"s1": consts.AppStatusInitialized}, nil)

	c.Evaluate(context.Background())
	if len(*calls) != 0 {
		t.Fatalf("pushed on bogus desired_run: %+v", *calls)
	}
	if err := status.NodeError(NodeRunResource, "s1"); err == nil || !strings.Contains(err.Error(), "unknown desired_run") {
		t.Fatalf("status = %v, want unknown desired_run", err)
	}
}
