package container

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/on-keyday/kscale/dpbroker"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	wkt "github.com/on-keyday/kscale/protobuf/wkt"
	"github.com/on-keyday/kscale/rpc"
)

// Service composes the generated container store (Handlers: CRUD + reconcile surface +
// persistence) with the hand-written Diff action, which needs broker access to query the
// southbound workload service. The control plane registers this in place of the bare
// store so ContainerService exposes diff; the inner *Handlers is still used directly for
// the reconcile loop, desired-state persistence, and ConfirmPush.
type Service struct {
	*Handlers
	Broker *dpbroker.Broker
	Logger *slog.Logger
}

var _ pb.ContainerServiceServer = (*Service)(nil)

// Diff reports, per workload node, how the node's ACTUAL CRI container state (queried
// live via WorkloadService.ListContainers, which returns only kscale-managed containers)
// diverges from the DESIRED container set matching that node. Per divergent container it
// emits one line:
//
//	<node> <name>: +missing (desired, not present[; pulling X | ; pull of X failed: E, retry in Ns])
//	<node> <name>: -extra   (kscale-managed but not desired — pending removal)
//	<node> <name>: state=EXITED [image desired=X actual=Y] [(pulling X)]   (present but not RUNNING / image drift / its replacement image still pulling)
//
// An empty result means every workload node is in sync with the desired state. Read-only:
// it only queries (ListContainers), never applies — the converge loop is what reconciles.
func (s *Service) Diff(ctx context.Context, _ *pbaccess.ResourceContainerActionDiffArgsDTO) (*pbaccess.ResourceContainerActionDiffResponseDTO, error) {
	var entries []string
	for _, p := range s.Broker.List("workload") {
		node := p.CommonName()

		// Desired set for this node — same specificity match reconcile uses to push.
		desired := map[string]*pbaccess.ResourceContainerActionGetResponseDTO{}
		for _, item := range s.Handlers.Desired() {
			if s.Broker.MatchSpecificity(item.Node, "workload", p) > 0 {
				desired[item.Name] = item
			}
		}

		// Actual CRI state reported by the node.
		c := pb.NewWorkloadServiceClient(rpc.NewTrsfStreamSource(p.Streams(), s.Logger))
		resp, err := c.ListContainers(ctx, &wkt.Empty{})
		if err != nil {
			entries = append(entries, fmt.Sprintf("%s: ERROR %v", node, err))
			continue
		}
		actual := map[string]*pb.WorkloadServiceContainerStatus{}
		for _, a := range resp.Containers {
			actual[a.Name] = a
		}

		for _, name := range unionSortedNames(desired, actual) {
			d, wantIt := desired[name]
			a, haveIt := actual[name]
			switch {
			case wantIt && (!haveIt || a.ContainerId == ""):
				// No container yet; an entry without one only reports a pending pull.
				reason := "desired, not present"
				if haveIt && a.PullState != "" {
					reason += "; " + pullNote(a)
				}
				entries = append(entries, fmt.Sprintf("%s %s: +missing (%s)", node, name, reason))
			case !wantIt && haveIt:
				entries = append(entries, fmt.Sprintf("%s %s: -extra (present, not desired)", node, name))
			default: // present in both — check running + image drift
				var notes []string
				if a.State != "RUNNING" {
					notes = append(notes, "state="+a.State)
				}
				if d.Image != a.Image {
					notes = append(notes, fmt.Sprintf("image desired=%s actual=%s", d.Image, a.Image))
				}
				if a.PullState != "" {
					notes = append(notes, "("+pullNote(a)+")")
				}
				if len(notes) > 0 {
					entries = append(entries, fmt.Sprintf("%s %s: %s", node, name, strings.Join(notes, " ")))
				}
			}
		}
	}
	return &pbaccess.ResourceContainerActionDiffResponseDTO{Entries: entries}, nil
}

// pullNote renders a pending background image pull: "pulling X", or "pull of X
// failed: <err>, retry in Ns".
func pullNote(a *pb.WorkloadServiceContainerStatus) string {
	if a.PullState != "FAILED" {
		return "pulling " + a.PullImage
	}
	note := fmt.Sprintf("pull of %s failed: %s", a.PullImage, a.PullError)
	if a.PullRetryAtUnixMs > 0 {
		in := max(time.Until(time.UnixMilli(a.PullRetryAtUnixMs)), 0).Round(time.Second)
		note += fmt.Sprintf(", retry in %s", in)
	}
	return note
}

// unionSortedNames returns the sorted union of keys across the desired and actual maps,
// so Diff output is deterministic.
func unionSortedNames(desired map[string]*pbaccess.ResourceContainerActionGetResponseDTO, actual map[string]*pb.WorkloadServiceContainerStatus) []string {
	set := map[string]struct{}{}
	for n := range desired {
		set[n] = struct{}{}
	}
	for n := range actual {
		set[n] = struct{}{}
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
