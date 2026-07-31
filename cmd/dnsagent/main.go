// Command dnsagent is the dns dataplane agent. Like the l4lb dpagent and the
// popcacheagent it enrolls with the control plane (as application "dns") and
// SERVES its southbound RPCs over the connection.
//
// All of the shared machinery (enroll, connect, the common DataplaneService — file
// plane + StreamStats — the accept-loop + magic registry, /metrics-over-transport,
// reconnect, cert auto-renewal) lives in the dataplane substrate. This main only
// does dns-specific flag parsing, builds the dns controller (which implements
// dataplane.Hooks), and calls dataplane.Run, registering the dns-specific
// DnsControlService via the RegisterExtra hook.
//
// Two server backends mirror ksdk: the built-in authoritative DNS server
// (--backend builtin) and the Cloudflare API backend (--backend cloudflare,
// the ksdk default). The Cloudflare path reconciles records via the API and needs
// an API token + zone ID configured (northbound, via DnsControlService).
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"github.com/on-keyday/kscale/dataplane"
	"github.com/on-keyday/kscale/dns/control"
	"github.com/on-keyday/kscale/dns/server"
	"github.com/on-keyday/kscale/internal/sigctx"
	"github.com/on-keyday/kscale/logbuf"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	"github.com/on-keyday/kscale/protobuf/wkt"
	"github.com/on-keyday/kscale/rpc"
)

// domain is the CA domain; default "kscale.local", overridden by --ca-domain at startup.
// It MUST match the control plane's --ca-domain (it forms this node's cert CommonName).
var domain = "kscale.local"

// dnsControl serves DnsControlService, bridging each method onto the dns
// controller. This is the typed-RPC replacement for ksdk's GenericControl set-dns-*
// commands and the StreamMagicACME bidi stream (now SetupAcmeChallenge /
// CleanupAcmeChallenge unary RPCs).
type dnsControl struct {
	pb.UnimplementedDnsControlServiceServer
	c *control.Controller
}

func (s *dnsControl) SetDnsPort(ctx context.Context, req *pb.DnsControlServiceSetDnsPortRequest) (*wkt.Empty, error) {
	return empty(s.c.SetPort(req.Port))
}
func (s *dnsControl) SetDnsDomain(ctx context.Context, req *pb.DnsControlServiceSetDnsDomainRequest) (*wkt.Empty, error) {
	return empty(s.c.SetDomain(req.Domain))
}
func (s *dnsControl) SetDnsMail(ctx context.Context, req *pb.DnsControlServiceSetDnsMailRequest) (*wkt.Empty, error) {
	return empty(s.c.SetMailAddress(req.Mail))
}
func (s *dnsControl) SetDnsApiToken(ctx context.Context, req *pb.DnsControlServiceSetDnsApiTokenRequest) (*wkt.Empty, error) {
	return empty(s.c.SetAPIToken(req.Token))
}
func (s *dnsControl) SetDnsApiZoneId(ctx context.Context, req *pb.DnsControlServiceSetDnsApiZoneIdRequest) (*wkt.Empty, error) {
	return empty(s.c.SetZoneID(req.ZoneId))
}

// ApplyConfig is the combined setter the dns_config resource's reconcile drives:
// apply port / domain / mail / api-token / api-zone-id in one call. Empty/zero fields
// leave that setting unchanged. The api-token is never logged.
func (s *dnsControl) ApplyConfig(ctx context.Context, req *pb.DnsControlServiceApplyConfigRequest) (*wkt.Empty, error) {
	if req.Port != 0 {
		if err := s.c.SetPort(uint16(req.Port)); err != nil {
			return nil, err
		}
	}
	if req.Domain != "" {
		if err := s.c.SetDomain(req.Domain); err != nil {
			return nil, err
		}
	}
	if req.Mail != "" {
		if err := s.c.SetMailAddress(req.Mail); err != nil {
			return nil, err
		}
	}
	if req.ApiToken != "" {
		if err := s.c.SetAPIToken(req.ApiToken); err != nil {
			return nil, err
		}
	}
	if req.ApiZoneId != "" {
		if err := s.c.SetZoneID(req.ApiZoneId); err != nil {
			return nil, err
		}
	}
	return &wkt.Empty{}, nil
}
func (s *dnsControl) ResetDnsAcme(ctx context.Context, req *pb.DnsControlServiceResetDnsAcmeRequest) (*wkt.Empty, error) {
	return empty(s.c.ResetAcmeToken(req.Domain))
}
func (s *dnsControl) SetupAcmeChallenge(ctx context.Context, req *pb.DnsControlServiceSetupAcmeChallengeRequest) (*wkt.Empty, error) {
	return empty(s.c.SetAcmeToken(req.Domain, req.Token))
}
func (s *dnsControl) CleanupAcmeChallenge(ctx context.Context, req *pb.DnsControlServiceCleanupAcmeChallengeRequest) (*wkt.Empty, error) {
	return empty(s.c.ClearAcmeToken(req.Domain, req.Token))
}

func empty(err error) (*wkt.Empty, error) {
	if err != nil {
		return nil, err
	}
	return &wkt.Empty{}, nil
}

func main() {
	logger := logbuf.NewStderrLogger(os.Stderr, slog.LevelInfo)
	fs := flag.NewFlagSet("dnsagent", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:9443", "control-plane UDP address")
	dataDir := fs.String("data", "/tmp/kscale-ca", "dir holding the dns bootstrap token / saved cert")
	node := fs.String("node", "node1", "this dns node's name")
	backend := fs.String("backend", "cloudflare", "dns backend: builtin (authoritative miekg server) | cloudflare (API)")
	dnsDomain := fs.String("domain", "", "authoritative/managed domain (0 = wait for control-plane config)")
	dnsPort := fs.Uint("dns-port", 0, "built-in server listen port (0 = wait for control-plane config / default 53)")
	autoStart := fs.Bool("auto-start", false, "start the dns server on connect; default off — wait for an explicit StartDataplane (node start)")
	fileDir := fs.String("file-dir", "", "node-local file store (default <data>/dp_files)")
	domainFlag := fs.String("ca-domain", domain, "CA domain — must match the control plane's --ca-domain")
	_ = fs.Parse(os.Args[1:])
	domain = *domainFlag
	ctx, stop := sigctx.Context()
	defer stop()

	var srv server.Server
	switch *backend {
	case "builtin":
		srv = server.NewServer(logger)
	case "cloudflare", "":
		srv = server.NewCloudflareDNS()
	default:
		logger.Error("unknown dns backend", "backend", *backend)
		os.Exit(1)
	}
	ctrl := control.New(logger, srv)

	if *dnsDomain != "" {
		if err := ctrl.SetDomain(*dnsDomain); err != nil {
			logger.Error("domain", "error", err)
			os.Exit(1)
		}
	}
	if *dnsPort != 0 {
		if err := ctrl.SetPort(uint16(*dnsPort)); err != nil {
			logger.Error("dns-port", "error", err)
			os.Exit(1)
		}
	}
	if *autoStart {
		if err := ctrl.Start(ctx); err != nil {
			logger.Error("auto-start", "error", err)
			os.Exit(1)
		}
		logger.Info("dns server started", "backend", *backend, "domain", *dnsDomain)
	}

	if err := dataplane.Run(ctx, dataplane.Config{
		Addr:    *addr,
		DataDir: *dataDir,
		Node:    *node,
		App:     "dns",
		Domain:  domain,
		FileDir: *fileDir,
		Hooks:   ctrl,
		// dns-specific RPC service rides the same per-connection manager.
		RegisterExtra: func(mgr *rpc.RPCManager) {
			pb.RegisterDnsControlServiceServer(mgr, &dnsControl{c: ctrl})
		},
	}, logger); err != nil && ctx.Err() == nil {
		logger.Error("dnsagent exited", "error", err)
		os.Exit(1)
	}
}
