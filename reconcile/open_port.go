package reconcile

import (
	"context"
	"log/slog"
	"time"

	"github.com/on-keyday/kscale/dpbroker"
	"github.com/on-keyday/kscale/peer"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/kscale/rpc"
	"github.com/on-keyday/kscale/service/openport"
)

// OpenPortDesired is the desired-state source the OpenPort controller reconciles
// (satisfied by the generated open_port store; schema.reconcile.custom keeps the loop
// hand-written here rather than generated).
type OpenPortDesired interface {
	Desired() []*pbaccess.ResourceOpenPortActionGetResponseDTO
	OnChange(func())
}

// OpenPort wires the open_port reconcile loop: push every desired ACL's ports to every
// router node via RouterService.DesireOpenPort (which SyncACLs the device), on desired
// change and on node connect. Hand-written (not generated) because the resource's
// []string "proto:port" ports must be parsed into the southbound []*PortInfo — a
// conversion the generated field-copy emitter can't express. Like the other reconcilers
// this is additive: an ACL dropped from desired is no longer pushed, but not removed
// from the device (RouterService has no whole-ACL delete).
func OpenPort(ctx context.Context, store OpenPortDesired, broker *dpbroker.Broker, status *Status, logger *slog.Logger) {
	push := func(p *peer.Peer) {
		c := pb.NewRouterServiceClient(rpc.NewTrsfStreamSource(p.Streams(), logger))
		var pushErr error
		for _, item := range store.Desired() {
			ports, err := openport.ParsePorts(item.Ports)
			if err != nil {
				logger.Error("reconcile open_port: bad ports", "acl", item.AclName, "error", err)
				pushErr = err
				continue
			}
			if _, err := c.DesireOpenPort(ctx, &pb.RouterServiceDesireOpenPortRequest{AclName: item.AclName, Ports: ports}); err != nil {
				logger.Error("reconcile open_port: DesireOpenPort failed", "acl", item.AclName, "error", err)
				pushErr = err
			}
		}
		status.Record("open_port", p.CommonName(), time.Now(), pushErr)
	}
	store.OnChange(func() {
		for _, p := range broker.List("router") {
			push(p)
		}
	})
	broker.OnConnect(func(dpType string, p *peer.Peer) {
		if dpbroker.Matches("router", dpType) {
			push(p) // converge the new node to current desired state
		}
	})
}
