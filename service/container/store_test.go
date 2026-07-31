package container_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/kscale/service/container"
)

// TestStoreRoundTrip locks in that the generated store carries the []string
// fields (command/args/env/mounts) intact through Apply → Get → List →
// Snapshot/Restore. Those repeated fields are the first use of []string in a
// kscale resource, so this guards the codegen path, not just this store.
func TestStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	h := container.New()

	apply := &pbaccess.ResourceContainerActionApplyArgsDTO{
		Name:    "web",
		Node:    "s2",
		Image:   "docker.io/library/nginx:latest",
		Command: []string{"/usr/sbin/nginx"},
		Args:    []string{"-g", "daemon off;"},
		Env:     []string{"TZ=UTC", "LOG_LEVEL=info"},
		Mounts:  []string{"/srv/www:/usr/share/nginx/html:ro"},
		Restart: "always",
	}
	if _, err := h.Apply(ctx, apply); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got, err := h.Get(ctx, &pbaccess.ResourceContainerActionGetArgsDTO{Name: "web"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Image != apply.Image || got.Restart != "always" {
		t.Fatalf("scalar mismatch: image=%q restart=%q", got.Image, got.Restart)
	}
	if !slices.Equal(got.Args, apply.Args) || !slices.Equal(got.Env, apply.Env) ||
		!slices.Equal(got.Command, apply.Command) || !slices.Equal(got.Mounts, apply.Mounts) {
		t.Fatalf("[]string field mismatch: %+v", got)
	}

	list, err := h.List(ctx, &pbaccess.ResourceContainerActionListArgsDTO{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Name != "web" {
		t.Fatalf("unexpected list: %+v", list.Items)
	}

	// Persistence round-trip (Snapshot/Restore back the CP's desired-state store).
	raw, err := h.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	h2 := container.New()
	if err := h2.Restore(raw); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	restored, err := h2.Get(ctx, &pbaccess.ResourceContainerActionGetArgsDTO{Name: "web"})
	if err != nil {
		t.Fatalf("Get after restore: %v", err)
	}
	if !slices.Equal(restored.Env, apply.Env) {
		t.Fatalf("env lost across persistence: %+v", restored.Env)
	}
	_ = json.RawMessage(raw)

	if _, err := h.Delete(ctx, &pbaccess.ResourceContainerActionDeleteArgsDTO{Name: "web"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if l, _ := h.List(ctx, &pbaccess.ResourceContainerActionListArgsDTO{}); len(l.Items) != 0 {
		t.Fatalf("expected empty after delete, got %d", len(l.Items))
	}
}
