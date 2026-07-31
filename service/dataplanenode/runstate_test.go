package dataplanenode

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/on-keyday/kscale/dpbroker"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
)

// sugarHandlers builds Handlers wired for the declarative start/stop path with
// no live connections — the declared inventory alone must be reachable.
func sugarHandlers(declared map[string]string) (*Handlers, *struct {
	nodes  map[string]string
	run    string
	forced []string
}) {
	rec := &struct {
		nodes  map[string]string
		run    string
		forced []string
	}{}
	h := &Handlers{
		Broker:        dpbroker.New(),
		Logger:        slog.Default(),
		ExpectedNodes: func() map[string]string { return declared },
		SetDesiredRun: func(nodes map[string]string, run string) { rec.nodes, rec.run = nodes, run },
		ForceRun:      func(cn string) { rec.forced = append(rec.forced, cn) },
	}
	return h, rec
}

// TestStartRecordsDesiredRunForDisconnectedDeclaredNodes: the declarative win
// over the imperative path — `node start` on a declared-but-offline node records
// intent (converged on connect) instead of erroring "no connected node".
func TestStartRecordsDesiredRunForDisconnectedDeclaredNodes(t *testing.T) {
	declared := map[string]string{
		"s1.l4lb.dp.system.kscale.local":     "l4lb",
		"s2.popcache.dp.system.kscale.local": "popcache",
	}
	h, rec := sugarHandlers(declared)
	resp, err := h.Start(context.Background(), &pbaccess.ResourceDataplaneNodeActionStartArgsDTO{CommonName: "*"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Nodes) != 2 || rec.run != "running" || len(rec.nodes) != 2 {
		t.Fatalf("resp=%+v recorded=%+v/%q", resp.Nodes, rec.nodes, rec.run)
	}
	if len(rec.forced) != 0 {
		t.Fatalf("force marked without --force: %v", rec.forced)
	}

	// dp_type group and short-label selectors resolve against the declared set.
	if _, err := h.Start(context.Background(), &pbaccess.ResourceDataplaneNodeActionStartArgsDTO{CommonName: "l4lb/*"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.nodes) != 1 || rec.nodes["s1.l4lb.dp.system.kscale.local"] != "l4lb" {
		t.Fatalf("l4lb/* resolved %+v", rec.nodes)
	}
	if _, err := h.Stop(context.Background(), &pbaccess.ResourceDataplaneNodeActionStopArgsDTO{CommonName: "s2"}); err != nil {
		t.Fatal(err)
	}
	if rec.run != "stopped" || len(rec.nodes) != 1 || rec.nodes["s2.popcache.dp.system.kscale.local"] != "popcache" {
		t.Fatalf("stop s2 recorded %+v/%q", rec.nodes, rec.run)
	}

	if _, err := h.Start(context.Background(), &pbaccess.ResourceDataplaneNodeActionStartArgsDTO{CommonName: "nosuch"}); err == nil {
		t.Fatal("unmatched selector must error")
	}
}

// TestStartForceAndConvergeFeedback: --force marks each target before the
// desired write; a node whose inline push failed comes back as an error naming
// it (the reconcile keeps retrying — the intent is still recorded).
func TestStartForceAndConvergeFeedback(t *testing.T) {
	declared := map[string]string{
		"s1.l4lb.dp.system.kscale.local": "l4lb",
		"s2.l4lb.dp.system.kscale.local": "l4lb",
	}
	h, rec := sugarHandlers(declared)
	h.ConfirmRunNode = func(cn string) error {
		if strings.HasPrefix(cn, "s2.") {
			return errors.New("start failed (attempt 1/5)")
		}
		return nil
	}
	resp, err := h.Start(context.Background(), &pbaccess.ResourceDataplaneNodeActionStartArgsDTO{CommonName: "*", Force: true})
	if err == nil || !strings.Contains(err.Error(), "s2.") {
		t.Fatalf("err = %v, want the failed node named", err)
	}
	if resp != nil {
		t.Fatalf("resp = %+v, want nil alongside the error", resp)
	}
	if len(rec.forced) != 2 {
		t.Fatalf("forced = %v, want both targets marked", rec.forced)
	}
	if rec.run != "running" {
		t.Fatalf("desired not recorded despite partial failure: %q", rec.run)
	}
}
