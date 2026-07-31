package nodewatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/kscale/protobuf/wire"
)

// captureModel scripts assistant turns like mockModel but also records the message
// history it was called with, so tests can assert multi-turn session growth.
type captureModel struct {
	turns []chatMessage
	errAt int // 1-based call index that fails (0 = never)
	calls int
	seen  [][]chatMessage
}

func (m *captureModel) chat(ctx context.Context, msgs []chatMessage, tools []toolSpec, onDelta deltaFunc) (chatMessage, error) {
	m.calls++
	cp := make([]chatMessage, len(msgs))
	copy(cp, msgs)
	m.seen = append(m.seen, cp)
	if m.errAt != 0 && m.calls == m.errAt {
		return chatMessage{}, errors.New("ollama down")
	}
	if len(m.turns) == 0 {
		return chatMessage{Content: "no more turns"}, nil
	}
	t := m.turns[0]
	m.turns = m.turns[1:]
	// Behave like the streaming client: forward the turn's thinking/content as
	// fragments before returning the assembled message.
	if onDelta != nil {
		if t.Thinking != "" {
			if err := onDelta("thinking", t.Thinking); err != nil {
				return chatMessage{}, err
			}
		}
		if t.Content != "" {
			if err := onDelta("content", t.Content); err != nil {
				return chatMessage{}, err
			}
		}
	}
	return t, nil
}

// chatStream returns a real ServerSendStream whose Send decodes back into evs.
func chatStream(evs *[]pb.ChatEvent) *pb.MonitorChatServiceSendServerStream {
	return wire.NewServerSendStream[pb.ChatEvent](func(body []byte) error {
		var ev pb.ChatEvent
		if err := ev.Decode(body); err != nil {
			return err
		}
		*evs = append(*evs, ev)
		return nil
	})
}

func send(t *testing.T, s *ChatServer, session, msg string) []pb.ChatEvent {
	t.Helper()
	var evs []pb.ChatEvent
	if err := s.Send(context.Background(), &pbaccess.ResourceMonitorChatActionSendArgsDTO{
		SessionId: session, Message: msg,
	}, chatStream(&evs)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	return evs
}

// TestChatTurnEvents: one turn with a tool call streams tool_call, tool_result,
// then the terminal assistant event carrying the final answer.
func TestChatTurnEvents(t *testing.T) {
	reg := NewRegistry()
	reg.Add(&recordTool{name: "node_list"})
	model := &captureModel{turns: []chatMessage{
		{Role: "assistant", ToolCalls: []toolCall{{Function: toolCallFunc{Name: "node_list", Arguments: json.RawMessage(`{}`)}}}},
		{Role: "assistant", Content: "3 nodes, all healthy"},
	}}
	s := NewChatServer(NewAgent(model, reg, 5, testLogger()))

	evs := send(t, s, "s1", "how do the nodes look?")
	kinds := make([]string, 0, len(evs))
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	// Each model call is announced (model_call) before it runs; the answer then
	// streams as delta fragments and the terminal assistant event carries the full
	// text (finalizing what the fragments built).
	want := []string{"model_call", "tool_call", "tool_result", "model_call", "delta", "assistant"}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("event kinds = %v, want %v", kinds, want)
	}
	if evs[1].Tool != "node_list" || evs[4].Text != "3 nodes, all healthy" || evs[5].Text != "3 nodes, all healthy" {
		t.Fatalf("unexpected events: %+v", evs)
	}
}

// TestChatThinkingEvents: a thinking model's reasoning is emitted as a "thinking"
// event before the step it precedes (tool call and final answer alike).
func TestChatThinkingEvents(t *testing.T) {
	reg := NewRegistry()
	reg.Add(&recordTool{name: "node_list"})
	model := &captureModel{turns: []chatMessage{
		{Role: "assistant", Thinking: "I should inspect the fleet first.",
			ToolCalls: []toolCall{{Function: toolCallFunc{Name: "node_list", Arguments: json.RawMessage(`{}`)}}}},
		{Role: "assistant", Thinking: "All nodes report RUNNING.", Content: "all healthy"},
	}}
	s := NewChatServer(NewAgent(model, reg, 5, testLogger()))

	evs := send(t, s, "s1", "how do the nodes look?")
	kinds := make([]string, 0, len(evs))
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	// Each call announces model_call, then fragments stream (thinking_delta, then
	// delta for content), then the full thinking/assistant events finalize. Call 1
	// has no content (tool call only); call 2 streams the answer.
	want := []string{
		"model_call", "thinking_delta", "thinking", "tool_call", "tool_result",
		"model_call", "thinking_delta", "delta", "thinking", "assistant",
	}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("event kinds = %v, want %v", kinds, want)
	}
	if evs[1].Text != "I should inspect the fleet first." || evs[2].Text != evs[1].Text {
		t.Fatalf("thinking events = %+v", evs[:3])
	}
}

// TestChatMultiTurnHistory: a second send on the same session includes the first
// turn's user + assistant messages; a different session starts fresh.
func TestChatMultiTurnHistory(t *testing.T) {
	model := &captureModel{turns: []chatMessage{
		{Role: "assistant", Content: "answer one"},
		{Role: "assistant", Content: "answer two"},
		{Role: "assistant", Content: "answer three"},
	}}
	s := NewChatServer(NewAgent(model, NewRegistry(), 5, testLogger()))

	send(t, s, "s1", "first question")
	send(t, s, "s1", "second question")
	got := model.seen[1]
	// system + user1 + assistant1 + user2
	// User turns are timestamp-prefixed ("[...] second question"); match the suffix.
	if len(got) != 4 || got[2].Content != "answer one" || !strings.HasSuffix(got[3].Content, "second question") {
		t.Fatalf("second turn history = %+v", got)
	}

	send(t, s, "s2", "fresh session")
	if fresh := model.seen[2]; len(fresh) != 2 || fresh[0].Role != "system" {
		t.Fatalf("fresh session history = %+v", fresh)
	}
}

// TestChatModelErrorKeepsHistory: a model failure emits a terminal error event and
// leaves the session at its pre-turn history so a retry is clean.
func TestChatModelErrorKeepsHistory(t *testing.T) {
	model := &captureModel{errAt: 2, turns: []chatMessage{
		{Role: "assistant", Content: "answer one"},
		{Role: "assistant", Content: "answer after retry"},
	}}
	s := NewChatServer(NewAgent(model, NewRegistry(), 5, testLogger()))

	send(t, s, "s1", "first question")
	evs := send(t, s, "s1", "failing question")
	// model_call is announced, then the model errors → terminal error event.
	if len(evs) != 2 || evs[0].Kind != "model_call" || evs[1].Kind != "error" {
		t.Fatalf("failing turn events = %+v, want [model_call, error]", evs)
	}
	send(t, s, "s1", "retry question")
	got := model.seen[2]
	// The failed user turn must not linger: system + user1 + assistant1 + retry-user.
	if len(got) != 4 || !strings.HasSuffix(got[3].Content, "retry question") {
		t.Fatalf("retry history = %+v", got)
	}
}

// TestChatClientSideToolSuspendResume: when the model calls a client-side tool, the
// turn suspends with a tool_request event (not executed on the agent); a Resume feeds
// the frontend's result back and the loop continues to the final answer.
func TestChatClientSideToolSuspendResume(t *testing.T) {
	reg := NewRegistry()
	reg.AddClientSide(&probeStub{name: "probe_http"})
	model := &captureModel{turns: []chatMessage{
		{Role: "assistant", ToolCalls: []toolCall{{Function: toolCallFunc{Name: "probe_http", Arguments: json.RawMessage(`{"url":"https://edge"}`)}}}},
		{Role: "assistant", Content: "the edge is reachable"},
	}}
	s := NewChatServer(NewAgent(model, reg, 5, testLogger()))

	// Turn 1: suspends at the probe. Last event must be tool_request, no answer yet.
	evs := send(t, s, "s1", "is the edge up?")
	last := evs[len(evs)-1]
	if last.Kind != "tool_request" || last.Tool != "probe_http" {
		t.Fatalf("expected turn to suspend on tool_request(probe_http), got %+v", evs)
	}
	for _, e := range evs {
		if e.Kind == "assistant" {
			t.Fatalf("should not have answered before the probe ran: %+v", evs)
		}
	}
	// The frontend runs the probe and resumes with the result.
	var rev []pb.ChatEvent
	err := s.Resume(context.Background(), &pbaccess.ResourceMonitorChatActionResumeArgsDTO{
		SessionId: "s1", Tool: "probe_http", Result: "HTTP HEAD https://edge\n  status: 200 OK",
	}, chatStream(&rev))
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(rev) == 0 || rev[len(rev)-1].Kind != "assistant" || rev[len(rev)-1].Text != "the edge is reachable" {
		t.Fatalf("resume did not reach the final answer: %+v", rev)
	}
	// The model's second call must have seen the probe result as a tool message.
	second := model.seen[1]
	var sawToolResult bool
	for _, m := range second {
		if m.Role == "tool" && m.ToolName == "probe_http" && strings.Contains(m.Content, "200 OK") {
			sawToolResult = true
		}
	}
	if !sawToolResult {
		t.Fatalf("resume did not feed the probe result to the model: %+v", second)
	}
}

// TestChatResumeWithoutPending: a resume on a session that isn't suspended errors.
func TestChatResumeWithoutPending(t *testing.T) {
	s := NewChatServer(NewAgent(&captureModel{}, NewRegistry(), 5, testLogger()))
	var evs []pb.ChatEvent
	if err := s.Resume(context.Background(), &pbaccess.ResourceMonitorChatActionResumeArgsDTO{
		SessionId: "s1", Tool: "probe_http", Result: "x",
	}, chatStream(&evs)); err == nil {
		t.Fatal("resume without a pending tool should error")
	}
}

// probeStub is a client-side tool whose Exec must never be called by the loop.
type probeStub struct{ name string }

func (p *probeStub) Name() string           { return p.name }
func (p *probeStub) Description() string    { return "external probe (frontend-run)" }
func (p *probeStub) Schema() map[string]any { return map[string]any{"type": "object"} }
func (p *probeStub) Exec(context.Context, json.RawMessage) (string, error) {
	panic("client-side probe Exec must not run on the agent")
}

// TestChatValidation: session_id and message are required.
func TestChatValidation(t *testing.T) {
	s := NewChatServer(NewAgent(&captureModel{}, NewRegistry(), 5, testLogger()))
	var evs []pb.ChatEvent
	if err := s.Send(context.Background(), &pbaccess.ResourceMonitorChatActionSendArgsDTO{Message: "hi"}, chatStream(&evs)); err == nil {
		t.Fatal("missing session_id should error")
	}
	if err := s.Send(context.Background(), &pbaccess.ResourceMonitorChatActionSendArgsDTO{SessionId: "s"}, chatStream(&evs)); err == nil {
		t.Fatal("empty message should error")
	}
}

// TestTrimHistory: overflow drops the oldest turns, keeps the system prompt, and
// never leaves a dangling tool result at the head of the kept tail.
func TestTrimHistory(t *testing.T) {
	msgs := []chatMessage{{Role: "system", Content: "sys"}}
	for i := 0; len(msgs) < maxChatMessagesPerSession+7; i++ {
		msgs = append(msgs,
			chatMessage{Role: "user", Content: fmt.Sprintf("q%d", i)},
			chatMessage{Role: "assistant", ToolCalls: []toolCall{{}}},
			chatMessage{Role: "tool", Content: "result"},
			chatMessage{Role: "assistant", Content: fmt.Sprintf("a%d", i)},
		)
	}
	got := trimHistory(msgs)
	if len(got) > maxChatMessagesPerSession {
		t.Fatalf("len = %d, want <= %d", len(got), maxChatMessagesPerSession)
	}
	if got[0].Role != "system" {
		t.Fatalf("system prompt dropped: %+v", got[0])
	}
	if got[1].Role != "user" {
		t.Fatalf("kept tail starts mid-turn with role %q", got[1].Role)
	}
	if got[len(got)-1].Content != msgs[len(msgs)-1].Content {
		t.Fatal("newest message dropped")
	}
}

// TestChatSessionEviction: exceeding maxChatSessions LRU-evicts the oldest session
// (its history restarts on next use).
func TestChatSessionEviction(t *testing.T) {
	turns := make([]chatMessage, 0, maxChatSessions+2)
	for i := 0; i < maxChatSessions+2; i++ {
		turns = append(turns, chatMessage{Role: "assistant", Content: "ok"})
	}
	model := &captureModel{turns: turns}
	s := NewChatServer(NewAgent(model, NewRegistry(), 5, testLogger()))

	send(t, s, "first", "hello")
	for i := 0; i < maxChatSessions; i++ {
		send(t, s, fmt.Sprintf("s%d", i), "hello")
	}
	send(t, s, "first", "are you still there?")
	last := model.seen[len(model.seen)-1]
	if len(last) != 2 {
		t.Fatalf("evicted session should restart fresh, got history %+v", last)
	}
}

// sinkFunc adapts a func to the EventSink an in-process frontend passes to
// SendTo/ResumeTo (the nodewatchmock harness pattern).
type sinkFunc func(*pb.ChatEvent) error

func (f sinkFunc) Send(ev *pb.ChatEvent) error { return f(ev) }

// TestSendToInProcessSink: SendTo drives a turn against a plain EventSink with no
// RPC stream — the surface the nodewatchmock harness relies on.
func TestSendToInProcessSink(t *testing.T) {
	model := &captureModel{turns: []chatMessage{{Role: "assistant", Content: "in-process ok"}}}
	s := NewChatServer(NewAgent(model, NewRegistry(), 5, testLogger()))

	var evs []pb.ChatEvent
	err := s.SendTo(context.Background(), &pbaccess.ResourceMonitorChatActionSendArgsDTO{
		SessionId: "local", Message: "hello",
	}, sinkFunc(func(ev *pb.ChatEvent) error {
		evs = append(evs, *ev)
		return nil
	}))
	if err != nil {
		t.Fatalf("SendTo: %v", err)
	}
	if len(evs) == 0 || evs[len(evs)-1].Kind != "assistant" || evs[len(evs)-1].Text != "in-process ok" {
		t.Fatalf("events = %+v", evs)
	}
}
