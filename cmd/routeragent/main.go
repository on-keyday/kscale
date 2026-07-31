// Command routeragent is the router dataplane agent. Like the other dataplane
// agents (dpagent/popcacheagent/dnsagent) it enrolls with the control plane (as
// application "router") and SERVES its southbound RPCs over the connection.
//
// All of the shared machinery (enroll, connect, the common DataplaneService — file
// plane + StreamStats — the accept-loop + magic registry, reconnect, cert auto-
// renewal) lives in the dataplane substrate. This main only does router-specific
// flag parsing, builds the router controller (which implements dataplane.Hooks),
// and calls dataplane.Run, registering the router-specific RouterService via the
// RegisterExtra hook.
//
// RouterService is the typed-RPC control surface for the managed Cisco IOS-XE
// device: ListOpenPort / DesireOpenPort drive the ACL sync (with the safety
// reload-in-5 rollback), and SetRouterHostname/Username/Password set the device
// connection — the typed-RPC replacement for ksdk's GenericControl set-router-*.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"github.com/on-keyday/kscale/dataplane"
	"github.com/on-keyday/kscale/internal/sigctx"
	"github.com/on-keyday/kscale/logbuf"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	"github.com/on-keyday/kscale/protobuf/wkt"
	"github.com/on-keyday/kscale/router/control"
	"github.com/on-keyday/kscale/router/dev"
	"github.com/on-keyday/kscale/rpc"
)

// domain is the CA domain; default "kscale.local", overridden by --ca-domain at startup.
// It MUST match the control plane's --ca-domain (it forms this node's cert CommonName).
var domain = "kscale.local"

// routerService serves RouterService, bridging each method onto the device Client
// held by the controller.
type routerService struct {
	pb.UnimplementedRouterServiceServer
	client *dev.Client
	logger *slog.Logger
}

func (s *routerService) ListOpenPort(ctx context.Context, req *pb.RouterServiceListOpenPortRequest) (*pb.RouterServiceListOpenPortResponse, error) {
	ports, err := s.client.GetCurrentACLPorts(req.AclName, s.logger)
	if err != nil {
		return nil, err
	}
	return &pb.RouterServiceListOpenPortResponse{Ports: ports}, nil
}

func (s *routerService) DesireOpenPort(ctx context.Context, req *pb.RouterServiceDesireOpenPortRequest) (*pb.RouterServiceDesireOpenPortResponse, error) {
	diff, err := s.client.SyncACL(req.AclName, req.Ports, req.DryRun, s.logger)
	if err != nil {
		return nil, err
	}
	return &pb.RouterServiceDesireOpenPortResponse{Diff: diff}, nil
}

func (s *routerService) SetRouterHostname(ctx context.Context, req *pb.RouterServiceSetRouterHostnameRequest) (*wkt.Empty, error) {
	s.client.SetHost(req.Hostname)
	s.logger.Info("router agent: router host updated", "host", req.Hostname)
	return &wkt.Empty{}, nil
}

func (s *routerService) SetRouterUsername(ctx context.Context, req *pb.RouterServiceSetRouterUsernameRequest) (*wkt.Empty, error) {
	s.client.SetUser(req.Username)
	s.logger.Info("router agent: router username updated", "username", req.Username)
	return &wkt.Empty{}, nil
}

func (s *routerService) SetRouterPassword(ctx context.Context, req *pb.RouterServiceSetRouterPasswordRequest) (*wkt.Empty, error) {
	s.client.SetPassword(req.Password)
	s.logger.Info("router agent: router password updated")
	return &wkt.Empty{}, nil
}

// ApplyConfig is the combined setter the router_config resource's reconcile drives:
// it applies hostname / username / password in one call. An empty field leaves that
// setting unchanged (so a config that omits a value doesn't wipe it). The password is
// never logged.
func (s *routerService) ApplyConfig(ctx context.Context, req *pb.RouterServiceApplyConfigRequest) (*wkt.Empty, error) {
	if req.Hostname != "" {
		s.client.SetHost(req.Hostname)
	}
	if req.Username != "" {
		s.client.SetUser(req.Username)
	}
	if req.Password != "" {
		s.client.SetPassword(req.Password)
	}
	s.logger.Info("router agent: router config applied", "host", req.Hostname, "username", req.Username)
	return &wkt.Empty{}, nil
}

func main() {
	logger := logbuf.NewStderrLogger(os.Stderr, slog.LevelInfo)
	fs := flag.NewFlagSet("routeragent", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:9443", "control-plane UDP address")
	dataDir := fs.String("data", "/tmp/kscale-ca", "dir holding the router bootstrap token / saved cert")
	node := fs.String("node", "node1", "this router node's name")
	host := fs.String("router-host", "", "managed router host (0 = wait for control-plane config)")
	user := fs.String("router-user", "", "managed router username")
	pass := fs.String("router-pass", "", "managed router password")
	fileDir := fs.String("file-dir", "", "node-local file store (default <data>/dp_files)")
	domainFlag := fs.String("ca-domain", domain, "CA domain — must match the control plane's --ca-domain")
	_ = fs.Parse(os.Args[1:])
	domain = *domainFlag
	ctx, stop := sigctx.Context()
	defer stop()

	ctrl := control.New(logger)
	if *host != "" {
		ctrl.Client().SetHost(*host)
	}
	if *user != "" {
		ctrl.Client().SetUser(*user)
	}
	if *pass != "" {
		ctrl.Client().SetPassword(*pass)
	}

	if err := dataplane.Run(ctx, dataplane.Config{
		Addr:    *addr,
		DataDir: *dataDir,
		Node:    *node,
		App:     "router",
		Domain:  domain,
		FileDir: *fileDir,
		Hooks:   ctrl,
		// router-specific RPC service rides the same per-connection manager.
		RegisterExtra: func(mgr *rpc.RPCManager) {
			pb.RegisterRouterServiceServer(mgr, &routerService{client: ctrl.Client(), logger: logger})
		},
	}, logger); err != nil && ctx.Err() == nil {
		logger.Error("routeragent exited", "error", err)
		os.Exit(1)
	}
}
