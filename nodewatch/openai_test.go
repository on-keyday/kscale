package nodewatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// openaiServer serves one canned /v1/chat/completions reply per request and records
// each decoded request body.
type openaiServer struct {
	replies []func(w http.ResponseWriter)
	got     []oaRequest
}

func (s *openaiServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var req oaRequest
		if err := json.Unmarshal(b, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.got = append(s.got, req)
		if len(s.got) > len(s.replies) {
			http.Error(w, "no more replies", http.StatusInternalServerError)
			return
		}
		s.replies[len(s.got)-1](w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func jsonReply(body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}
}

func sseReply(chunks ...string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}
}

// TestOpenAIAgentLoop drives a whole observe pass over the OpenAI wire: a tool call
// whose arguments arrive as a JSON string, then a final answer. The tool must see
// the arguments as an object, and the second request must carry the assistant's
// call and the tool result tied together by the same tool_call_id. The server
// answers the (streaming) requests with plain JSON, which must be read as such.
func TestOpenAIAgentLoop(t *testing.T) {
	s := &openaiServer{replies: []func(http.ResponseWriter){
		jsonReply(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"srv-1","type":"function","function":{"name":"node_list","arguments":"{\"dp_type\":\"l4lb\"}"}}]}}]}`),
		jsonReply(`{"choices":[{"message":{"role":"assistant","content":"all healthy","reasoning_content":"looked fine"}}]}`),
	}}
	srv := s.start(t)
	tool := &recordTool{name: "node_list"}
	reg := NewRegistry()
	reg.Add(tool)
	agent, err := NewLLMAgent(Config{API: APIOpenAI, LLMHost: srv.URL + "/", Model: "m", MaxSteps: 5}, reg, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(tool.got) != 1 || tool.got[0] != `{"dp_type":"l4lb"}` {
		t.Fatalf("tool args = %q, want one object", tool.got)
	}
	if len(s.got) != 2 {
		t.Fatalf("requests = %d, want 2", len(s.got))
	}
	if len(s.got[0].Tools) != 1 || s.got[0].Model != "m" {
		t.Fatalf("first request = %+v", s.got[0])
	}
	msgs := s.got[1].Messages
	if len(msgs) != 4 {
		t.Fatalf("second request messages = %+v", msgs)
	}
	call, result := msgs[2], msgs[3]
	if call.Role != "assistant" || len(call.ToolCalls) != 1 || call.ToolCalls[0].Function.Arguments != `{"dp_type":"l4lb"}` {
		t.Fatalf("assistant call on the wire = %+v", call)
	}
	if result.Role != "tool" || result.ToolCallID == "" || result.ToolCallID != call.ToolCalls[0].ID {
		t.Fatalf("tool result id %q does not match call id %q", result.ToolCallID, call.ToolCalls[0].ID)
	}
	if !strings.Contains(result.Content, "tool-result") {
		t.Fatalf("tool result content = %q", result.Content)
	}
}

// TestOpenAIStream: reasoning/content fragments are forwarded in order, and tool-call
// argument fragments are reassembled per index (two interleaved calls).
func TestOpenAIStream(t *testing.T) {
	s := &openaiServer{replies: []func(http.ResponseWriter){sseReply(
		`{"choices":[{"delta":{"role":"assistant","reasoning_content":"think "}}]}`,
		`{"choices":[{"delta":{"reasoning":"more"}}]}`,
		`{"choices":[{"delta":{"content":"checking"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"node_list","arguments":"{\"x\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","type":"function","function":{"name":"vip_list","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`{"choices":[],"usage":{"total_tokens":3}}`,
	)}}
	srv := s.start(t)
	c := newOpenAIClient(srv.URL, "m", time.Minute)
	var deltas []string
	got, err := c.chat(context.Background(), []chatMessage{{Role: "user", Content: "hi"}}, nil, func(kind, text string) error {
		deltas = append(deltas, kind+":"+text)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !s.got[0].Stream {
		t.Fatal("request was not streaming")
	}
	if want := "thinking:think |thinking:more|content:checking"; strings.Join(deltas, "|") != want {
		t.Fatalf("deltas = %q, want %q", strings.Join(deltas, "|"), want)
	}
	if got.Thinking != "think more" || got.Content != "checking" {
		t.Fatalf("assembled = %+v", got)
	}
	if len(got.ToolCalls) != 2 ||
		got.ToolCalls[0].Function.Name != "node_list" || string(got.ToolCalls[0].Function.Arguments) != `{"x":1}` ||
		got.ToolCalls[1].Function.Name != "vip_list" || string(got.ToolCalls[1].Function.Arguments) != `{}` {
		t.Fatalf("tool calls = %+v", got.ToolCalls)
	}
}

// TestToOpenAIMessagesIDs: ids pair each tool result with its own call across
// multiple assistant turns, and a malformed argument string survives as a JSON
// string instead of corrupting the history.
func TestToOpenAIMessagesIDs(t *testing.T) {
	bad := fromOpenAIToolCall("t", `{"broken"`)
	if !json.Valid(bad.Function.Arguments) {
		t.Fatalf("malformed args stored as invalid JSON: %s", bad.Function.Arguments)
	}
	tc := func(name string) toolCall {
		return toolCall{Function: toolCallFunc{Name: name, Arguments: json.RawMessage(`{}`)}}
	}
	out := toOpenAIMessages([]chatMessage{
		{Role: "system"},
		{Role: "user"},
		{Role: "assistant", ToolCalls: []toolCall{tc("a"), tc("b")}},
		{Role: "tool", ToolName: "a"},
		{Role: "tool", ToolName: "b"},
		{Role: "assistant", ToolCalls: []toolCall{tc("c")}},
		{Role: "tool", ToolName: "c"},
	})
	pairs := [][2]int{{3, 0}, {4, 1}}
	for _, p := range pairs {
		if out[p[0]].ToolCallID != out[2].ToolCalls[p[1]].ID {
			t.Fatalf("message %d id %q != call %q", p[0], out[p[0]].ToolCallID, out[2].ToolCalls[p[1]].ID)
		}
	}
	if out[6].ToolCallID != out[5].ToolCalls[0].ID || out[6].ToolCallID == out[3].ToolCallID {
		t.Fatalf("second turn ids: call %q result %q", out[5].ToolCalls[0].ID, out[6].ToolCallID)
	}
}

func TestNewLLMAgentRejectsUnknownAPI(t *testing.T) {
	if _, err := NewLLMAgent(Config{API: "grpc"}, NewRegistry(), testLogger()); err == nil {
		t.Fatal("unknown API accepted")
	}
}
