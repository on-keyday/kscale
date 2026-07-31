package nodewatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// DefaultMaxToolResult caps how much of a tool result is fed back to the model, so a
// large JSON dump can't blow the context window mid-loop (the "forgetting" failure).
// Sized for the 8192 default context; raise it together with NumCtx on GPU hosts
// (Config.MaxToolResult / --max-tool-result).
const DefaultMaxToolResult = 4000

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n...[truncated %d bytes]", len(s)-n)
}

// nowStamp is the timestamp format prepended to time-sensitive messages so the model
// can order events and judge freshness (the plain message order isn't enough for a
// local model to tell "already ran" from "not yet"). Local wall-clock on the agent.
func nowStamp() string { return time.Now().Format("2006-01-02 15:04:05") }

// stampToolResult tags a tool/probe result with when it was gathered.
func stampToolResult(result string) string {
	return "[gathered " + nowStamp() + "] " + result
}

const systemPrompt = `You are nodewatch, a monitoring agent for a kscale self-hosted CDN control plane.
Your job: inspect the current dataplane node state using the read-only kscale tools,
judge whether anything is anomalous (disconnected nodes, resource drift, abnormal
stats), and report via the notify tool. Call tools to gather state; when you have
enough, call notify exactly once with your judgement (severity "info" if all healthy).
Be concise and factual. Do not invent data — only report what the tools returned.
Do NOT assume conventional defaults about how this deployment is configured — e.g.
that a standard port (443, 80, ...) is open, that a particular endpoint or listener
exists, or that a service is reachable. What is actually configured comes from the
tools, not from convention. Derive what to expect from the reported configuration and
judge only against that; something conventionally expected but not actually configured
here is NOT an anomaly. If you have not confirmed from the tools that something is
meant to be present, do not treat its absence as a problem.`

const userPrompt = `Check the current kscale node state and notify of any anomalies (or an all-clear).`

// Agent runs one observe -> judge -> notify loop against the model + tool registry.
type Agent struct {
	model    chatModel
	registry *Registry
	maxSteps int
	// maxToolResult caps how many bytes of one tool result reach the model
	// (DefaultMaxToolResult unless overridden via Config.MaxToolResult).
	maxToolResult int
	logger        *slog.Logger
	// modelName / think identify the backing Ollama model, surfaced to the model
	// itself in the system prompt so it knows what it is running as (self-calibration
	// for a small local model). Empty modelName omits the runtime line (e.g. in tests).
	modelName string
	think     bool
}

// NewAgent wires an agent. maxSteps bounds the tool-calling loop per run.
func NewAgent(model chatModel, registry *Registry, maxSteps int, logger *slog.Logger) *Agent {
	return &Agent{model: model, registry: registry, maxSteps: maxSteps, maxToolResult: DefaultMaxToolResult, logger: logger}
}

// NewOllamaAgent builds an Agent backed by a real Ollama client (the exported entry
// point for cmd/nodewatch; chatModel is unexported so callers go through this).
func NewOllamaAgent(cfg Config, registry *Registry, logger *slog.Logger) *Agent {
	a := NewAgent(newOllamaClient(cfg.OllamaHost, cfg.Model, cfg.Think, cfg.RequestTimeout, cfg.NumCtx), registry, cfg.MaxSteps, logger)
	a.modelName = cfg.Model
	a.think = cfg.Think
	if cfg.MaxToolResult > 0 {
		a.maxToolResult = cfg.MaxToolResult
	}
	return a
}

// systemContent appends a runtime-identity line to a base system prompt, telling the
// model which local Ollama model it is running as. Kept factual so the model can
// self-calibrate: a small local model should keep reasoning tight and lean on tools
// rather than prior knowledge. Omitted when modelName is unset (tests).
func (a *Agent) systemContent(base string) string {
	if a.modelName == "" {
		return base
	}
	mode := ""
	if a.think {
		mode = " with thinking enabled"
	}
	return base + fmt.Sprintf("\nRuntime: you are running as the local model %q served via Ollama%s — a resource-constrained local model, not a large hosted one. Keep your reasoning and output tight, and rely on the tools for facts rather than assuming from prior knowledge.", a.modelName, mode)
}

// Run executes one monitoring pass: the model calls read tools, then notify. It
// terminates on the first turn with no tool calls (the model's final answer) or when
// maxSteps is reached. Tool errors are fed back to the model, never fatal.
func (a *Agent) Run(ctx context.Context) error {
	msgs := []chatMessage{
		{Role: "system", Content: a.systemContent(systemPrompt)},
		{Role: "user", Content: userPrompt},
	}
	_, _, _, err := a.runTurn(ctx, msgs, nil)
	return err
}

// emitFunc observes one step event of a turn as it happens. kind is
// "thinking_delta" | "delta" (live generation fragments) | "thinking" |
// "tool_call" | "tool_result" | "assistant" (step boundaries; thinking/assistant
// carry the full text and finalize the fragments that preceded them). text carries
// the fragment / answer / (truncated) tool result, tool/args identify the
// invocation. An emit error aborts the turn (the observer — a chat stream — is
// gone).
type emitFunc func(kind, text, tool, args string) error

// deltaFor builds the streaming callback for a live observer (nil for the observe
// pass, which has no viewer so streaming would be wasted).
func deltaFor(emit emitFunc) deltaFunc {
	if emit == nil {
		return nil
	}
	return func(kind, text string) error {
		if kind == "thinking" {
			return emit("thinking_delta", text, "", "")
		}
		return emit("delta", text, "", "")
	}
}

// noEmit is the observe pass's log-only sink.
func noEmit(kind, text, tool, args string) error { return nil }

// runTurn drives the tool-calling loop over msgs until the model answers without
// tool calls (final != nil), the loop suspends on a client-side tool (pending != nil
// — pending[0] awaits a frontend-run result, pending[1:] are the same message's
// remaining calls), or maxSteps is exhausted (both nil). emit may be nil (observe
// pass). It returns the grown history so callers can persist it.
func (a *Agent) runTurn(ctx context.Context, msgs []chatMessage, emit emitFunc) (updated []chatMessage, final *chatMessage, pending []toolCall, err error) {
	if emit == nil {
		emit = noEmit
	}
	return a.modelLoop(ctx, msgs, emit, deltaFor(emit))
}

// resumeTurn continues a suspended turn: it records the just-approved client-side
// tool's result (or a denial) for pending[0], processes the message's remaining
// calls, then resumes the model loop. Signature mirrors runTurn.
func (a *Agent) resumeTurn(ctx context.Context, msgs []chatMessage, pending []toolCall, result string, emit emitFunc) (updated []chatMessage, final *chatMessage, newPending []toolCall, err error) {
	if emit == nil {
		emit = noEmit
	}
	if len(pending) == 0 {
		return msgs, nil, nil, errors.New("resume with no pending tool call")
	}
	msgs = append(msgs, chatMessage{Role: "tool", ToolName: pending[0].Function.Name, Content: stampToolResult(truncate(result, a.maxToolResult))})
	rest, np, err := a.processTools(ctx, msgs, pending[1:], emit)
	if err != nil {
		return rest, nil, nil, err
	}
	if np != nil {
		return rest, nil, np, nil // another client-side tool in the same message
	}
	return a.modelLoop(ctx, rest, emit, deltaFor(emit))
}

// modelLoop runs model call → process its tool calls → repeat, up to maxSteps.
func (a *Agent) modelLoop(ctx context.Context, msgs []chatMessage, emit emitFunc, onDelta deltaFunc) ([]chatMessage, *chatMessage, []toolCall, error) {
	specs := a.registry.specs()
	for step := 0; step < a.maxSteps; step++ {
		// Announce the model call before it starts: proves the relay chain reached
		// the agent and it is now waiting on the LLM (not stuck / failing to call
		// it) — the gap before the first token is model latency, not a lost request.
		if err := emit("model_call", "", "", ""); err != nil {
			return msgs, nil, nil, err
		}
		resp, err := a.model.chat(ctx, msgs, specs, onDelta)
		if err != nil {
			return msgs, nil, nil, err
		}
		msgs = append(msgs, resp)
		if resp.Thinking != "" {
			a.logger.Debug("nodewatch thinking", "step", step, "len", len(resp.Thinking))
			if err := emit("thinking", truncate(resp.Thinking, a.maxToolResult), "", ""); err != nil {
				return msgs, nil, nil, err
			}
		}
		if len(resp.ToolCalls) == 0 {
			if resp.Content != "" {
				a.logger.Info("nodewatch final", "text", resp.Content)
			}
			if err := emit("assistant", resp.Content, "", ""); err != nil {
				return msgs, nil, nil, err
			}
			return msgs, &msgs[len(msgs)-1], nil, nil
		}
		updated, pending, err := a.processTools(ctx, msgs, resp.ToolCalls, emit)
		if err != nil {
			return updated, nil, nil, err
		}
		msgs = updated
		if pending != nil {
			return msgs, nil, pending, nil // suspend for a frontend-run tool
		}
	}
	a.logger.Warn("nodewatch loop hit maxSteps without a final answer", "maxSteps", a.maxSteps)
	return msgs, nil, nil, nil
}

// processTools runs a message's tool calls in order. Server-side tools execute here
// (result appended); the first client-side tool stops processing and is returned as
// pending[0] (with the rest of the calls as pending[1:]) after emitting tool_request
// so the frontend can run it. pending == nil means every call was handled.
func (a *Agent) processTools(ctx context.Context, msgs []chatMessage, tcs []toolCall, emit emitFunc) (updated []chatMessage, pending []toolCall, err error) {
	for i, tc := range tcs {
		if a.registry.isClientSide(tc.Function.Name) {
			// Hand off to the frontend: announce the request and suspend with this
			// call plus the message's remaining calls still to process.
			if err := emit("tool_request", "", tc.Function.Name, string(tc.Function.Arguments)); err != nil {
				return msgs, nil, err
			}
			return msgs, tcs[i:], nil
		}
		if err := emit("tool_call", "", tc.Function.Name, string(tc.Function.Arguments)); err != nil {
			return msgs, nil, err
		}
		result := truncate(a.registry.exec(ctx, tc), a.maxToolResult)
		a.logger.Info("nodewatch tool call", "tool", tc.Function.Name,
			"args", string(tc.Function.Arguments), "result_len", len(result))
		if err := emit("tool_result", result, tc.Function.Name, ""); err != nil {
			return msgs, nil, err
		}
		msgs = append(msgs, chatMessage{Role: "tool", ToolName: tc.Function.Name, Content: stampToolResult(result)})
	}
	return msgs, nil, nil
}
