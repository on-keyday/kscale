package client

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/on-keyday/kscale/protobuf/wire"
)

// DumpDesired renders the LIVE desired state of every declarative resource as a
// yaml-apply document — the same shape `cli --apply` consumes, so the dump round-trips.
// It is sourced from each resource's `list`, so write-only / sensitive fields (secret
// values, router passwords, dns api_tokens) are blanked by construction: the dump never
// exposes secrets. The set mirrors ApplyExample (resources with an `apply` action). A
// resource whose list is denied/empty/unlistable is noted as a comment and skipped.
func DumpDesired(ctx context.Context, src *wire.StreamSource) (string, error) {
	var b strings.Builder
	b.WriteString("# kscale desired-state dump (live). Re-appliable with:  cli --apply <file>\n")
	b.WriteString("# Sensitive fields (secret values, passwords, tokens) are blanked by design.\n")
	for _, r := range ResourceSpecs {
		applySpec, ok := ActionSpecFor(r.Command, "apply")
		if !ok {
			continue // not declarative (no apply action)
		}
		if _, ok := ActionSpecFor(r.Command, "list"); !ok {
			continue // nothing to enumerate
		}
		out, err := DispatchResource(ctx, src, r.Command, "list", map[string]string{})
		if err != nil {
			b.WriteString("\n# " + r.Command + ": list skipped (" + err.Error() + ")\n")
			continue
		}
		var resp struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			b.WriteString("\n# " + r.Command + ": unparseable list (" + err.Error() + ")\n")
			continue
		}
		for _, item := range resp.Items {
			args := map[string]string{}
			for _, a := range applySpec.Args {
				if v, ok := item[a.Name]; ok {
					args[a.Name] = stringifyArg(a, v)
				}
			}
			b.WriteString("\n" + ApplyEntryYAML(r.Command, args))
		}
	}
	return b.String(), nil
}

// stringifyArg renders a json-decoded list item field as the raw string ApplyEntryYAML
// expects: list / structured args as their JSON encoding (so they re-parse at apply),
// scalars as their plain form (no float exponents, no extra quoting).
func stringifyArg(a ArgSpec, v any) string {
	if v == nil {
		return ""
	}
	if !scalarArgType(a.Type) {
		j, _ := json.Marshal(v)
		return string(j)
	}
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		return fmt.Sprint(v)
	}
}

func scalarArgType(t string) bool {
	return t == "string" || t == "bool" || t == "time.Duration" ||
		strings.HasPrefix(t, "uint") || strings.HasPrefix(t, "int")
}
