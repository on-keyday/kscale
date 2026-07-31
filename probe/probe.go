// Package probe runs external-vantage diagnostics against a public endpoint —
// HTTP(S), TLS, DNS, QUIC/HTTP3, TCP connect, plus ping/traceroute. It is executed
// on the katui controller (which sits outside the lab over the SSH tunnel), so the
// results reflect what a real external client sees; the monitor LLM proposes a probe
// as a client-side tool and the operator approves it before Run is called.
//
// Specs() advertises the probe kinds to the LLM (name + description + JSON-schema
// params); Run() executes one. The two are split so nodewatch can advertise without
// pulling the execution path into its decision.
package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// Spec is one probe kind advertised to the model as a tool.
type Spec struct {
	Name        string         // tool name, e.g. "probe_http"
	Description string         // one line for the model
	Params      map[string]any // JSON schema (object) for the params
}

// runner executes a probe kind with decoded params, returning human/LLM-readable text.
type runner func(ctx context.Context, args map[string]string) (string, error)

type kind struct {
	spec Spec
	run  runner
}

// registry holds every probe kind by tool name. Adding a kind here makes it both
// advertised (Specs) and runnable (Run) — the single extension point.
var registry = map[string]kind{}

func register(k kind) { registry[k.spec.Name] = k }

// obj builds an object JSON schema from property specs.
func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func inti(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func boolp(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }

// Specs returns every probe kind's advertisement, sorted for determinism.
func Specs() []Spec {
	out := make([]Spec, 0, len(registry))
	for _, k := range registry {
		out = append(out, k.spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// IsProbe reports whether a tool name is a probe kind.
func IsProbe(tool string) bool { _, ok := registry[tool]; return ok }

// Run executes the named probe. rawArgs is the tool-call arguments JSON (an object).
// An unknown kind or a bad-args payload is an error; a probe that runs but finds the
// target unhealthy returns its findings as text (not an error) so the LLM can read them.
func Run(ctx context.Context, tool string, rawArgs json.RawMessage) (string, error) {
	k, ok := registry[tool]
	if !ok {
		return "", fmt.Errorf("unknown probe %q", tool)
	}
	args := map[string]string{}
	if len(rawArgs) > 0 && string(rawArgs) != "null" {
		var m map[string]any
		if err := json.Unmarshal(rawArgs, &m); err != nil {
			return "", fmt.Errorf("bad probe args: %w", err)
		}
		for key, v := range m {
			args[key] = fmt.Sprint(v)
		}
	}
	return k.run(ctx, args)
}
