package peer

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/on-keyday/kscale/access"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
	"github.com/on-keyday/objtrsf/trsf/wire"
)

type Peer struct {
	client   Client
	policy   *access.ContextCollector // per-connection ABAC collector; carries the authenticated User (server-side)
	rtt      atomic.Int64             // latest measured round-trip time (ns), updated on each Pong; 0 = no sample yet
	ctx      context.Context          // per-connection context, cancelled when this connection dies (see WrapAcceptedConn)
	cancel   context.CancelFunc       // cancels ctx; idempotent
	lastPong atomic.Int64             // unixnano of the last received pong — liveness signal for livenessLoop
}

// RTT returns the most recent round-trip time measured from the ping/pong loop,
// or 0 if no Pong has been observed yet. Set atomically by the Pong handler.
func (p *Peer) RTT() time.Duration { return time.Duration(p.rtt.Load()) }

func (p *Peer) setRTT(d time.Duration) { p.rtt.Store(int64(d)) }

// livenessTimeout mirrors ksdk's per-message ReceiveMessageTimeout: if the peer
// does not answer our ping within this window the connection is presumed dead and
// torn down so the caller reconnects. ksdk detected dead peers via a 30s read
// timeout on its main receive loop (agent/client/control.go); kscale's recompose
// split receiving into a background AutoReceive that never timed out, silently
// wedging dp agents whose CP connection died — the endpoint GC reaped the
// connection but nothing unblocked serve(), so Run()'s reconnect loop never fired.
const livenessTimeout = 30 * time.Second

// Context returns the per-connection context. It is cancelled when this
// connection dies — transport error, peer Close (io.EOF), or liveness timeout.
// Serve loops MUST pass it to AcceptBidirectionalStream (not the parent ctx) so a
// dead connection unblocks them and the caller can reconnect. Defensive fallback
// to Background for a zero-value Peer.
func (p *Peer) Context() context.Context {
	if p.ctx != nil {
		return p.ctx
	}
	return context.Background()
}

// Done reports connection death; equivalent to Context().Done().
func (p *Peer) Done() <-chan struct{} { return p.Context().Done() }

func (p *Peer) touchPong(t time.Time) { p.lastPong.Store(t.UnixNano()) }

// livenessLoop tears the connection down (cancelling Context) if no pong arrives
// within livenessTimeout. A healthy peer answers our ping every pingInterval, so
// lastPong is refreshed well inside the window; only a silently-dead peer (CP
// restarted / route lost) lets it age out. Restores ksdk's read-timeout failover,
// which the background-AutoReceive recompose dropped.
func (p *Peer) livenessLoop(ctx context.Context, pingInterval time.Duration, logger *slog.Logger) {
	check := pingInterval
	if check <= 0 || check > livenessTimeout {
		check = livenessTimeout / 3
	}
	ticker := time.NewTicker(check)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if since := time.Since(time.Unix(0, p.lastPong.Load())); since > livenessTimeout {
				logger.Warn("peer liveness timeout; tearing down for reconnect",
					"since_last_pong", since, "timeout", livenessTimeout)
				p.Connection().Close()
				p.cancel()
				return
			}
		}
	}
}

func (p *Peer) Connection() objproto.Connection { return p.client.Connection() }

func (p *Peer) Streams() trsf.Multiplexer { return p.client.Streams() }

// Policy returns the per-connection ABAC context collector (carries the
// authenticated caller as its User once CollectUser has run).
func (p *Peer) Policy() *access.ContextCollector { return p.policy }

// CommonName returns the CA-verified certificate CommonName of the caller for a
// server-accepted peer, or "" if no authenticated User is attached (e.g. the
// client side).
func (p *Peer) CommonName() string {
	if p.policy != nil && p.policy.User != nil {
		return p.policy.User.Authority().FullName().CommonName()
	}
	return ""
}

type Client interface {
	Connection() objproto.Connection
	Streams() trsf.Multiplexer
}

type client struct {
	conn    objproto.Connection
	streams trsf.Multiplexer
}

func (c *client) Connection() objproto.Connection {
	return c.conn
}

func (c *client) Streams() trsf.Multiplexer {
	return c.streams
}

func NewPeer(client Client, policy *access.ContextCollector) *Peer {
	return &Peer{
		client: client,
		policy: policy,
	}
}

type trsfTransport interface {
	trsf.PacketNumberIssuer
	trsf.UnderlayingBidirectionalTransport
}

// ControlHandler receives objproto message-level application payloads that are
// not transport pings: kind = the first byte (a kscale app kind, distinct from
// trsf/wire.ApplicationPayloadKind), payload = the remainder. The RPC spine does
// NOT use this path — RPC rides bidirectional streams via StreamHandler. It
// exists to complete the harness-style pattern for future app sub-protocols.
type ControlHandler func(kind byte, payload []byte)

// StreamHandler handles a freshly accepted bidirectional stream. The stream's
// leading magic is NOT yet consumed; the handler owns reading it (e.g.
// rpc.DecodeMagic) and dispatching. The ctx passed in carries the authenticated
// *Peer (peer.GetPeer). This is the seam where the wiring layer joins the peer
// substrate to the rpc dispatcher — keeping peer/ free of any rpc/ import.
type StreamHandler func(ctx context.Context, logger *slog.Logger, stream trsf.BidirectionalStream)

// Authenticator runs an application-level identity handshake (the CA mTLS /
// bootstrap handshake) on a freshly accepted objproto connection BEFORE the trsf
// transport is spun up — the handshake exchanges raw objproto messages, so it
// must complete before trsf's AutoReceive starts consuming them. It returns the
// authenticated caller as an access.UserContext (carries the authority + roles)
// and whether to proceed serving RPC over this connection (false e.g. for a
// one-shot bootstrap-enrollment connection). Injected so peer/ stays free of any
// ca/ import.
type Authenticator func(ctx context.Context, conn objproto.Connection) (user access.UserContext, proceed bool, err error)

type Server struct {
	endpoint     objproto.Endpoint
	policy       *access.ContextCollector
	pingInterval time.Duration
	onControl    ControlHandler
	onStream     StreamHandler
	authenticate Authenticator
	onPeer       func(*Peer, access.UserContext) bool
}

func NewServer(endpoint objproto.Endpoint, pingInterval time.Duration, policy *access.ContextCollector) *Server {
	return &Server{
		endpoint:     endpoint,
		policy:       policy,
		pingInterval: pingInterval,
	}
}

// SetOnStream installs the accepted-stream handler. Call it BEFORE Serve so no
// inbound stream races an unset handler (hook-before-Serve ordering).
func (s *Server) SetOnStream(h StreamHandler) { s.onStream = h }

// SetOnControl installs the message-level control handler (optional).
func (s *Server) SetOnControl(h ControlHandler) { s.onControl = h }

// SetAuthenticate installs the per-connection identity handshake. Call before
// Serve. The handshake runs on the raw objproto connection before trsf starts.
func (s *Server) SetAuthenticate(a Authenticator) { s.authenticate = a }

// SetOnPeer installs a hook run after a connection is authenticated and wrapped,
// with the authenticated *Peer and its UserContext. If it returns true the peer
// is considered "taken over" (e.g. registered in a dataplane broker so the
// control plane can open RPC streams to it) and the default accept-stream serving
// (onStream) is skipped. Returning false serves the peer normally. Keeps peer/
// free of any broker/app knowledge — the wiring layer decides by inspecting the
// user's application.
func (s *Server) SetOnPeer(f func(*Peer, access.UserContext) bool) { s.onPeer = f }

func ping(ctx context.Context, conn trsf.UnderlayingSendTransport, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			conn.SendMessage(trsf.EncodePing(time.Now()))
		case <-ctx.Done():
			return
		}
	}
}

// newTrsfMux spins the trsf multiplexer over conn and starts the receive/send/ping
// loops. isServer selects the trsf stream-numbering role. Shared by server-accept
// and client-wrap so both go through one transport-construction path.
func newTrsfMux(ctx context.Context, conn trsfTransport, isServer bool, pingInterval time.Duration, logger *slog.Logger, onEvent func(event *objproto.Message, err error)) trsf.Multiplexer {
	transport := trsf.NewStreams(ctx, isServer, trsf.DefaultInitialMTU, trsf.DefaultMaxMTU, conn, logger)
	go trsf.AutoReceive(ctx, transport, conn, onEvent, trsf.WithDeliverPong())
	go trsf.AutoSend(ctx, transport, conn, func(err error) {
		if err == nil {
			err = io.EOF
		}
		onEvent(nil, err)
	})
	go ping(ctx, conn, pingInterval)
	return transport
}

// eventHandler builds the AutoReceive callback: transport error → close, Pong →
// RTT log (+ onRTT to record the latest sample on the Peer), any other
// application payload → onControl (or a debug log). onRTT may be nil.
func eventHandler(logger *slog.Logger, conn objproto.Connection, cancel context.CancelFunc, onControl ControlHandler, p *Peer) func(event *objproto.Message, err error) {
	return func(event *objproto.Message, err error) {
		if err != nil {
			// Transport error, peer Close (io.EOF), or GC-reaped connection. Close
			// the objproto conn AND cancel the per-connection ctx so serve loops
			// blocked on AcceptBidirectionalStream return and the caller reconnects.
			logger.Error("connection closed", "error", err)
			conn.Close()
			cancel()
			return
		}
		if len(event.Data) == 0 {
			return
		}
		kind := wire.ApplicationPayloadKind(event.Data[0])
		switch kind {
		case wire.ApplicationPayloadKind_Pong:
			if t, ok := trsf.DecodePingPong(event.Data); ok {
				rtt := time.Since(t)
				p.touchPong(time.Now()) // peer answered our ping → connection is alive
				p.setRTT(rtt)
				logger.Debug("received pong", "sent_time", t, "rtt", rtt)
			} else {
				logger.Warn("failed to decode pong")
			}
		default:
			if onControl != nil {
				onControl(event.Data[0], event.Data[1:])
			} else {
				logger.Debug("received application payload", "kind", kind.String(), "length", len(event.Data))
			}
		}
	}
}

// WrapAcceptedConn builds a *Peer from an already-handshaked connection, spinning
// the trsf transport. isServer selects the stream-numbering role; policy is the
// per-connection ABAC collector (with CollectUser already applied server-side, or
// nil/userless client-side); onControl may be nil.
func WrapAcceptedConn(ctx context.Context, conn objproto.Connection, isServer bool, pingInterval time.Duration, policy *access.ContextCollector, logger *slog.Logger, onControl ControlHandler) *Peer {
	// Per-connection context: cancelled when this connection dies so serve loops
	// unblock and the caller reconnects. ksdk did this in its framework reconnect
	// loop (cancelCtx/cancel around ApplicationAgent.Run, agent/client/client.go);
	// kscale's recompose lost it, wedging dp agents on connection loss.
	connCtx, cancel := context.WithCancel(ctx)
	// The Pong handler records RTT/liveness onto the Peer, so the Peer must exist
	// before the trsf mux (which starts AutoReceive) is built. Wire the client in after.
	p := NewPeer(nil, policy)
	p.ctx = connCtx
	p.cancel = cancel
	p.lastPong.Store(time.Now().UnixNano()) // grace window before the first pong
	mux := newTrsfMux(connCtx, conn, isServer, pingInterval, logger, eventHandler(logger, conn, cancel, onControl, p))
	p.client = &client{conn: conn, streams: mux}
	go p.livenessLoop(connCtx, pingInterval, logger)
	return p
}

func (s *Server) Serve(ctx context.Context, logger *slog.Logger) error {
	// Select on ctx alongside the accept channel: Serve is the daemon's main
	// blocking call, and the accept channel only closes with the endpoint — which
	// nothing closes on shutdown. Without the ctx arm, SIGTERM (sigctx cancels
	// ctx) stopped the connections but main never returned, so the process sat
	// holding the listen socket until systemd's 90s SIGKILL.
	newConns := s.endpoint.GetNewActiveConnectionChannel()
	for {
		var conn objproto.Connection
		select {
		case <-ctx.Done():
			return ctx.Err()
		case c, ok := <-newConns:
			if !ok {
				return nil
			}
			conn = c
		}
		go func(conn objproto.Connection) {
			connPolicy := s.policy
			var user access.UserContext
			if s.authenticate != nil {
				u, proceed, err := s.authenticate(ctx, conn)
				if err != nil {
					logger.Error("authentication failed", "error", err)
					conn.Close()
					return
				}
				if !proceed {
					// e.g. a one-shot bootstrap-enrollment connection: the cert
					// was issued during the handshake; nothing more to serve.
					conn.Close()
					return
				}
				user = u
				connPolicy = s.policy.CollectUser(user)
				logger.Info("authenticated peer", "common_name", user.Authority().FullName().CommonName())
			}
			peer := WrapAcceptedConn(ctx, conn, true, s.pingInterval, connPolicy, logger, s.onControl)
			if s.onPeer != nil && s.onPeer(peer, user) {
				// Taken over by the hook (e.g. a dataplane peer registered in a
				// broker); the control plane will drive RPCs to it, so don't serve.
				return
			}
			if err := s.handlePeer(ctx, logger, peer); err != nil {
				logger.Error("failed to handle peer", "error", err)
				conn.Close()
			}
		}(conn)
	}
}

// handlePeer accepts bidirectional streams from the peer and hands each to
// onStream (run in its own goroutine), with the authenticated *Peer attached to
// the ctx. With no onStream installed, inbound streams are rejected.
func (s *Server) handlePeer(ctx context.Context, logger *slog.Logger, peer *Peer) error {
	mux := peer.Streams()
	// Serve on the peer's per-connection context so a dead connection unblocks the
	// accept loop below (and tears down in-flight stream handlers) instead of
	// blocking forever on the parent ctx.
	connCtx := WithPeer(peer.Context(), peer)
	for {
		stream, err := mux.AcceptBidirectionalStream(peer.Context())
		if err != nil {
			return err
		}
		if h := s.onStream; h != nil {
			go h(connCtx, logger, stream)
		} else {
			stream.CloseBoth()
		}
	}
}
