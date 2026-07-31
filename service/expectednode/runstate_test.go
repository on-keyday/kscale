package expectednode

import (
	"context"
	"testing"

	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
)

// TestSetDesiredRunSurvivesInventoryReapply: the deploy tooling re-applying the
// declared inventory must NOT wipe the run-state — desired_run/run_generation
// are readonly fields the generated Apply carries over. This is what keeps
// `node start` intent alive across Ansible runs.
func TestSetDesiredRunSurvivesInventoryReapply(t *testing.T) {
	h := New()
	ctx := context.Background()
	if _, err := h.Apply(ctx, &pbaccess.ResourceExpectedNodeActionApplyArgsDTO{CommonName: "s1", DpType: "l4lb"}); err != nil {
		t.Fatal(err)
	}
	h.SetDesiredRun(map[string]string{"s1": "l4lb"}, "running")

	// Deploy tooling re-applies the same inventory entry.
	if _, err := h.Apply(ctx, &pbaccess.ResourceExpectedNodeActionApplyArgsDTO{CommonName: "s1", DpType: "l4lb"}); err != nil {
		t.Fatal(err)
	}
	got, err := h.Get(ctx, &pbaccess.ResourceExpectedNodeActionGetArgsDTO{CommonName: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.DesiredRun != "running" || got.RunGeneration != 1 {
		t.Fatalf("after re-apply: desired_run=%q gen=%d, want running/1", got.DesiredRun, got.RunGeneration)
	}
}

// TestSetDesiredRunGenerationAndCreation: every call bumps run_generation (the
// operator's "try again" that clears a reconcile hold) even when the value is
// unchanged; an undeclared node is created so `node start` works on a node the
// inventory never listed; existing dp_type is preserved over the caller's.
func TestSetDesiredRunGenerationAndCreation(t *testing.T) {
	h := New()
	notified := 0
	h.OnChange(func() { notified++ })

	h.SetDesiredRun(map[string]string{"new.node": "popcache"}, "running")
	h.SetDesiredRun(map[string]string{"new.node": "SHOULD-NOT-OVERWRITE"}, "running")
	got, err := h.Get(context.Background(), &pbaccess.ResourceExpectedNodeActionGetArgsDTO{CommonName: "new.node"})
	if err != nil {
		t.Fatal(err)
	}
	if got.DpType != "popcache" || got.DesiredRun != "running" || got.RunGeneration != 2 {
		t.Fatalf("got %+v, want popcache/running/gen=2", got)
	}
	if notified != 2 {
		t.Fatalf("notify fired %d times, want once per batch", notified)
	}

	h.SetDesiredRun(map[string]string{"new.node": "popcache"}, "stopped")
	got, _ = h.Get(context.Background(), &pbaccess.ResourceExpectedNodeActionGetArgsDTO{CommonName: "new.node"})
	if got.DesiredRun != "stopped" || got.RunGeneration != 3 {
		t.Fatalf("got %+v, want stopped/gen=3", got)
	}
}
