package rpc_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbwire "github.com/on-keyday/kscale/protobuf/wire"
	"github.com/on-keyday/kscale/protobuf/wkt"
	"github.com/on-keyday/kscale/rpc"
	framewire "github.com/on-keyday/kscale/rpc/wire"
	"github.com/on-keyday/objtrsf/trsf/mock"
)

// setupStreamPair は in-process transport pair + RPC server dispatch を立て、
// server-streaming method を試すための client StreamSource を返す。
// (setupRPCPair の server-streaming 版。整数 client 型を作らず、呼出側が
// pbwire.NewReceiveStream で stream を開く。)
func setupStreamPair(t *testing.T, mgr *rpc.RPCManager) *pbwire.StreamSource {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	clientT, serverT := mock.SetupClientServerEx(t, slog.LevelWarn)
	mock.BackgroundIO(t, clientT, serverT)
	runServerDispatch(t, t.Context(), mgr, serverT, logger)
	return rpc.NewTrsfStreamSource(clientT, logger)
}

// TestServerStreaming_HappyPath は server が N 件 stream して stream_end で
// 正常終端し、client の Recv ループが io.EOF で抜けることを検証する。
func TestServerStreaming_HappyPath(t *testing.T) {
	mgr := rpc.NewRPCManager()
	mgr.RegisterStreamMethod("ksdk.rpc.TestStream", "Count",
		func(ctx context.Context, reqBody []byte, send func(respBody []byte) error) error {
			for i := 0; i < 3; i++ {
				info := &pb.WasmInfo{Id: uint32(i)}
				b, err := info.Append(nil)
				if err != nil {
					return err
				}
				if err := send(b); err != nil {
					return err
				}
			}
			return nil
		})

	src := setupStreamPair(t, mgr)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	rs, err := pbwire.NewReceiveStream[wkt.Empty, pb.WasmInfo, *wkt.Empty, *pb.WasmInfo](
		ctx, "ksdk.rpc.TestStream", "Count", src, &wkt.Empty{})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer rs.Close()

	var ids []uint32
	for {
		info, err := rs.Recv(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv failed: %v", err)
		}
		ids = append(ids, info.Id)
	}
	if len(ids) != 3 || ids[0] != 0 || ids[1] != 1 || ids[2] != 2 {
		t.Fatalf("expected ids [0 1 2], got %v", ids)
	}
}

// TestServerStreaming_MidStreamError は handler が途中で error を返した場合、
// それまでに送った response は届き、その後 stream_end ではなく error status
// frame (=*rpc.RPCError) が返ることを検証する。
func TestServerStreaming_MidStreamError(t *testing.T) {
	mgr := rpc.NewRPCManager()
	mgr.RegisterStreamMethod("ksdk.rpc.TestStream", "FailAfterOne",
		func(ctx context.Context, reqBody []byte, send func(respBody []byte) error) error {
			info := &pb.WasmInfo{Id: 7}
			b, err := info.Append(nil)
			if err != nil {
				return err
			}
			if err := send(b); err != nil {
				return err
			}
			return errors.New("boom")
		})

	src := setupStreamPair(t, mgr)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	rs, err := pbwire.NewReceiveStream[wkt.Empty, pb.WasmInfo, *wkt.Empty, *pb.WasmInfo](
		ctx, "ksdk.rpc.TestStream", "FailAfterOne", src, &wkt.Empty{})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer rs.Close()

	// 1 件目は届く。
	first, err := rs.Recv(ctx)
	if err != nil {
		t.Fatalf("first Recv failed: %v", err)
	}
	if first.Id != 7 {
		t.Fatalf("expected first id 7, got %d", first.Id)
	}
	// 2 件目は error status frame。io.EOF ではなく *rpc.RPCError。
	_, err = rs.Recv(ctx)
	if errors.Is(err, io.EOF) {
		t.Fatal("expected RPCError, got io.EOF (stream_end)")
	}
	var rpcErr *rpc.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected *rpc.RPCError, got %T: %v", err, err)
	}
	if rpcErr.Status != framewire.RPCStatus_Unknown {
		t.Errorf("expected status Unknown, got %s", rpcErr.Status.String())
	}
	if rpcErr.Message != "boom" {
		t.Errorf("expected message 'boom', got %q", rpcErr.Message)
	}
}

// mockStreamTest implements the generated pb.StreamTestServiceServer
// (proto/streamtest_service.proto) to exercise the protoc-plugin's
// server-streaming codegen end to end: RegisterStreamTestServiceServer ->
// RegisterStreamMethod, the ServerSendStream handler arg, and the client stub's
// ReceiveStream.
type mockStreamTest struct {
	n      int // number of items to send
	failAt int // -1 = no error; otherwise return an error after sending failAt items
}

func (m *mockStreamTest) Watch(ctx context.Context, _ *wkt.Empty, stream *pb.StreamTestServiceWatchServerStream) error {
	for i := 0; i < m.n; i++ {
		if m.failAt >= 0 && i == m.failAt {
			return errors.New("watch boom")
		}
		if err := stream.Send(&pb.StreamTestItem{Seq: uint32(i), Note: "x"}); err != nil {
			return err
		}
	}
	return nil
}

// TestGeneratedServerStreaming_HappyPath drives the generated client+server
// stubs for a server-streaming method (no hand-written registration).
func TestGeneratedServerStreaming_HappyPath(t *testing.T) {
	mgr := rpc.NewRPCManager()
	pb.RegisterStreamTestServiceServer(mgr, &mockStreamTest{n: 4, failAt: -1})
	src := setupStreamPair(t, mgr)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	client := pb.NewStreamTestServiceClient(src)
	cs, err := client.Watch(ctx, &wkt.Empty{})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer cs.Close()

	var seqs []uint32
	for {
		item, err := cs.Recv(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		seqs = append(seqs, item.Seq)
	}
	if len(seqs) != 4 || seqs[0] != 0 || seqs[3] != 3 {
		t.Fatalf("expected seqs [0 1 2 3], got %v", seqs)
	}
}

// TestGeneratedServerStreaming_Error verifies a handler error mid-stream
// surfaces to the generated client as *rpc.RPCError (not io.EOF).
func TestGeneratedServerStreaming_Error(t *testing.T) {
	mgr := rpc.NewRPCManager()
	pb.RegisterStreamTestServiceServer(mgr, &mockStreamTest{n: 5, failAt: 2})
	src := setupStreamPair(t, mgr)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	client := pb.NewStreamTestServiceClient(src)
	cs, err := client.Watch(ctx, &wkt.Empty{})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer cs.Close()

	var got int
	var lastErr error
	for {
		_, err := cs.Recv(ctx)
		if err != nil {
			lastErr = err
			break
		}
		got++
	}
	if got != 2 {
		t.Fatalf("expected 2 items before error, got %d", got)
	}
	if errors.Is(lastErr, io.EOF) {
		t.Fatal("expected RPCError, got io.EOF")
	}
	var rpcErr *rpc.RPCError
	if !errors.As(lastErr, &rpcErr) {
		t.Fatalf("expected *rpc.RPCError, got %T: %v", lastErr, lastErr)
	}
	if rpcErr.Message != "watch boom" {
		t.Errorf("expected 'watch boom', got %q", rpcErr.Message)
	}
}
