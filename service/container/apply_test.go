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

// TestApplyDevices: a malformed device name is refused at apply time; a CDI name
// is stored intact and comes back from Get.
func TestApplyDevices(t *testing.T) {
	ctx := context.Background()
	s := &container.Service{Handlers: container.New()}
	_, err := s.Apply(ctx, &pbaccess.ResourceContainerActionApplyArgsDTO{
		Name: "bad", Node: "*", Image: "busybox:1.37", Devices: []string{"/dev/nvidia0"},
	})
	if err == nil || !strings.Contains(err.Error(), "CDI name") {
		t.Fatalf("malformed device accepted (err=%v)", err)
	}
	if len(s.Desired()) != 0 {
		t.Fatalf("rejected spec was stored: %+v", s.Desired())
	}
	if _, err := s.Apply(ctx, &pbaccess.ResourceContainerActionApplyArgsDTO{
		Name: "llm", Node: "*", Image: "llama:cuda", Devices: []string{"nvidia.com/gpu=all"},
	}); err != nil {
		t.Fatalf("CDI device rejected: %v", err)
	}
	got, err := s.Get(ctx, &pbaccess.ResourceContainerActionGetArgsDTO{Name: "llm"})
	if err != nil || len(got.Devices) != 1 || got.Devices[0] != "nvidia.com/gpu=all" {
		t.Fatalf("device not stored intact: %+v, %v", got, err)
	}
}
