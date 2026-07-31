package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/on-keyday/kscale/consts"
	pbproxy "github.com/on-keyday/kscale/protobuf/proto/trsf_proxy"
	"github.com/on-keyday/objtrsf/trsf"
	"github.com/on-keyday/kscale/trsf/proxy/wire"
)

type streamListener struct {
	ctx    context.Context
	c      chan trsf.BidirectionalStream
	logger *slog.Logger
}

var _ net.Listener = &streamListener{}

type deadlineUpdate struct{}

func (s deadlineUpdate) Error() string {
	return "deadline update"
}

type streamWrapper struct {
	stream      trsf.BidirectionalStream
	dialNetwork string
	dialAddr    string

	rCtxLock        sync.Mutex
	readDeadlineCtx context.Context
	readCancel      context.CancelCauseFunc

	wCtxLock         sync.Mutex
	writeDeadlineCtx context.Context
	writeCancel      context.CancelCauseFunc
}

var _ net.Conn = &streamWrapper{}

func doWithDeadlineUpdate(lock *sync.Mutex, ctx *context.Context, f func(ctx context.Context) (int, error)) (int, error) {
	for {
		lock.Lock()
		curCtx := *ctx
		lock.Unlock()
		n, err := f(curCtx)
		if err == nil {
			return n, nil
		}
		if _, ok := context.Cause(curCtx).(deadlineUpdate); ok {
			continue
		}
		return n, err
	}
}

func (s *streamWrapper) Read(b []byte) (n int, err error) {
	return doWithDeadlineUpdate(&s.rCtxLock, &s.readDeadlineCtx, func(ctx context.Context) (int, error) {
		return s.stream.ReadContext(ctx, b)
	})
}

func (s *streamWrapper) Write(b []byte) (n int, err error) {
	return doWithDeadlineUpdate(&s.wCtxLock, &s.writeDeadlineCtx, func(ctx context.Context) (int, error) {
		return s.stream.WriteContext(ctx, b)
	})
}

func (s *streamWrapper) Close() error {
	return s.stream.Close()
}
func (s *streamWrapper) LocalAddr() net.Addr {
	return nil
}
func (s *streamWrapper) RemoteAddr() net.Addr {
	return nil
}
func (s *streamWrapper) SetDeadline(t time.Time) error {
	s.SetReadDeadline(t)
	s.SetWriteDeadline(t)
	return nil
}

func replaceWithNewCancelAndCtx(lock *sync.Mutex, oldCtx *context.Context, oldCancel *context.CancelCauseFunc, newCtx context.Context, newCancel context.CancelCauseFunc) {
	lock.Lock()
	defer lock.Unlock()
	if *oldCancel != nil {
		(*oldCancel)(deadlineUpdate{})
	}
	*oldCtx = newCtx
	*oldCancel = newCancel
}

func (s *streamWrapper) SetReadDeadline(t time.Time) error {
	cancelCtx, withCauceCancel := context.WithCancelCause(context.Background())
	if t.IsZero() {
		replaceWithNewCancelAndCtx(&s.rCtxLock, &s.readDeadlineCtx, &s.readCancel, cancelCtx, withCauceCancel)
		return nil
	}
	deadline, cancel := context.WithDeadline(cancelCtx, t)
	replaceWithNewCancelAndCtx(&s.rCtxLock, &s.readDeadlineCtx, &s.readCancel, deadline, func(cause error) {
		withCauceCancel(cause)
		cancel()
	})
	return nil
}
func (s *streamWrapper) SetWriteDeadline(t time.Time) error {
	cancelCtx, withCauceCancel := context.WithCancelCause(context.Background())
	if t.IsZero() {
		replaceWithNewCancelAndCtx(&s.wCtxLock, &s.writeDeadlineCtx, &s.writeCancel, cancelCtx, withCauceCancel)
		return nil
	}
	deadline, cancel := context.WithDeadline(context.Background(), t)
	replaceWithNewCancelAndCtx(&s.wCtxLock, &s.writeDeadlineCtx, &s.writeCancel, deadline, func(cause error) {
		withCauceCancel(cause)
		cancel()
	})
	return nil
}

func newStreamWrapper(stream trsf.BidirectionalStream) *streamWrapper {
	w := &streamWrapper{
		stream: stream,
	}
	readCancelCtx, readCancel := context.WithCancelCause(context.Background())
	w.readDeadlineCtx = readCancelCtx
	w.readCancel = readCancel

	writeCancelCtx, writeCancel := context.WithCancelCause(context.Background())
	w.writeDeadlineCtx = writeCancelCtx
	w.writeCancel = writeCancel
	return w
}

// DecodeNetworkAddr reads exactly one DialPrefaceFrame (varint length +
// body bytes) directly off the stream — no bufio. Buffered reading would
// over-consume into subsequent HTTP request bytes that the caller
// (http.Server) needs to read next, hanging the request parse forever.
func DecodeNetworkAddr(stream trsf.BidirectionalStream) (string, string, error) {
	var frame wire.DialPrefaceFrame
	if err := frame.Read(stream); err != nil {
		return "", "", fmt.Errorf("read dial preface frame: %w", err)
	}
	preface := &pbproxy.DialPreface{}
	if err := preface.Decode(frame.Body); err != nil {
		return "", "", fmt.Errorf("decode dial preface: %w", err)
	}
	return preface.Network, preface.Addr, nil
}

func (a *streamListener) Accept() (net.Conn, error) {
	for {
		select {
		case <-a.ctx.Done():
			return nil, a.ctx.Err()
		case conn, ok := <-a.c:
			if !ok {
				return nil, net.ErrClosed
			}
			network, addr, err := DecodeNetworkAddr(conn)
			if err != nil {
				a.logger.Error("failed to decode network addr from stream", "error", err)
				continue
			}
			sw := newStreamWrapper(conn)
			sw.dialNetwork = network
			sw.dialAddr = addr
			return sw, nil
		}
	}
}

func (a *streamListener) Addr() net.Addr {
	return nil
}
func (a *streamListener) Close() error {
	return nil
}

func (a *streamListener) Send(stream trsf.BidirectionalStream) error {
	select {
	case <-a.ctx.Done():
		return a.ctx.Err()
	case a.c <- stream:
		return nil
	}
}

type TransferProxyListener interface {
	net.Listener
	Send(stream trsf.BidirectionalStream) error
}

func NewTransferProxyListener(ctx context.Context) TransferProxyListener {
	return &streamListener{
		ctx: ctx,
		c:   make(chan trsf.BidirectionalStream, 16),
	}
}

type DialerWrapper struct {
	ctx    context.Context
	dialer trsf.Multiplexer
}

func EncodeNetworkAddr(network, addr string) ([]byte, error) {
	body, err := (&pbproxy.DialPreface{Network: network, Addr: addr}).Append(nil)
	if err != nil {
		return nil, fmt.Errorf("encode dial preface: %w", err)
	}
	var frame wire.DialPrefaceFrame
	if !frame.SetBody(body) {
		return nil, fmt.Errorf("dial preface body too large (%d bytes)", len(body))
	}
	return frame.Append(nil)
}

func WritePreface(stream trsf.BidirectionalStream, network, addr string) error {
	dialInfo, err := EncodeNetworkAddr(network, addr)
	if err != nil {
		return err
	}
	return stream.AppendDataContext(context.Background(), false, []byte(consts.StreamMagicHTTP[:]), dialInfo)
}

func (d *DialerWrapper) DialContext(ctx context.Context, network string, addr string) (net.Conn, error) {
	stream := d.dialer.CreateBidirectionalStream()
	err := WritePreface(stream, network, addr)
	if err != nil {
		stream.CloseBoth()
		return nil, err
	}
	sw := newStreamWrapper(stream)
	sw.dialNetwork = network
	sw.dialAddr = addr
	return sw, nil
}

func NewDialerWrapper(ctx context.Context, dialer trsf.Multiplexer) *DialerWrapper {
	return &DialerWrapper{
		ctx:    ctx,
		dialer: dialer,
	}
}
