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
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/kscale/protobuf/wkt"
	"github.com/on-keyday/kscale/rpc"
	framewire "github.com/on-keyday/kscale/rpc/wire"
	"github.com/on-keyday/objtrsf/trsf"
	"github.com/on-keyday/objtrsf/trsf/mock"
)

// mockWasmService は WasmServiceServer interface を満たす test 用 impl。
type mockWasmService struct {
	registered     []*pb.WasmInfo
	failOnRegister error
}

func (s *mockWasmService) Register(ctx context.Context, req *pb.WasmServiceRegisterRequest) (*wkt.Empty, error) {
	if s.failOnRegister != nil {
		return nil, s.failOnRegister
	}
	s.registered = append(s.registered, &pb.WasmInfo{
		Id:         req.Id,
		BinaryPath: req.BinaryPath,
		Method:     req.Method,
		Path:       req.Path,
	})
	return &wkt.Empty{}, nil
}

func (s *mockWasmService) Unregister(ctx context.Context, req *pb.WasmServiceUnregisterRequest) (*wkt.Empty, error) {
	for i, info := range s.registered {
		if info.Id == req.Id {
			s.registered = append(s.registered[:i], s.registered[i+1:]...)
			return &wkt.Empty{}, nil
		}
	}
	return nil, errors.New("not found")
}

func (s *mockWasmService) ListAttached(ctx context.Context, _ *wkt.Empty) (*pb.WasmServiceListAttachedResponse, error) {
	return &pb.WasmServiceListAttachedResponse{Info: s.registered}, nil
}

// runServerDispatch は server transport から bidi stream を accept し続けて
// magic 'RPCS' のときに HandleService に dispatch する。agent/client/control.go
// の dispatch loop を test 用に最小化したもの。
func runServerDispatch(t *testing.T, ctx context.Context, mgr *rpc.RPCManager, server trsf.Transport, logger *slog.Logger) {
	t.Helper()
	go func() {
		for {
			conn, err := server.AcceptBidirectionalStream(ctx)
			if err != nil {
				return
			}
			go func() {
				magic, err := rpc.DecodeMagic(ctx, conn)
				if err != nil {
					return
				}
				if magic != rpc.StreamMagicRPCS {
					conn.CloseBoth()
					return
				}
				mgr.HandleService(ctx, logger.With("role", "server-dispatch"), conn)
			}()
		}
	}()
}

// setupRPCPair は in-process な trsf transport pair + RPC server dispatch +
// WasmService client を立ち上げる。impl が register された状態で client を返す。
func setupRPCPair(t *testing.T, impl pb.WasmServiceServer) (pb.WasmServiceClient, *rpc.RPCManager) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	clientT, serverT := mock.SetupClientServerEx(t, slog.LevelWarn)
	mock.BackgroundIO(t, clientT, serverT)

	mgr := rpc.NewRPCManager()
	pb.RegisterWasmServiceServer(mgr, impl)

	runServerDispatch(t, t.Context(), mgr, serverT, logger)

	client := pb.NewWasmServiceClient(rpc.NewTrsfStreamSource(clientT, logger))
	return client, mgr
}

func TestWasmService_RegisterRoundTrip(t *testing.T) {
	impl := &mockWasmService{}
	client, _ := setupRPCPair(t, impl)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := client.Register(ctx, &pb.WasmServiceRegisterRequest{
		Id:         42,
		BinaryPath: "/wasm/test.wasm",
		Method:     "GET",
		Path:       "/api",
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	if len(impl.registered) != 1 {
		t.Fatalf("expected 1 registered module, got %d", len(impl.registered))
	}
	if impl.registered[0].Id != 42 || impl.registered[0].BinaryPath != "/wasm/test.wasm" {
		t.Fatalf("unexpected registered content: %+v", impl.registered[0])
	}
}

func TestWasmService_ListAttached(t *testing.T) {
	impl := &mockWasmService{
		registered: []*pb.WasmInfo{
			{Id: 1, BinaryPath: "/a.wasm", Method: "GET", Path: "/a"},
			{Id: 2, BinaryPath: "/b.wasm", Method: "POST", Path: "/b"},
		},
	}
	client, _ := setupRPCPair(t, impl)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	resp, err := client.ListAttached(ctx, &wkt.Empty{})
	if err != nil {
		t.Fatalf("ListAttached failed: %v", err)
	}
	if len(resp.Info) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(resp.Info))
	}
	if resp.Info[0].Id != 1 || resp.Info[1].Id != 2 {
		t.Fatalf("unexpected ids: %d, %d", resp.Info[0].Id, resp.Info[1].Id)
	}
}

func TestWasmService_HandlerError_PropagatesAsRPCError(t *testing.T) {
	impl := &mockWasmService{
		failOnRegister: errors.New("disk full"),
	}
	client, _ := setupRPCPair(t, impl)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := client.Register(ctx, &pb.WasmServiceRegisterRequest{Id: 1})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var rpcErr *rpc.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected *rpc.RPCError, got %T: %v", err, err)
	}
	// handler 由来のエラーは Unknown status で運ばれる。
	if rpcErr.Status != framewire.RPCStatus_Unknown {
		t.Errorf("expected status Unknown, got %s (full err: %v)", rpcErr.Status.String(), err)
	}
	if rpcErr.Message != "disk full" {
		t.Errorf("expected message 'disk full', got %q", rpcErr.Message)
	}
}

// TestWasmService_UnknownMethod_ReturnsUnimplemented は service interface に
// 無い method を直接 RPCManager に登録「しない」ことで、Server 側 dispatch が
// Unimplemented status を返すケースを検証する。
//
// ※ plugin 生成 client は静的に method 名を持っているので、未登録のメソッドを
// 「呼ぶ」には service 単位を別物にする必要がある。本 test では impl ごと
// register しない構成 (RPCManager は作るが Register*Server を呼ばない) で
// 任意の WasmService method 呼出が unimplemented になることを確認する。
func TestWasmService_NotRegistered_ReturnsUnimplemented(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	clientT, serverT := mock.SetupClientServerEx(t, slog.LevelWarn)
	mock.BackgroundIO(t, clientT, serverT)

	mgr := rpc.NewRPCManager()
	// あえて Register*Server を呼ばない
	runServerDispatch(t, t.Context(), mgr, serverT, logger)

	client := pb.NewWasmServiceClient(rpc.NewTrsfStreamSource(clientT, logger))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := client.ListAttached(ctx, &wkt.Empty{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var rpcErr *rpc.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected *rpc.RPCError, got %T: %v", err, err)
	}
	if rpcErr.Status != framewire.RPCStatus_Unimplemented {
		t.Errorf("expected status Unimplemented, got %s", rpcErr.Status.String())
	}
}

// mockDataplaneService は DataplaneServiceServer を満たす test 用 impl。
// UpdateVip 以外は UnimplementedDataplaneServiceServer で埋める。
type mockDataplaneService struct {
	pb.UnimplementedDataplaneServiceServer
	gotVip       string
	gotSecret    []byte
	startCalled  bool
	failOnUpdate error
	gotFileName  string
	gotSaveAs    string
	logCalled bool
}

func (s *mockDataplaneService) UpdateVip(ctx context.Context, req *pb.DataplaneServiceUpdateVipRequest) (*wkt.Empty, error) {
	if s.failOnUpdate != nil {
		return nil, s.failOnUpdate
	}
	s.gotVip = req.Vip
	return &wkt.Empty{}, nil
}

func (s *mockDataplaneService) SyncSecret(ctx context.Context, req *pb.DataplaneServiceSyncSecretRequest) (*wkt.Empty, error) {
	s.gotSecret = req.SharedSecret
	return &wkt.Empty{}, nil
}

func (s *mockDataplaneService) StartDataplane(ctx context.Context, _ *wkt.Empty) (*wkt.Empty, error) {
	s.startCalled = true
	return &wkt.Empty{}, nil
}

func (s *mockDataplaneService) SendFile(ctx context.Context, req *pb.DataplaneServiceSendFileRequest) (*wkt.Empty, error) {
	s.gotFileName = req.FileName
	s.gotSaveAs = req.SaveAs
	return &wkt.Empty{}, nil
}

func (s *mockDataplaneService) StreamLogs(ctx context.Context, _ *wkt.Empty, stream *pb.DataplaneServiceStreamLogsServerStream) error {
	s.logCalled = true
	return stream.Send(&pb.LogRecord{
		Level:   "INFO",
		Message: "hello",
		Attrs:   []*pb.LogAttr{{Key: "k", Value: "v"}},
	})
}

func setupDataplanePair(t *testing.T, impl pb.DataplaneServiceServer) pb.DataplaneServiceClient {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	clientT, serverT := mock.SetupClientServerEx(t, slog.LevelWarn)
	mock.BackgroundIO(t, clientT, serverT)

	mgr := rpc.NewRPCManager()
	pb.RegisterDataplaneServiceServer(mgr, impl)
	runServerDispatch(t, t.Context(), mgr, serverT, logger)

	return pb.NewDataplaneServiceClient(rpc.NewTrsfStreamSource(clientT, logger))
}

// AgentControl command の RPC 化 (UpdateVip) の round-trip。req.Vip が server まで届く。
func TestDataplaneService_UpdateVipRoundTrip(t *testing.T) {
	impl := &mockDataplaneService{}
	client := setupDataplanePair(t, impl)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if _, err := client.UpdateVip(ctx, &pb.DataplaneServiceUpdateVipRequest{Vip: "192.0.2.1"}); err != nil {
		t.Fatalf("UpdateVip failed: %v", err)
	}
	if impl.gotVip != "192.0.2.1" {
		t.Fatalf("expected vip 192.0.2.1, got %q", impl.gotVip)
	}
}

// 足場の核心: DP 側 UpdateVip の err が制御プレーン側に RPCError として伝播する
// (旧 AgentControl 投げっぱなしでは握り潰されていた経路)。
func TestDataplaneService_UpdateVipError_PropagatesAsRPCError(t *testing.T) {
	impl := &mockDataplaneService{failOnUpdate: errors.New("interface not found")}
	client := setupDataplanePair(t, impl)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := client.UpdateVip(ctx, &pb.DataplaneServiceUpdateVipRequest{Vip: "192.0.2.1"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var rpcErr *rpc.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected *rpc.RPCError, got %T: %v", err, err)
	}
	if rpcErr.Status != framewire.RPCStatus_Unknown {
		t.Errorf("expected status Unknown, got %s", rpcErr.Status.String())
	}
	if rpcErr.Message != "interface not found" {
		t.Errorf("expected message 'interface not found', got %q", rpcErr.Message)
	}
}

func TestDataplaneService_SyncSecretRoundTrip(t *testing.T) {
	impl := &mockDataplaneService{}
	client := setupDataplanePair(t, impl)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	secret := []byte{1, 2, 3, 4, 5}
	if _, err := client.SyncSecret(ctx, &pb.DataplaneServiceSyncSecretRequest{SharedSecret: secret}); err != nil {
		t.Fatalf("SyncSecret failed: %v", err)
	}
	if string(impl.gotSecret) != string(secret) {
		t.Fatalf("expected secret %v, got %v", secret, impl.gotSecret)
	}
}

func TestDataplaneService_StartDataplaneSignal(t *testing.T) {
	impl := &mockDataplaneService{}
	client := setupDataplanePair(t, impl)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if _, err := client.StartDataplane(ctx, &wkt.Empty{}); err != nil {
		t.Fatalf("StartDataplane failed: %v", err)
	}
	if !impl.startCalled {
		t.Fatal("StartDataplane impl was not invoked")
	}
}

// send-file (旧 AgentControl_SendFilePullFile) の RPC 化 round-trip。
// file_name / save_as が server まで届く。
func TestDataplaneService_SendFileRoundTrip(t *testing.T) {
	impl := &mockDataplaneService{}
	client := setupDataplanePair(t, impl)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if _, err := client.SendFile(ctx, &pb.DataplaneServiceSendFileRequest{FileName: "cert.pem", SaveAs: "saved.pem"}); err != nil {
		t.Fatalf("SendFile failed: %v", err)
	}
	if impl.gotFileName != "cert.pem" || impl.gotSaveAs != "saved.pem" {
		t.Fatalf("expected (cert.pem, saved.pem), got (%q, %q)", impl.gotFileName, impl.gotSaveAs)
	}
}

// view-logs (旧 AgentControl_ViewLogsStartLog/StopLog) の RPC 化 round-trip。
// StreamLogs: the CP opens the stream (the lifecycle is the on/off toggle) and
// receives the node's structured log records.
func TestDataplaneService_StreamLogsRoundTrip(t *testing.T) {
	impl := &mockDataplaneService{}
	client := setupDataplanePair(t, impl)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	stream, err := client.StreamLogs(ctx, &wkt.Empty{})
	if err != nil {
		t.Fatalf("StreamLogs failed: %v", err)
	}
	rec, err := stream.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv failed: %v", err)
	}
	if !impl.logCalled {
		t.Fatalf("expected StreamLogs to reach server")
	}
	if rec.Message != "hello" || rec.Level != "INFO" {
		t.Fatalf("record = %+v, want INFO/hello", rec)
	}
	if len(rec.Attrs) != 1 || rec.Attrs[0].Key != "k" || rec.Attrs[0].Value != "v" {
		t.Fatalf("attrs = %v, want [{k v}]", rec.Attrs)
	}
}

// mockRouterService は GenericControl から RouterService へ typed 移行した
// set-router-hostname の round-trip 検証用。
type mockRouterService struct {
	pb.UnimplementedRouterServiceServer
	gotHostname string
}

func (s *mockRouterService) SetRouterHostname(ctx context.Context, req *pb.RouterServiceSetRouterHostnameRequest) (*wkt.Empty, error) {
	s.gotHostname = req.Hostname
	return &wkt.Empty{}, nil
}

func setupRouterPair(t *testing.T, impl pb.RouterServiceServer) pb.RouterServiceClient {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	clientT, serverT := mock.SetupClientServerEx(t, slog.LevelWarn)
	mock.BackgroundIO(t, clientT, serverT)

	mgr := rpc.NewRPCManager()
	pb.RegisterRouterServiceServer(mgr, impl)
	runServerDispatch(t, t.Context(), mgr, serverT, logger)

	return pb.NewRouterServiceClient(rpc.NewTrsfStreamSource(clientT, logger))
}

func TestRouterService_SetRouterHostnameRoundTrip(t *testing.T) {
	impl := &mockRouterService{}
	client := setupRouterPair(t, impl)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if _, err := client.SetRouterHostname(ctx, &pb.RouterServiceSetRouterHostnameRequest{Hostname: "router.example"}); err != nil {
		t.Fatalf("SetRouterHostname failed: %v", err)
	}
	if impl.gotHostname != "router.example" {
		t.Fatalf("expected hostname router.example, got %q", impl.gotHostname)
	}
}

// mockDnsControlService は GenericControl から DnsControlService へ typed 移行した
// set-dns-port の round-trip 検証用 (uint16 hold の往復)。
type mockDnsControlService struct {
	pb.UnimplementedDnsControlServiceServer
	gotPort          uint16
	gotSetupDomain   string
	gotSetupToken    string
	gotCleanupDomain string
	gotCleanupToken  string
}

func (s *mockDnsControlService) SetDnsPort(ctx context.Context, req *pb.DnsControlServiceSetDnsPortRequest) (*wkt.Empty, error) {
	s.gotPort = req.Port
	return &wkt.Empty{}, nil
}

func (s *mockDnsControlService) SetupAcmeChallenge(ctx context.Context, req *pb.DnsControlServiceSetupAcmeChallengeRequest) (*wkt.Empty, error) {
	s.gotSetupDomain = req.Domain
	s.gotSetupToken = req.Token
	return &wkt.Empty{}, nil
}

func (s *mockDnsControlService) CleanupAcmeChallenge(ctx context.Context, req *pb.DnsControlServiceCleanupAcmeChallengeRequest) (*wkt.Empty, error) {
	s.gotCleanupDomain = req.Domain
	s.gotCleanupToken = req.Token
	return &wkt.Empty{}, nil
}

func setupDnsControlPair(t *testing.T, impl pb.DnsControlServiceServer) pb.DnsControlServiceClient {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	clientT, serverT := mock.SetupClientServerEx(t, slog.LevelWarn)
	mock.BackgroundIO(t, clientT, serverT)

	mgr := rpc.NewRPCManager()
	pb.RegisterDnsControlServiceServer(mgr, impl)
	runServerDispatch(t, t.Context(), mgr, serverT, logger)

	return pb.NewDnsControlServiceClient(rpc.NewTrsfStreamSource(clientT, logger))
}

func TestDnsControlService_SetDnsPortRoundTrip(t *testing.T) {
	impl := &mockDnsControlService{}
	client := setupDnsControlPair(t, impl)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if _, err := client.SetDnsPort(ctx, &pb.DnsControlServiceSetDnsPortRequest{Port: 5353}); err != nil {
		t.Fatalf("SetDnsPort failed: %v", err)
	}
	if impl.gotPort != 5353 {
		t.Fatalf("expected port 5353, got %d", impl.gotPort)
	}
}

// ACME challenge (旧 StreamMagicACME custom bidi stream) の DnsControlService
// unary RPC 移行の round-trip。domain/token が server まで届く。
func TestDnsControlService_SetupAcmeChallengeRoundTrip(t *testing.T) {
	impl := &mockDnsControlService{}
	client := setupDnsControlPair(t, impl)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if _, err := client.SetupAcmeChallenge(ctx, &pb.DnsControlServiceSetupAcmeChallengeRequest{Domain: "example.com", Token: "tok-123"}); err != nil {
		t.Fatalf("SetupAcmeChallenge failed: %v", err)
	}
	if impl.gotSetupDomain != "example.com" || impl.gotSetupToken != "tok-123" {
		t.Fatalf("expected (example.com, tok-123), got (%q, %q)", impl.gotSetupDomain, impl.gotSetupToken)
	}
}

func TestDnsControlService_CleanupAcmeChallengeRoundTrip(t *testing.T) {
	impl := &mockDnsControlService{}
	client := setupDnsControlPair(t, impl)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if _, err := client.CleanupAcmeChallenge(ctx, &pb.DnsControlServiceCleanupAcmeChallengeRequest{Domain: "example.com", Token: "tok-123"}); err != nil {
		t.Fatalf("CleanupAcmeChallenge failed: %v", err)
	}
	if impl.gotCleanupDomain != "example.com" || impl.gotCleanupToken != "tok-123" {
		t.Fatalf("expected (example.com, tok-123), got (%q, %q)", impl.gotCleanupDomain, impl.gotCleanupToken)
	}
}

// mockPopcacheControlService は GenericControl から PopcacheControlService へ
// typed 移行した set-cert-path の round-trip 検証用。
type mockPopcacheControlService struct {
	pb.UnimplementedPopcacheControlServiceServer
	gotPath string
}

func (s *mockPopcacheControlService) SetCertPath(ctx context.Context, req *pb.PopcacheControlServiceSetCertPathRequest) (*wkt.Empty, error) {
	s.gotPath = req.Path
	return &wkt.Empty{}, nil
}

func setupPopcacheControlPair(t *testing.T, impl pb.PopcacheControlServiceServer) pb.PopcacheControlServiceClient {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	clientT, serverT := mock.SetupClientServerEx(t, slog.LevelWarn)
	mock.BackgroundIO(t, clientT, serverT)

	mgr := rpc.NewRPCManager()
	pb.RegisterPopcacheControlServiceServer(mgr, impl)
	runServerDispatch(t, t.Context(), mgr, serverT, logger)

	return pb.NewPopcacheControlServiceClient(rpc.NewTrsfStreamSource(clientT, logger))
}

func TestPopcacheControlService_SetCertPathRoundTrip(t *testing.T) {
	impl := &mockPopcacheControlService{}
	client := setupPopcacheControlPair(t, impl)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if _, err := client.SetCertPath(ctx, &pb.PopcacheControlServiceSetCertPathRequest{Path: "/etc/tls/cert.pem"}); err != nil {
		t.Fatalf("SetCertPath failed: %v", err)
	}
	if impl.gotPath != "/etc/tls/cert.pem" {
		t.Fatalf("expected path /etc/tls/cert.pem, got %q", impl.gotPath)
	}
}

// mockCertificateService は generator が resource.yaml (rpc_service: true) から
// 自動生成した CertificateService の round-trip 検証用。service 型は pb
// (protobuf/proto)、request/response DTO は pbaccess (protobuf/proto/access)。
type mockCertificateService struct {
	pb.UnimplementedCertificateServiceServer
	gotCommonName string
}

func (s *mockCertificateService) Delete(ctx context.Context, req *pbaccess.ResourceCertificateActionDeleteArgsDTO) (*pbaccess.ResourceCertificateActionDeleteResponseDTO, error) {
	s.gotCommonName = req.CommonName
	return &pbaccess.ResourceCertificateActionDeleteResponseDTO{CommonName: req.CommonName}, nil
}

func setupCertificatePair(t *testing.T, impl pb.CertificateServiceServer) pb.CertificateServiceClient {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	clientT, serverT := mock.SetupClientServerEx(t, slog.LevelWarn)
	mock.BackgroundIO(t, clientT, serverT)

	mgr := rpc.NewRPCManager()
	pb.RegisterCertificateServiceServer(mgr, impl)
	runServerDispatch(t, t.Context(), mgr, serverT, logger)

	return pb.NewCertificateServiceClient(rpc.NewTrsfStreamSource(clientT, logger))
}

// generator が resource.yaml から自動生成した per-resource service の round-trip。
// RegisterCertificateServiceServer / client stub が rpc framework 上で正しく
// dispatch されること (request/response DTO の往復) を確認する。
func TestCertificateService_DeleteRoundTrip(t *testing.T) {
	impl := &mockCertificateService{}
	client := setupCertificatePair(t, impl)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	resp, err := client.Delete(ctx, &pbaccess.ResourceCertificateActionDeleteArgsDTO{CommonName: "example.com"})
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if impl.gotCommonName != "example.com" {
		t.Fatalf("expected common_name example.com, got %q", impl.gotCommonName)
	}
	if resp.CommonName != "example.com" {
		t.Fatalf("expected response common_name example.com, got %q", resp.CommonName)
	}
}
