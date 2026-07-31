package rpc

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	pbwire "github.com/on-keyday/kscale/protobuf/wire"
	framewire "github.com/on-keyday/kscale/rpc/wire"
	"github.com/on-keyday/objtrsf/trsf"
)

// StreamMagicRPCS is the 4-byte prefix that marks a bidirectional stream as an
// RPC stream. It is written raw (no length prefix) by createWithStream and read
// back by DecodeMagic on the server dispatch path.
const StreamMagicRPCS = "RPCS"

// DecodeMagic reads the fixed 4-byte StreamMagicRPCS prefix off a freshly
// accepted bidirectional stream. Symmetric with the createWithStream write side;
// the server dispatch loop uses the returned magic to select the sub-protocol.
func DecodeMagic(ctx context.Context, conn trsf.BidirectionalStream) (string, error) {
	var buf [len(StreamMagicRPCS)]byte
	if _, err := io.ReadFull(&withContextReader{c: ctx, r: conn}, buf[:]); err != nil {
		return "", err
	}
	return string(buf[:]), nil
}

func createWithStream(conn trsf.BidirectionalStream, service, method string) (pbwire.RawStream, error) {
	if err := conn.AppendData(false, []byte(StreamMagicRPCS)); err != nil {
		return nil, err
	}
	hdr := framewire.RPCStreamHeader{
		Service: []byte(service),
		Method:  []byte(method),
	}
	hdr.ServiceLen.Value = uint64(len(service))
	hdr.MethodLen.Value = uint64(len(method))
	buf, err := hdr.Append(nil)
	if err != nil {
		return nil, err
	}
	if err := conn.AppendData(false, buf); err != nil {
		return nil, err
	}
	return &trsfRawStream{conn: conn}, nil
}

func NewTrsfSingleStream(conn trsf.BidirectionalStream, logger *slog.Logger) *pbwire.StreamSource {
	return &pbwire.StreamSource{
		Create: func(service, method string) (pbwire.RawStream, error) {
			return createWithStream(conn, service, method)
		},
		Logger: logger,
	}
}

// NewTrsfStreamSource は trsf.Multiplexer を protobuf/wire.StreamSource に
// アダプトする。生成された StreamSource は plugin が生成する DefaultXxxClient
// に渡され、各 RPC 呼出ごとに新規 BidirectionalStream を開いて
// trsf-level magic (consts.StreamMagicRPCS) + RPCStreamHeader を送り、
// length-prefixed body をやり取りする。
func NewTrsfStreamSource(mux trsf.Multiplexer, logger *slog.Logger) *pbwire.StreamSource {
	return &pbwire.StreamSource{
		Create: func(service, method string) (pbwire.RawStream, error) {
			conn := mux.CreateBidirectionalStream()
			return createWithStream(conn, service, method)
		},
		Logger: logger,
	}
}

type trsfRawStream struct {
	conn trsf.BidirectionalStream
}

func (r *trsfRawStream) SendMessage(ctx context.Context, data []byte) error {
	msg := framewire.RPCMessage{Body: data}
	msg.BodyLen.Value = uint64(len(data))
	buf, err := msg.Append(nil)
	if err != nil {
		return err
	}
	return r.conn.AppendDataContext(ctx, false, buf)
}

// RPCError は HandleService から返された非 ok response を Go の error として
// 表現する。呼出側で errors.As(err, &rpcErr) して status code を見たい場合に使う。
type RPCError struct {
	Status  framewire.RPCStatus
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("rpc %s: %s", e.Status.String(), e.Message)
}

type withContextReader struct {
	c context.Context
	r trsf.BidirectionalStream
}

func (w *withContextReader) Read(p []byte) (int, error) {
	return w.r.ReadContext(w.c, p)
}

func (r *trsfRawStream) ReadMessage(ctx context.Context) ([]byte, error) {
	var msg framewire.RPCResponseMessage
	if err := msg.Read(&withContextReader{c: ctx, r: r.conn}); err != nil {
		return nil, err
	}
	switch msg.Status {
	case framewire.RPCStatus_Ok:
		return msg.Body, nil
	case framewire.RPCStatus_StreamEnd:
		// server-streaming の正常終端マーカ。client の Recv ループは io.EOF で
		// 抜ける (unary は server がこのフレームを送らないので影響なし)。
		return nil, io.EOF
	default:
		return nil, &RPCError{Status: msg.Status, Message: string(msg.Body)}
	}
}

// Close は send 側のみ閉じる (wire.Call が "送信完了 -> 応答 Recv" の手順で
// Close を呼ぶため)。読み側は ReadMessage / CloseBoth で別途扱う。
func (r *trsfRawStream) Close() error {
	return r.conn.Close()
}
