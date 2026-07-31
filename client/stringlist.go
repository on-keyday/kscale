package client

import (
	"encoding/json"
	"fmt"
	"strings"
)

// parseStringList parses a []string arg value from the CLI (or any args-map
// caller, e.g. the nodewatch tools). Two forms:
//
//   - JSON array when the value starts with '[': --allowed_hosts '["a","b"]'.
//     Malformed JSON is an error (no comma fallback — a value that looks like
//     JSON but isn't is more likely a typo than a literal element).
//   - Otherwise a comma-separated list: --allowed_hosts a,b. Elements are
//     whitespace-trimmed, empty elements dropped; use the JSON form for
//     elements that themselves contain commas or brackets.
//
// A blank value is an empty list (so an apply can explicitly clear the field).
func parseStringList(v string) ([]string, error) {
	s := strings.TrimSpace(v)
	if s == "" {
		return nil, nil
	}
	if strings.HasPrefix(s, "[") {
		var out []string
		if err := json.Unmarshal([]byte(s), &out); err != nil {
			return nil, fmt.Errorf("bad JSON string list %q: %w", v, err)
		}
		return out, nil
	}
	var out []string
	for _, e := range strings.Split(s, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out, nil
}
