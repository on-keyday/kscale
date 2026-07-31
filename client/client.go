// Package client is the resource-agnostic client plumbing for talking to a
// kscale control-plane peer: kubelet-style bootstrap enrollment, the mTLS
// handshake, peer wrapping, and building a StreamSource that any generated typed
// RPC client (pb.New{Resource}ServiceClient) can ride. Written once here so each
// resource's CLI/dispatch does not re-spell it (kscale charter, client codegen).
//
// Demo identity policy (which CommonName, which token, where the cert is cached)
// stays with the caller; this package only does the transport-level plumbing.
package client

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/on-keyday/kscale/ca"
	"github.com/on-keyday/kscale/peer"
	wire "github.com/on-keyday/kscale/protobuf/wire"
	"github.com/on-keyday/kscale/rpc"
	"github.com/on-keyday/objtrsf/objproto"
)

// Enroll returns the BootstrapInfo for commonName, reusing a cert previously
// cached at savePath if present (so repeated calls share one identity and avoid
// re-issuing the same CommonName), otherwise enrolling via the bootstrap token
// and caching the result. app is the cert usage/application carried into the
// handshake (it becomes the caller's roles=[app]).
func Enroll(ctx context.Context, ep objproto.Endpoint, cid objproto.ConnectionID, commonName string, token []byte, app, savePath string) (*ca.BootstrapInfo, error) {
	if savePath != "" {
		if data, err := os.ReadFile(savePath); err == nil {
			return ca.LoadCertInfo(data)
		}
	}
	boot, err := ca.BootstrapProtocol(ctx, cid, ep, "ed25519", commonName, token, app)
	if err != nil {
		return nil, fmt.Errorf("bootstrap enroll: %w", err)
	}
	if savePath != "" {
		if dumped, err := boot.DumpResult(); err == nil {
			_ = os.WriteFile(savePath, dumped, 0o600)
		}
	}
	return boot, nil
}

// Connect runs the mTLS handshake with boot and wraps the connection in a peer.
// It returns both the *peer.Peer — the connection handle, for lifecycle
// (p.Connection().Close()), the verified CommonName, and any future
// control-handler / extra streams — and a StreamSource ready to build typed RPC
// clients over it (rpc.NewTrsfStreamSource). Resource-agnostic.
func Connect(ctx context.Context, ep objproto.Endpoint, cid objproto.ConnectionID, app string, boot *ca.BootstrapInfo, pingInterval time.Duration, logger *slog.Logger) (*peer.Peer, *wire.StreamSource, error) {
	conn, err := ca.HandshakeProtocol(ctx, ep, cid, "client", app, boot)
	if err != nil {
		return nil, nil, fmt.Errorf("mTLS handshake: %w", err)
	}
	p := peer.WrapAcceptedConn(ctx, conn, false, pingInterval, nil, logger, nil)
	return p, rpc.NewTrsfStreamSource(p.Streams(), logger), nil
}
