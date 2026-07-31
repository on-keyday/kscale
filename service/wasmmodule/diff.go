package wasmmodule

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/on-keyday/kscale/dpbroker"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	wkt "github.com/on-keyday/kscale/protobuf/wkt"
	"github.com/on-keyday/kscale/rpc"
)

// Service composes the generated wasm_module store (Handlers: CRUD + reconcile surface +
// persistence) with the hand-written Diff action, which needs broker access to query the
// southbound wasm service. The control plane registers this in place of the bare store so
// WasmModuleService exposes diff; the inner *Handlers is still used directly for the
// reconcile loop, desired-state persistence, and ConfirmPush.
type Service struct {
	*Handlers
	Broker *dpbroker.Broker
	Logger *slog.Logger
}

var _ pb.WasmModuleServiceServer = (*Service)(nil)

// Diff reports, per popcache node, how the node's ACTUAL attached WASM modules (queried
// live via WasmService.ListAttached) diverge from the DESIRED wasm_module set (every
// desired module is registered on every popcache node). Per divergent module it emits one
// line:
//
//	<node> module <id>: +missing (desired, not attached)
//	<node> module <id>: -extra   (attached, not desired — pending prune)
//	<node> module <id>: path desired=/a actual=/b [method ... match_type ... binary ...]
//
// An empty result means every popcache node is in sync with the desired state. Read-only:
// it only queries (ListAttached), never applies — the converge loop is what reconciles.
func (s *Service) Diff(ctx context.Context, _ *pbaccess.ResourceWasmModuleActionDiffArgsDTO) (*pbaccess.ResourceWasmModuleActionDiffResponseDTO, error) {
	desired := map[uint32]*pbaccess.ResourceWasmModuleActionGetResponseDTO{}
	for _, item := range s.Handlers.Desired() {
		desired[item.ModuleId] = item
	}
	var entries []string
	for _, p := range s.Broker.List("popcache") {
		node := p.CommonName()
		c := pb.NewWasmServiceClient(rpc.NewTrsfStreamSource(p.Streams(), s.Logger))
		resp, err := c.ListAttached(ctx, &wkt.Empty{})
		if err != nil {
			entries = append(entries, fmt.Sprintf("%s: ERROR %v", node, err))
			continue
		}
		actual := map[uint32]*pb.WasmInfo{}
		for _, a := range resp.Info {
			actual[a.Id] = a
		}
		for _, id := range unionSortedIDs(desired, actual) {
			d, wantIt := desired[id]
			a, haveIt := actual[id]
			switch {
			case wantIt && !haveIt:
				entries = append(entries, fmt.Sprintf("%s module %d: +missing (desired, not attached)", node, id))
			case !wantIt && haveIt:
				entries = append(entries, fmt.Sprintf("%s module %d: -extra (attached, not desired)", node, id))
			default: // attached on the node — check routing/binary drift
				var notes []string
				if d.Path != a.Path {
					notes = append(notes, fmt.Sprintf("path desired=%s actual=%s", d.Path, a.Path))
				}
				if d.Method != a.Method {
					notes = append(notes, fmt.Sprintf("method desired=%s actual=%s", d.Method, a.Method))
				}
				if d.MatchType != a.MatchType {
					notes = append(notes, fmt.Sprintf("match_type desired=%s actual=%s", d.MatchType, a.MatchType))
				}
				if d.FilePath != a.BinaryPath {
					notes = append(notes, fmt.Sprintf("binary desired=%s actual=%s", d.FilePath, a.BinaryPath))
				}
				if dh, ah := strings.Join(d.AllowedHosts, ","), strings.Join(a.AllowedHosts, ","); dh != ah {
					notes = append(notes, fmt.Sprintf("allowed_hosts desired=[%s] actual=[%s]", dh, ah))
				}
				if d.ComputeBudgetMs != a.ComputeBudgetMs {
					notes = append(notes, fmt.Sprintf("compute_budget_ms desired=%d actual=%d", d.ComputeBudgetMs, a.ComputeBudgetMs))
				}
				if len(notes) > 0 {
					entries = append(entries, fmt.Sprintf("%s module %d: %s", node, id, strings.Join(notes, " ")))
				}
			}
		}
	}
	return &pbaccess.ResourceWasmModuleActionDiffResponseDTO{Entries: entries}, nil
}

// unionSortedIDs returns the sorted union of module ids across the desired and actual
// maps, so Diff output is deterministic.
func unionSortedIDs(desired map[uint32]*pbaccess.ResourceWasmModuleActionGetResponseDTO, actual map[uint32]*pb.WasmInfo) []uint32 {
	set := map[uint32]struct{}{}
	for id := range desired {
		set[id] = struct{}{}
	}
	for id := range actual {
		set[id] = struct{}{}
	}
	ids := make([]uint32, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
