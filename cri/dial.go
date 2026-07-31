package cri

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	api "github.com/on-keyday/kscale/protobuf/proto/cri"
	"github.com/on-keyday/kscale/protobuf/wire"
	"github.com/on-keyday/kscale/protobuf/wire/h2"
	"github.com/on-keyday/kscale/protobuf/wire/h2/hpack"
)

// DefaultReceiveTimeout bounds how long a single response chunk may take
// before the connection is declared dead. PullImage of a large image is the
// slow legitimate case, so this is generous.
const DefaultReceiveTimeout = 5 * time.Minute

var errClosed = errors.New("cri: connection closed")

// StatusError is a non-OK gRPC status returned by the CRI server (carried in
// the response trailers as grpc-status / grpc-message).
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("cri: grpc status %d: %s", e.Code, e.Message)
}

// Conn is one HTTP/2 connection to a CRI endpoint. It owns the read/write
// pump goroutines; when either side of the socket fails, the whole connection
// is aborted so every in-flight RPC fails instead of hanging on internal
// h2 channels.
type Conn struct {
	nc             net.Conn
	h2c            *h2.HTTP2Conn
	receiveTimeout time.Duration

	failOnce sync.Once
	done     chan struct{}
	err      error
}

// DialConn opens a single connection to the CRI unix socket at socketPath.
// Most callers want Dial (a Client that re-dials transparently) instead.
func DialConn(socketPath string, receiveTimeout time.Duration) (*Conn, error) {
	nc, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("cri: dial %s: %w", socketPath, err)
	}
	h2c := h2.NewHTTP2Conn(true)
	if err := h2c.Start(); err != nil {
		nc.Close()
		return nil, fmt.Errorf("cri: start http2: %w", err)
	}
	c := &Conn{
		nc:             nc,
		h2c:            h2c,
		receiveTimeout: receiveTimeout,
		done:           make(chan struct{}),
	}
	go c.writePump()
	go c.readPump()
	return c, nil
}

// fail records the first error, aborts the h2 connection (waking every blocked
// stream reader/writer) and closes the socket. Idempotent.
func (c *Conn) fail(err error) {
	c.failOnce.Do(func() {
		c.err = err
		c.h2c.Abort()
		c.nc.Close()
		close(c.done)
	})
}

func (c *Conn) writePump() {
	for {
		data, err := c.h2c.Encode()
		if err != nil {
			c.fail(err)
			return
		}
		if _, err := c.nc.Write(data); err != nil {
			c.fail(err)
			return
		}
	}
}

func (c *Conn) readPump() {
	// Decode copies whatever it retains beyond the call, so buf is reusable.
	buf := make([]byte, 64<<10)
	for {
		n, err := c.nc.Read(buf)
		if n > 0 {
			if derr := c.h2c.Decode(buf[:n]); derr != nil {
				c.fail(derr)
				return
			}
		}
		if err != nil {
			c.fail(err)
			return
		}
	}
}

func (c *Conn) Close() error {
	c.fail(errClosed)
	return nil
}

// Done is closed once the connection has died; Err then reports why.
func (c *Conn) Done() <-chan struct{} { return c.done }

func (c *Conn) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

func (c *Conn) alive() bool { return c.Err() == nil }

// createStream opens one gRPC request stream (`POST /<service>/<method>`) and
// wraps it in the gRPC message framer the pbg stubs consume.
func (c *Conn) createStream(service, method string) (wire.RawStream, error) {
	stream, ok := c.h2c.CreateStream()
	if !ok {
		if err := c.Err(); err != nil {
			return nil, fmt.Errorf("cri: open stream for %s/%s: %w", service, method, err)
		}
		return nil, fmt.Errorf("cri: open stream for %s/%s: concurrent stream limit", service, method)
	}
	err := stream.WriteHeaderOrdered([]hpack.KeyValue{
		{Key: ":method", Value: "POST"},
		{Key: ":scheme", Value: "http"},
		{Key: ":path", Value: "/" + service + "/" + method},
		{Key: ":authority", Value: "localhost"},
		{Key: "content-type", Value: "application/grpc"},
		{Key: "user-agent", Value: "kscale-cri/0.1"},
		{Key: "te", Value: "trailers"},
	}, hpack.BestEffort)
	if err != nil {
		return nil, fmt.Errorf("cri: write request header: %w", err)
	}
	return &wire.GRPCFramer{
		Send: func(data []byte) error {
			_, err := stream.Write(data)
			return err
		},
		Receive: func() ([]byte, error) { return c.receive(stream) },
		DoClose: stream.Close,
	}, nil
}

// receive waits for the next DATA chunk of stream. On clean end-of-stream it
// converts a non-OK grpc-status trailer into *StatusError. If receiveTimeout
// elapses, the whole connection is aborted — a CRI server that stops
// answering is treated as dead — which also unblocks the inner TakePeerData,
// so the helper goroutine never leaks.
func (c *Conn) receive(stream *h2.HTTP2Stream) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := stream.TakePeerData()
		ch <- result{data, err}
	}()
	var r result
	if c.receiveTimeout > 0 {
		t := time.NewTimer(c.receiveTimeout)
		defer t.Stop()
		select {
		case r = <-ch:
		case <-t.C:
			c.fail(fmt.Errorf("cri: receive timed out after %v", c.receiveTimeout))
			r = <-ch
		}
	} else {
		r = <-ch
	}
	if errors.Is(r.err, io.EOF) {
		if st := statusFromTrailer(stream.PeerHeader()); st != nil {
			return nil, st
		}
		if err := c.Err(); err != nil && !errors.Is(err, errClosed) {
			return nil, fmt.Errorf("cri: connection failed: %w", err)
		}
	}
	return r.data, r.err
}

func statusFromTrailer(hdr http.Header) *StatusError {
	if hdr == nil {
		return nil
	}
	code := hdr.Get("grpc-status")
	if code == "" || code == "0" {
		return nil
	}
	n, err := strconv.Atoi(code)
	if err != nil {
		n = -1
	}
	msg := hdr.Get("grpc-message")
	// grpc-message is percent-encoded (gRPC's scheme matches path escaping
	// closely enough); fall back to the raw string on malformed input.
	if decoded, err := url.PathUnescape(msg); err == nil {
		msg = decoded
	}
	return &StatusError{Code: n, Message: msg}
}

// Client is a CRI client bound to a socket path. It lazily re-dials: when the
// current connection has died, the next RPC establishes a fresh one, so a
// containerd restart heals on the following converge instead of requiring an
// agent restart.
type Client struct {
	*CRI
	socketPath     string
	receiveTimeout time.Duration

	mu   sync.Mutex
	conn *Conn
}

// NewClient returns a Client for the CRI endpoint at socketPath WITHOUT dialing
// upfront — the connection is established (and re-established) on first use. Use
// this when the runtime socket may not be up yet (e.g. an agent that starts
// before/alongside containerd); the first RPC that finds it down returns the
// dial error, and the next one retries. logger may be nil.
func NewClient(socketPath string, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	c := &Client{
		socketPath:     socketPath,
		receiveTimeout: DefaultReceiveTimeout,
	}
	source := &wire.StreamSource{Logger: logger, Create: c.createStream}
	c.CRI = NewCRI(api.NewRuntimeServiceClient(source), api.NewImageServiceClient(source))
	return c
}

// Dial is NewClient plus an eager connection check: it fails fast if the socket
// is unreachable rather than deferring the error to the first RPC. logger may be
// nil.
func Dial(socketPath string, logger *slog.Logger) (*Client, error) {
	c := NewClient(socketPath, logger)
	if _, err := c.ensure(); err != nil {
		return nil, err
	}
	return c, nil
}

// SetReceiveTimeout overrides DefaultReceiveTimeout. It applies to
// connections dialed afterwards, so call it right after Dial.
func (c *Client) SetReceiveTimeout(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.receiveTimeout = d
	if c.conn != nil {
		c.conn.receiveTimeout = d
	}
}

func (c *Client) ensure() (*Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil && c.conn.alive() {
		return c.conn, nil
	}
	conn, err := DialConn(c.socketPath, c.receiveTimeout)
	if err != nil {
		return nil, err
	}
	c.conn = conn
	return conn, nil
}

func (c *Client) createStream(service, method string) (wire.RawStream, error) {
	conn, err := c.ensure()
	if err != nil {
		return nil, err
	}
	return conn.createStream(service, method)
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	return nil
}
