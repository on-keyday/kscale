// Package reconcile — peering_support holds the hand-written runtime shared by the
// generated membership-driven peering controllers (peering_gen.go): the stat-cache
// accessor interface and the batch->DestEntry extraction. The per-binding loops
// and push helpers are generated from resource.yaml's `peering:` section.
package reconcile

import (
	pbstat "github.com/on-keyday/kscale/protobuf/proto/stat"
	"github.com/on-keyday/kscale/stat"
)

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
