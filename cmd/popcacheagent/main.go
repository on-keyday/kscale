// Command popcacheagent is the popcache dataplane agent. Like the l4lb dpagent it
// enrolls with the control plane (as application "popcache") and SERVES its
// southbound RPCs over the connection.
//
// All of the shared machinery (enroll, connect, the common DataplaneService —
// including the file plane + StreamStats — the accept-loop + magic registry,
// /metrics-over-transport, reconnect, cert auto-renewal) lives in the dataplane
// substrate. This main only does popcache-specific flag parsing, builds the
// popcache controller (which implements dataplane.Hooks), and calls
// dataplane.Run, registering the popcache-specific RPC services
// (PopcacheControlService + WasmService) via the RegisterExtra hook.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/on-keyday/kscale/dataplane"
	"github.com/on-keyday/kscale/internal/sigctx"
	"github.com/on-keyday/kscale/logbuf"
	"github.com/on-keyday/kscale/popcache/control"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	"github.com/on-keyday/kscale/protobuf/wkt"
	"github.com/on-keyday/kscale/rpc"
)

// domain is the CA domain; default "kscale.local", overridden by --ca-domain at startup.
// It MUST match the control plane's --ca-domain (it forms this node's cert CommonName).
var domain = "kscale.local"

// popcacheControl serves PopcacheControlService, bridging each setter onto the
// popcache controller (parsing the string-typed port/bool args).
type popcacheControl struct {
	pb.UnimplementedPopcacheControlServiceServer
	c *control.Controller
}

func (s *popcacheControl) SetCertPath(ctx context.Context, req *pb.PopcacheControlServiceSetCertPathRequest) (*wkt.Empty, error) {
	s.c.SetCertPath(req.Path)
	return &wkt.Empty{}, nil
}
func (s *popcacheControl) SetKeyPath(ctx context.Context, req *pb.PopcacheControlServiceSetKeyPathRequest) (*wkt.Empty, error) {
	s.c.SetKeyPath(req.Path)
	return &wkt.Empty{}, nil
}
func (s *popcacheControl) SetHttpPort(ctx context.Context, req *pb.PopcacheControlServiceSetHttpPortRequest) (*wkt.Empty, error) {
	return s.setPort(req.Port, s.c.SetHttpPort)
}
func (s *popcacheControl) SetHttpsPort(ctx context.Context, req *pb.PopcacheControlServiceSetHttpsPortRequest) (*wkt.Empty, error) {
	return s.setPort(req.Port, s.c.SetHttpsPort)
}
func (s *popcacheControl) SetHttp3Port(ctx context.Context, req *pb.PopcacheControlServiceSetHttp3PortRequest) (*wkt.Empty, error) {
	return s.setPort(req.Port, s.c.SetHttp3Port)
}
func (s *popcacheControl) SetOrigin(ctx context.Context, req *pb.PopcacheControlServiceSetOriginRequest) (*wkt.Empty, error) {
	if err := s.c.SetOrigin(req.Origin); err != nil {
		return nil, err
	}
	return &wkt.Empty{}, nil
}

// ApplyConfig is the combined setter the popcache_config reconcile drives: set
// origin and/or HTTP port in one call (empty/zero leaves a setting unchanged).
func (s *popcacheControl) ApplyConfig(ctx context.Context, req *pb.PopcacheControlServiceApplyConfigRequest) (*wkt.Empty, error) {
	if req.Origin != "" {
		if err := s.c.SetOrigin(req.Origin); err != nil {
			return nil, err
		}
	}
	if req.Port != 0 {
		s.c.SetHttpPort(uint16(req.Port))
	}
	if req.HttpsPort != 0 {
		s.c.SetHttpsPort(uint16(req.HttpsPort))
	}
	if req.Http3Port != 0 {
		s.c.SetHttp3Port(uint16(req.Http3Port))
	}
	// TLS cert/key paths: declarative + reconciled, so they're re-applied on every
	// reconnect (an agent restart no longer forgets where its cert is — the file
	// persists on disk, the path comes back from the desired state). Set separately
	// from ACME, which only delivers the cert file to that path.
	certChanged := false
	if req.CertPath != "" {
		s.c.SetCertPath(req.CertPath)
		certChanged = true
	}
	if req.KeyPath != "" {
		s.c.SetKeyPath(req.KeyPath)
		certChanged = true
	}
	if certChanged {
		// Pick up the new path on an already-running server; a not-yet-started one
		// loads it when it starts, so a "not running" error here is fine to ignore.
		_ = s.c.ReloadTlsCert()
	}
	// qlog is a declarative bool — the desired value is authoritative, so always apply.
	s.c.SetQlogEnabled(req.QlogEnabled)
	// Declarative like qlog: always apply (0 = built-in default), so removing the
	// knobs from the desired config resets the node.
	s.c.SetWasmComputeBudgets(req.WasmComputeDefaultMs, req.WasmComputeMaxMs)
	return &wkt.Empty{}, nil
}
func (s *popcacheControl) SetQlogEnabled(ctx context.Context, req *pb.PopcacheControlServiceSetQlogEnabledRequest) (*wkt.Empty, error) {
	b, err := strconv.ParseBool(req.Enabled)
	if err != nil {
		return nil, fmt.Errorf("popcacheagent: invalid qlog enabled %q: %w", req.Enabled, err)
	}
	s.c.SetQlogEnabled(b)
	return &wkt.Empty{}, nil
}
func (s *popcacheControl) ReloadTlsCert(ctx context.Context, _ *wkt.Empty) (*wkt.Empty, error) {
	return &wkt.Empty{}, s.c.ReloadTlsCert()
}

func (s *popcacheControl) setPort(raw string, set func(uint16)) (*wkt.Empty, error) {
	n, err := strconv.ParseUint(raw, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("popcacheagent: invalid port %q: %w", raw, err)
	}
	set(uint16(n))
	return &wkt.Empty{}, nil
}

func main() {
	logger := logbuf.NewStderrLogger(os.Stderr, slog.LevelInfo)
	fs := flag.NewFlagSet("popcacheagent", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:9443", "control-plane UDP address")
	dataDir := fs.String("data", "/tmp/kscale-ca", "dir holding the popcache bootstrap token / saved cert")
	node := fs.String("node", "node1", "this popcache node's name")
	httpPort := fs.Uint("http-port", 0, "local HTTP cache listen port (0 = wait for control-plane config)")
	origin := fs.String("origin", "", "origin URL to proxy/cache (e.g. http://127.0.0.1:8080)")
	autoStart := fs.Bool("auto-start", false, "start the HTTP cache server on connect; default off — wait for an explicit StartDataplane (node start)")
	fileDir := fs.String("file-dir", "", "node-local file store (default <data>/dp_files)")
	domainFlag := fs.String("ca-domain", domain, "CA domain — must match the control plane's --ca-domain")
	_ = fs.Parse(os.Args[1:])
	domain = *domainFlag
	ctx, stop := sigctx.Context()
	defer stop()

	// Node-local file root, shared between the substrate's file plane (DpFile.send)
	// and the popcache server (WasmService.Register reads the same dir).
	fdir := *fileDir
	if fdir == "" {
		fdir = filepath.Join(*dataDir, "dp_files")
	}
	if err := os.MkdirAll(fdir, 0o755); err != nil {
		logger.Error("file dir", "error", err)
		os.Exit(1)
	}
	ctrl, err := control.New(logger, fdir)
	if err != nil {
		logger.Error("controller", "error", err)
		os.Exit(1)
	}
	if *httpPort != 0 {
		ctrl.SetHttpPort(uint16(*httpPort))
	}
	if *origin != "" {
		if err := ctrl.SetOrigin(*origin); err != nil {
			logger.Error("origin", "error", err)
			os.Exit(1)
		}
	}

	if *autoStart {
		if err := ctrl.Run(ctx); err != nil {
			logger.Error("auto-start", "error", err)
			os.Exit(1)
		}
		logger.Info("popcache HTTP cache server started", "http_port", *httpPort, "origin", *origin)
	}

	if err := dataplane.Run(ctx, dataplane.Config{
		Addr:    *addr,
		DataDir: *dataDir,
		Node:    *node,
		App:     "popcache",
		Domain:  domain,
		FileDir: fdir,
		Hooks:   ctrl,
		// popcache-specific RPC services ride the same per-connection manager.
		RegisterExtra: func(mgr *rpc.RPCManager) {
			pb.RegisterPopcacheControlServiceServer(mgr, &popcacheControl{c: ctrl})
			pb.RegisterWasmServiceServer(mgr, ctrl.Server().WasmService())
		},
	}, logger); err != nil && ctx.Err() == nil {
		logger.Error("popcacheagent exited", "error", err)
		os.Exit(1)
	}
}
