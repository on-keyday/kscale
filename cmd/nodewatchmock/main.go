// Command nodewatchmock is a standalone evaluation harness for the nodewatch LLM
// agent: the same Ollama tool-calling loop, chat sessions, and katui chat UX as the
// production deployment, but with the kscale control plane replaced by a scenario
// file (canned tool/probe results). It exists to evaluate bigger models on a rented
// GPU box — copy the single binary over, point it at the local Ollama, and drive the
// agent against bundled fleet scenarios (healthy / node-down / config-drift /
// edge-down) without a lab. See notes/ai/2026_07_11_nodewatch_mock.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/kscale/internal/sigctx"
	"github.com/on-keyday/kscale/logbuf"
	"github.com/on-keyday/kscale/nodewatch"
)

func main() {
	ctx, stop := sigctx.Context()
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "nodewatchmock:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("nodewatchmock", flag.ExitOnError)
	ollamaHost := fs.String("ollama-host", "http://localhost:11434", "Ollama base URL")
	modelName := fs.String("model", "qwen2.5", "Ollama model (tool-calling capable)")
	maxSteps := fs.Int("max-steps", 8, "max tool-calling steps per turn / monitoring pass")
	think := fs.Bool("think", false, "enable reasoning on thinking-capable models; Ollama rejects it on others")
	reqTimeout := fs.Duration("request-timeout", 5*time.Minute, "per-/api/chat-call IDLE timeout (max gap with no stream progress, not total duration)")
	numCtx := fs.Int("num-ctx", 8192, "Ollama context window (num_ctx); raise on GPU hosts so long histories / big tool results fit")
	maxToolResult := fs.Int("max-tool-result", nodewatch.DefaultMaxToolResult, "max bytes of one tool result fed to the model; raise together with --num-ctx")
	scenarioName := fs.String("scenario", "healthy", "bundled scenario name or a YAML file path")
	listScenarios := fs.Bool("list-scenarios", false, "list the bundled scenarios and exit")
	observe := fs.Bool("observe", false, "run one autonomous monitoring pass (observe → judge → notify) instead of the chat TUI")
	toolsCSV := fs.String("tools", "", "comma-separated read-tool allowlist (empty = curated DefaultMonitorTools)")
	logPath := fs.String("log", "", "write agent debug logs to this file (chat TUI mode; default discards, --observe logs to stderr)")
	_ = fs.Parse(args)

	if *listScenarios {
		for _, n := range BundledScenarios() {
			s, err := LoadScenario(n)
			if err != nil {
				return err
			}
			fmt.Printf("%-14s %s\n", n, s.Description)
		}
		return nil
	}

	scen, err := LoadScenario(*scenarioName)
	if err != nil {
		return err
	}

	cfg := nodewatch.Config{
		OllamaHost: *ollamaHost, Model: *modelName, MaxSteps: *maxSteps,
		Think: *think, RequestTimeout: *reqTimeout, NumCtx: *numCtx, MaxToolResult: *maxToolResult,
	}
	cfg.WithDefaults()
	allow := nodewatch.DefaultMonitorTools
	if *toolsCSV != "" {
		allow = strings.Split(*toolsCSV, ",")
	}

	if *observe {
		// Autonomous pass, mirroring cmd/nodewatch's observe registry: the scenario's
		// read tools + the notify sink (which prints the judgement to stdout).
		logger := logbuf.NewStderrLogger(os.Stderr, slog.LevelInfo)
		registry := nodewatch.NewRegistry()
		registry.AddAll(mockReadTools(scen, allow))
		registry.Add(nodewatch.NotifyTool(logger))
		agent := nodewatch.NewOllamaAgent(cfg, registry, logger)
		logger.Info("nodewatchmock: observe pass", "scenario", scen.Name, "model", cfg.Model, "ollama", cfg.OllamaHost)
		return agent.Run(ctx)
	}

	// Chat TUI. The logger must not write to the terminal (it would corrupt the TUI);
	// tool activity is visible as chat events anyway.
	logSink := io.Discard
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		logSink = f
	}
	logger := slog.New(slog.NewTextHandler(logSink, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Mirror cmd/nodewatch's chat registry: scenario read tools + client-side probe
	// tools (the TUI "runs" an approved probe by answering it from the scenario).
	chatRegistry := nodewatch.NewRegistry()
	chatRegistry.AddAll(mockReadTools(scen, allow))
	for _, t := range nodewatch.ProbeTools() {
		chatRegistry.AddClientSide(t)
	}
	chatServer := nodewatch.NewChatServer(nodewatch.NewOllamaAgent(cfg, chatRegistry, logger))

	head := fmt.Sprintf("scenario %s · %s", scen.Name, cfg.Model)
	p := tea.NewProgram(newModel(ctx, chatServer, scen, head), tea.WithAltScreen(), tea.WithContext(ctx))
	_, err = p.Run()
	return err
}
