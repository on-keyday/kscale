package nodewatch

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
)

// mockModel scripts assistant turns; chat returns the next one and counts calls.
type mockModel struct {
	turns []chatMessage
	i     int
	calls int
}

func (m *mockModel) chat(ctx context.Context, msgs []chatMessage, tools []toolSpec, onDelta deltaFunc) (chatMessage, error) {
	m.calls++
	if m.i >= len(m.turns) {
		return chatMessage{Content: "no more turns"}, nil
	}
	t := m.turns[m.i]
	m.i++
	return t, nil
}

// recordTool records the raw args of each Exec and returns a canned result.
type recordTool struct {
	name string
	mu   sync.Mutex
	got  []string
}

func (r *recordTool) Name() string           { return r.name }
func (r *recordTool) Description() string    { return "test tool" }
func (r *recordTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (r *recordTool) Exec(ctx context.Context, args json.RawMessage) (string, error) {
	r.mu.Lock()
	r.got = append(r.got, string(args))
	r.mu.Unlock()
	return "tool-result", nil
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestAgentLoop: the model calls a tool, observes its result, then finalizes. The loop
// must Exec the tool and terminate on the no-tool-call turn.
func TestAgentLoop(t *testing.T) {
	tool := &recordTool{name: "node_list"}
	reg := NewRegistry()
	reg.Add(tool)
	model := &mockModel{turns: []chatMessage{
		{Role: "assistant", ToolCalls: []toolCall{{Function: toolCallFunc{Name: "node_list", Arguments: json.RawMessage(`{}`)}}}},
		{Role: "assistant", Content: "all healthy"}, // final: no tool calls
	}}
	if err := NewAgent(model, reg, 5, testLogger()).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(tool.got) != 1 {
		t.Fatalf("tool Exec count = %d, want 1", len(tool.got))
	}
	if model.calls != 2 {
		t.Fatalf("model calls = %d, want 2 (tool turn + final)", model.calls)
	}
}

// TestUnknownToolNonFatal: a call to an unregistered tool returns an error string to
// the model (observed) and does not crash the loop.
func TestUnknownToolNonFatal(t *testing.T) {
	reg := NewRegistry()
	model := &mockModel{turns: []chatMessage{
		{Role: "assistant", ToolCalls: []toolCall{{Function: toolCallFunc{Name: "ghost", Arguments: json.RawMessage(`{}`)}}}},
		{Role: "assistant", Content: "done"},
	}}
	if err := NewAgent(model, reg, 5, testLogger()).Run(context.Background()); err != nil {
		t.Fatalf("Run should not error on unknown tool: %v", err)
	}
}

// TestReadToolsFromSpecs: the agent's read tools are derived from ResourceSpecs, with
// hyphenated resource names sanitized for the model.
func TestReadToolsFromSpecs(t *testing.T) {
	names := map[string]bool{}
	for _, x := range ReadTools(nil, nil) {
		names[x.Name()] = true
	}
	for _, want := range []string{"node_list", "interface_list", "popcache_config_list"} {
		if !names[want] {
			t.Errorf("missing read tool %q (have %v)", want, names)
		}
	}
}
