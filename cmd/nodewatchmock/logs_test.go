package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/on-keyday/kscale/nodewatch"
)

func testLogScenario() *Scenario {
	return &Scenario{Logs: map[string][]string{
		"s1.popcache.dp.system.kscale.local": {
			"-2h level=INFO msg=\"request served\" path=/hello status=200",
			"-15m level=WARN msg=\"tls handshake error\" remote=203.0.113.199",
			"-5m level=INFO msg=\"request served\" path=/ status=200",
		},
		"s2.popcache.dp.system.kscale.local": {
			"-30m level=ERROR msg=\"wasm compute budget exceeded\" module=1",
		},
	}}
}

// corpusTool builds the prod-generated logs_tail rebound to the scenario corpus,
// exactly as mockReadTools wires it.
func corpusTool(t *testing.T, s *Scenario) nodewatch.Tool {
	t.Helper()
	for _, tool := range mockReadTools(s, []string{"logs_tail"}) {
		if tool.Name() == "logs_tail" {
			if _, ok := tool.(*corpusLogsTool); !ok && len(s.Logs) > 0 {
				t.Fatalf("logs_tail is %T, want the corpus-driven implementation", tool)
			}
			return tool
		}
	}
	t.Fatal("generated tool surface has no logs_tail")
	return nil
}

// tailResp decodes the prod-shaped {note, entries} response.
type tailResp struct {
	Note    string   `json:"note"`
	Entries []string `json:"entries"`
}

func tailExec(t *testing.T, s *Scenario, args string) tailResp {
	t.Helper()
	out, err := corpusTool(t, s).Exec(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("logs_tail %s: %v", args, err)
	}
	var r tailResp
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("logs_tail %s: not the prod {note, entries} shape: %v\n%s", args, err, out)
	}
	return r
}

// TestLogsTailFilters: common_name/level/contains narrow the corpus; entries are
// tagged, timestamped, NEWEST FIRST; the note discloses counts and ordering.
func TestLogsTailFilters(t *testing.T) {
	s := testLogScenario()

	all := tailExec(t, s, `{}`)
	if len(all.Entries) != 4 || !strings.Contains(all.Note, "4 matching lines") || !strings.Contains(all.Note, "NEWEST FIRST") {
		t.Fatalf("unfiltered = %+v", all)
	}
	// Newest first: the -5m line leads, the -2h line ends.
	if !strings.Contains(all.Entries[0], "path=/ status=200") || !strings.Contains(all.Entries[3], "path=/hello") {
		t.Fatalf("ordering wrong: %v", all.Entries)
	}
	if !strings.HasPrefix(all.Entries[0], "[s1.popcache] ") {
		t.Fatalf("missing source tag: %q", all.Entries[0])
	}

	// level is a MINIMUM: warn includes the ERROR line.
	warn := tailExec(t, s, `{"level":"warn"}`)
	if len(warn.Entries) != 2 ||
		!strings.Contains(warn.Entries[0], "tls handshake error") || !strings.Contains(warn.Entries[1], "budget exceeded") {
		t.Fatalf("min-level filter: %+v", warn)
	}
	s2 := tailExec(t, s, `{"common_name":"s2.popcache"}`)
	if len(s2.Entries) != 1 || !strings.HasPrefix(s2.Entries[0], "[s2.popcache]") {
		t.Fatalf("common_name filter: %+v", s2)
	}
	if needle := tailExec(t, s, `{"contains":"203.0.113.199"}`); len(needle.Entries) != 1 {
		t.Fatalf("contains filter: %+v", needle)
	}
	if none := tailExec(t, s, `{"contains":"no-such-thing"}`); len(none.Entries) != 0 || !strings.Contains(none.Note, "0 matching") {
		t.Fatalf("no-match: %+v", none)
	}
}

// TestLogsTailLimit: lines keeps the NEWEST N (newest-first order preserved), so a
// downstream head-keeping truncation can only ever drop the oldest lines.
func TestLogsTailLimit(t *testing.T) {
	out := tailExec(t, testLogScenario(), `{"lines":2}`)
	if len(out.Entries) != 2 ||
		!strings.Contains(out.Entries[0], "path=/ status=200") || !strings.Contains(out.Entries[1], "tls handshake error") {
		t.Fatalf("kept the wrong end: %+v", out)
	}
	if !strings.Contains(out.Note, "capped at 2") {
		t.Fatalf("note must disclose the cap: %q", out.Note)
	}
}

// TestLogsTailBadAgeToken: a malformed scenario line is a loud error (a broken
// corpus must fail the run, not silently vanish).
func TestLogsTailBadAgeToken(t *testing.T) {
	s := &Scenario{Logs: map[string][]string{"n": {"level=INFO msg=x"}}}
	if _, err := corpusTool(t, s).Exec(context.Background(), nil); err == nil {
		t.Fatal("want an error for a line without an age token")
	}
}

// TestLogsTailWithoutCorpus: a scenario with no logs section keeps the plain
// scenario-entry tool (prod surface intact, answers from tools:/fallback).
func TestLogsTailWithoutCorpus(t *testing.T) {
	s := &Scenario{}
	tool := corpusTool(t, s)
	if _, ok := tool.(*corpusLogsTool); ok {
		t.Fatal("corpus tool wired despite an empty logs section")
	}
	out, err := tool.Exec(context.Background(), nil)
	if err != nil || !strings.Contains(out, "no data available") {
		t.Fatalf("fallback = %q, %v", out, err)
	}
}

// TestLogNoiseScenario: the bundled corpus loads; a min-level warn tail surfaces
// both needles; and even a naive default tail keeps the recent TLS burst (newest
// end always survives).
func TestLogNoiseScenario(t *testing.T) {
	s, err := LoadScenario("log-noise")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Logs) == 0 {
		t.Fatal("log-noise has no logs section")
	}
	joined := func(r tailResp) string { return strings.Join(r.Entries, "\n") }
	warn := joined(tailExec(t, s, `{"level":"warn"}`))
	if !strings.Contains(warn, "tls handshake error") || !strings.Contains(warn, "wasm compute budget exceeded") {
		t.Fatalf("needles not found under warn:\n%s", warn)
	}
	if naive := joined(tailExec(t, s, `{}`)); !strings.Contains(naive, "tls handshake error") {
		t.Fatalf("recent TLS burst missing from a naive default tail:\n%s", naive)
	}
}
