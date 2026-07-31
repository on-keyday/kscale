// Command dpagent is the l4lb dataplane agent. It enrolls with the control plane
// as application "l4lb", then — instead of being an RPC client like the admin CLI
// — SERVES DataplaneService over the same connection, so the control plane (the
// acceptor) opens RPC streams to it and reconciles/controls it.
//
// All of the shared machinery (enroll, connect, the common DataplaneService, the
// accept-loop + magic registry, StreamStats, reconnect, cert auto-renewal) lives
// in the dataplane substrate; this main only does l4lb-specific flag parsing,
// builds the l4lb controller (which implements dataplane.Hooks), and calls
// dataplane.Run. The controller keeps ksdk's stateful stage-or-apply lifecycle:
// VIP / secret / interface / MTU are staged before StartDataplane, then applied
// live to the eBPF driver. The driver is a logging stub (--driver stub) or the
// real l4lbdrv.L4LB (--driver l4lb, needs an XDP host).
package main

import (
	"flag"
	"log/slog"
	"os"

	"github.com/on-keyday/kscale/dataplane"
	"github.com/on-keyday/kscale/internal/sigctx"
	"github.com/on-keyday/kscale/l4lb/control"
	"github.com/on-keyday/kscale/l4lb/l4lbdrv"
	"github.com/on-keyday/kscale/logbuf"
)

// domain is the CA domain; default "kscale.local", overridden by --ca-domain at startup.
// It MUST match the control plane's --ca-domain (it forms this node's cert CommonName).
var domain = "kscale.local"

func main() {
	logger := logbuf.NewStderrLogger(os.Stderr, slog.LevelInfo)
	fs := flag.NewFlagSet("dpagent", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:9443", "control-plane UDP address")
	dataDir := fs.String("data", "/tmp/kscale-ca", "dir holding the l4lb bootstrap token / saved cert")
	node := fs.String("node", "node1", "this l4lb node's name")
	driverKind := fs.String("driver", "stub", "VIP driver: stub | l4lb (l4lb needs an XDP host + eBPF objects)")
	autoStart := fs.Bool("auto-start", false, "start the dataplane on connect; default off — wait for an explicit StartDataplane (node start)")
	binPath := fs.String("bin-path", "", "l4lb: compiled XDP balancer object")
	xdpHook := fs.String("xdp-hook", "", "l4lb: XDP cap-hook object")
	cryptoBin := fs.String("crypto-bin", "", "l4lb: crypto eBPF object")
	pinDir := fs.String("ebpf-pin-dir", "", "l4lb: eBPF pin directory")
	fileDir := fs.String("file-dir", "", "node-local file store (default <data>/dp_files)")
	domainFlag := fs.String("ca-domain", domain, "CA domain — must match the control plane's --ca-domain")
	_ = fs.Parse(os.Args[1:])
	domain = *domainFlag
	ctx, stop := sigctx.Context()
	defer stop()

	var factory control.DriverFactory
	switch *driverKind {
	case "stub", "":
		factory = control.StubFactory(logger)
	case "l4lb":
		factory = control.L4lbdrvFactory(logger)
	default:
		logger.Error("unknown driver", "driver", *driverKind)
		os.Exit(1)
	}
	ctrl := control.New(&l4lbdrv.FixedConfig{
		BinPath: *binPath, XdpCapHookPath: *xdpHook, CryptoBin: *cryptoBin, EBPFPinDir: *pinDir,
	}, factory, logger)

	if *autoStart {
		if err := ctrl.Start(ctx); err != nil {
			logger.Error("auto-start", "error", err)
			os.Exit(1)
		}
	}

	if err := dataplane.Run(ctx, dataplane.Config{
		Addr:    *addr,
		DataDir: *dataDir,
		Node:    *node,
		App:     "l4lb",
		Domain:  domain,
		FileDir: *fileDir,
		Hooks:   ctrl,
	}, logger); err != nil && ctx.Err() == nil {
		logger.Error("dpagent exited", "error", err)
		os.Exit(1)
	}
}
