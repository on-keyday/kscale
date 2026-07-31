package client

import (
	"crypto/tls"
	"fmt"
	"log/slog"

	"github.com/on-keyday/kscale/internal/demo"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/transport"
)

// Endpoint builds a client objproto.Endpoint and the server ConnectionID from a
// control-plane address. The address may be a full connection ID
// ("<transport>:<host>:<port>-<id>", e.g. "ws:cp.example:9444-*"), or a bare
// "host:port" which defaults to udp with a random id. Supported transports:
//
//	udp        — the default dataplane transport
//	ws / wss   — WebSocket (TCP); lets a client reach the CP over an SSH-forwarded
//	             TCP port when the UDP port is not routable (e.g. an admin on the CLI
//	             tunnelling to a lab CP). wss verifies the server cert via system roots.
//
// The transport now lives in the address itself, so callers need no separate flag.
func Endpoint(logger *slog.Logger, addr string) (objproto.ConnectionID, objproto.Endpoint, error) {
	cid, err := ResolveConnectionID(addr)
	if err != nil {
		return objproto.ConnectionID{}, nil, err
	}
	sess := objproto.NewEndpoint(logger, objproto.EndpointModeClient)
	switch cid.Transport {
	case "udp":
		if _, err := transport.UDPEndpointEx(sess, logger, 0, sess.GetSenderChannel()); err != nil {
			return objproto.ConnectionID{}, nil, err
		}
	case "ws", "wss":
		var tlsCfg *tls.Config
		if cid.Transport == "wss" {
			tlsCfg = &tls.Config{}
		}
		if err := transport.WebSocketEndpointEx(sess, nil, transport.WebSocketConfig{
			Logger: logger,
			Path:   demo.WebSocketPath,
			Mode:   objproto.EndpointModeClient,
			TLS:    tlsCfg,
		}, nil); err != nil {
			return objproto.ConnectionID{}, nil, err
		}
	default:
		return objproto.ConnectionID{}, nil, fmt.Errorf("unsupported transport %q in address %q", cid.Transport, addr)
	}
	return cid, sess, nil
}

// ResolveConnectionID parses a control-plane address into a server ConnectionID,
// generating a FRESH random id for a "-*" address each call. Endpoint uses it once
// at startup; a caller that needs an additional connection on an existing endpoint
// (e.g. a fresh admin connection for a remote shell) calls it again to get a
// distinct id — reusing the startup cid would collide ("connection already exists").
func ResolveConnectionID(addr string) (objproto.ConnectionID, error) {
	opt := objproto.ParseOption_AllowRandomID | objproto.ParseOption_ResolveAddr
	cid, err := objproto.ParseConnectionID(addr, opt)
	if err != nil {
		// Bare host:port → default to udp with a random id.
		cid, err = objproto.ParseConnectionID("udp:"+addr+"-*", opt)
		if err != nil {
			return objproto.ConnectionID{}, fmt.Errorf("parse control-plane address %q: %w", addr, err)
		}
	}
	return cid, nil
}
