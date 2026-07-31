package nodewatch

import (
	"context"
	"errors"
	"fmt"
	"sync"

	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
)

// chatSystemPrompt frames the interactive persona: same fleet, same read-only tools
// as the observe pass, but answering an administrator instead of judging autonomously.
const chatSystemPrompt = `You are nodewatch, the monitoring agent for a kscale self-hosted CDN control plane, answering an administrator's questions interactively.
Use the read-only kscale tools to inspect the live internal state. To check the
PUBLIC-facing surface from outside the lab, use the probe_* tools (http, tls, dns,
quic, tcp, ping, traceroute) — these run on the operator's frontend from a real
external vantage and require operator approval, so use them when a question is about
what an external client sees or when internal state looks fine but the edge may be
unreachable. Correlate external probe results with internal state (e.g. edge returns
502 externally but popcache reports RUNNING internally → suspect the l4lb front).
When a question needs no fresh state, answer directly. Be concise and factual. Do not
invent data — only report what the tools returned.
Do NOT assume conventional defaults about how this deployment is configured — e.g.
that a standard port (443, 80, ...) is open, that a particular endpoint or listener
exists, or that a service is reachable. What is actually configured comes from the
tools, not from convention. In particular, before probing an external port/endpoint,
base the expectation on the configured listener set the internal tools report — do not
probe a port merely because it is conventionally expected, and do not conclude
"down/misconfigured" when a conventionally-expected-but-not-configured port is closed.
Something not configured here is not an anomaly; if unsure whether something is meant
to be present, check the config first rather than presuming the standard case.
URL anatomy — this matters because this deployment serves on NON-standard ports: a
URL is scheme://host[:port]/path, and when :port is OMITTED the scheme's default is
used (http means port 80, https means port 443). So "https://edge.example" does NOT
probe "whatever port the service uses" — it probes port 443, silently. Here that is
usually the WRONG port: the real listeners come from the config (e.g.
popcache_config_list reports the http/https/http3 ports). ALWAYS write the explicit
:port from the configured listener set into every probe URL. If a probe fails,
first re-check that the URL's effective port (explicit, or the scheme default you
implied by omitting it) matches the configured one — a mismatch means YOUR URL was
wrong, not that the service is down.
Each user message is prefixed with [timestamp] and each tool result with
[gathered timestamp]. Use these to order events: a tool result reflects state only
as of its gathered time. Do NOT assume an earlier result is still current — if the
user asks about the present ("is it still down?", "check again"), call the tool
again rather than reusing a stale result. Track which probes/tools you have already
run in this conversation by their timestamps.`

const (
	// maxChatSessions bounds concurrent conversation histories; beyond it the
	// least-recently-used session is dropped (memory-only, nothing to clean up).
	maxChatSessions = 8
	// maxChatMessagesPerSession bounds one history so a long conversation can't
	// outgrow the model's context window; oldest turns are dropped, system kept.
	maxChatMessagesPerSession = 40
)

// ChatServer implements MonitorChatService on the agent's own peer connection: the
// control plane relays an admin's `monitor_chat send` here (the ABAC gate already
// ran on the control plane). Sessions are keyed by the client-chosen session_id and
// live in memory only — a fresh id is a fresh conversation.
type ChatServer struct {
	pb.UnimplementedMonitorChatServiceServer
	agent *Agent

	mu       sync.Mutex
	sessions map[string]*chatSession
	order    []string // LRU order, oldest first
}

// chatSession is one conversation. mu is held for a whole turn, serializing
// concurrent sends on the same session (second caller waits, histories never race).
// pending is non-nil while the turn is suspended waiting for a frontend-run
// client-side tool: pending[0] is the awaited call, pending[1:] the same message's
// remaining calls. A resume clears it.
type chatSession struct {
	mu      sync.Mutex
	msgs    []chatMessage
	pending []toolCall
}

// NewChatServer wires the chat RPC surface over agent (its model + read tools).
// Callers give chat its own Agent so the toolset can differ from the observe pass
// (no notify tool — the answer goes to the human on the stream).
func NewChatServer(agent *Agent) *ChatServer {
	return &ChatServer{agent: agent, sessions: map[string]*chatSession{}}
}

var _ pb.MonitorChatServiceServer = (*ChatServer)(nil)

// EventSink receives a turn's streamed ChatEvents — the server-stream surface
// narrowed to what the chat logic needs, so an in-process frontend (e.g. the
// nodewatchmock harness) can drive a turn without the RPC stack. The generated
// ServerSendStream types satisfy it.
type EventSink interface {
	Send(*pb.ChatEvent) error
}

// Send runs one turn over the RPC stream; the logic lives in SendTo.
func (s *ChatServer) Send(ctx context.Context, req *pbaccess.ResourceMonitorChatActionSendArgsDTO, stream *pb.MonitorChatServiceSendServerStream) error {
	return s.SendTo(ctx, req, stream)
}

// SendTo runs one turn: append the user message to the session's history, drive the
// tool loop streaming step events, then persist the grown history. Model/tool
// failures are reported as a terminal "error" event (the session survives for a
// retry); only a broken sink aborts with an error.
func (s *ChatServer) SendTo(ctx context.Context, req *pbaccess.ResourceMonitorChatActionSendArgsDTO, sink EventSink) error {
	if req.SessionId == "" {
		return errors.New("monitor_chat: session_id is required")
	}
	if req.Message == "" {
		return errors.New("monitor_chat: empty message")
	}
	sess := s.session(req.SessionId)
	sess.mu.Lock()
	defer sess.mu.Unlock()

	msgs := sess.msgs
	if len(msgs) == 0 {
		msgs = []chatMessage{{Role: "system", Content: s.agent.systemContent(chatSystemPrompt + renderRecipes())}}
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: "[" + nowStamp() + "] " + req.Message})

	emit := emitTo(sink)
	updated, final, pending, err := s.agent.runTurn(ctx, msgs, emit)
	return s.settle(sess, emit, updated, final, pending, err)
}

// Resume continues a suspended turn over the RPC stream; the logic lives in ResumeTo.
func (s *ChatServer) Resume(ctx context.Context, req *pbaccess.ResourceMonitorChatActionResumeArgsDTO, stream *pb.MonitorChatServiceResumeServerStream) error {
	return s.ResumeTo(ctx, req, stream)
}

// ResumeTo continues a turn suspended on a client-side tool: it feeds the frontend's
// probe result (or a denial) back and drives the loop until the next answer or the
// next client-side tool. It errors if the session isn't actually suspended on this
// tool (a stale/duplicate resume).
func (s *ChatServer) ResumeTo(ctx context.Context, req *pbaccess.ResourceMonitorChatActionResumeArgsDTO, sink EventSink) error {
	if req.SessionId == "" {
		return errors.New("monitor_chat: session_id is required")
	}
	sess := s.session(req.SessionId)
	sess.mu.Lock()
	defer sess.mu.Unlock()

	if len(sess.pending) == 0 {
		return errors.New("monitor_chat: session is not awaiting a tool result")
	}
	if want := sess.pending[0].Function.Name; req.Tool != want {
		return fmt.Errorf("monitor_chat: resume tool %q does not match the pending %q", req.Tool, want)
	}
	result := req.Result
	if req.Denied != "" {
		result = "The operator declined to run this probe: " + req.Denied
	}
	pending := sess.pending
	sess.pending = nil

	emit := emitTo(sink)
	updated, final, newPending, err := s.agent.resumeTurn(ctx, sess.msgs, pending, result, emit)
	return s.settle(sess, emit, updated, final, newPending, err)
}

// settle records the outcome of a (sub)turn on the session: persist the history and
// any new suspend point, and surface a model/tool error or a step-budget stall as a
// terminal event. A suspend (pending != nil) is a clean pause, not an error.
func (s *ChatServer) settle(sess *chatSession, emit emitFunc, updated []chatMessage, final *chatMessage, pending []toolCall, err error) error {
	if err != nil {
		// Keep the pre-turn history so the client can retry. A broken stream makes
		// this emit fail too, surfacing the transport error to the relay.
		return emit("error", err.Error(), "", "")
	}
	if pending != nil {
		// Suspended for a frontend-run tool: persist the grown history + the pending
		// calls; the tool_request event was already emitted. The turn's stream ends
		// here; the frontend resumes it.
		sess.msgs = updated
		sess.pending = pending
		return nil
	}
	if final == nil {
		if err := emit("error", "no final answer within the tool-step budget; try a narrower question", "", ""); err != nil {
			return err
		}
	}
	sess.msgs = trimHistory(updated)
	return nil
}

// emitTo adapts an event sink to the emitFunc the tool loop calls.
func emitTo(sink EventSink) emitFunc {
	return func(kind, text, tool, args string) error {
		return sink.Send(&pb.ChatEvent{Kind: kind, Text: text, Tool: tool, Args: args})
	}
}

// session returns the conversation for id, creating it (and LRU-evicting the
// oldest beyond maxChatSessions) if needed.
func (s *ChatServer) session(id string) *chatSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		for i, v := range s.order {
			if v == id {
				s.order = append(append(s.order[:i:i], s.order[i+1:]...), id)
				break
			}
		}
		return sess
	}
	if len(s.sessions) >= maxChatSessions {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.sessions, oldest)
	}
	sess := &chatSession{}
	s.sessions[id] = sess
	s.order = append(s.order, id)
	return sess
}

// trimHistory drops the oldest turns beyond maxChatMessagesPerSession, keeping the
// system prompt and never starting the kept tail mid-turn (a dangling tool result
// with no preceding user/assistant context confuses the model).
func trimHistory(msgs []chatMessage) []chatMessage {
	if len(msgs) <= maxChatMessagesPerSession {
		return msgs
	}
	head := 0
	if msgs[0].Role == "system" {
		head = 1
	}
	tail := msgs[len(msgs)-(maxChatMessagesPerSession-head):]
	for len(tail) > 0 && tail[0].Role != "user" {
		tail = tail[1:]
	}
	return append(msgs[:head:head], tail...)
}
