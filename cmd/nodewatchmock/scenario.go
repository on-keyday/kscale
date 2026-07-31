package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
)

// A Scenario is the mock's entire world: what every kscale read tool and every
// external probe returns. It replaces the live fleet so the agent loop + chat UX can
// be exercised against a big rented-GPU model with no control plane. Results are the
// verbatim strings the real paths would produce (client.DispatchResource output JSON
// for tools, probe.Run text for probes) so the model sees production-shaped data.
type Scenario struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	// Tools / Probes map a tool name (node_list, probe_http, …) to its candidate
	// results. The first entry whose `when` matches the call's args wins; an entry
	// with no `when` is the default. No entry matching → a neutral "no data" text.
	Tools  map[string][]Entry `yaml:"tools"`
	Probes map[string][]Entry `yaml:"probes"`
	// Logs maps a node common name to its buffered log lines, oldest first. Each
	// line starts with an age token ("-38m ", "-2h5m ") — how long before "now" it
	// was emitted; the logs_tail tool renders it as an absolute timestamp at call
	// time so the model's freshness reasoning works against the chat's live clock.
	// A scenario with logs gets the logs_tail tool registered; others don't.
	Logs map[string][]string `yaml:"logs"`
}

// Entry is one candidate result for a tool, optionally guarded by an args matcher.
type Entry struct {
	// When matches when, for every key, the call's args[key] (stringified) contains
	// the given substring. Empty/omitted = always matches (the default entry).
	When   map[string]string `yaml:"when,omitempty"`
	Result string            `yaml:"result"`
}

//go:embed scenarios/*.yaml
var embeddedScenarios embed.FS

// LoadScenario resolves name as a bundled scenario (scenarios/<name>.yaml) first,
// then as a filesystem path, so `--scenario node-down` and `--scenario ./mine.yaml`
// both work.
func LoadScenario(name string) (*Scenario, error) {
	data, err := embeddedScenarios.ReadFile("scenarios/" + name + ".yaml")
	if err != nil {
		data, err = os.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("scenario %q: not a bundled name (%s) and not a readable file: %w",
				name, strings.Join(BundledScenarios(), ", "), err)
		}
	}
	var s Scenario
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("scenario %q: %w", name, err)
	}
	if s.Name == "" {
		s.Name = strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	}
	return &s, nil
}

// BundledScenarios lists the embedded scenario names, sorted.
func BundledScenarios() []string {
	ents, _ := embeddedScenarios.ReadDir("scenarios")
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	sort.Strings(names)
	return names
}

// ToolResult resolves a kscale read tool call against the scenario.
func (s *Scenario) ToolResult(tool string, args json.RawMessage) string {
	return resolve(s.Tools, tool, args)
}

// ProbeResult resolves an external probe run against the scenario (the mock stand-in
// for probe.Run on the frontend).
func (s *Scenario) ProbeResult(tool string, args json.RawMessage) string {
	return resolve(s.Probes, tool, args)
}

// resolve picks the first entry whose matcher accepts args. The fallback text is
// deliberately neutral (not an invented failure): a missing entry is a gap in the
// scenario, and inventing an error would steer the model's judgement.
func resolve(m map[string][]Entry, tool string, args json.RawMessage) string {
	for _, e := range m[tool] {
		if matches(e.When, args) {
			return strings.TrimRight(e.Result, "\n")
		}
	}
	return fmt.Sprintf("no data available for %s with these arguments", tool)
}

// matches reports whether every when[key] is a substring of the call's stringified
// args[key]. A nil/empty matcher always matches.
func matches(when map[string]string, args json.RawMessage) bool {
	if len(when) == 0 {
		return true
	}
	var got map[string]any
	if len(args) > 0 {
		_ = json.Unmarshal(args, &got)
	}
	for k, want := range when {
		v, ok := got[k]
		if !ok || !strings.Contains(fmt.Sprint(v), want) {
			return false
		}
	}
	return true
}
