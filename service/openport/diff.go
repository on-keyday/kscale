package openport

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/on-keyday/kscale/dpbroker"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/kscale/rpc"
)

// Service composes the generated open_port store (Handlers: CRUD + reconcile surface +
// persistence) with the hand-written Diff action, which needs broker access to query the
// southbound router service. The control plane registers this in place of the bare store
// so OpenPortService exposes diff; the inner *Handlers is still used directly for the
// reconcile loop, desired-state persistence, and ConfirmPush.
type Service struct {
	*Handlers
	Broker *dpbroker.Broker
	Logger *slog.Logger
}

var _ pb.OpenPortServiceServer = (*Service)(nil)

// Diff reports, per router node and desired ACL, what the device would change to converge
// (DesireOpenPort dry run -> ACLDiff add/remove lines). An empty result means every router
// is already in sync with the desired open_port state. Read-only: dry run makes no change.
func (s *Service) Diff(ctx context.Context, _ *pbaccess.ResourceOpenPortActionDiffArgsDTO) (*pbaccess.ResourceOpenPortActionDiffResponseDTO, error) {
	desired := s.Handlers.Desired()
	var entries []string
	for _, p := range s.Broker.List("router") {
		node := p.CommonName()
		c := pb.NewRouterServiceClient(rpc.NewTrsfStreamSource(p.Streams(), s.Logger))
		for _, item := range desired {
			ports, err := ParsePorts(item.Ports)
			if err != nil {
				entries = append(entries, fmt.Sprintf("%s %s: ERROR %v", node, item.AclName, err))
				continue
			}
			resp, err := c.DesireOpenPort(ctx, &pb.RouterServiceDesireOpenPortRequest{AclName: item.AclName, Ports: ports, DryRun: true})
			if err != nil {
				entries = append(entries, fmt.Sprintf("%s %s: ERROR %v", node, item.AclName, err))
				continue
			}
			d := resp.Diff
			if d == nil || (len(d.Add) == 0 && len(d.Remove) == 0) {
				continue // this ACL is in sync on this node
			}
			parts := make([]string, 0, len(d.Add)+len(d.Remove))
			for _, a := range d.Add {
				parts = append(parts, "+"+a)
			}
			for _, r := range d.Remove {
				parts = append(parts, "-"+r)
			}
			entries = append(entries, fmt.Sprintf("%s %s: %s", node, item.AclName, strings.Join(parts, " ")))
		}
	}
	return &pbaccess.ResourceOpenPortActionDiffResponseDTO{Entries: entries}, nil
}
