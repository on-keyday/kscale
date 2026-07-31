package workload_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/on-keyday/kscale/cri"
	"github.com/on-keyday/kscale/workload"
)

// TestRealConverge drives the Engine against a real containerd, running an actual
// busybox container under host network (no CNI). Opt-in — needs a CRI-enabled
// containerd, runc, and image-pull egress:
//
//	CRI_SOCKET=/run/user/1000/kscri/containerd.sock go test ./workload/ -run TestRealConverge -v
func TestRealConverge(t *testing.T) {
	socket := os.Getenv("CRI_SOCKET")
	if socket == "" {
		t.Skip("set CRI_SOCKET to run against a real containerd")
	}
	client := cri.NewClient(socket, nil)
	e := workload.NewEngine(client, nil)
	ctx := context.Background()

	spec := workload.Spec{
		Name:    "kscale-selftest",
		Image:   "docker.io/library/busybox:latest",
		Command: []string{"sleep"},
		Args:    []string{"3600"},
		Env:     []string{"FOO=bar"},
		Restart: "always",
	}
	// Best-effort cleanup even if an assertion fails midway.
	defer func() {
		if err := e.Apply(context.Background(), nil); err != nil {
			t.Logf("cleanup: %v", err)
		}
	}()

	if err := e.Apply(ctx, []workload.Spec{spec}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Poll until the container reports RUNNING (image pull + start take a moment).
	var final workload.Status
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		list, err := e.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) == 1 {
			final = list[0]
			if final.State == "RUNNING" {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if final.State != "RUNNING" {
		t.Fatalf("container never reached RUNNING: %+v", final)
	}
	t.Logf("running: name=%s id=%s image=%s", final.Name, final.ContainerID, final.Image)

	// Idempotency against the real runtime: re-Apply the same spec keeps the same
	// container (no recreate).
	if err := e.Apply(ctx, []workload.Spec{spec}); err != nil {
		t.Fatalf("re-Apply: %v", err)
	}
	list, _ := e.List(ctx)
	if len(list) != 1 || list[0].ContainerID != final.ContainerID {
		t.Fatalf("re-Apply recreated the container: %+v", list)
	}

	// Removal: empty desired set tears it down.
	if err := e.Apply(ctx, nil); err != nil {
		t.Fatalf("remove Apply: %v", err)
	}
	if list, _ := e.List(ctx); len(list) != 0 {
		t.Fatalf("container not removed: %+v", list)
	}
}
