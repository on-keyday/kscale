package nodewatch

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/on-keyday/kscale/probe"
)

// probeTool advertises one external-probe kind to the model. It is registered
// client-side: the frontend (katui, outside the lab) runs the probe from a real
// external vantage and feeds the result back via resume, so Exec is never called
// here — the tool loop intercepts client-side calls and suspends.
type probeTool struct {
	spec probe.Spec
}

func (p *probeTool) Name() string           { return p.spec.Name }
func (p *probeTool) Description() string    { return p.spec.Description }
func (p *probeTool) Schema() map[string]any { return p.spec.Params }
func (p *probeTool) Exec(context.Context, json.RawMessage) (string, error) {
	return "", errors.New("probe tools run on the frontend, not the agent")
}

// ProbeTools returns a client-side Tool per probe kind, for AddClientSide.
func ProbeTools() []Tool {
	specs := probe.Specs()
	tools := make([]Tool, 0, len(specs))
	for _, s := range specs {
		tools = append(tools, &probeTool{spec: s})
	}
	return tools
}
