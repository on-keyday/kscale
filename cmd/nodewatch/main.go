// Command nodewatch is a kscale-integrated LLM monitoring agent: it enrolls with the
// control plane as an ordinary (ABAC-gated) client, exposes kscale's read-only
// resource ops as tools, and runs a local-LLM (Ollama) native tool-calling loop to
// observe node state and notify on anomalies.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/on-keyday/kscale/agentserve"
	"github.com/on-keyday/kscale/ca"
	"github.com/on-keyday/kscale/client"
	"github.com/on-keyday/kscale/internal/demo"
	"github.com/on-keyday/kscale/internal/sigctx"
	"github.com/on-keyday/kscale/logbuf"
	"github.com/on-keyday/kscale/nodewatch"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	"github.com/on-keyday/kscale/rpc"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/transport"
)

func main() {
	logger := logbuf.NewStderrLogger(os.Stderr, slog.LevelInfo)
	ctx, stop := sigctx.Context()
	defer stop()
	if err := run(ctx, logger, os.Args[1:]); err != nil {
		logger.Error("nodewatch failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("nodewatch", flag.ExitOnError)
	ollamaHost := fs.String("ollama-host", "http://localhost:11434", "Ollama base URL")
	model := fs.String("model", "qwen2.5", "Ollama model (tool-calling capable)")
	maxSteps := fs.Int("max-steps", 8, "max tool-calling steps per monitoring pass")
	think := fs.Bool("think", false, "enable reasoning on thinking-capable models (qwen3, gemma4, …); Ollama rejects it on others")
	reqTimeout := fs.Duration("request-timeout", 5*time.Minute, "per-/api/chat-call IDLE timeout — max gap with no stream progress, not total duration (a healthy CPU generation streams for minutes; only a real stall trips it)")
	numCtx := fs.Int("num-ctx", 8192, "Ollama context window (num_ctx); raise on GPU hosts so long histories / big tool results fit")
	maxToolResult := fs.Int("max-tool-result", nodewatch.DefaultMaxToolResult, "max bytes of one tool result fed to the model; raise together with --num-ctx")
	addr := fs.String("addr", "127.0.0.1:9443", "control plane UDP address")
	dataDir := fs.String("data", "/tmp/kscale-ca", "dir holding bootstrap tokens / saved certs")
	role := fs.String("role", "monitor", "identity role (monitor = read-only via ABAC; the agent's *_monitor.json policies)")
	interval := fs.Duration("interval", 0, "periodic run interval (0 = run once and exit)")
	toolsCSV := fs.String("tools", "", "comma-separated tool allowlist (empty = curated DefaultMonitorTools)")
	domainFlag := fs.String("ca-domain", demo.Domain, "CA domain — must match the control plane's --ca-domain")
	commonNameFlag := fs.String("common-name", "", "cert CommonName to enroll as (default <role>.manager.ca.admin.<ca-domain>)")
	_ = fs.Parse(args)
	demo.Domain = *domainFlag

	cfg := nodewatch.Config{
		OllamaHost: *ollamaHost, Model: *model, MaxSteps: *maxSteps, Think: *think,
		RequestTimeout: *reqTimeout, NumCtx: *numCtx, MaxToolResult: *maxToolResult,
		CPAddr: *addr, DataDir: *dataDir, Role: *role, Interval: *interval,
	}
	if *toolsCSV != "" {
		cfg.AllowedTools = strings.Split(*toolsCSV, ",")
	} else {
		// Curated default keeps the toolset small (better local-model tool selection).
		cfg.AllowedTools = nodewatch.DefaultMonitorTools
	}
	cfg.WithDefaults()

	// Connect to the control plane as an ordinary client (same enroll path as the CLI).
	ap, err := netip.ParseAddrPort(cfg.CPAddr)
	if err != nil {
		return err
	}
	ep, err := transport.UDPEndpoint(logger, 0, objproto.EndpointModeClient)
	if err != nil {
		return err
	}
	// Random connection id (not a fixed id=1): a fixed id wedges on restart — the CP keeps
	// the prior connection for that id until keepalive expires, so the next start fails with
	// "connection already exists for handshake".
	cid, err := objproto.NewRandomConnectionID("udp", ap)
	if err != nil {
		return err
	}
	// Enroll ONCE — the bootstrap token is single-use. Reconnects reuse the saved
	// cert, kept fresh by the renewal loop through holder.
	boot, cn, savePath, err := enroll(ctx, ep, cid, cfg.DataDir, cfg.Role, *commonNameFlag)
	if err != nil {
		return err
	}
	holder := client.NewBootHolder(boot)

	// Cert auto-renewal: nodewatch is a standalone client (not on the dataplane
	// substrate), so renew its cert before it expires — otherwise the saved cert goes
	// stale and, the bootstrap token being single-use, even a restart can't re-enroll.
	go client.RenewCertLoop(ctx, ep, cid, cn, cfg.Role, savePath, client.DefaultCertRenewInterval, holder, logger)

	// Tools: every read-only resource op (ABAC gates what the role may actually call)
	// plus the notify sink. The kscale read tools are (re)pointed at the live
	// connection by maintainConnection below; notify is connection-free.
	registry := nodewatch.NewRegistry()
	registry.Add(nodewatch.NotifyTool(logger))

	agent := nodewatch.NewOllamaAgent(cfg, registry, logger)

	// Interactive chat gets its own agent over the same read tools but without the
	// notify sink — a chat answer goes to the admin on the stream, not the alert path.
	// The ChatServer (and its in-memory sessions) survives reconnects; only the tool
	// registry is re-pointed. External probe tools are registered client-side: the
	// model proposes them, the katui frontend runs them from outside the lab (real
	// external vantage) and resumes with the result.
	chatRegistry := nodewatch.NewRegistry()
	for _, t := range nodewatch.ProbeTools() {
		chatRegistry.AddClientSide(t)
	}
	chatServer := nodewatch.NewChatServer(nodewatch.NewOllamaAgent(cfg, chatRegistry, logger))

	// Connection session loop (the resilience the dataplane substrate has and this
	// agent lacked): connect, expose the tools + served RPC surface over the peer,
	// and when the connection dies (control-plane restart), dial again — otherwise
	// nodewatch is orphaned until someone restarts the service (see
	// notes/bugs/bug_2026_07_07_agent_registry_cn_race.md, 関連する未修正の問題).
	firstUp := make(chan struct{})
	var once sync.Once
	go maintainConnection(ctx, ep, cid, cfg, holder, logger,
		[]*nodewatch.Registry{registry, chatRegistry}, chatServer,
		func() { once.Do(func() { close(firstUp) }) })

	// Don't start observing before the first connection: the read tools aren't
	// registered yet, and a toolless pass would just make the model hallucinate.
	select {
	case <-firstUp:
	case <-ctx.Done():
		return ctx.Err()
	}

	if cfg.Interval <= 0 {
		logger.Info("nodewatch: single monitoring pass", "model", cfg.Model, "ollama", cfg.OllamaHost)
		return agent.Run(ctx)
	}
	logger.Info("nodewatch: periodic monitoring", "interval", cfg.Interval, "model", cfg.Model)
	return runPeriodic(ctx, agent, cfg.Interval, logger)
}

// maintainConnection owns the control-plane connection for the process lifetime:
// dial (fresh random connection id per attempt — a reused id wedges on the CP until
// keepalive expiry), re-point every kscale read tool at the new connection, serve
// the agent's RPC surface (logs + monitor chat) over the peer until it drops, then
// dial again. Connect failures retry every 5s (the CP may be mid-restart).
func maintainConnection(ctx context.Context, ep objproto.Endpoint, template objproto.ConnectionID,
	cfg nodewatch.Config, holder *client.BootHolder, logger *slog.Logger,
	toolRegistries []*nodewatch.Registry, chatServer *nodewatch.ChatServer, onUp func()) {
	for {
		cid, err := objproto.NewRandomConnectionID(template.Transport, template.Addr)
		if err != nil {
			logger.Error("nodewatch: connection id", "error", err)
			if !sleepCtx(ctx, 5*time.Second) {
				return
			}
			continue
		}
		p, src, err := client.Connect(ctx, ep, cid, cfg.Role, holder.Get(), demo.PingInterval, logger)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Error("nodewatch: control plane connect failed; retrying in 5s", "error", err)
			if !sleepCtx(ctx, 5*time.Second) {
				return
			}
			continue
		}
		for _, reg := range toolRegistries {
			reg.AddAll(nodewatch.ReadTools(src, cfg.AllowedTools))
		}
		logger.Info("nodewatch: connected to control plane", "cp", cfg.CPAddr)
		onUp()
		// Serve our own logs (relayed via `logs stream monitor`) and the monitor chat
		// service back over the peer; returns when the peer drops or ctx is cancelled.
		agentserve.Serve(ctx, p, logger, func(reg rpc.Registry) {
			pb.RegisterMonitorChatServiceServer(reg, chatServer)
		})
		p.Connection().Close()
		if ctx.Err() != nil {
			return
		}
		logger.Error("nodewatch: control plane connection lost; reconnecting in 5s")
		if !sleepCtx(ctx, 5*time.Second) {
			return
		}
	}
}

// sleepCtx sleeps d, returning false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// runPeriodic runs an immediate pass then one every interval until ctx is cancelled.
func runPeriodic(ctx context.Context, agent *nodewatch.Agent, interval time.Duration, logger *slog.Logger) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := agent.Run(ctx); err != nil {
			logger.Error("nodewatch pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// enroll loads the role's bootstrap token (first run) or reuses a saved cert, mirroring
// cmd/cli so nodewatch authenticates as a normal kscale client. Also returns the resolved
// CN + cert save path so the caller can drive cert auto-renewal (client.RenewCertLoop).
func enroll(ctx context.Context, ep objproto.Endpoint, cid objproto.ConnectionID, dataDir, role, commonName string) (boot *ca.BootstrapInfo, cn, savePath string, err error) {
	savePath = client.ClientCertPath(dataDir, role)
	var token []byte
	if _, statErr := os.Stat(savePath); os.IsNotExist(statErr) {
		token, err = os.ReadFile(filepath.Join(dataDir, "bootstrap."+role+".token"))
		if err != nil {
			return nil, "", "", err
		}
	}
	cn = commonName
	if cn == "" {
		cn = demo.ClientCN(role)
	}
	boot, err = client.Enroll(ctx, ep, cid, cn, token, role, savePath)
	return boot, cn, savePath, err
}
