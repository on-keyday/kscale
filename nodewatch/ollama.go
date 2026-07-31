package nodewatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// The chat* / tool* types mirror the subset of the Ollama /api/chat wire format we
// use. Hand-rolled rather than importing github.com/ollama/ollama/api, whose module
// drags in the whole Ollama server tree — we only need chat-with-tools.

type chatMessage struct {
	Role      string     `json:"role"` // system | user | assistant | tool
	Content   string     `json:"content"`
	Thinking  string     `json:"thinking,omitempty"` // role=assistant: the model's reasoning (thinking models with think enabled)
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
	ToolName  string     `json:"tool_name,omitempty"` // role=tool: the tool that produced Content
}

type toolCall struct {
	Function toolCallFunc `json:"function"`
}

type toolCallFunc struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"` // Ollama emits an object
}

// toolSpec advertises one tool to the model.
type toolSpec struct {
	Type     string       `json:"type"` // "function"
	Function functionSpec `json:"function"`
}

type functionSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"` // JSON schema
}

// chatOptions are the Ollama runtime knobs that matter for an agent loop: a large
// enough num_ctx so accumulating tool results don't overflow the window (the default
// is small — context overflow mid-loop causes "forgetting"), and a low temperature
// for deterministic tool selection.
type chatOptions struct {
	NumCtx      int     `json:"num_ctx"`
	Temperature float64 `json:"temperature"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Tools    []toolSpec    `json:"tools,omitempty"`
	Stream   bool          `json:"stream"`
	Think    bool          `json:"think,omitempty"` // thinking models only: Ollama 400s on others
	Options  chatOptions   `json:"options"`
}

type chatResponse struct {
	Message chatMessage `json:"message"`
	Done    bool        `json:"done"`
}

// deltaFunc observes generation fragments as the model produces them. kind is
// "thinking" (reasoning fragment) or "content" (answer fragment). An error aborts
// the call (the observer — a chat stream — is gone).
type deltaFunc func(kind, text string) error

// chatModel is the loop's dependency on the LLM — an interface so tests can script a
// mock without a running Ollama. onDelta may be nil: the call is then non-streaming
// (the observe pass has no live viewer, so fragments would be wasted work).
type chatModel interface {
	chat(ctx context.Context, msgs []chatMessage, tools []toolSpec, onDelta deltaFunc) (chatMessage, error)
}

// ollamaClient is a minimal Ollama /api/chat caller (the chat-with-tools subset).
type ollamaClient struct {
	host   string
	model  string
	think  bool
	numCtx int
	temp   float64
	// idleTimeout bounds the time with NO progress (no new stream chunk / no response),
	// NOT the total call duration. http.Client.Timeout would cap the whole request
	// including the streaming body read, so a healthy-but-slow CPU generation streaming
	// for minutes gets killed mid-stream ("Client.Timeout ... while reading body"); an
	// idle timer only fires on a real stall. See chat().
	idleTimeout time.Duration
	hc          *http.Client
}

func newOllamaClient(host, model string, think bool, timeout time.Duration, numCtx int) *ollamaClient {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if numCtx <= 0 {
		numCtx = 8192
	}
	// hc has NO Timeout on purpose — the per-call idle timer in chat() is the deadline.
	return &ollamaClient{host: host, model: model, think: think, numCtx: numCtx, temp: 0.1, idleTimeout: timeout, hc: &http.Client{}}
}

// chat sends one /api/chat turn and returns the assembled assistant message. With
// onDelta set the request streams (NDJSON chunks) and every thinking/content
// fragment is forwarded as it is generated; with onDelta nil it is a single
// non-streaming exchange.
func (o *ollamaClient) chat(ctx context.Context, msgs []chatMessage, tools []toolSpec, onDelta deltaFunc) (chatMessage, error) {
	body, err := json.Marshal(chatRequest{
		Model: o.model, Messages: msgs, Tools: tools, Stream: onDelta != nil, Think: o.think,
		Options: chatOptions{NumCtx: o.numCtx, Temperature: o.temp},
	})
	if err != nil {
		return chatMessage{}, err
	}
	// Idle deadline: cancel the request if no progress is made for idleTimeout —
	// armed before the send (so it also bounds connect + time-to-first-chunk) and reset
	// on every received chunk below. This replaces http.Client.Timeout, which would cap
	// the TOTAL request incl. the full stream and kill a healthy long generation.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := time.AfterFunc(o.idleTimeout, cancel)
	defer idle.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.host+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return chatMessage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.hc.Do(req)
	if err != nil {
		return chatMessage{}, fmt.Errorf("ollama chat: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return chatMessage{}, fmt.Errorf("ollama chat: %s: %s", resp.Status, b)
	}
	dec := json.NewDecoder(resp.Body)
	if onDelta == nil {
		var cr chatResponse
		if err := dec.Decode(&cr); err != nil {
			return chatMessage{}, fmt.Errorf("ollama decode: %w", err)
		}
		return cr.Message, nil
	}
	// Streaming: chunks carry thinking/content fragments (and, near the end, tool
	// calls); assemble the full message while forwarding each fragment.
	full := chatMessage{Role: "assistant"}
	for {
		var cr chatResponse
		if err := dec.Decode(&cr); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return chatMessage{}, fmt.Errorf("ollama stream decode: %w", err)
		}
		idle.Reset(o.idleTimeout) // progress: a chunk arrived, restart the stall timer
		if cr.Message.Role != "" {
			full.Role = cr.Message.Role
		}
		if cr.Message.Thinking != "" {
			full.Thinking += cr.Message.Thinking
			if err := onDelta("thinking", cr.Message.Thinking); err != nil {
				return chatMessage{}, err
			}
		}
		if cr.Message.Content != "" {
			full.Content += cr.Message.Content
			if err := onDelta("content", cr.Message.Content); err != nil {
				return chatMessage{}, err
			}
		}
		full.ToolCalls = append(full.ToolCalls, cr.Message.ToolCalls...)
		if cr.Done {
			break
		}
	}
	return full, nil
}
