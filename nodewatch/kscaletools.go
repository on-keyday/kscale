package nodewatch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/on-keyday/kscale/client"
	"github.com/on-keyday/kscale/protobuf/wire"
)

// kscaleTool wraps one kscale resource read-op (e.g. node/list) as an agent Tool,
// dispatching via client.DispatchResource — the same path the CLI uses. The CP's ABAC
// (the agent's role) is what actually authorizes the call, so the agent cannot exceed
// its policy regardless of which tool the model picks.
type kscaleTool struct {
	name     string // sanitized tool name (e.g. popcache_config_list)
	desc     string
	resource string // original resource command_name (e.g. popcache-config)
	action   string
	args     []client.ArgSpec
	src      *wire.StreamSource
}

func (k *kscaleTool) Name() string        { return k.name }
func (k *kscaleTool) Description() string { return k.desc }

func (k *kscaleTool) Schema() map[string]any {
	props := map[string]any{}
	for _, a := range k.args {
		props[a.Name] = map[string]any{"type": jsonType(a.Type), "description": a.Type}
	}
	return map[string]any{"type": "object", "properties": props}
}

func (k *kscaleTool) Exec(ctx context.Context, raw json.RawMessage) (string, error) {
	args := map[string]string{}
	if len(raw) > 0 && string(raw) != "null" {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return "", fmt.Errorf("bad args: %w", err)
		}
		for kk, vv := range m {
			args[kk] = fmt.Sprint(vv)
		}
	}
	return client.DispatchResource(ctx, k.src, k.resource, k.action, args)
}

// readActions are the resource verbs safe to expose to the agent (no mutation, no
// open-ended streaming — watch is excluded as it never returns within a loop step).
// diff is read-only (a dry query of actual-vs-desired drift, e.g. container/open-port),
// so it is safe to surface. tail is the bounded log query (logs tail) — read-only and
// returning, unlike stream.
var readActions = map[string]bool{"list": true, "get": true, "diff": true, "tail": true}

// DefaultMonitorTools is a curated read-tool set for monitoring. Kept deliberately
// small: exposing every read op (~20 tools) degrades local-model tool selection.
// cmd/nodewatch uses it as the default allowlist; --tools overrides for more/less.
var DefaultMonitorTools = []string{
	"node_list",       // which dataplane nodes are connected
	"connection_list", // per-connection health (rtt, etc.)
	"stats_get",       // per-node reported stats
	"vip_list",        // desired vs applied VIPs
	"interface_list",  // desired vs applied per-node interfaces
	"popcache_config_list",
	"open_port_list",
	"open_port_diff",  // actual router ACL vs desired open-port drift — monitor's actual-state view
	"dns_config_list", // desired DNS agent config (zones/records) — for DNS-side troubleshooting
	"dns_config_get",
	"container_list", // declared container workloads per node — so the monitor can see what should be running
	"container_diff", // actual (CRI) vs desired container drift per workload node — the monitor's actual-state view
	"wasm_list",      // declared WASM edge modules (path/method routing) — what the edge should serve
	"wasm_diff",      // actual (attached on popcache) vs desired WASM module drift — the monitor's actual-state view
	"logs_tail",      // newest buffered log lines fleet-wide (filterable) — error/why investigation
}

// ReadTools builds a Tool per read-only resource action from the generated
// ResourceSpecs, so the agent's observation surface tracks the resource model with no
// per-resource code. allow (if non-empty) restricts to those sanitized tool names.
func ReadTools(src *wire.StreamSource, allow []string) []Tool {
	allowSet := map[string]bool{}
	for _, a := range allow {
		allowSet[a] = true
	}
	var tools []Tool
	for _, r := range client.ResourceSpecs {
		for _, act := range r.Actions {
			if !readActions[act.Command] {
				continue
			}
			name := toolName(r.Command + "_" + act.Command) // e.g. node_list
			if len(allowSet) > 0 && !allowSet[name] {
				continue
			}
			// Prefer the resource model's monitor hint (resource.yaml `monitor:` ->
			// ActionSpec.Description) so the tool carries "what it returns / when to
			// use it" guidance; fall back to a generic template when unset.
			desc := act.Description
			if desc == "" {
				desc = fmt.Sprintf("kscale %s %s (read-only). Returns the current state as JSON.", r.Command, act.Command)
			}
			tools = append(tools, &kscaleTool{
				name:     name,
				desc:     desc,
				resource: r.Command,
				action:   act.Command,
				args:     act.Args,
				src:      src,
			})
		}
	}
	return tools
}

// notifyTool is the read-only sink: the agent's judgement/alerts land here.
type notifyTool struct{ logger *slog.Logger }

// NotifyTool builds the notify sink tool.
func NotifyTool(logger *slog.Logger) Tool { return &notifyTool{logger: logger} }

func (n *notifyTool) Name() string { return "notify" }
func (n *notifyTool) Description() string {
	return "Report your monitoring judgement: anomalies found, or an all-clear. Call this once you have gathered enough state. severity is info|warning|critical."
}

func (n *notifyTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"severity": map[string]any{"type": "string", "description": "info | warning | critical"},
			"message":  map[string]any{"type": "string", "description": "the notification text"},
		},
		"required": []string{"message"},
	}
}

func (n *notifyTool) Exec(ctx context.Context, raw json.RawMessage) (string, error) {
	var p struct {
		Severity string `json:"severity"`
		Message  string `json:"message"`
	}
	_ = json.Unmarshal(raw, &p)
	if p.Severity == "" {
		p.Severity = "info"
	}
	n.logger.Info("nodewatch notify", "severity", p.Severity, "message", p.Message)
	fmt.Printf("[nodewatch %s] %s\n", p.Severity, p.Message)
	return "notification sent", nil
}

// toolName sanitizes a resource_action pair into a model-friendly tool name (some
// models reject '-' in function names): popcache-config_list -> popcache_config_list.
func toolName(s string) string { return strings.ReplaceAll(s, "-", "_") }

// jsonType maps a kscale ArgSpec type to a JSON-schema primitive type.
func jsonType(t string) string {
	switch {
	case strings.HasPrefix(t, "uint"), strings.HasPrefix(t, "int"):
		return "integer"
	case t == "bool":
		return "boolean"
	default:
		return "string"
	}
}
