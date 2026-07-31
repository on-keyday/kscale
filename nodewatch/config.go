// Package nodewatch is a kscale-integrated LLM monitoring agent: it connects to the
// control plane as an ordinary (ABAC-gated) client, exposes kscale's read-only
// resource ops as tools, and runs a local-LLM (Ollama) native tool-calling loop to
// observe node state and notify on anomalies. See notes/ai/2026_06_28_nodewatch_design.md.
package nodewatch

import "time"

// Config drives the agent; cmd/nodewatch loads it from flags/YAML.
type Config struct {
	// Ollama (config-driven; the agent runs where Ollama lives, not where it is built).
	OllamaHost string // e.g. http://localhost:11434
	Model      string // tool-calling capable model, e.g. qwen2.5
	MaxSteps   int    // loop step cap per run
	// Think asks a thinking-capable model (qwen3, gemma4, …) to reason before each
	// answer; the reasoning arrives separately (message.thinking) and is surfaced as
	// "thinking" chat events. Ollama rejects think=true on non-thinking models, so
	// this stays opt-in rather than defaulting on.
	Think bool
	// RequestTimeout bounds the IDLE time within one /api/chat call — the longest gap
	// with no progress (no new stream chunk / no response), not the total call duration.
	// A healthy CPU-only generation streams for minutes and must NOT be capped on total
	// time (that killed calls mid-stream); this only fires when Ollama actually stalls.
	RequestTimeout time.Duration
	// NumCtx is the Ollama context window (num_ctx). The 8192 default is sized for a
	// small CPU-bound model: big enough that accumulating tool results don't overflow
	// mid-loop, small enough to fit its RAM. On a GPU box raise it (32768+) so long
	// chat histories and log-heavy tool results actually fit.
	NumCtx int
	// MaxToolResult caps how many bytes of one tool result reach the model (0 =
	// DefaultMaxToolResult). The default suits NumCtx 8192; raise the two together —
	// with a big context this cap is what limits how much of a log/state dump the
	// model can actually see per call.
	MaxToolResult int

	// kscale control-plane connection (the agent enrolls like the CLI/TUI).
	CPAddr  string // control plane UDP addr
	DataDir string // bootstrap tokens / saved certs
	Role    string // identity role; "viewer" keeps the agent read-only via ABAC

	// Scheduling.
	Interval time.Duration // periodic run interval (0 = run once)

	// AllowedTools, if non-empty, restricts the agent to these tool names (in
	// addition to ABAC). Empty = every read tool the resource model exposes.
	AllowedTools []string
}

// WithDefaults fills unset fields with sensible values.
func (c *Config) WithDefaults() {
	if c.OllamaHost == "" {
		c.OllamaHost = "http://localhost:11434"
	}
	if c.Model == "" {
		c.Model = "qwen2.5"
	}
	if c.MaxSteps == 0 {
		c.MaxSteps = 8
	}
	if c.CPAddr == "" {
		c.CPAddr = "127.0.0.1:9443"
	}
	if c.Role == "" {
		c.Role = "monitor"
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = 5 * time.Minute
	}
	if c.NumCtx == 0 {
		c.NumCtx = 8192
	}
	if c.MaxToolResult == 0 {
		c.MaxToolResult = DefaultMaxToolResult
	}
}
