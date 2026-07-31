package cri_test

import (
	"context"
	"os"
	"testing"

	"github.com/on-keyday/kscale/cri"
	api "github.com/on-keyday/kscale/protobuf/proto/cri"
)

// TestRealContainerd exercises the client against a real CRI endpoint using
// read-only RPCs. Opt-in:
//
//	CRI_SOCKET=/run/user/1000/docker/containerd/containerd.sock go test ./cri/ -run TestRealContainerd -v
func TestRealContainerd(t *testing.T) {
	socketPath := os.Getenv("CRI_SOCKET")
	if socketPath == "" {
		t.Skip("set CRI_SOCKET to run against a real containerd")
	}
	client, err := cri.Dial(socketPath, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	ctx := context.Background()

	ver, err := client.Version(ctx, &api.VersionRequest{Version: "0.1.0"})
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	t.Logf("runtime: %s %s (api %s)", ver.RuntimeName, ver.RuntimeVersion, ver.RuntimeApiVersion)

	containers, err := client.ListContainers(ctx, &api.ListContainersRequest{})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	t.Logf("containers: %d", len(containers.Containers))

	sandboxes, err := client.ListPodSandbox(ctx, &api.ListPodSandboxRequest{})
	if err != nil {
		t.Fatalf("ListPodSandbox: %v", err)
	}
	t.Logf("sandboxes: %d", len(sandboxes.Items))

	images, err := client.ListImages(ctx, &api.ListImagesRequest{})
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	t.Logf("images: %d", len(images.Images))
}
