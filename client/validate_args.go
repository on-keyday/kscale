package client

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ValidateActionArgs checks each supplied value against its arg's declared type — the
// same parses the generated dispatch performs (parseStringList for []string, strconv
// for ints, time.ParseDuration for durations) — so a malformed value is rejected BEFORE the
// request is built (e.g. by the TUI the moment you submit a form, instead of failing
// only at apply). Blank values are skipped (treated as unset). Unknown args are ignored
// (arg-name validity is a separate, structural check). Returns the first problem, or nil.
func ValidateActionArgs(resource, action string, args map[string]string) error {
	spec, ok := ActionSpecFor(resource, action)
	if !ok {
		return fmt.Errorf("unknown resource/action %s/%s", resource, action)
	}
	byName := make(map[string]ArgSpec, len(spec.Args))
	for _, a := range spec.Args {
		byName[a.Name] = a
	}
	for name, v := range args {
		a, ok := byName[name]
		if !ok || strings.TrimSpace(v) == "" {
			continue
		}
		if err := validateArgValue(a, v); err != nil {
			return err
		}
	}
	return nil
}

// validateArgValue mirrors the typed binders in the generated dispatch. It only checks
// the types whose parse can fail on free-form input ([]string, integers, durations);
// string / []byte / connection-id / structured args are passed through (string-like, or
// bound from a path/JSON the dispatch handles).
func validateArgValue(a ArgSpec, v string) error {
	switch a.Type {
	case "[]string":
		if _, err := parseStringList(v); err != nil {
			return fmt.Errorf("%s: %v", a.Name, err)
		}
	case "time.Duration":
		if _, err := time.ParseDuration(v); err != nil {
			return fmt.Errorf("%s: invalid duration %q (e.g. 5m, 1h30m)", a.Name, v)
		}
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64":
		if _, err := strconv.ParseInt(v, 10, 64); err != nil {
			return fmt.Errorf("%s: %q is not a whole number", a.Name, v)
		}
	}
	return nil
}
