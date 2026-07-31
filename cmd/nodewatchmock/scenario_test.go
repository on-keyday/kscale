package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/on-keyday/kscale/nodewatch"
	"github.com/on-keyday/kscale/probe"
)

// TestBundledScenariosLoad: every embedded scenario parses and self-identifies.
func TestBundledScenariosLoad(t *testing.T) {
	names := BundledScenarios()
	if len(names) < 4 {
		t.Fatalf("bundled scenarios = %v, want at least the 4 shipped ones", names)
	}
	for _, n := range names {
		s, err := LoadScenario(n)
		if err != nil {
			t.Fatalf("LoadScenario(%q): %v", n, err)
		}
		if s.Name != n || s.Description == "" {
			t.Fatalf("scenario %q: name=%q description=%q", n, s.Name, s.Description)
		}
	}
}

// TestScenarioToolNamesExist: every tool/probe a scenario answers must exist in the
// live advertised surface (generated read tools + probe kinds), so a renamed or
// removed tool fails this test instead of silently orphaning scenario data.
func TestScenarioToolNamesExist(t *testing.T) {
	valid := map[string]bool{}
	for _, tool := range nodewatch.ReadTools(nil, nil) {
		valid[tool.Name()] = true
	}
	probeNames := map[string]bool{}
	for _, sp := range probe.Specs() {
		probeNames[sp.Name] = true
	}
	for _, n := range BundledScenarios() {
		s, err := LoadScenario(n)
		if err != nil {
			t.Fatal(err)
		}
		for name := range s.Tools {
			if !valid[name] {
				t.Errorf("scenario %q: tools.%s is not a generated read tool", n, name)
			}
		}
		for name := range s.Probes {
			if !probeNames[name] {
				t.Errorf("scenario %q: probes.%s is not a probe kind", n, name)
			}
		}
	}
}

// TestScenarioResultsAreJSON: tool results that look like JSON must be valid JSON
// (they stand in for client.DispatchResource output the model will read).
func TestScenarioResultsAreJSON(t *testing.T) {
	for _, n := range BundledScenarios() {
		s, err := LoadScenario(n)
		if err != nil {
			t.Fatal(err)
		}
		for name, entries := range s.Tools {
			for i, e := range entries {
				r := strings.TrimSpace(e.Result)
				if strings.HasPrefix(r, "{") && !json.Valid([]byte(r)) {
					t.Errorf("scenario %q: tools.%s[%d] result is not valid JSON", n, name, i)
				}
			}
		}
	}
}

// TestScenarioMatching: `when` picks by args substring, first match wins, the
// no-when entry is the default, and an unknown tool gets the neutral fallback.
func TestScenarioMatching(t *testing.T) {
	s := &Scenario{Tools: map[string][]Entry{
		"stats_get": {
			{When: map[string]string{"common_name": "s2"}, Result: "s2 stats"},
			{Result: "all stats"},
		},
	}}
	if got := s.ToolResult("stats_get", json.RawMessage(`{"common_name":"s2.popcache.dp.system.kscale.local"}`)); got != "s2 stats" {
		t.Fatalf("matched = %q, want s2 stats", got)
	}
	if got := s.ToolResult("stats_get", json.RawMessage(`{"common_name":"s1.popcache"}`)); got != "all stats" {
		t.Fatalf("default = %q, want all stats", got)
	}
	if got := s.ToolResult("stats_get", nil); got != "all stats" {
		t.Fatalf("nil args = %q, want all stats", got)
	}
	if got := s.ToolResult("node_list", nil); !strings.Contains(got, "no data available") {
		t.Fatalf("unknown tool = %q, want the neutral fallback", got)
	}
}

// TestMockReadTools: the mock tools keep the production name/schema surface but
// answer from the scenario.
func TestMockReadTools(t *testing.T) {
	s := &Scenario{Tools: map[string][]Entry{
		"node_list": {{Result: `{"items": []}`}},
	}}
	tools := mockReadTools(s, []string{"node_list", "stats_get"})
	if len(tools) != 2 {
		t.Fatalf("allowlist of 2 built %d tools", len(tools))
	}
	byName := map[string]nodewatch.Tool{}
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	nl := byName["node_list"]
	if nl == nil || nl.Description() == "" || nl.Schema() == nil {
		t.Fatalf("node_list surface incomplete: %+v", nl)
	}
	out, err := nl.Exec(context.Background(), nil)
	if err != nil || out != `{"items": []}` {
		t.Fatalf("Exec = %q, %v", out, err)
	}
	if out, _ := byName["stats_get"].Exec(context.Background(), nil); !strings.Contains(out, "no data available") {
		t.Fatalf("unconfigured tool Exec = %q", out)
	}
}
