package main

import (
	"context"
	"encoding/json"

	"github.com/on-keyday/kscale/nodewatch"
)

// scenarioTool wraps a production-built tool, keeping its exact name / description /
// JSON schema (the surface the model sees) while answering from the scenario instead
// of the live control plane. Fidelity of the advertised tool set is the point: the
// rented-GPU evaluation should exercise the same tool-selection problem as the real
// deployment.
type scenarioTool struct {
	nodewatch.Tool
	scen *Scenario
}

func (t *scenarioTool) Exec(ctx context.Context, args json.RawMessage) (string, error) {
	return t.scen.ToolResult(t.Name(), args), nil
}

// mockReadTools builds the kscale read tools from the generated ResourceSpecs (via
// nodewatch.ReadTools — nil StreamSource is safe because Exec is overridden and the
// source is only touched on dispatch) and rebinds their execution to the scenario.
// logs_tail is special: when the scenario carries a log corpus it gets the
// corpus-driven implementation (real filtering, prod-shaped response) instead of a
// static entry lookup.
func mockReadTools(scen *Scenario, allow []string) []nodewatch.Tool {
	real := nodewatch.ReadTools(nil, allow)
	tools := make([]nodewatch.Tool, 0, len(real))
	for _, t := range real {
		if t.Name() == "logs_tail" && len(scen.Logs) > 0 {
			tools = append(tools, &corpusLogsTool{Tool: t, scen: scen})
			continue
		}
		tools = append(tools, &scenarioTool{Tool: t, scen: scen})
	}
	return tools
}
