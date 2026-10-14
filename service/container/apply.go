package container

import (
	"context"
	"fmt"

	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/kscale/workload"
)

// Apply refuses a spec the workload agent would refuse (raw hostPath mounts,
// malformed volumes / ports / network) before it is stored, so the caller gets
// the error instead of a desired entry that silently never converges. The agent
// validates again on its side: this is feedback, not the enforcement point.
func (s *Service) Apply(ctx context.Context, req *pbaccess.ResourceContainerActionApplyArgsDTO) (*pbaccess.ResourceContainerActionApplyResponseDTO, error) {
	spec := workload.Spec{
		Name: req.Name, Image: req.Image, Command: req.Command, Args: req.Args, Env: req.Env,
		Mounts: req.Mounts, Restart: req.Restart, Network: req.Network, Ports: req.Ports, Volumes: req.Volumes,
	}
	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("container %q: %w", req.Name, err)
	}
	return s.Handlers.Apply(ctx, req)
}
