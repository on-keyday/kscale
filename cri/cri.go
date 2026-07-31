// Package cri is a client for the Kubernetes Container Runtime Interface
// (containerd, CRI-O, ...) spoken entirely over kscale's own stack:
// protobuf/wire/h2 for HTTP/2, wire.GRPCFramer for the gRPC message framing,
// and the pbg-generated CRI v1 stubs for the messages. No grpc-go and no
// x/net/http2 — see notes/ai/2026_07_07_container_workload_cri_design.md.
package cri

import (
	api "github.com/on-keyday/kscale/protobuf/proto/cri"
)

// CRI bundles the two CRI services every runtime endpoint serves on the same
// socket. It embeds both client interfaces, so all 45 RPCs are callable
// directly on it.
type CRI struct {
	api.RuntimeServiceClient
	api.ImageServiceClient
}

func NewCRI(runtime api.RuntimeServiceClient, image api.ImageServiceClient) *CRI {
	return &CRI{
		RuntimeServiceClient: runtime,
		ImageServiceClient:   image,
	}
}
