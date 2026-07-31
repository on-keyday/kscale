package nodewatch

import (
	"fmt"
	"strings"
)

// recipe is a curated "when asked X, use these tools" playbook. The rendered
// list is appended to the chat system prompt to steer the local model's tool
// selection toward a good starting sequence instead of it guessing which of the
// read/probe tools to call. `tools` names the tools the approach references and
// is validated against the live tool set by the tests (so a renamed/removed tool
// fails the build rather than silently rotting a recipe).
type recipe struct {
	when     string
	approach string
	tools    []string
}

// monitorRecipes are the interactive-chat playbooks. Keep them few and concrete;
// they complement the per-tool descriptions (resource.yaml `monitor:` hints), not
// replace them.
var monitorRecipes = []recipe{
	{
		when:     "Which servers/nodes are up, is a node connected, fleet inventory",
		approach: "call node_list (connection + run state), then connection_list for link health; add stats_get if load matters.",
		tools:    []string{"node_list", "connection_list", "stats_get"},
	},
	{
		when:     "A node's load, request rate, or cache detail",
		approach: "node_list to find the node's common_name, then stats_get with --common_name.",
		tools:    []string{"node_list", "stats_get"},
	},
	{
		when:     "External reachability / edge health (does it work from outside, 502s)",
		approach: "first read internal expectations — popcache_config_list (configured listeners/ports) and dns_config_list / dns_config_get (public hostnames/records that should resolve); then probe from outside — probe_dns, then probe_http / probe_tls / probe_quic, always with the configured port written explicitly into the URL (omitting :port probes the scheme default 443/80, which is usually wrong here); correlate (internal RUNNING but external failure -> suspect the l4lb front or a DNS mismatch).",
		tools:    []string{"popcache_config_list", "dns_config_list", "dns_config_get", "probe_dns", "probe_http", "probe_tls", "probe_quic"},
	},
	{
		when:     "Config drift (desired vs actual)",
		approach: "open_port_diff for router ACL drift, container_diff for workload container drift, wasm_diff for edge WASM module drift (actual vs declared).",
		tools:    []string{"open_port_diff", "container_diff", "wasm_diff"},
	},
	{
		when:     "What the edge serves / a path returns wasm output unexpectedly (or not at all)",
		approach: "wasm_list to see which WASM modules are declared on which paths/methods, wasm_diff to check they are actually attached on the popcache nodes; then probe_http the path from outside if needed.",
		tools:    []string{"wasm_list", "wasm_diff", "probe_http"},
	},
	{
		when:     "Errors, 'why did X happen', or what happened recently on a node",
		approach: "logs_tail with narrow filters — level=warn (or error) first, then contains/common_name to focus; entries come NEWEST FIRST and older lines beyond the cap are dropped, so refine the filters instead of raising lines.",
		tools:    []string{"logs_tail"},
	},
}

// renderRecipes formats the playbooks as a bullet list for the system prompt.
func renderRecipes() string {
	var b strings.Builder
	b.WriteString("\nCommon questions and how to answer them (a starting point, adapt as needed):")
	for _, r := range monitorRecipes {
		fmt.Fprintf(&b, "\n- %s: %s", r.when, r.approach)
	}
	return b.String()
}
