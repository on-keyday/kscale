package client

import (
	"context"
	"fmt"

	"github.com/goccy/go-yaml"
	"github.com/on-keyday/kscale/protobuf/wire"
)

// ApplyEntry is one declarative resource in a yaml config: `resource` selects the
// resource by command_name, the remaining keys are its apply args (field -> value).
//
//	- resource: vip
//	  vip: 192.0.2.10
//	- resource: secret
//	  value: 0123456789abcdef
type ApplyEntry map[string]any

// ApplyResult is the per-entry outcome of ApplyConfig.
type ApplyResult struct {
	Resource string
	Output   string
	Err      error
}

// ApplyConfig reads a yaml list of declarative resources and applies each via the
// generated DispatchResource(resource, "apply", args) — kubectl-apply style. It is
// driven entirely by the generated ResourceSpecs: each entry must name a resource
// with an `apply` action, and its keys are validated against that action's args.
// No per-resource code — a new declarative resource is appliable for free.
func ApplyConfig(ctx context.Context, src *wire.StreamSource, data []byte) ([]ApplyResult, error) {
	entries, err := parseEntries(data)
	if err != nil {
		return nil, err
	}
	var results []ApplyResult
	for i, e := range entries {
		res, args, err := validateEntry(i, e)
		if err != nil {
			return results, err
		}
		out, err := DispatchResource(ctx, src, res, "apply", args)
		results = append(results, ApplyResult{Resource: res, Output: out, Err: err})
		if err != nil {
			return results, fmt.Errorf("entry %d (%s) apply: %w", i, res, err)
		}
	}
	return results, nil
}

// ValidateConfig checks a yaml-apply config OFFLINE (no connection): the document
// shape plus each entry's resource + arg names against the generated ResourceSpecs.
// Returns one message per problem (empty slice = valid). Same checks ApplyConfig
// runs, so a clean ValidateConfig means apply won't be rejected for shape/args.
func ValidateConfig(data []byte) []string {
	entries, err := parseEntries(data)
	if err != nil {
		return []string{err.Error()}
	}
	var problems []string
	for i, e := range entries {
		if _, _, err := validateEntry(i, e); err != nil {
			problems = append(problems, err.Error())
		}
	}
	return problems
}

func parseEntries(data []byte) ([]ApplyEntry, error) {
	var entries []ApplyEntry
	if err := yaml.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	return entries, nil
}

// validateEntry checks one entry against ResourceSpecs (resource present, has an
// `apply` action, all keys are valid apply args) and returns its (resource, args).
// ParsedEntry is one validated yaml-apply entry: the resource (command_name) and its
// apply args as strings.
type ParsedEntry struct {
	Resource string
	Args     map[string]string
}

// ParseApplyEntries parses a yaml-apply document into validated (resource, args) pairs
// — the same parse + per-entry validation ApplyConfig runs, with no connection. Used
// by the TUI to load an existing file into its document builder, and to re-open an
// entry for editing.
func ParseApplyEntries(data []byte) ([]ParsedEntry, error) {
	entries, err := parseEntries(data)
	if err != nil {
		return nil, err
	}
	out := make([]ParsedEntry, 0, len(entries))
	for i, e := range entries {
		res, args, err := validateEntry(i, e)
		if err != nil {
			return nil, err
		}
		out = append(out, ParsedEntry{Resource: res, Args: args})
	}
	return out, nil
}

func validateEntry(i int, e ApplyEntry) (string, map[string]string, error) {
	res, _ := e["resource"].(string)
	if res == "" {
		return "", nil, fmt.Errorf("entry %d: missing `resource`", i)
	}
	spec, ok := ActionSpecFor(res, "apply")
	if !ok {
		return "", nil, fmt.Errorf("entry %d: resource %q has no `apply` action (not declarative?)", i, res)
	}
	allowed := map[string]bool{}
	for _, a := range spec.Args {
		allowed[a.Name] = true
	}
	args := map[string]string{}
	for k, v := range e {
		if k == "resource" {
			continue
		}
		if !allowed[k] {
			return "", nil, fmt.Errorf("entry %d (%s): unknown arg %q (apply accepts %v)", i, res, k, argNames(spec))
		}
		args[k] = fmt.Sprint(v)
	}
	return res, args, nil
}

func argNames(s ActionSpec) []string {
	out := make([]string, len(s.Args))
	for i, a := range s.Args {
		out[i] = a.Name
	}
	return out
}
