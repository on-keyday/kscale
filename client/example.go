package client

import (
	"strconv"
	"strings"
)

// ApplyEntryYAML formats one FILLED declarative resource as a yaml-apply list entry
// (the same shape as ApplyExample), so values collected from a form re-parse cleanly
// via ApplyConfig. String / duration / list values are double-quoted (so ":", leading
// digits, and JSON arrays survive as strings rather than re-parsing as yaml sequences);
// numeric / bool values are emitted bare. Returns "" if resourceCmd has no apply action.
// Empty fields fall back to the typed placeholder.
func ApplyEntryYAML(resourceCmd string, args map[string]string) string {
	spec, ok := ActionSpecFor(resourceCmd, "apply")
	if !ok {
		return ""
	}
	var b strings.Builder
	b.WriteString("- resource: " + resourceCmd + "\n")
	for _, a := range spec.Args {
		b.WriteString("  " + a.Name + ": " + yamlValue(a, args[a.Name]) + "\n")
	}
	return b.String()
}

func yamlValue(a ArgSpec, v string) string {
	if v == "" {
		return yamlPlaceholder(a)
	}
	switch {
	case a.Type == "bool",
		strings.HasPrefix(a.Type, "uint"), strings.HasPrefix(a.Type, "int"):
		// bare scalar: yaml parses it back as the right typed value.
		return v
	default:
		// string, time.Duration, and JSON-bound args ([]string, structured) are emitted
		// as a QUOTED yaml string so they round-trip verbatim. A bare yaml sequence
		// (ports: ["80","443"]) would re-parse to a []any and fmt.Sprint back to
		// "[80 443]", which then fails json.Unmarshal at apply.
		return strconv.Quote(v)
	}
}

// ApplyExample returns a yaml-apply skeleton for every declarative resource (those
// with an `apply` action), generated from the same ResourceSpecs the CLI / TUI /
// yaml-apply are driven by — a copy-paste starting point for `cli --apply`. Each
// entry is `resource: <name>` plus that resource's apply args, each with a
// type-appropriate placeholder and a `# <type>` hint. No per-resource code: a new
// declarative resource appears here for free.
func ApplyExample() string {
	var b strings.Builder
	b.WriteString("# kscale declarative apply config: a YAML list of resources.\n")
	b.WriteString("# Apply with:  cli --apply <file>.yaml   (keep only the entries you need)\n")
	b.WriteString("# Each item is `resource: <name>` plus that resource's apply args.\n")
	for _, r := range ResourceSpecs {
		spec, ok := ActionSpecFor(r.Command, "apply")
		if !ok {
			continue // not declarative (no apply action)
		}
		b.WriteString("\n- resource: " + r.Command + "\n")
		for _, a := range spec.Args {
			b.WriteString("  " + a.Name + ": " + yamlPlaceholder(a) + "  # " + ArgTypeHint(a.Type) + "\n")
		}
	}
	return b.String()
}

// yamlPlaceholder is a type-appropriate empty value for an apply arg (its declared
// default if set, else a typed zero).
func yamlPlaceholder(a ArgSpec) string {
	if a.Default != "" {
		return a.Default
	}
	switch {
	case a.Type == "[]byte":
		return `"/path/to/file"` // its bytes are read + uploaded (see ArgTypeHint)
	case strings.HasPrefix(a.Type, "[]"):
		return "[]"
	case a.Type == "bool":
		return "false"
	case strings.HasPrefix(a.Type, "uint"), strings.HasPrefix(a.Type, "int"):
		return "0"
	default:
		return `""`
	}
}
