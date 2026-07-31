package wire

import (
	"context"
	"fmt"
	"log/slog"
)

type RawStream interface {
	ReadMessage(context.Context) ([]byte, error)
	SendMessage(context.Context, []byte) error
	Close() error
}

type StreamSource struct {
	Create func(service, method string) (RawStream, error)
	Logger *slog.Logger
}

type appenddecoder interface {
	Append([]byte) ([]byte, error)
	Decode([]byte) error
}

type Stream[T any, U any, TP interface {
	*T
	appenddecoder
}, UP interface {
	*U
	appenddecoder
}] struct {
	service string
	method  string
	creator *StreamSource
	source  RawStream
}

func NewStream[T any, U any, TP interface {
	*T
	appenddecoder
}, UP interface {
	*U
	appenddecoder
}](service, method string, source *StreamSource) (*Stream[T, U, TP, UP], error) {
	if source == nil || source.Create == nil {
		return nil, fmt.Errorf("invalid StreamSource: Create function is required")
	}
	stream, err := source.Create(service, method)
	if err != nil {
		return nil, fmt.Errorf("failed to create stream: %w", err)
	}
	return &Stream[T, U, TP, UP]{
		service: service,
		method:  method,
		creator: source,
		source:  stream,
	}, nil
}

func (s *Stream[T, U, TP, UP]) Send(ctx context.Context, msg *T) error {
	var p TP = (TP)(msg)
	data, err := p.Append(nil)
	if err != nil {
		return err
	}
	return s.source.SendMessage(ctx, data)
}

func (s *Stream[T, U, TP, UP]) Recv(ctx context.Context) (*U, error) {
	data, err := s.source.ReadMessage(ctx)
	if err != nil {
		return nil, err
	}
	var msg U
	err = (UP)(&msg).Decode(data)
	if err != nil {
		return nil, err
	}
	return &msg, nil
}

type ClientStream[T any, U any, TP interface {
	*T
	appenddecoder
}, UP interface {
	*U
	appenddecoder
}] struct {
	stream Stream[T, U, TP, UP]
}

func NewClientStream[T any, U any, TP interface {
	*T
	appenddecoder
}, UP interface {
	*U
	appenddecoder
}](service, method string, source *StreamSource) (*ClientStream[T, U, TP, UP], error) {
	stream, err := NewStream[T, U, TP, UP](service, method, source)
	if err != nil {
		return nil, err
	}
	return &ClientStream[T, U, TP, UP]{
		stream: *stream,
	}, nil
}

func (c *ClientStream[T, U, TP, UP]) Send(ctx context.Context, msg *T) error {
	return c.stream.Send(ctx, msg)
}

func (c *ClientStream[T, U, TP, UP]) CloseAndRecv(ctx context.Context) (*U, error) {
	err := c.stream.source.Close()
	if err != nil {
		return nil, err
	}
	return c.stream.Recv(ctx)
}

type ServerStream[T any, U any, TP interface {
	*T
	appenddecoder
}, UP interface {
	*U
	appenddecoder
}] struct {
	stream Stream[U, T, UP, TP]
}

func NewServerStream[T any, U any, TP interface {
	*T
	appenddecoder
}, UP interface {
	*U
	appenddecoder
}](service, method string, source *StreamSource) (*ServerStream[T, U, TP, UP], error) {
	stream, err := NewStream[U, T, UP, TP](service, method, source)
	if err != nil {
		return nil, err
	}
	return &ServerStream[T, U, TP, UP]{
		stream: *stream,
	}, nil
}

func (s *ServerStream[T, U, TP, UP]) Recv(ctx context.Context) (*T, error) {
	return s.stream.Recv(ctx)
}

func (s *ServerStream[T, U, TP, UP]) SendAndClose(ctx context.Context, msg *U) error {
	err := s.stream.Send(ctx, msg)
	if err != nil {
		return err
	}
	return s.stream.source.Close()
}

type ReceiveStream[T any, U any, P interface {
	*T
	appenddecoder
}, UP interface {
	*U
	appenddecoder
}] struct {
	stream Stream[T, U, P, UP]
}

func NewReceiveStream[T any, U any, P interface {
	*T
	appenddecoder
}, UP interface {
	*U
	appenddecoder
}](ctx context.Context, service, method string, source *StreamSource, in *T) (*ReceiveStream[T, U, P, UP], error) {
	stream, err := NewStream[T, U, P, UP](service, method, source)
	if err != nil {
		return nil, err
	}
	s := &ReceiveStream[T, U, P, UP]{
		stream: *stream,
	}
	err = s.stream.Send(ctx, in)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *ReceiveStream[T, U, P, UP]) Recv(ctx context.Context) (*U, error) {
	return s.stream.Recv(ctx)
}

func (s *ReceiveStream[T, U, P, UP]) Close() error {
	return s.stream.source.Close()
}

type SendStream[T any, P interface {
	*T
	appenddecoder
}] struct {
	stream Stream[T, T, P, P]
}

func NewSendStream[T any, P interface {
	*T
	appenddecoder
}](service, method string, source *StreamSource) (*SendStream[T, P], error) {
	stream, err := NewStream[T, T, P, P](service, method, source)
	if err != nil {
		return nil, err
	}
	return &SendStream[T, P]{
		stream: *stream,
	}, nil
}

func (s *SendStream[T, P]) Send(ctx context.Context, msg *T) error {
	return s.stream.Send(ctx, msg)
}

func (s *SendStream[T, P]) Close() error {
	return s.stream.source.Close()
}

// ServerSendStream は server-streaming RPC handler が複数 response を push する
// ための server 側ヘルパ。client 側の SendStream とは異なり新規 stream を開かず
// (request frame を書かず)、RPC dispatcher (rpc.RPCManager.handleStream) が注入
// する send closure 経由で各 response を 1 件ずつ書き出す。終端 (stream_end) は
// handler が return した時点で dispatcher 側が付ける。
type ServerSendStream[T any, P interface {
	*T
	appenddecoder
}] struct {
	send func(body []byte) error
}

func NewServerSendStream[T any, P interface {
	*T
	appenddecoder
}](send func(body []byte) error) *ServerSendStream[T, P] {
	return &ServerSendStream[T, P]{send: send}
}

func (s *ServerSendStream[T, P]) Send(msg *T) error {
	data, err := (P)(msg).Append(nil)
	if err != nil {
		return err
	}
	return s.send(data)
}

type Call[T any, TP interface {
	*T
	appenddecoder
}, U any, UP interface {
	*U
	appenddecoder
}] struct {
	stream Stream[T, U, TP, UP]
}

func NewCall[T any, U any, TP interface {
	*T
	appenddecoder
}, UP interface {
	*U
	appenddecoder
}](ctx context.Context, service, method string, source *StreamSource, in *T) (*U, error) {
	stream, err := NewStream[T, U, TP, UP](service, method, source)
	if err != nil {
		return nil, err
	}
	call := &Call[T, TP, U, UP]{
		stream: *stream,
	}
	return call.InvokeContext(ctx, in)
}

func (c *Call[T, TP, U, UP]) Invoke(msg *T) (*U, error) {
	return c.InvokeContext(context.Background(), msg)
}

// InvokeContext は ctx を受けるが、現状の RawStream は ctx をサポートしないため
// 内部での cancellation 監視は未実装。将来 StreamSource/RawStream に ctx 対応を
// 入れた段階で伝播させる。それまでは ctx は呼び出し側の意図記録 + ログ用途。
func (c *Call[T, TP, U, UP]) InvokeContext(ctx context.Context, msg *T) (*U, error) {
	_ = ctx
	c.stream.creator.Logger.Debug("invoking rpc", "service", c.stream.service, "method", c.stream.method)
	err := c.stream.Send(ctx, msg)
	if err != nil {
		return nil, err
	}
	err = c.stream.source.Close()
	if err != nil {
		return nil, err
	}
	return c.stream.Recv(ctx)
}
