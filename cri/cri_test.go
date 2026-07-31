package cri_test

// The mock server deliberately uses grpc-go + the reference (protoc-gen-go)
// stubs, so every test round-trip doubles as an interop check of our own
// h2 + GRPCFramer + pbg stack against a real gRPC implementation.

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/kscale/cri"
	api "github.com/on-keyday/kscale/protobuf/proto/cri"
	refv1 "github.com/on-keyday/kscale/protobuf/reference/proto/cri"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type mockRuntime struct {
	refv1.UnimplementedRuntimeServiceServer
	versionDelay time.Duration
}

func (m *mockRuntime) Version(ctx context.Context, req *refv1.VersionRequest) (*refv1.VersionResponse, error) {
	if m.versionDelay > 0 {
		select {
		case <-time.After(m.versionDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &refv1.VersionResponse{
		Version:           "0.1.0",
		RuntimeName:       "mockd",
		RuntimeVersion:    "v9.9.9",
		RuntimeApiVersion: "v1",
	}, nil
}

func (m *mockRuntime) StopPodSandbox(ctx context.Context, req *refv1.StopPodSandboxRequest) (*refv1.StopPodSandboxResponse, error) {
	return nil, status.Error(codes.NotFound, "no such sandbox: "+req.PodSandboxId)
}

func (m *mockRuntime) ListContainers(ctx context.Context, req *refv1.ListContainersRequest) (*refv1.ListContainersResponse, error) {
	// A "big" label selector requests a >1MiB response — combined with the
	// oversized request this exercises HTTP/2 flow control (WINDOW_UPDATE)
	// in both directions, since the initial window is only 64KiB.
	if req.Filter != nil && req.Filter.LabelSelector["big"] != "" {
		return &refv1.ListContainersResponse{
			Containers: []*refv1.Container{{
				Id:     "big",
				Labels: map[string]string{"data": strings.Repeat("x", 1<<20)},
			}},
		}, nil
	}
	return &refv1.ListContainersResponse{
		Containers: []*refv1.Container{
			{Id: "c1", Metadata: &refv1.ContainerMetadata{Name: "one"}, State: refv1.ContainerState_CONTAINER_RUNNING},
			{Id: "c2", Metadata: &refv1.ContainerMetadata{Name: "two"}, State: refv1.ContainerState_CONTAINER_EXITED},
		},
	}, nil
}

type mockImage struct {
	refv1.UnimplementedImageServiceServer
}

func (m *mockImage) ListImages(ctx context.Context, req *refv1.ListImagesRequest) (*refv1.ListImagesResponse, error) {
	return &refv1.ListImagesResponse{
		Images: []*refv1.Image{{Id: "img1"}},
	}, nil
}

func serveMock(t *testing.T, socketPath string, rt *mockRuntime) *grpc.Server {
	t.Helper()
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen %s: %v", socketPath, err)
	}
	s := grpc.NewServer()
	refv1.RegisterRuntimeServiceServer(s, rt)
	refv1.RegisterImageServiceServer(s, &mockImage{})
	go s.Serve(l)
	return s
}

func TestRoundTrip(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "cri.sock")
	s := serveMock(t, socketPath, &mockRuntime{})
	defer s.Stop()

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
	if ver.RuntimeName != "mockd" || ver.RuntimeVersion != "v9.9.9" {
		t.Fatalf("unexpected version response: %+v", ver)
	}

	// Second unary call on the same connection (stream id advances).
	containers, err := client.ListContainers(ctx, &api.ListContainersRequest{})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(containers.Containers) != 2 || containers.Containers[0].Id != "c1" {
		t.Fatalf("unexpected containers: %+v", containers.Containers)
	}
	if containers.Containers[1].State != api.ContainerState_CONTAINER_EXITED {
		t.Fatalf("unexpected container state: %v", containers.Containers[1].State)
	}

	// The other service on the same socket.
	images, err := client.ListImages(ctx, &api.ListImagesRequest{})
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(images.Images) != 1 || images.Images[0].Id != "img1" {
		t.Fatalf("unexpected images: %+v", images.Images)
	}
}

func TestFlowControl(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "cri.sock")
	s := serveMock(t, socketPath, &mockRuntime{})
	defer s.Stop()

	client, err := cri.Dial(socketPath, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	// Request >64KiB (send-side window) asking for a >1MiB response
	// (receive-side window).
	resp, err := client.ListContainers(context.Background(), &api.ListContainersRequest{
		Filter: &api.ContainerFilter{LabelSelector: map[string]string{
			"big": "yes",
			"pad": strings.Repeat("y", 200<<10),
		}},
	})
	if err != nil {
		t.Fatalf("ListContainers(big): %v", err)
	}
	if len(resp.Containers) != 1 || len(resp.Containers[0].Labels["data"]) != 1<<20 {
		t.Fatalf("unexpected big response: %d containers", len(resp.Containers))
	}
}

func TestStatusError(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "cri.sock")
	s := serveMock(t, socketPath, &mockRuntime{})
	defer s.Stop()

	client, err := cri.Dial(socketPath, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	_, err = client.StopPodSandbox(context.Background(), &api.StopPodSandboxRequest{PodSandboxId: "nope"})
	if err == nil {
		t.Fatal("expected error")
	}
	var st *cri.StatusError
	if !errors.As(err, &st) {
		t.Fatalf("expected *StatusError, got %T: %v", err, err)
	}
	if st.Code != int(codes.NotFound) {
		t.Fatalf("expected code %d, got %d (%v)", codes.NotFound, st.Code, st)
	}
	if st.Message != "no such sandbox: nope" {
		t.Fatalf("unexpected message: %q", st.Message)
	}

	// The connection must stay usable after a status error.
	if _, err := client.Version(context.Background(), &api.VersionRequest{}); err != nil {
		t.Fatalf("Version after status error: %v", err)
	}
}

func TestReconnect(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "cri.sock")
	s := serveMock(t, socketPath, &mockRuntime{})

	client, err := cri.Dial(socketPath, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	if _, err := client.Version(context.Background(), &api.VersionRequest{}); err != nil {
		t.Fatalf("Version before restart: %v", err)
	}

	// Kill the server (closes the client's connection) and bring up a new one
	// on the same path — the client must heal by re-dialing.
	s.Stop()
	s2 := serveMock(t, socketPath, &mockRuntime{})
	defer s2.Stop()

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err = client.Version(context.Background(), &api.VersionRequest{})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("client did not recover after server restart: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestReceiveTimeout(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "cri.sock")
	s := serveMock(t, socketPath, &mockRuntime{versionDelay: 2 * time.Second})
	defer s.Stop()

	client, err := cri.Dial(socketPath, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	client.SetReceiveTimeout(100 * time.Millisecond)

	start := time.Now()
	_, err = client.Version(context.Background(), &api.VersionRequest{})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timeout did not fire in time: %v (err=%v)", elapsed, err)
	}
}
