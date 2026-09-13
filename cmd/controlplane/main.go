// Command controlplane is the kscale control plane daemon. It runs a self-signed
// CA, seeds the demo admin/viewer authorities + the per-dp-kind node authorities,
// issues bootstrap tokens, and hosts every resource's generated ABAC gate over the
// peer/RPC transport. It owns the dataplane broker (node inventory), the stat
// collector, and the reconcile loops (vip/secret/wasm_module/popcache_config) that
// fan desired state out to the connected dataplane agents.
//
// The admin-facing client is a separate binary, cmd/cli.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/on-keyday/kscale/access"
	"github.com/on-keyday/kscale/access/audit"
	"github.com/on-keyday/kscale/access/predefined"
	"github.com/on-keyday/kscale/alert"
	"github.com/on-keyday/kscale/ca"
	castorage "github.com/on-keyday/kscale/ca/storage"
	"github.com/on-keyday/kscale/consts"
	"github.com/on-keyday/kscale/desiredstate"
	"github.com/on-keyday/kscale/dpbroker"
	"github.com/on-keyday/kscale/internal/demo"
	"github.com/on-keyday/kscale/internal/epclean"
	"github.com/on-keyday/kscale/internal/safe"
	"github.com/on-keyday/kscale/internal/sigctx"
	"github.com/on-keyday/kscale/logbuf"
	"github.com/on-keyday/kscale/peer"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	pbstat "github.com/on-keyday/kscale/protobuf/proto/stat"
	"github.com/on-keyday/kscale/protobuf/wkt"
	"github.com/on-keyday/kscale/reconcile"
	"github.com/on-keyday/kscale/rpc"
	"github.com/on-keyday/kscale/service"
	"github.com/on-keyday/kscale/service/acme"
	alertsvc "github.com/on-keyday/kscale/service/alert"
	"github.com/on-keyday/kscale/service/bootstraptoken"
	"github.com/on-keyday/kscale/service/certificate"
	"github.com/on-keyday/kscale/service/connection"
	"github.com/on-keyday/kscale/service/container"
	"github.com/on-keyday/kscale/service/cplanefile"
	"github.com/on-keyday/kscale/service/dataplanenode"
	"github.com/on-keyday/kscale/service/dnsconfig"
	"github.com/on-keyday/kscale/service/dpfile"
	"github.com/on-keyday/kscale/service/expectednode"
	"github.com/on-keyday/kscale/service/iface"
	"github.com/on-keyday/kscale/service/l4lbobject"
	"github.com/on-keyday/kscale/service/logs"
	"github.com/on-keyday/kscale/service/monitorchat"
	"github.com/on-keyday/kscale/service/mtu"
	"github.com/on-keyday/kscale/service/nodefile"
	"github.com/on-keyday/kscale/service/openport"
	"github.com/on-keyday/kscale/service/popcacheconfig"
	"github.com/on-keyday/kscale/service/routerconfig"
	"github.com/on-keyday/kscale/service/secret"
	"github.com/on-keyday/kscale/service/stats"
	"github.com/on-keyday/kscale/service/userhierarchyauthority"
	"github.com/on-keyday/kscale/service/vip"
	"github.com/on-keyday/kscale/service/wasmmodule"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/transport"
	"github.com/on-keyday/objtrsf/trsf"
)

func main() {
	// Capture the control plane's own logs into logbuf so `cli logs` (empty
	// common_name) can stream them, just like the dataplane agents do.
	logger := logbuf.NewStderrLogger(os.Stderr, slog.LevelInfo)
	ctx, stop := sigctx.Context()
	defer stop()
	if err := run(ctx, logger, os.Args[1:]); err != nil {
		if errors.Is(err, context.Canceled) {
			// Signal-driven shutdown (sigctx cancelled the root ctx): a clean stop,
			// not a failure.
			logger.Info("controlplane stopped")
			return
		}
		logger.Error("controlplane failed", "error", err)
		os.Exit(1)
	}
}

// loadOrCreateCASecret resolves the 32-byte secret that encrypts the CA storage +
// ca_state.dat. Priority:
//  1. $KSCALE_CA_SECRET (64 hex chars) — production: injected from a vault/credential,
//     so the secret is never co-located with the encrypted data on disk.
//  2. <dir>/ca_secret.key — a persisted per-deployment secret.
//  3. otherwise generate one and persist it (0600).
//
// This replaces a hardcoded demo key (byte(i+1)) that anyone with the source could
// derive; option 3 alone already removes that, and option 1 keeps the secret off the
// data disk entirely. (Changing the secret can't decrypt a CA created under a
// different one — start fresh or migrate.)
func loadOrCreateCASecret(dir string) ([]byte, error) {
	if hexs := strings.TrimSpace(os.Getenv("KSCALE_CA_SECRET")); hexs != "" {
		b, err := hex.DecodeString(hexs)
		if err != nil {
			return nil, fmt.Errorf("KSCALE_CA_SECRET: %w", err)
		}
		if len(b) != 32 {
			return nil, fmt.Errorf("KSCALE_CA_SECRET must be 32 bytes (64 hex chars), got %d", len(b))
		}
		return b, nil
	}
	path := filepath.Join(dir, "ca_secret.key")
	if b, err := os.ReadFile(path); err == nil {
		if len(b) != 32 {
			return nil, fmt.Errorf("%s: expected 32 bytes, got %d", path, len(b))
		}
		return b, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, err
	}
	return b, nil
}

func setupCA(dir string) (*ca.CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	secret, err := loadOrCreateCASecret(dir)
	if err != nil {
		return nil, err
	}
	storageDir := filepath.Join(dir, "storage")
	if err := os.MkdirAll(storageDir, 0o700); err != nil {
		return nil, err
	}
	st, err := castorage.NewDirStorage(storageDir, time.Now, secret)
	if err != nil {
		return nil, err
	}
	statePath := filepath.Join(dir, "ca_state.dat")
	// Reuse the persisted CA across restarts. Minting a fresh root key every startup
	// (the previous behaviour) re-signs a NEW "kscale Root CA" each boot, which
	// invalidates every cert issued under the old key — the dataplane agents then fail
	// the handshake with "x509: Ed25519 verification failure ... kscale Root CA" forever
	// (no amount of re-enrollment converges, since the next restart changes it again).
	// When ca_state.dat exists we MUST reload it (a load failure is fatal rather than
	// silently regenerated, which would re-trigger the churn); only mint a new CA on
	// first init. NewSelfSignedCA / NewFromCAState both persist the state.
	if _, statErr := os.Stat(statePath); statErr == nil {
		data, lerr := castorage.LoadAESEncryptedDataFromFile(statePath, secret)
		if lerr != nil {
			return nil, fmt.Errorf("load CA state %s: %w", statePath, lerr)
		}
		return ca.NewFromCAState(data, st, statePath, secret)
	}
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	return ca.NewSelfSignedCA(priv, "kscale Root CA", st, statePath, secret)
}

// commonNameToAuthority resolves a verified cert CommonName to a leaf authority
// in the root tree (ported from ksdk agent/incoming/handshake.go).
func commonNameToAuthority(cn string, root access.RootAuthority) (access.Authority, error) {
	fn, err := access.ParseCommonNameToFullNameStrict(cn, demo.Domain)
	if err != nil {
		return nil, err
	}
	a, ok := root.GetDescendant(fn)
	if !ok {
		// Lazy-create is restricted to MANAGEMENT identities (admin.ca.manager.*: admin /
		// viewer / monitor, incl. the nodewatch/metricsgw monitor CNs). A dataplane/machine
		// authority (system.dp.*, etc.) must be created explicitly by an admin
		// (`authority apply`) before its node enrolls — so every dp kind is uniform (no
		// l4lb/popcache hardcoded seed vs dns/router special case) and nothing self-registers
		// an authority just by holding a cert. The startup ListCommonNames replay still
		// reconstructs already-enrolled nodes' authorities from their persisted certs.
		if !(len(fn.Names) >= 3 && fn.Names[0] == "admin" && fn.Names[1] == "ca" && fn.Names[2] == "manager") {
			return nil, fmt.Errorf("unknown authority for %q — an admin must create it (authority apply) before this node can enroll", cn)
		}
		la, err := root.CreateLeafDescendant(fn, true)
		if err != nil {
			return nil, fmt.Errorf("unknown authority for %q (lazy create failed: %w)", cn, err)
		}
		a = la
	}
	if !a.IsLeaf() {
		return nil, fmt.Errorf("authority for %q is not a leaf", cn)
	}
	return a, nil
}

// runRootIssuer is the production bootstrap of the trust chain: while the CA has no
// admin (and no outstanding bootstrap token), it auto-issues a single first-admin
// bootstrap token to <data>/bootstrap.admin.token, then stops once an admin enrolls
// (removing the now-stale file). Every other role's token is minted on demand by
// that admin. Ported from ksdk's agent/system/root_issuer.go. The admin is the first
// management-tier cert to appear (the operator), since all other tokens require an
// already-authenticated admin to mint.
func runRootIssuer(ctx context.Context, caObj *ca.CA, dataDir string, tokenTTL, certExp time.Duration, logger *slog.Logger) {
	// The first-admin identity is exactly ClientCN("admin") = admin.manager.ca.admin.<domain>.
	// We must match THIS, not the whole "manager.ca.admin.<domain>" management-tier suffix:
	// viewer and monitor (nodewatch, metricsgw) enroll with CNs that share that suffix but
	// are NOT admins, so a broad suffix count wedged the re-issue gate forever once any
	// monitor held a (valid, auto-renewed) cert. The cert index carries no role, so we key
	// on the admin's deterministic CN. GetSuffixCommonNames already drops expired certs, so a
	// merely-lapsed admin correctly reads as absent and the root-issuer self-heals.
	adminCN := demo.ClientCN("admin")
	mgmtSuffix := "manager.ca.admin." + demo.Domain
	tokenPath := filepath.Join(dataDir, "bootstrap.admin.token")
	tick := func() {
		// Drop expired tokens first: GetBootstrapTokenCount counts expired entries, so
		// a first-admin token that is issued but never used would otherwise wedge the
		// gate below forever (no admin + an expired-but-counted token => never re-issue,
		// and no admin means no way to run a manual gc). Cleaning here keeps the
		// root-issuer self-healing — including when the admin later disappears.
		caObj.DeleteExpiredBootstrapTokens(logger)
		mgmt, err := caObj.GetSuffixCommonNames(mgmtSuffix)
		if err != nil {
			logger.Error("root issuer: failed to count admins", "error", err)
			return
		}
		adminExists := false
		for _, cn := range mgmt {
			if cn == adminCN { // an actual admin, not a viewer/monitor sharing the suffix
				adminExists = true
				break
			}
		}
		if adminExists {
			_ = os.Remove(tokenPath) // an admin exists; drop the now-stale token file
			return
		}
		if caObj.GetBootstrapTokenCount() > 0 {
			return // a token is already outstanding — wait for it to be used or expire
		}
		token, err := caObj.GenerateBootstrapToken(tokenTTL, "admin", demo.Domain, certExp)
		if err != nil {
			logger.Error("root issuer: failed to generate token", "error", err)
			return
		}
		if err := os.WriteFile(tokenPath, token, 0o600); err != nil {
			logger.Error("root issuer: failed to write token", "error", err)
			return
		}
		logger.Info("root issuer: issued first-admin bootstrap token", "path", tokenPath, "ttl", tokenTTL.String())
	}
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	tick() // immediately on startup
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}

func run(ctx context.Context, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("controlplane", flag.ExitOnError)
	port := fs.Uint("port", 9443, "UDP listen port")
	wsPort := fs.Uint("ws-port", 9444, "WebSocket (TCP) transport listen port, for clients that can't reach the UDP port (e.g. an admin CLI over an SSH-forwarded port). 0 disables it.")
	dataDir := fs.String("data", "/tmp/kscale-ca", "CA + bootstrap-token data dir")
	demoSeed := fs.Bool("demo", false, "demo mode: unconditionally seed a short-lived bootstrap token for every role (admin/viewer/monitor/l4lb/popcache) at startup so a local CP + agents sharing one --data dir can all enroll. Off (default, production) uses the root-issuer: a first-admin token is auto-issued only while no admin exists; everything else is minted on demand by that admin.")
	adminTokenTTL := fs.Duration("admin-token-ttl", time.Hour, "validity of the auto-issued first-admin bootstrap token (production mode)")
	adminCertExp := fs.Duration("admin-cert-exp", 168*time.Hour, "validity of the admin certificate issued from the first-admin token (production mode)")
	machineCertExp := fs.Duration("machine-cert-exp", 168*time.Hour, "validity a dataplane (machine) certificate adopts on RENEWAL. Renewal used to carry the issued period forward forever, so the only way to change a node's lifetime was to re-enroll it. This does not change what a FRESH enrollment gets — that comes from the expires_period the admin minted the token with, capped by the bootstrap policy.")
	auditTrace := fs.Bool("audit", false, "print the full ABAC policy evaluation (resolved attribute values + per-condition results + decision) to stderr — use to see why a request is allowed/denied")
	domainFlag := fs.String("ca-domain", demo.Domain, "CA domain: the authority-tree root + cert CommonName suffix. MUST match every client and dataplane agent's --ca-domain.")
	_ = fs.Parse(args)
	demo.Domain = *domainFlag // configure the shared CA domain before it's used below

	caObj, err := setupCA(*dataDir)
	if err != nil {
		return fmt.Errorf("setup CA: %w", err)
	}
	// Let a renewing machine (dataplane) cert pick up the currently configured lifetime.
	// Without this the period is frozen at enrollment: on 2026-09-13 the fleet's 24h certs
	// had lapsed during a multi-week CP outage, and raising the lifetime would have meant
	// re-enrolling every node.
	//
	// "Machine" is tested POSITIVELY, as system.* — the same thing the bootstrap policy
	// means by it ($env.args.domain suffix $env.authority.system.). Everything else returns
	// 0 and keeps the period it has: the admin's comes from the root-issuer at
	// --admin-cert-exp, the monitors' from a minted token, and neither should be reshaped by
	// a renewal. Asking "is it NOT management?" instead would look equivalent and is not —
	// admin.dp.manager is an admin-tier identity the bootstrap policy issues for, and it
	// would fall through to the machine period, as would any tier added later. An unknown
	// CN must keep its lifetime, not inherit the dataplane's.
	caObj.RenewCertExp = func(_ string, cn string) time.Duration {
		fn, err := access.ParseCommonNameToFullNameStrict(cn, demo.Domain)
		if err != nil {
			return 0 // not a CN we can classify — leave its period alone
		}
		if len(fn.Names) >= 1 && fn.Names[0] == "system" {
			return *machineCertExp
		}
		return 0
	}

	// Seed each demo principal as a leaf under admin.ca.manager so its issued cert
	// can authenticate (commonNameToAuthority requires an existing leaf).
	// The root authority is named with the domain (ksdk convention,
	// cmd/agent/main.go) so the tree is domain-rooted: $env.authority.* and
	// $user.authority.full_name carry the domain, matching the certificate CNs
	// and the domain-qualified authority paths the bootstrap policy compares.
	root := access.NewRootAuthority(demo.Domain)
	for _, role := range demo.ManagerRoles {
		if _, err := root.CreateLeafDescendant(access.NewFullName([]string{"admin", "ca", "manager", role}), true); err != nil {
			return fmt.Errorf("seed authority %s: %w", role, err)
		}
	}
	// No dataplane seeding in production: create_authority gates the authority_name with a
	// literal (label-anchored suffix), not $env.authority, so an admin can `authority apply`
	// the first node under system.dp even though system.dp doesn't exist yet
	// (CreateLeafDescendant makeParent builds the chain). Every dp kind is uniform.
	if *demoSeed {
		// Demo convenience only: seed every dp kind's node authority so a local dp-agent can
		// enroll without an explicit `authority apply` (production requires the apply).
		for _, kind := range predefined.DataplaneTypes() {
			if _, err := root.CreateLeafDescendant(access.NewFullName([]string{"system", "dp", kind, "node1"}), true); err != nil {
				return fmt.Errorf("seed %s authority: %w", kind, err)
			}
		}
	}

	// Rebuild the authority tree from the persisted CA storage on every startup
	// (ported from ksdk cmd/agent/main.go setupAuthorityFromCA). The authority tree
	// is in-memory: descendants created at enroll time (UserHierarchyAuthorityService
	// .Create -> root.CreateLeafDescendant) live only in `root`, NOT in ca_state.dat.
	// Without this replay a CP restart drops every dynamically-enrolled node (s1/s2/s3,
	// dns/router) back to just the hardcoded seeds above, so their already-issued certs
	// verify but then fail authentication with "unknown authority for ..." forever (no
	// re-enroll converges across the next restart). The certs themselves ARE persisted
	// in castorage, so replaying ListCommonNames reconstructs the full tree idempotently
	// (CreateLeafDescendant reuses any node already seeded). See notes/bugs/bug_2026_06_30_*.
	cns, err := caObj.ListCommonNames()
	if err != nil {
		return fmt.Errorf("list common names for authority rebuild: %w", err)
	}
	for _, cn := range cns {
		if _, err := root.CreateLeafDescendant(access.ParseCommonNameToFullName(cn, demo.Domain), true); err != nil {
			return fmt.Errorf("rebuild authority for %q: %w", cn, err)
		}
	}
	logger.Info("rebuilt authority tree from persisted CA storage", "common_names", len(cns))

	// Bootstrap tokens: admin (policy allows), viewer (policy denies), monitor
	// (read-only nodewatch agent), and the dataplane agents (l4lb/popcache). The
	// role/app is carried as the cert app.
	if *demoSeed {
		// Demo/dev convenience: seed a short-lived token for every role so a local CP
		// + agents sharing one --data dir can all enroll without minting.
		for _, role := range []string{"admin", "viewer", "monitor", "l4lb", "popcache"} {
			token, err := caObj.GenerateBootstrapToken(10*time.Minute, role, demo.Domain, time.Hour)
			if err != nil {
				return fmt.Errorf("generate %s token: %w", role, err)
			}
			if err := os.WriteFile(filepath.Join(*dataDir, "bootstrap."+role+".token"), token, 0o600); err != nil {
				return err
			}
		}
		logger.Info("demo: seeded bootstrap tokens", "roles", "admin,viewer,monitor,l4lb,popcache")
	} else {
		// Production: a root-issuer auto-issues the FIRST admin bootstrap token (only
		// while no admin exists) to <data>/bootstrap.admin.token, then stops once an
		// admin enrolls; every other role's token is minted on demand by that admin.
		// Mirrors ksdk's agent/system/root_issuer.
		safe.Go(logger, "root-issuer", func() { runRootIssuer(ctx, caObj, *dataDir, *adminTokenTTL, *adminCertExp, logger) })
	}

	// Dual-stack endpoint: a single objproto endpoint served over BOTH UDP and (when
	// --ws-port > 0) WebSocket. UDP is the dataplane default; the WS leg lets a client
	// reach the CP over an SSH-forwarded TCP port when the UDP port isn't routable.
	var ep objproto.Endpoint
	if *wsPort > 0 {
		mux := http.NewServeMux()
		ds, err := transport.UDPWebsocketDualStackEndpoint(transport.UDPWebsocketDualStackConfig{
			Logger:  logger,
			UDPPort: uint16(*port),
			Mux:     mux,
			WS:      transport.WebSocketConfig{Logger: logger, Path: demo.WebSocketPath, Mode: objproto.EndpointModeServer},
		})
		if err != nil {
			return err
		}
		ep = ds.Endpoint
		wsAddr := fmt.Sprintf(":%d", *wsPort)
		safe.Go(logger, "ws-transport", func() {
			logger.Info("websocket transport serving", "addr", wsAddr, "path", demo.WebSocketPath)
			if err := http.ListenAndServe(wsAddr, mux); err != nil && err != http.ErrServerClosed {
				logger.Error("websocket transport serve failed", "addr", wsAddr, "error", err)
			}
		})
	} else {
		ep, err = transport.UDPEndpoint(logger, uint16(*port), objproto.EndpointModeServer)
		if err != nil {
			return err
		}
	}

	// Evict stale endpoint state (handshakes / inactive connections / proxies) and expired
	// CA material periodically — ksdk's agent/system/cleaner ran these every 10s; without
	// them the endpoint + CA storage grow unboundedly. (DeleteExpiredBootstrapTokens is
	// also run by the root-issuer, but a viewer/monitor-only CP never starts that loop.)
	safe.Go(logger, "endpoint-cleaner", func() { epclean.Run(ctx, ep, logger) })
	safe.Go(logger, "ca-gc", func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				caObj.DeleteExpiredBootstrapTokens(logger)
				if err := caObj.DeleteExpiredCertificates(logger); err != nil {
					logger.Warn("ca-gc: prune expired certificates", "error", err)
				}
			}
		}
	})

	controller := access.NewAccessController(predefined.PolicyMap)
	resources := predefined.ResourceMap()
	auditWriter := io.Writer(io.Discard)
	if *auditTrace {
		// Print the full ABAC evaluation (policy start, each attribute's resolved
		// value, every condition's compare result, the decision) to stderr — the
		// way to see WHY a request was allowed/denied and what an attribute like
		// $env.authority.system.dp.popcache actually resolves to.
		auditWriter = os.Stderr
	}
	auditLog := audit.NewLoggerAudit(auditWriter)

	mgr := rpc.NewRPCManager()
	// gd bundles the singletons every gated adapter needs; the generated
	// service.Register<R>(mgr, gd, inner) wraps each business handler in its
	// <R>Gated authz+audit gate and registers it — so only the resource-specific
	// Inner is hand-written, and no registration can omit/mismatch the gate fields.
	gd := service.GateDeps{Controller: controller, Root: root, Audit: auditLog, Resources: resources}

	// The broker (dataplane inventory) and the stat cache are created up front: the
	// reconcile loops + DataplaneNode/Stats/Logs services share the broker, and the
	// Vip resource reflects its status (applied_on) from the stat cache.
	broker := dpbroker.New()
	agents := dpbroker.NewAgentRegistry()    // client agents (monitor/nodewatch) that serve their own logs
	reconcileStatus := reconcile.NewStatus() // last per-node push result of each declarative resource
	statCache := stats.NewCache()

	// The CP's own prometheus surface (drift gauge, future CP metrics). Not served
	// on a plain port — StatsService.ScrapeMetrics returns it for the "controlplane"
	// target, so the metrics gateway (cmd/metricsgw) pulls it over the authenticated
	// transport alongside the dataplane nodes' metrics.
	driftGauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kscale_resource_drift",
		Help: "Count of items a node reports that are not in the resource's desired set.",
	}, []string{"resource", "node"})
	cpMetrics := prometheus.NewRegistry()
	cpMetrics.MustRegister(driftGauge)

	service.RegisterUserHierarchyAuthority(mgr, gd, &userhierarchyauthority.Handlers{Authorities: caObj.ListCommonNames, Root: root, Domain: demo.Domain})
	service.RegisterCertificate(mgr, gd, &certificate.Handlers{
		Certs:  caObj.ListCertificates,
		Revoke: caObj.RevokeCertificate,
		Prune: func() error {
			caObj.DeleteExpiredBootstrapTokens(logger)
			return caObj.DeleteExpiredCertificates(logger)
		},
		Count: caObj.GetCertificateCount,
	})
	wasmStore := wasmmodule.New()
	// Register the Service wrapper (generated store + hand-written Diff action, which
	// queries each popcache node's actual attached modules); wasmStore is still used
	// directly below for reconcile, desired-state, and ConfirmPush.
	service.RegisterWasmModule(mgr, gd, &wasmmodule.Service{Handlers: wasmStore, Broker: broker, Logger: logger})
	popcacheConfigStore := popcacheconfig.New()
	service.RegisterPopcacheConfig(mgr, gd, popcacheConfigStore)
	routerConfigStore := routerconfig.New()
	service.RegisterRouterConfig(mgr, gd, routerConfigStore)
	dnsConfigStore := dnsconfig.New()
	service.RegisterDnsConfig(mgr, gd, dnsConfigStore)
	mtuStore := mtu.New()
	service.RegisterMtu(mgr, gd, mtuStore)
	openPortStore := openport.New()
	// Register the Service wrapper (generated store + hand-written Diff action, which
	// needs the broker to query routers); the inner store is still used for reconcile,
	// persistence, and ConfirmPush below.
	service.RegisterOpenPort(mgr, gd, &openport.Service{Handlers: openPortStore, Broker: broker, Logger: logger})
	nodeFileStore := nodefile.New()
	service.RegisterNodeFile(mgr, gd, nodeFileStore)
	// container: declarative container workloads reconciled to nodes over CRI. The
	// store is live (admin CRUD) now; the reconcile consumer lands with the workload
	// dp_type agent (reconcile/container.go).
	containerStore := container.New()
	// Register the Service wrapper (generated store + hand-written Diff action, which
	// queries each workload node's actual CRI state); containerStore is still used
	// directly below for reconcile, desired-state, and ConfirmPush.
	service.RegisterContainer(mgr, gd, &container.Service{Handlers: containerStore, Broker: broker, Logger: logger})
	l4lbObjectStore := l4lbobject.New()
	service.RegisterL4LbObject(mgr, gd, l4lbObjectStore)
	service.RegisterBootstrapToken(mgr, gd, &bootstraptoken.Handlers{Generate: caObj.GenerateBootstrapToken, RevokeToken: caObj.RevokeBootstrapToken, Domain: demo.Domain})
	// acme: obtain Let's Encrypt certs via DNS-01 (fanned to the dns dataplane) and
	// distribute to popcache over the transport.
	service.RegisterAcme(mgr, gd, &acme.Handlers{CA: caObj, Broker: broker, Logger: logger, Ctx: ctx})

	// Vip's desired-state store is shared with the reconcile loop (M3); its status
	// (applied_on) is observed from the actual VIP set the dataplane reports.
	vipStore := vip.NewStore()
	vipObserver := reconcile.NewVipObserver(statCache, broker)
	service.RegisterVip(mgr, gd, &vip.Handlers{Store: vipStore, Observer: vipObserver, ConfirmPush: func() error { return reconcileStatus.ResourceError("vip") }})
	secretStore := secret.NewStore()
	service.RegisterSecret(mgr, gd, &secret.Handlers{Store: secretStore, ConfirmPush: func() error { return reconcileStatus.ResourceError("secret") }})
	// interface: the XDP interface l4lb nodes attach to, reconciled via BindInterface.
	ifaceStore := iface.NewStore()
	ifaceObserver := reconcile.NewInterfaceObserver(statCache, broker)
	service.RegisterInterface(mgr, gd, &iface.Handlers{Store: ifaceStore, Observer: ifaceObserver, ConfirmPush: func() error { return reconcileStatus.ResourceError("interface") }})
	// expected_node: the declared node inventory (written by the deploy tooling)
	// plus each node's desired run-state (written by `node start`/`stop`). Feeds
	// dataplane_node's NotConnected fold-in below; the run-state is converged by
	// reconcile.NodeRun (wired after the desired-state load, into nodeRunCtrl).
	expectedNodeStore := expectednode.New()
	service.RegisterExpectedNode(mgr, gd, expectedNodeStore)
	// appStatusOf reads a node's observed lifecycle state (Running/Stopped/
	// Initialized/Error) from the stat cache — Unknown until it has reported.
	// Shared by the northbound app_status column and the node_run reconcile.
	appStatusOf := func(cn string) consts.AppStatus {
		if sb := statCache.Get(cn); sb != nil {
			for _, st := range sb.Stats {
				if st != nil && st.CdnAppRealtime != nil && st.CdnAppRealtime.Appstat != nil {
					return consts.AppStatus(*st.CdnAppRealtime.Appstat)
				}
			}
		}
		return consts.AppStatusUnknown
	}
	// nodeRunCtrl is assigned below (after the desired-state load); the handler
	// closures guard nil so registration order stays a non-issue — nothing calls
	// them until the server starts serving.
	var nodeRunCtrl *reconcile.NodeRunController
	service.RegisterDataplaneNode(mgr, gd, &dataplanenode.Handlers{
		Broker: broker, Logger: logger,
		// surface each node's dataplane running state (Running/Stopped/…) from the stat
		// cache, so `node list/get` shows current state alongside start/stop.
		AppStatus:       func(cn string) string { return appStatusOf(cn).String() },
		ReconcileErrors: reconcileStatus.ErrorsForNode,
		// start/stop as declarative sugar: write desired_run + generation bump,
		// then read back the inline node_run push outcome per node.
		SetDesiredRun:  expectedNodeStore.SetDesiredRun,
		ConfirmRunNode: func(cn string) error { return reconcileStatus.NodeError(reconcile.NodeRunResource, cn) },
		ForceRun: func(cn string) {
			if nodeRunCtrl != nil {
				nodeRunCtrl.ForceNext(cn)
			}
		},
		// Declared inventory: node list shows a declared-but-unconnected CN as a
		// NotConnected row instead of silently omitting it.
		ExpectedNodes: func() map[string]string {
			resp, err := expectedNodeStore.List(ctx, &pbaccess.ResourceExpectedNodeActionListArgsDTO{})
			if err != nil {
				return nil
			}
			m := make(map[string]string, len(resp.Items))
			for _, it := range resp.Items {
				m[it.CommonName] = it.DpType
			}
			return m
		},
	})
	cplaneFiles, err := cplanefile.NewStore(filepath.Join(*dataDir, "cplane_files"))
	if err != nil {
		return err
	}
	// connection: every live transport connection on the endpoint (churn/leak view),
	// CN-labeled via the broker — distinct from dataplane_node's one-per-node view.
	service.RegisterConnection(mgr, gd, &connection.Handlers{Endpoint: ep, Broker: broker})
	// alert: deterministic evaluator over broker / stat cache / reconcile status / CA cert
	// validity. Logs transitions (so they hit the log feed) and exposes the active set.
	alertEval := &alert.Evaluator{Broker: broker, Stat: statCache, Reconcile: reconcileStatus, CA: caObj, Logger: logger}
	safe.Go(logger, "alert-evaluator", func() { alertEval.Run(ctx, 30*time.Second) })
	service.RegisterAlert(mgr, gd, &alertsvc.Handlers{Eval: alertEval})
	service.RegisterCplaneFile(mgr, gd, &cplanefile.Handlers{Store: cplaneFiles})
	service.RegisterDpFile(mgr, gd, &dpfile.Handlers{Broker: broker, CplaneFiles: cplaneFiles, Logger: logger})
	service.RegisterStats(mgr, gd, &stats.Handlers{Cache: statCache, Broker: broker, Logger: logger, SelfGatherer: cpMetrics})
	service.RegisterLogs(mgr, gd, &logs.Handlers{Broker: broker, Logger: logger, ResolveAgent: agents.Resolve, ListAgents: agents.List})
	service.RegisterMonitorChat(mgr, gd, &monitorchat.Handlers{Logger: logger, ResolveAgent: agents.Resolve, ListAgents: agents.List})
	// Stat collector: on each node connect, open the southbound StreamStats and
	// feed the latest batch into the cache that Stats.get/watch read.
	broker.OnConnect(func(dpType string, p *peer.Peer) {
		safe.Go(logger, "stat-collector", func() {
			c := pb.NewDataplaneServiceClient(rpc.NewTrsfStreamSource(p.Streams(), logger))
			stream, err := c.StreamStats(ctx, &wkt.Empty{})
			if err != nil {
				logger.Error("stat collector: StreamStats", "node", p.CommonName(), "error", err)
				return
			}
			connID := p.Connection().ConnectionID().String()
			for {
				batch, err := stream.Recv(ctx)
				if err != nil {
					return
				}
				// Stamp the canonical CP-side transport connection id onto each
				// Stats' ConnectionStat (the dp only sets rtt; it can't know its
				// CP-side id). connection.conn_id then matches the connection
				// resource's connection_id for the same peer.
				for _, st := range batch.Stats {
					if st.Connection == nil {
						st.Connection = &pbstat.ConnectionStat{}
					}
					st.Connection.ConnId = connID
				}
				statCache.Set(p.CommonName(), batch)
			}
		})
	})
	// Drop a disconnected node's cached stats — but only if no live peer with the
	// same CommonName remains (a reconnect supersedes the old peer and is already
	// repopulating the cache; deleting on the old peer's late disconnect would wipe
	// the live data).
	broker.OnDisconnect(func(dpType string, p *peer.Peer) {
		if _, live := broker.Find(p.CommonName()); !live {
			statCache.Delete(p.CommonName())
			reconcileStatus.ClearNode(p.CommonName()) // drop its now-stale reconcile status
		}
	})

	// Desired-state persistence: every declarative store is in-memory, so without this
	// a CP restart drops ALL desired state (blanking `* list`, breaking drift, leaving
	// the fleet un-reconciled until a human re-applies). Persist the combined snapshot
	// to ONE encrypted file — it carries write-only secrets (secret values, router
	// passwords, dns tokens), so it must not be plaintext. Load BEFORE the reconcile
	// loops so a restored VIP/router/etc. is pushed to nodes as they (re)connect.
	desiredKey, err := loadOrCreateCASecret(*dataDir)
	if err != nil {
		return err
	}
	desiredMgr := desiredstate.NewManager(filepath.Join(*dataDir, "desired.dat"), desiredKey, logger)
	desiredMgr.Register("vip", vipStore)
	desiredMgr.Register("secret", secretStore)
	desiredMgr.Register("interface", ifaceStore)
	desiredMgr.Register("mtu", mtuStore)
	desiredMgr.Register("dns_config", dnsConfigStore)
	desiredMgr.Register("popcache_config", popcacheConfigStore)
	desiredMgr.Register("router_config", routerConfigStore)
	desiredMgr.Register("wasm_module", wasmStore)
	desiredMgr.Register("open_port", openPortStore)
	desiredMgr.Register("expected_node", expectedNodeStore)
	desiredMgr.Register("node_file", nodeFileStore)
	desiredMgr.Register("container", containerStore)
	desiredMgr.Register("l4lb_object", l4lbObjectStore)
	if err := desiredMgr.Load(); err != nil {
		return err
	}
	safe.Go(logger, "desired-state-persist", func() { desiredMgr.Run(ctx, 5*time.Second) })

	// M3 reconcile: push desired state to nodes on change and on connect.
	reconcile.Vip(ctx, vipStore, broker, reconcileStatus, logger)
	reconcile.Secret(ctx, secretStore, broker, reconcileStatus, logger)
	reconcile.Interface(ctx, ifaceStore, broker, reconcileStatus, logger)
	// Membership-driven l4lb<->popcache peering (generated from resource.yaml's
	// peering: section): forward pushes popcache backends to l4lb's eBPF dest table,
	// reverse pushes l4lb fronts to popcache for IPIP decap tunnels.
	reconcile.PeeringPopcacheL4Lb(ctx, statCache, broker, logger)
	reconcile.PeeringL4LbPopcache(ctx, statCache, broker, logger)

	// Drift detection: for each resource with an observed status, periodically diff
	// the nodes' actual reported set against desired and warn + gauge on undesired
	// items (kscale_resource_drift{resource,node}). The gauge is exposed on the CP's
	// own /metrics when --metrics-port is set (a foothold for CP-side scraping).
	reconcile.WatchDrift(ctx, "vip", vip.DriftLister{Store: vipStore}, vipObserver, driftGauge, logger, 10*time.Second)
	reconcile.WatchDrift(ctx, "interface", iface.DriftLister{Store: ifaceStore}, ifaceObserver, driftGauge, logger, 10*time.Second)
	// Reconcile desired wasm modules onto popcache nodes (replaces the old
	// imperative dataplane_node.register_wasm op).
	reconcile.WasmModule(ctx, wasmStore, broker, reconcileStatus, logger)
	// Reconcile desired popcache config onto popcache nodes (replaces the old
	// imperative dataplane_node.configure_popcache op).
	reconcile.PopcacheConfig(ctx, popcacheConfigStore, broker, reconcileStatus, logger)
	// Reconcile each router node's desired connection config (hostname/login) onto
	// that node (replaces ksdk's GenericControl set-router-* ops).
	reconcile.RouterConfig(ctx, routerConfigStore, broker, reconcileStatus, logger)
	// Reconcile the DNS upstream-provider config onto dns nodes, and each l4lb node's
	// MTU (replace ksdk's GenericControl set-dns-* and update-mtu ops).
	reconcile.DnsConfig(ctx, dnsConfigStore, broker, reconcileStatus, logger)
	reconcile.Mtu(ctx, mtuStore, broker, reconcileStatus, logger)
	// Reconcile desired router ACLs (open_port) onto router nodes via
	// RouterService.DesireOpenPort — hand-written (reconcile/open_port.go) to parse the
	// "proto:port" strings into PortInfo. This is what makes open-port actually converge
	// the device instead of being a disconnected desired-state store.
	reconcile.OpenPort(ctx, openPortStore, broker, reconcileStatus, logger)
	// Reconcile desired node files: ship each declared cplane_file to its node as save_as
	// (the declarative counterpart of the imperative dp-file send).
	reconcile.NodeFile(ctx, nodeFileStore, broker, cplaneFiles.Read, cplaneFiles.OnUpload, reconcileStatus, logger)
	// Reconcile desired containers onto workload nodes via WorkloadService.ApplyContainers
	// (each node gets its full matching set; the agent converges it to containerd over CRI).
	reconcile.Container(ctx, containerStore, broker, reconcileStatus, logger)
	// Reconcile which eBPF objects each l4lb node loads (DataplaneService.SetBalancerObject).
	reconcile.L4LbObject(ctx, l4lbObjectStore, broker, reconcileStatus, logger)
	// node_run: converge each declared node's desired run-state (`node start`/`stop`
	// intent) — start survives agent/CP restarts, failures back off then hold.
	// Registered LAST so its OnConnect fires after every config plane has pushed:
	// a reconnected node gets its declarative config staged before any start (the
	// Unknown-status guard delays the start to the first stat report anyway, and
	// the reconcile-errors gate holds it if any of those pushes failed).
	nodeRunCtrl = reconcile.NodeRun(ctx, expectedNodeStore, broker, appStatusOf, reconcileStatus, logger, 5*time.Second)

	// Synchronous apply feedback: a store's notify() runs the reconcile push inline, so
	// the apply handler can read back this resource's current per-node push status and
	// return any error — surfacing a failed push at `<resource> apply` instead of a
	// silent OK (the reconcile loop still retries on reconnect / future change).
	wasmStore.ConfirmPush = func() error { return reconcileStatus.ResourceError("wasm_module") }
	popcacheConfigStore.ConfirmPush = func() error { return reconcileStatus.ResourceError("popcache_config") }
	routerConfigStore.ConfirmPush = func() error { return reconcileStatus.ResourceError("router_config") }
	dnsConfigStore.ConfirmPush = func() error { return reconcileStatus.ResourceError("dns_config") }
	mtuStore.ConfirmPush = func() error { return reconcileStatus.ResourceError("mtu") }
	openPortStore.ConfirmPush = func() error { return reconcileStatus.ResourceError("open_port") }
	nodeFileStore.ConfirmPush = func() error { return reconcileStatus.ResourceError("node_file") }
	containerStore.ConfirmPush = func() error { return reconcileStatus.ResourceError("container") }
	l4lbObjectStore.ConfirmPush = func() error { return reconcileStatus.ResourceError("l4lb_object") }

	srv := peer.NewServer(ep, demo.PingInterval, access.NewContextCollector())
	// Dataplane peers (app = a dp type) are registered in the broker for the
	// reconcile loop to drive; admin peers fall through to the normal serving.
	srv.SetOnPeer(func(p *peer.Peer, user access.UserContext) bool {
		if user == nil {
			return false
		}
		app, ok := user.GetAttribute("application")
		if !ok {
			return false
		}
		s, _ := app.Value().(string)
		// Dataplane peers (app = a reconcile dp_type) are registered in the broker for
		// the reconcile loop to drive. The dp_type set is generated from resource.yaml's
		// reconcile.dp_type declarations (predefined.IsDataplaneType) — a new dp_type is
		// recognized automatically without editing this switch.
		if predefined.IsDataplaneType(s) {
			broker.Add(s, p)
			logger.Info("dataplane node joined", "dp_type", s, "common_name", p.CommonName())
			return true
		}
		if s == "monitor" {
			// A client agent (nodewatch) that serves its own logs over the peer — tracked
			// separately from the dataplane broker (so reconcile never targets it) so the
			// control plane can relay `logs stream monitor`. Served as a normal client.
			// Registration is per-connection: the agent's cert renewals ride the same CN,
			// so cleanup removes exactly this connection, never the CN wholesale.
			cn := p.CommonName()
			agents.Add(cn, p)
			safe.Go(logger, "agent-cleanup", func() { <-p.Connection().Done(); agents.Remove(cn, p) })
			logger.Info("monitor agent joined", "common_name", cn)
		}
		return false
	})
	// Every accepted connection runs the CA handshake first. A bootstrap request
	// gets a cert issued (don't serve RPC over it); an authenticated request is
	// resolved to a leaf authority + roles=[app] UserContext for the ABAC gate.
	srv.SetAuthenticate(func(ctx context.Context, conn objproto.Connection) (access.UserContext, bool, error) {
		res, err := caObj.CAHandshake(ctx, logger, conn, "server", ca.EqualAppName, func(cn string) error { return nil })
		if err != nil {
			return nil, false, err
		}
		if res.App == "bootstrap" {
			return nil, false, nil
		}
		authority, err := commonNameToAuthority(res.CommonName, root)
		if err != nil {
			return nil, false, err
		}
		// login_time as a structured time attribute so policies can gate on
		// recency via $user.login_time.since (mirrors ksdk config.TimeAttributeToMap;
		// the remote_shell policy requires login_time.since < 10s). A plain
		// time.Time attribute has no `.since` sub-field, so the policy would never
		// match. Captured per connection auth, so a long-lived admin must reconnect
		// to open a shell.
		loginAt := time.Now()
		user := access.NewUserContext(authority, []access.Attribute{
			access.NewAttribute("application", res.App),
			access.NewAttribute("roles", []string{res.App}),
			access.NewAttributeManager("login_time", []access.Attribute{
				access.NewTypedDynamicAttribute("since", func() time.Duration { return time.Since(loginAt) }),
			}),
		})
		return user, true, nil
	})
	srv.SetOnStream(func(ctx context.Context, logger *slog.Logger, stream trsf.BidirectionalStream) {
		if p := peer.GetPeer(ctx); p != nil {
			// Debug, not Info: this fires on EVERY rpc stream (every katui/cli poll), so at
			// Info it floods the log feed with routine "rpc stream from …" noise.
			logger.Debug("rpc stream from authenticated caller", "common_name", p.CommonName())
		}
		magic, err := rpc.DecodeMagic(ctx, stream)
		if err != nil {
			return
		}
		switch magic {
		case rpc.StreamMagicRPCS:
			mgr.HandleService(ctx, logger, stream)
		case consts.StreamMagicEXEC:
			// Remote shell: ABAC-gate + relay to the target dp (or run on the CP).
			handleExecStream(ctx, logger, gd, broker, stream)
		default:
			stream.CloseBoth()
		}
	})

	fmt.Fprintf(os.Stderr, "serving on udp :%d\n", *port)
	return srv.Serve(ctx, logger)
}
