package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/on-keyday/kscale/logbuf"
	"github.com/on-keyday/kscale/nodewatch"
)

// corpusLogsTool answers the PRODUCTION logs_tail tool (generated from the logs
// resource's tail action — same name/description/schema the real deployment
// advertises) from the scenario's log corpus. Filtering runs in real code so the
// model's arguments actually change what it gets back, and the response mirrors the
// CP handler byte-for-byte in shape: a {note, entries} JSON with entries NEWEST
// FIRST — so the loop's head-keeping truncation drops the oldest lines, never the
// newest (the gemma4:31b evaluation caught the oldest-first variant silently eating
// a recent error burst).
type corpusLogsTool struct {
	nodewatch.Tool // the generated logs_tail: name / description / schema
	scen           *Scenario
}

const (
	logsTailDefault = 50
	logsTailMax     = 200 // mirrors the per-source logbuf ring
)

// logLine is one parsed scenario line: how long ago it was emitted plus its body.
type logLine struct {
	age   time.Duration
	label string // "[s1.popcache]"-style source tag
	body  string
}

func (t *corpusLogsTool) Exec(ctx context.Context, raw json.RawMessage) (string, error) {
	var p struct {
		CommonName string `json:"common_name"`
		Level      string `json:"level"`
		Contains   string `json:"contains"`
		Lines      int    `json:"lines"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &p); err != nil {
			return "", fmt.Errorf("bad args: %w", err)
		}
	}
	lines := p.Lines
	if lines <= 0 {
		lines = logsTailDefault
	}
	if lines > logsTailMax {
		lines = logsTailMax
	}
	all, err := t.collect(p.CommonName, p.Level, p.Contains)
	if err != nil {
		return "", err
	}

	// Newest first, capped — the prod CP handler's ordering and cap.
	sort.SliceStable(all, func(i, j int) bool { return all[i].age < all[j].age })
	if len(all) > lines {
		all = all[:lines]
	}
	now := time.Now()
	entries := make([]string, 0, len(all))
	for _, l := range all {
		entries = append(entries, fmt.Sprintf("[%s] %s %s", l.label, now.Add(-l.age).Format(time.RFC3339), l.body))
	}
	note := fmt.Sprintf("%d matching lines from %d sources, NEWEST FIRST (capped at %d; each source retains only its ~%d most recent lines — narrow with level/contains/common_name rather than raising lines)",
		len(entries), len(t.scen.Logs), lines, logsTailMax)
	out, err := json.MarshalIndent(struct {
		Note    string   `json:"note"`
		Entries []string `json:"entries,omitempty"`
	}{Note: note, Entries: entries}, "", "  ")
	return string(out), err
}

// collect parses and filters the scenario corpus. level is a MINIMUM level (the
// prod semantics): "warn" includes WARN and ERROR.
func (t *corpusLogsTool) collect(node, level, contains string) ([]logLine, error) {
	hasLevel := level != ""
	var min slog.Level
	if hasLevel {
		var err error
		if min, err = logbuf.ParseLevel(level); err != nil {
			return nil, err
		}
	}
	var out []logLine
	cns := make([]string, 0, len(t.scen.Logs))
	for cn := range t.scen.Logs {
		cns = append(cns, cn)
	}
	sort.Strings(cns)
	for _, cn := range cns {
		if node != "" && node != "*" && !strings.Contains(cn, node) {
			continue
		}
		label := nodeTag(cn)
		for _, raw := range t.scen.Logs[cn] {
			ageTok, body, ok := strings.Cut(raw, " ")
			if !ok || !strings.HasPrefix(ageTok, "-") {
				return nil, fmt.Errorf("scenario log line for %s must start with an age token like \"-38m \": %q", cn, raw)
			}
			age, err := time.ParseDuration(ageTok[1:])
			if err != nil {
				return nil, fmt.Errorf("scenario log line for %s: bad age %q: %w", cn, ageTok, err)
			}
			if hasLevel {
				lvl, err := lineLevel(body)
				if err != nil || lvl < min {
					continue
				}
			}
			if contains != "" && !strings.Contains(body, contains) {
				continue
			}
			out = append(out, logLine{age: age, label: label, body: body})
		}
	}
	return out, nil
}

// lineLevel extracts the "level=X" token of a corpus line.
func lineLevel(body string) (slog.Level, error) {
	for _, f := range strings.Fields(body) {
		if v, ok := strings.CutPrefix(f, "level="); ok {
			return logbuf.ParseLevel(v)
		}
	}
	return 0, fmt.Errorf("no level token")
}

// nodeTag shortens a CN to its first two labels ("s1.popcache.dp.system…" ->
// "s1.popcache"), the same node label style the CP's tail fan-in uses.
func nodeTag(cn string) string {
	parts := strings.SplitN(cn, ".", 3)
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return cn
}
