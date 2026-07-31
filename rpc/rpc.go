package rpc

import (
	"context"
	"log/slog"
	"sync"

	"github.com/on-keyday/kscale/rpc/wire"
	"github.com/on-keyday/objtrsf/trsf"
)

// RPCManager は protobuf 経路の per-service per-method dispatcher を保持する。
// plugin 生成の Register*Server 関数が RegisterMethod / RegisterStreamMethod
// 経由で登録する。unary と server-streaming は別 map に保持し、HandleService が
// 同一 lookup で両者を捌く。
type RPCManager struct {
	lock          sync.RWMutex
	methods       map[string]map[string]MethodHandler       // service -> method -> unary handler
	streamMethods map[string]map[string]StreamMethodHandler // service -> method -> server-streaming handler
}

// MethodHandler は unary per-method dispatcher。body は length-prefix で読み出された
// request の protobuf bytes。返り値はそのまま response として length-prefix で
// 書き戻される。
type MethodHandler = func(ctx context.Context, body []byte) ([]byte, error)

// StreamMethodHandler は server-streaming per-method dispatcher。reqBody は
// length-prefix で読み出された request の protobuf bytes。handler は send を
// 任意回呼んで response を 1 件ずつ stream し、nil を返すと HandleService が
// stream_end マーカ (frame.bgn の RPCStatus_StreamEnd) を書いて正常終端する。
// handler が non-nil error を返した場合はその時点で error status frame を書いて
// 終端する (途中まで送った response はそのまま client に届く)。
type StreamMethodHandler = func(ctx context.Context, reqBody []byte, send func(respBody []byte) error) error

// Registry は plugin 生成の Register*Server 関数が呼び出す登録口。
// *RPCManager が実装する。
type Registry interface {
	RegisterMethod(service, method string, h MethodHandler)
	RegisterStreamMethod(service, method string, h StreamMethodHandler)
}

func NewRPCManager() *RPCManager {
	return &RPCManager{
		methods:       make(map[string]map[string]MethodHandler),
		streamMethods: make(map[string]map[string]StreamMethodHandler),
	}
}

func (c *RPCManager) RegisterMethod(service, method string, h MethodHandler) {
	c.lock.Lock()
	defer c.lock.Unlock()
	methods, ok := c.methods[service]
	if !ok {
		methods = make(map[string]MethodHandler)
		c.methods[service] = methods
	}
	methods[method] = h
}

func (c *RPCManager) RegisterStreamMethod(service, method string, h StreamMethodHandler) {
	c.lock.Lock()
	defer c.lock.Unlock()
	methods, ok := c.streamMethods[service]
	if !ok {
		methods = make(map[string]StreamMethodHandler)
		c.streamMethods[service] = methods
	}
	methods[method] = h
}

func (c *RPCManager) lookupMethod(service, method string) (MethodHandler, bool) {
	c.lock.RLock()
	defer c.lock.RUnlock()
	methods, ok := c.methods[service]
	if !ok {
		return nil, false
	}
	h, ok := methods[method]
	return h, ok
}

func (c *RPCManager) lookupStreamMethod(service, method string) (StreamMethodHandler, bool) {
	c.lock.RLock()
	defer c.lock.RUnlock()
	methods, ok := c.streamMethods[service]
	if !ok {
		return nil, false
	}
	h, ok := methods[method]
	return h, ok
}

// HandleService は protobuf 経路の RPC dispatcher。trsf-level magic
// (consts.StreamMagicRPCS) は呼出側で trsf.DecodeMagic で消費済の前提。
// frame.bgn の RPCStreamHeader + RPCMessage を読み、登録済 MethodHandler を
// 呼んで RPCResponseMessage を書き返す。エラー時は status を非 ok に設定し
// body を UTF-8 エラーメッセージに置き換える。
func (c *RPCManager) HandleService(ctx context.Context, logger *slog.Logger, conn trsf.BidirectionalStream) {
	defer conn.CloseBoth()
	var hdr wire.RPCStreamHeader
	if err := hdr.Read(conn); err != nil {
		logger.Error("Failed to read RPC stream header", "error", err)
		writeRPCError(ctx, logger, conn, wire.RPCStatus_Internal, "failed to read stream header: "+err.Error())
		return
	}
	service := string(hdr.Service)
	method := string(hdr.Method)
	logger.Debug("invoking rpc", "service", service, "method", method)
	var reqMsg wire.RPCMessage
	if err := reqMsg.Read(conn); err != nil {
		logger.Error("Failed to read RPC request message", "error", err, "service", service, "method", method)
		writeRPCError(ctx, logger, conn, wire.RPCStatus_InvalidArgument, "failed to read request: "+err.Error())
		return
	}
	if handler, ok := c.lookupMethod(service, method); ok {
		respBody, err := handler(ctx, reqMsg.Body)
		if err != nil {
			logger.Error("RPC handler error", "service", service, "method", method, "error", err)
			writeRPCError(ctx, logger, conn, wire.RPCStatus_Unknown, err.Error())
			return
		}
		respMsg := wire.RPCResponseMessage{
			Status: wire.RPCStatus_Ok,
			Body:   respBody,
		}
		respMsg.BodyLen.Value = uint64(len(respBody))
		buf, err := respMsg.Append(nil)
		if err != nil {
			logger.Error("Failed to encode RPC response", "service", service, "method", method, "error", err)
			writeRPCError(ctx, logger, conn, wire.RPCStatus_Internal, "failed to encode response: "+err.Error())
			return
		}
		if err := conn.AppendDataContext(ctx, false, buf); err != nil {
			logger.Error("Failed to write RPC response", "service", service, "method", method, "error", err)
		}
		logger.Debug("rpc done", "service", service, "method", method)
		return
	}
	if streamHandler, ok := c.lookupStreamMethod(service, method); ok {
		c.handleStream(ctx, logger, conn, service, method, reqMsg.Body, streamHandler)
		return
	}
	logger.Error("Unknown RPC method", "service", service, "method", method)
	writeRPCError(ctx, logger, conn, wire.RPCStatus_Unimplemented, "unknown method "+service+"."+method)
}

// handleStream は server-streaming method を捌く。handler に渡す send は 1 件ごと
// に RPCResponseMessage{Ok} を書き出し、handler が nil を返せば末尾に
// RPCResponseMessage{StreamEnd} を書いて正常終端する。途中 send 失敗・handler
// error はそれぞれ error status frame で終端する。
func (c *RPCManager) handleStream(ctx context.Context, logger *slog.Logger, conn trsf.BidirectionalStream, service, method string, reqBody []byte, handler StreamMethodHandler) {
	send := func(respBody []byte) error {
		respMsg := wire.RPCResponseMessage{
			Status: wire.RPCStatus_Ok,
			Body:   respBody,
		}
		respMsg.BodyLen.Value = uint64(len(respBody))
		buf, err := respMsg.Append(nil)
		if err != nil {
			return err
		}
		return conn.AppendDataContext(ctx, false, buf)
	}
	if err := handler(ctx, reqBody, send); err != nil {
		logger.Error("RPC stream handler error", "service", service, "method", method, "error", err)
		writeRPCError(ctx, logger, conn, wire.RPCStatus_Unknown, err.Error())
		return
	}
	endMsg := wire.RPCResponseMessage{Status: wire.RPCStatus_StreamEnd}
	buf, err := endMsg.Append(nil)
	if err != nil {
		logger.Error("Failed to encode RPC stream_end", "service", service, "method", method, "error", err)
		return
	}
	if err := conn.AppendDataContext(ctx, false, buf); err != nil {
		logger.Error("Failed to write RPC stream_end", "service", service, "method", method, "error", err)
	}
	logger.Debug("rpc stream done", "service", service, "method", method)
}

// writeRPCError は status 非 ok の RPCResponseMessage を書き出す。書き出し
// 自体が失敗した場合はログのみ (もう送信経路が壊れている)。
func writeRPCError(ctx context.Context, logger *slog.Logger, conn trsf.BidirectionalStream, status wire.RPCStatus, msg string) {
	respMsg := wire.RPCResponseMessage{
		Status: status,
		Body:   []byte(msg),
	}
	respMsg.BodyLen.Value = uint64(len(msg))
	buf, err := respMsg.Append(nil)
	if err != nil {
		logger.Error("Failed to encode RPC error response", "status", status.String(), "error", err)
		return
	}
	if err := conn.AppendDataContext(ctx, false, buf); err != nil {
		logger.Error("Failed to write RPC error response", "status", status.String(), "error", err)
	}
}
