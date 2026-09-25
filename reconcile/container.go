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
)

// ContainerDesired is the desired-state source the Container controller reconciles
// (satisfied by the generated container store).
type ContainerDesired interface {
	Desired() []*pbaccess.ResourceContainerActionGetResponseDTO
	OnChange(func())
}

// Container wires the container reconcile loop. Unlike the per-item file plane,
// the workload agent's WorkloadService.ApplyContainers takes the FULL desired set
// for a node in one call, so this pushes each connected workload node its complete
// matching set — on desired change and on (re)connect. An empty set is a valid
// push: it removes every managed container on that node. Only workload dp_type
// nodes serve WorkloadService, so other node types are skipped. Custom (not
// generated) because of the per-node aggregation + the workload-only targeting.
func Container(ctx context.Context, store ContainerDesired, broker *dpbroker.Broker, status *Status, logger *slog.Logger) {
	// pushPeer sends peer p its complete desired container set (every desired
	// container whose Node selector matches p). Always called for workload peers,
	// even with an empty set.
	pushPeer := func(p *peer.Peer) {
		req := &pb.WorkloadServiceApplyContainersRequest{}
		for _, item := range store.Desired() {
			if broker.MatchSpecificity(item.Node, "workload", p) > 0 {
				req.Containers = append(req.Containers, &pb.WorkloadServiceContainerSpec{
					Name:    item.Name,
					Image:   item.Image,
					Command: item.Command,
					Args:    item.Args,
					Env:     item.Env,
					Mounts:  item.Mounts,
					Restart: item.Restart,
					Network: item.Network,
					Ports:   item.Ports,
				})
			}
		}
		c := pb.NewWorkloadServiceClient(rpc.NewTrsfStreamSource(p.Streams(), logger))
		_, err := c.ApplyContainers(ctx, req)
		status.Record("container", p.CommonName(), time.Now(), err)
		if err != nil {
			logger.Error("reconcile container: ApplyContainers failed", "node", p.CommonName(), "count", len(req.Containers), "error", err)
		}
	}

	pushAllWorkloadNodes := func() {
		broker.ForEachNode(func(dpType string, p *peer.Peer) {
			if dpType == "workload" {
				pushPeer(p)
			}
		})
	}

	store.OnChange(pushAllWorkloadNodes)
	broker.OnConnect(func(dpType string, p *peer.Peer) {
		if dpType == "workload" {
			pushPeer(p) // converge the (re)connected workload node to its desired set
		}
	})
}
