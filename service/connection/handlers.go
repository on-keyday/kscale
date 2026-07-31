// Package connection backs ConnectionService — a flat list of EVERY live transport
// connection on the control plane endpoint (ep.ListActiveConnections()), as opposed to
// dataplane_node's one-per-node broker view. Multiple connections per node are normal (the
// main connection, the cert-renewal connection, and any not-yet-GC'd reconnect remnants),
// so this is the view for spotting connection churn / leaks. common_name is filled by
// cross-referencing the broker; a connection that matches no live node (renewal / orphan)
// is left empty.
package connection

import (
	"context"
	"fmt"
	"time"

	"github.com/on-keyday/kscale/dpbroker"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/objtrsf/objproto"
)

type Handlers struct {
	pb.UnimplementedConnectionServiceServer
	Endpoint objproto.Endpoint
	Broker   *dpbroker.Broker
}

var _ pb.ConnectionServiceServer = (*Handlers)(nil)

// cnByConn maps each live node peer's connection id -> its common name (only dataplane
// nodes; admin / monitor / cert-renewal connections stay unlabeled).
func (h *Handlers) cnByConn() map[string]string {
	m := make(map[string]string)
	for _, n := range h.Broker.Nodes() {
		m[n.ConnID] = n.CommonName
	}
	return m
}

func item(c objproto.Connection, cnByConn map[string]string) *pbaccess.ResourceConnectionActionGetResponseDTO {
	cid := c.ConnectionID()
	return &pbaccess.ResourceConnectionActionGetResponseDTO{
		ConnectionId:  cid.String(),
		RemoteAddress: cid.Addr.String(),
		CommonName:    cnByConn[cid.String()],
		Age:           time.Since(c.ConnectedAt()).Round(time.Second).String(),
		Active:        c.IsActive(),
	}
}

func (h *Handlers) List(ctx context.Context, _ *pbaccess.ResourceConnectionActionListArgsDTO) (*pbaccess.ResourceConnectionActionListResponseDTO, error) {
	cn := h.cnByConn()
	conns := h.Endpoint.ListActiveConnections()
	items := make([]*pbaccess.ResourceConnectionActionGetResponseDTO, 0, len(conns))
	for _, c := range conns {
		items = append(items, item(c, cn))
	}
	return &pbaccess.ResourceConnectionActionListResponseDTO{Items: items}, nil
}

func (h *Handlers) Get(ctx context.Context, req *pbaccess.ResourceConnectionActionGetArgsDTO) (*pbaccess.ResourceConnectionActionGetResponseDTO, error) {
	cn := h.cnByConn()
	for _, c := range h.Endpoint.ListActiveConnections() {
		if c.ConnectionID().String() == req.ConnectionId {
			return item(c, cn), nil
		}
	}
	return nil, fmt.Errorf("connection %q not found", req.ConnectionId)
}
