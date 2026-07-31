package nodewatch

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Tool is one capability the agent can invoke — the extension point. Exec returns an
// LLM/human-readable result; an error is returned to the model (observed, not fatal),
// so a future action tool can add its own approval gate inside Exec.
type Tool interface {
	Name() string
	Description() string
	Schema() map[string]any // JSON schema for parameters
	Exec(ctx context.Context, args json.RawMessage) (string, error)
}

// Registry holds the agent's tools and advertises them to the model. It is
// mutation-safe under concurrent use: the connection-session loop re-registers the
// kscale read tools on every reconnect while observe/chat turns keep reading.
type Registry struct {
	mu         sync.RWMutex
	tools      map[string]Tool
	clientSide map[string]bool // advertised but NOT executed here — the frontend runs them
	timeout    time.Duration
}

// NewRegistry builds an empty registry; per-tool Exec is bounded by timeout.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}, clientSide: map[string]bool{}, timeout: 30 * time.Second}
}

// Add registers a tool (last registration of a name wins).
func (r *Registry) Add(t Tool) {
	r.mu.Lock()
	r.tools[t.Name()] = t
	r.mu.Unlock()
}

// AddClientSide advertises a tool the model may call but that this agent must NOT
// execute — the chat frontend runs it (an external probe from its own vantage) and
// feeds the result back via resume. Exec is never invoked for these; the tool loop
// suspends on them instead.
func (r *Registry) AddClientSide(t Tool) {
	r.mu.Lock()
	r.tools[t.Name()] = t
	r.clientSide[t.Name()] = true
	r.mu.Unlock()
}

// isClientSide reports whether a tool name must be run by the frontend.
func (r *Registry) isClientSide(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.clientSide[name]
}

// AddAll registers several tools.
func (r *Registry) AddAll(ts []Tool) {
	for _, t := range ts {
		r.Add(t)
	}
}

// specs returns the tool specs advertised to the model, sorted for determinism.
func (r *Registry) specs() []toolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]toolSpec, 0, len(names))
	for _, n := range names {
		t := r.tools[n]
		out = append(out, toolSpec{Type: "function", Function: functionSpec{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Schema(),
		}})
	}
	return out
}

// exec runs the named tool with a timeout + panic recovery. An unknown tool or an
// Exec error/panic is returned as a string for the model to observe — never fatal.
func (r *Registry) exec(ctx context.Context, tc toolCall) string {
	r.mu.RLock()
	t, ok := r.tools[tc.Function.Name]
	r.mu.RUnlock()
	if !ok {
		return fmt.Sprintf("error: unknown tool %q", tc.Function.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return safeExec(ctx, t, tc.Function.Arguments)
}

func safeExec(ctx context.Context, t Tool, args json.RawMessage) (result string) {
	defer func() {
		if rec := recover(); rec != nil {
			result = fmt.Sprintf("error: tool %q panicked: %v", t.Name(), rec)
		}
	}()
	out, err := t.Exec(ctx, args)
	if err != nil {
		return "error: " + err.Error()
	}
	return out
}
