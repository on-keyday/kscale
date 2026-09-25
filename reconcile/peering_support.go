// Package reconcile — peering_support holds the hand-written runtime shared by the
// generated membership-driven peering controllers (peering_gen.go): the stat-cache
// accessor interface and the batch->DestEntry extraction. The per-binding loops
// and push helpers are generated from resource.yaml's `peering:` section.
package reconcile

import (
	"time"

	pbstat "github.com/on-keyday/kscale/protobuf/proto/stat"
	"github.com/on-keyday/kscale/stat"
)

// peeringStartupGrace is how long after a target first appears to a peering
// controller (after a CP start, or the target's reconnect) a push with NO sources
// is held back. Counting from the target, not from the CP start, matters: agents
// can take far longer than the grace to reconnect after a CP restart (~45s in the
// e2e harness), which a CP-start clock would already have used up. The stat cache
// starts empty and fills as nodes stream in; a target whose own stats arrive first would otherwise be
// pushed an empty source set — on a live CP restart the l4lb dest table went
// self-only (no popcache backends) for ~21s until the popcache stats landed
// (notes/bugs/bug_2026_09_25_cp_restart_l4lb_self_only_dests.md). After the grace an
// empty set is pushed as before (all sources really gone). Partial sets are still
// pushed during the grace: they shrink the table but keep serving.
const peeringStartupGrace = 30 * time.Second

// DestStatSource is the per-node stat snapshot the peering reconcile reads
// (satisfied by service/stats.Cache; an interface to avoid the import).
type DestStatSource interface {
	Get(commonName string) *pbstat.StatBatch
}

// destFromBatch merges a node's StatBatch (NetworkSpec rides the base entry,
// CdnAppRealtime a separate per-dp entry) into the single Stats the membership
// gate reads, then extracts its DestEntry.
func destFromBatch(batch *pbstat.StatBatch) (stat.DestEntry, error) {
	merged := &pbstat.Stats{}
	for _, s := range batch.Stats {
		if s.NetworkSpec != nil {
			merged.NetworkSpec = s.NetworkSpec
		}
		if s.CdnAppRealtime != nil {
			merged.CdnAppRealtime = s.CdnAppRealtime
		}
	}
	return stat.GetDestEntryFromStat(merged)
}
