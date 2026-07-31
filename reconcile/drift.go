// Package reconcile — drift is the hand-written generic drift watcher shared by
// the generated status observers. For any declarative resource with a
// status_observed_from path, the generated <R>Observer exposes ObservedSets()
// (each node's actual reported set); WatchDrift compares that against the desired
// set (the resource's Store) and surfaces nodes that report items NOT desired
// — "extra" = real drift a node carries that the desired state never asked for.
package reconcile

import (
	"context"
	"log/slog"
	"time"

	"github.com/on-keyday/kscale/internal/safe"
	"github.com/prometheus/client_golang/prometheus"
)

// DriftSource is the observed-state side: the generated <R>Observer.ObservedSets.
type DriftSource interface {
	ObservedSets() map[string][]string // node CommonName -> its actually-reported items
}

// DesiredLister is the desired-state side: the resource's hand-written Store.
type DesiredLister interface {
	Desired() []string
}

// WatchDrift periodically diffs each node's observed set against the desired set
// and, for any node reporting undesired items (extra), logs a warning and sets the
// drift gauge kscale_resource_drift{resource,node} to the extra count (0 when a
// node is clean). Missing items (desired but not yet reported) are left to the
// reconcile loop's idempotent re-push, so they are not flagged as drift here.
// gauge may be nil (log-only).
func WatchDrift(ctx context.Context, resource string, desired DesiredLister, observed DriftSource, gauge *prometheus.GaugeVec, logger *slog.Logger, interval time.Duration) {
	safe.Go(logger, "drift-watch:"+resource, func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				want := map[string]struct{}{}
				for _, d := range desired.Desired() {
					want[d] = struct{}{}
				}
				for node, items := range observed.ObservedSets() {
					var extra []string
					for _, it := range items {
						if _, ok := want[it]; !ok {
							extra = append(extra, it)
						}
					}
					if gauge != nil {
						gauge.WithLabelValues(resource, node).Set(float64(len(extra)))
					}
					if len(extra) > 0 {
						logger.Warn("reconcile drift: node reports undesired items",
							"resource", resource, "node", node, "extra", extra)
					}
				}
			}
		}
	})
}
