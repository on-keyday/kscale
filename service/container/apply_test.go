package container_test

import (
	"context"
	"strings"
	"testing"

	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/kscale/service/container"
)

// TestApplyRejectsHostMounts: the control plane refuses a raw hostPath mount at
// apply time and stores nothing; a volume spec goes through.
func TestApplyRejectsHostMounts(t *testing.T) {
	ctx := context.Background()
	s := &container.Service{Handlers: container.New()}
	_, err := s.Apply(ctx, &pbaccess.ResourceContainerActionApplyArgsDTO{
		Name: "evil", Node: "*", Image: "busybox:1.37", Mounts: []string{"/:/host"},
	})
	if err == nil || !strings.Contains(err.Error(), "hostPath mounts are not allowed") {
		t.Fatalf("hostPath mount accepted (err=%v)", err)
	}
	if len(s.Desired()) != 0 {
		t.Fatalf("rejected spec was stored: %+v", s.Desired())
	}
	if _, err := s.Apply(ctx, &pbaccess.ResourceContainerActionApplyArgsDTO{
		Name: "ok", Node: "*", Image: "busybox:1.37", Volumes: []string{"data:/data"},
	}); err != nil {
		t.Fatalf("volume spec rejected: %v", err)
	}
	if d := s.Desired(); len(d) != 1 || d[0].Volumes[0] != "data:/data" {
		t.Fatalf("volume spec not stored intact: %+v", d)
	}
}
