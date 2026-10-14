package nodewatch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// openaiClient is a minimal OpenAI-compatible /v1/chat/completions caller (the
// chat-with-tools subset) — the API llama-server, vLLM and Ollama's /v1 all speak, so
// the LLM runtime can change without touching the agent. The agent's history stays in
// the Ollama-shaped chatMessage; this client maps at the wire boundary where the two
// formats differ:
//   - tool-call arguments are a JSON *string* here (an object in Ollama), and a stream
//     delivers them as fragments keyed by the call's index;
//   - a tool result is tied to its call by tool_call_id, not by tool name. The history
//     keeps no ids, so they are re-derived from position on every request
//     (toOpenAIMessages) — consistent within one request, which is all a server needs;
//   - reasoning is outside the standard: servers return it as reasoning_content
//     (llama-server, vLLM) or reasoning (others); both are read. Turning it on and the
//     context window are SERVER-side settings (e.g. llama-server --reasoning-budget /
//     --chat-template-kwargs / -c) — there is no portable request field for either, so
//     the request carries neither.
type openaiClient struct {
	host  string
	model string
	temp  float64
	// idleTimeout bounds the time with NO progress, not the total call duration —
	// same contract as ollamaClient.idleTimeout.
	idleTimeout time.Duration
	hc          *http.Client
}

func newOpenAIClient(host, model string, timeout time.Duration) *openaiClient {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	// hc has NO Timeout on purpose — the per-call idle timer in chat() is the deadline.
	return &openaiClient{host: strings.TrimRight(host, "/"), model: model, temp: 0.1, idleTimeout: timeout, hc: &http.Client{}}
}

type oaMessage struct {
	Role       string       `json:"role"`
	Content    string       `json:"content"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	Index    *int   `json:"index,omitempty"` // stream deltas only: which call a fragment extends
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"` // a JSON document encoded as a string
	} `json:"function"`
}

type oaRequest struct {
	Model       string      `json:"model"`
	Messages    []oaMessage `json:"messages"`
	Tools       []toolSpec  `json:"tools,omitempty"` // same shape as Ollama's
	Stream      bool        `json:"stream"`
	Temperature float64     `json:"temperature"`
}

// oaReply is a response message (non-streaming) or a stream delta — the same fields
// either way.
type oaReply struct {
	Content          string       `json:"content"`
	ReasoningContent string       `json:"reasoning_content"`
	Reasoning        string       `json:"reasoning"`
	ToolCalls        []oaToolCall `json:"tool_calls"`
}

func (r oaReply) reasoning() string {
	if r.ReasoningContent != "" {
		return r.ReasoningContent
	}
	return r.Reasoning
}

type oaResponse struct {
	Choices []struct {
		Message oaReply `json:"message"`
		Delta   oaReply `json:"delta"`
	} `json:"choices"`
}

// toOpenAIMessages converts the agent history to the wire format. Each assistant
// message's tool calls get ids from their position ("call_<msg>_<call>"), and the
// tool results that follow claim them in order — the loop appends results in call
// order and trimHistory never cuts a turn mid-way, so the two always line up. A tool
// result with no call left to claim (should not happen) gets a unique orphan id
// rather than a reused one.
func toOpenAIMessages(msgs []chatMessage) []oaMessage {
	out := make([]oaMessage, 0, len(msgs))
	var open []string // ids of the latest assistant's calls not yet answered
	for i, m := range msgs {
		om := oaMessage{Role: m.Role, Content: m.Content}
		switch m.Role {
		case "assistant":
			open = open[:0]
			for j, tc := range m.ToolCalls {
				id := fmt.Sprintf("call_%d_%d", i, j)
				open = append(open, id)
				var c oaToolCall
				c.ID, c.Type = id, "function"
				c.Function.Name = tc.Function.Name
				c.Function.Arguments = string(tc.Function.Arguments)
				if c.Function.Arguments == "" {
					c.Function.Arguments = "{}"
				}
				om.ToolCalls = append(om.ToolCalls, c)
			}
		case "tool":
			if len(open) > 0 {
				om.ToolCallID, open = open[0], open[1:]
			} else {
				om.ToolCallID = fmt.Sprintf("call_%d_orphan", i)
			}
		}
		out = append(out, om)
	}
	return out
}

// fromOpenAIToolCall converts a completed wire call to the agent's form. The
// arguments string is passed through as raw JSON when it parses; otherwise it is
// wrapped as a JSON string so the tool sees (and reports) the malformed input
// instead of the history holding invalid JSON.
func fromOpenAIToolCall(name, args string) toolCall {
	args = strings.TrimSpace(args)
	raw := json.RawMessage(args)
	switch {
	case args == "":
		raw = json.RawMessage("{}")
	case !json.Valid(raw):
		raw, _ = json.Marshal(args)
	}
	return toolCall{Function: toolCallFunc{Name: name, Arguments: raw}}
}

// chat sends one /v1/chat/completions turn and returns the assembled assistant
// message. With onDelta set the request streams (SSE) and every reasoning/content
// fragment is forwarded as it is generated; with onDelta nil it is a single
// non-streaming exchange.
func (o *openaiClient) chat(ctx context.Context, msgs []chatMessage, tools []toolSpec, onDelta deltaFunc) (chatMessage, error) {
	body, err := json.Marshal(oaRequest{
		Model: o.model, Messages: toOpenAIMessages(msgs), Tools: tools,
		Stream: onDelta != nil, Temperature: o.temp,
	})
	if err != nil {
		return chatMessage{}, err
	}
	// Idle deadline, armed before the send and reset on every received line — see
	// ollamaClient.chat for why this replaces http.Client.Timeout.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := time.AfterFunc(o.idleTimeout, cancel)
	defer idle.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.host+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return chatMessage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.hc.Do(req)
	if err != nil {
		return chatMessage{}, fmt.Errorf("openai chat: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return chatMessage{}, fmt.Errorf("openai chat: %s: %s", resp.Status, b)
	}
	// A server that ignores stream:true answers with plain JSON; reading that as SSE
	// would silently yield an empty message, so dispatch on what actually came back.
	if onDelta == nil || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		var or oaResponse
		if err := json.NewDecoder(resp.Body).Decode(&or); err != nil {
			return chatMessage{}, fmt.Errorf("openai decode: %w", err)
		}
		if len(or.Choices) == 0 {
			return chatMessage{}, fmt.Errorf("openai chat: response has no choices")
		}
		m := or.Choices[0].Message
		out := chatMessage{Role: "assistant", Content: m.Content, Thinking: m.reasoning()}
		for _, tc := range m.ToolCalls {
			out.ToolCalls = append(out.ToolCalls, fromOpenAIToolCall(tc.Function.Name, tc.Function.Arguments))
		}
		return out, nil
	}

	// Streaming: SSE "data: <chunk>" lines until "data: [DONE]". Content/reasoning
	// fragments are forwarded as they arrive; tool calls are assembled per index (the
	// name arrives once, the arguments string in pieces) and emitted at the end.
	full := chatMessage{Role: "assistant"}
	type partial struct {
		name string
		args strings.Builder
	}
	calls := map[int]*partial{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		idle.Reset(o.idleTimeout) // progress: a line arrived, restart the stall timer
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue // blank separator, ": comment", event:/id: fields
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var or oaResponse
		if err := json.Unmarshal([]byte(data), &or); err != nil {
			return chatMessage{}, fmt.Errorf("openai stream decode: %w", err)
		}
		if len(or.Choices) == 0 {
			continue // e.g. a trailing usage-only chunk
		}
		d := or.Choices[0].Delta
		if r := d.reasoning(); r != "" {
			full.Thinking += r
			if err := onDelta("thinking", r); err != nil {
				return chatMessage{}, err
			}
		}
		if d.Content != "" {
			full.Content += d.Content
			if err := onDelta("content", d.Content); err != nil {
				return chatMessage{}, err
			}
		}
		for _, tc := range d.ToolCalls {
			idx := len(calls) // no index: a server sending whole calls, one per entry
			if tc.Index != nil {
				idx = *tc.Index
			}
			p := calls[idx]
			if p == nil {
				p = &partial{}
				calls[idx] = p
			}
			if tc.Function.Name != "" {
				p.name = tc.Function.Name
			}
			p.args.WriteString(tc.Function.Arguments)
		}
	}
	if err := sc.Err(); err != nil {
		return chatMessage{}, fmt.Errorf("openai stream read: %w", err)
	}
	idxs := make([]int, 0, len(calls))
	for i := range calls {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		full.ToolCalls = append(full.ToolCalls, fromOpenAIToolCall(calls[i].name, calls[i].args.String()))
	}
	return full, nil
}
