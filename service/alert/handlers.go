// Package alert backs AlertService — a read-only list of the control plane's currently
// active alerts, derived by the deterministic evaluator in the top-level alert package.
package alert

import (
	"context"
	"time"

	eval "github.com/on-keyday/kscale/alert"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
)

type Handlers struct {
	pb.UnimplementedAlertServiceServer
	Eval *eval.Evaluator
}

var _ pb.AlertServiceServer = (*Handlers)(nil)

func (h *Handlers) List(ctx context.Context, _ *pbaccess.ResourceAlertActionListArgsDTO) (*pbaccess.ResourceAlertActionListResponseDTO, error) {
	active := h.Eval.Active()
	items := make([]*pbaccess.ResourceAlertActionGetResponseDTO, 0, len(active))
	for _, a := range active {
		items = append(items, &pbaccess.ResourceAlertActionGetResponseDTO{
			Kind:     a.Kind,
			Severity: a.Severity,
			Node:     a.Node,
			Message:  a.Message,
			Age:      time.Since(a.Since).Round(time.Second).String(),
		})
	}
	return &pbaccess.ResourceAlertActionListResponseDTO{Items: items}, nil
}
