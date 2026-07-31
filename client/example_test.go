package client

import (
	"strings"
	"testing"
)

// TestApplyEntryYAML_RoundTrips asserts the YAML katui's document builder emits is
// valid yaml-apply: it must pass the same offline ValidateConfig that `cli --apply`
// runs, both per-entry and concatenated into a multi-resource document.
func TestApplyEntryYAML_RoundTrips(t *testing.T) {
	vip := ApplyEntryYAML("vip", map[string]string{"vip": "192.0.2.10"})
	if vip == "" {
		t.Fatal("vip should be declarative (has an apply action)")
	}
	if probs := ValidateConfig([]byte(vip)); len(probs) != 0 {
		t.Fatalf("vip entry should validate, got %v\n%s", probs, vip)
	}

	// router-config: multi-field, with a password that needs quoting (contains ':').
	rc := ApplyEntryYAML("router-config", map[string]string{
		"node": "s3.router.dp", "hostname": "10.0.0.1", "username": "admin", "password": "p:w@ss",
	})
	if probs := ValidateConfig([]byte(rc)); len(probs) != 0 {
		t.Fatalf("router-config entry should validate, got %v\n%s", probs, rc)
	}

	// A document of several concatenated entries is still one valid yaml-apply list.
	doc := vip + rc + ApplyEntryYAML("mtu", map[string]string{"node": "s3", "mtu": "1400"})
	if probs := ValidateConfig([]byte(doc)); len(probs) != 0 {
		t.Fatalf("multi-entry document should validate, got %v\n%s", probs, doc)
	}

	// Numeric / bool fields stay bare (no quotes) so they parse as typed scalars.
	pc := ApplyEntryYAML("popcache-config", map[string]string{"name": "default", "http_port": "8080", "qlog_enabled": "true"})
	if !strings.Contains(pc, "http_port: 8080") || !strings.Contains(pc, "qlog_enabled: true") {
		t.Fatalf("numeric/bool fields should be emitted bare:\n%s", pc)
	}

	// An unknown resource yields no entry.
	if ApplyEntryYAML("does-not-exist", nil) != "" {
		t.Fatal("unknown resource should yield no entry")
	}
}

// TestParseApplyEntries_RoundTrip asserts the TUI's load/edit parse recovers the
// resource + args that ApplyEntryYAML emitted (so a saved doc reloads cleanly).
func TestParseApplyEntries_RoundTrip(t *testing.T) {
	doc := ApplyEntryYAML("vip", map[string]string{"vip": "192.0.2.10"}) +
		ApplyEntryYAML("mtu", map[string]string{"node": "s3", "mtu": "1400"})
	parsed, err := ParseApplyEntries([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("want 2 entries, got %d", len(parsed))
	}
	if parsed[0].Resource != "vip" || parsed[0].Args["vip"] != "192.0.2.10" {
		t.Fatalf("entry 0 = %+v", parsed[0])
	}
	if parsed[1].Resource != "mtu" || parsed[1].Args["node"] != "s3" || parsed[1].Args["mtu"] != "1400" {
		t.Fatalf("entry 1 = %+v", parsed[1])
	}
}

// TestApplyEntryYAML_ListRoundTrips asserts a []string arg survives the doc round-trip:
// it must be emitted as a quoted JSON string (not a bare yaml sequence), so ParseApplyEntries
// recovers the exact JSON the dispatch's json.Unmarshal needs — a bare list would come back
// as "[80 443]" and fail at apply.
func TestApplyEntryYAML_ListRoundTrips(t *testing.T) {
	entry := ApplyEntryYAML("open-port", map[string]string{"acl_name": "web", "ports": `["80","443"]`})
	if entry == "" {
		t.Fatal("open-port should be declarative (has an apply action)")
	}
	if probs := ValidateConfig([]byte(entry)); len(probs) != 0 {
		t.Fatalf("entry should validate, got %v\n%s", probs, entry)
	}
	parsed, err := ParseApplyEntries([]byte(entry))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed) != 1 || parsed[0].Args["ports"] != `["80","443"]` {
		t.Fatalf("ports did not round-trip: %q\n%s", parsed[0].Args["ports"], entry)
	}
	// The round-tripped value must be exactly what the dispatch json.Unmarshals.
	if err := ValidateActionArgs("open-port", "apply", parsed[0].Args); err != nil {
		t.Fatalf("round-tripped args should validate: %v", err)
	}
}

// TestValidateActionArgs covers the on-the-spot value check the TUI runs on submit.
func TestValidateActionArgs(t *testing.T) {
	if err := ValidateActionArgs("open-port", "apply", map[string]string{"acl_name": "web", "ports": `["80"]`}); err != nil {
		t.Fatalf("valid args rejected: %v", err)
	}
	if err := ValidateActionArgs("open-port", "apply", map[string]string{"ports": ""}); err != nil {
		t.Fatalf("blank value should be skipped (unset), not rejected: %v", err)
	}
	if err := ValidateActionArgs("open-port", "apply", map[string]string{"ports": "[80"}); err == nil {
		t.Fatal("malformed []string JSON should be rejected")
	}
	if err := ValidateActionArgs("popcache-config", "apply", map[string]string{"http_port": "abc"}); err == nil {
		t.Fatal("non-numeric uint should be rejected")
	}
}
