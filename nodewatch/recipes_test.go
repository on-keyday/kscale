package nodewatch

import (
	"strings"
	"testing"
)

// toolUniverse is every tool name the chat agent can actually advertise: all
// kscale read-tools (built with a nil source — Exec is never called here) plus
// the client-side probe tools.
func toolUniverse() map[string]bool {
	u := map[string]bool{}
	for _, t := range ReadTools(nil, nil) {
		u[t.Name()] = true
	}
	for _, t := range ProbeTools() {
		u[t.Name()] = true
	}
	return u
}

// TestRecipesReferenceRealTools guards against a recipe naming a tool that does
// not exist (e.g. after a resource/tool rename): every recipe.tools entry must be
// a real advertised tool.
func TestRecipesReferenceRealTools(t *testing.T) {
	u := toolUniverse()
	for _, r := range monitorRecipes {
		if len(r.tools) == 0 {
			t.Errorf("recipe %q lists no tools", r.when)
		}
		for _, name := range r.tools {
			if !u[name] {
				t.Errorf("recipe %q references unknown tool %q", r.when, name)
			}
		}
	}
}

// TestRecipesToolsAppearInApproach keeps the machine-checkable tools list in sync
// with the prose the model actually reads: each named tool must be mentioned in
// the approach text.
func TestRecipesToolsAppearInApproach(t *testing.T) {
	for _, r := range monitorRecipes {
		for _, name := range r.tools {
			if !strings.Contains(r.approach, name) {
				t.Errorf("recipe %q lists tool %q but the approach text does not mention it", r.when, name)
			}
		}
	}
}

// TestRenderRecipes renders every recipe's question into the prompt block.
func TestRenderRecipes(t *testing.T) {
	out := renderRecipes()
	for _, r := range monitorRecipes {
		if !strings.Contains(out, r.when) {
			t.Errorf("rendered recipes missing %q", r.when)
		}
	}
}
