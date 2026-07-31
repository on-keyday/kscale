package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/kscale/probe"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
)

// Chat mode is an interactive conversation with the monitor agent's local LLM
// (nodewatch), relayed by the control plane over monitor_chat send. One enter = one
// turn = one server-stream of ChatEvents: the agent's tool calls stream in as muted
// activity lines while the model works, then the final answer lands as "monitor ▶".
// The session (history) lives on the agent, keyed by chatSession — Ctrl+L mints a
// fresh id, which is a fresh conversation; leaving the screen keeps it.

const chatLineLimit = 400 // transcript ring: plenty of scrollback, bounded memory
const chatLogPage = 10    // logical lines per PgUp/PgDn step

// chatEvMsg carries one streamed ChatEvent of the in-flight turn, or its termination.
type chatEvMsg struct {
	ev   *pb.ChatEvent
	err  error
	done bool
}

// chatTickMsg drives the in-flight turn's elapsed-seconds counter, so the status
// line visibly advances while the (minutes-long, CPU-bound) model generates —
// distinguishing "still working" from a frozen UI.
type chatTickMsg struct{}

func chatTickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return chatTickMsg{} })
}

// chatProbeDoneMsg carries a locally-run probe's output back to the Update loop.
// manual distinguishes an operator-initiated probe (fed to the model as a new turn)
// from an LLM-proposed one (resumed into the suspended turn).
type chatProbeDoneMsg struct {
	tool   string
	args   string
	result string
	err    error
	manual bool
}

// runProbeCmd executes the probe locally (this frontend = external vantage) and
// returns its output. It runs in a tea.Cmd goroutine because ping/traceroute can
// take tens of seconds.
func (m *model) runProbeCmd(tool, args string, manual bool) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		out, err := probe.Run(ctx, tool, json.RawMessage(args))
		return chatProbeDoneMsg{tool: tool, args: args, result: out, err: err, manual: manual}
	}
}

// --- manual probe palette (operator-initiated) ---

// openProbePalette starts the kind picker (p key, when idle).
func (m *model) openProbePalette() {
	m.chatProbePalette = true
	m.chatProbeCompose = false
	m.chatPaletteCur = 0
	m.chatStatus = stMuted.Render("pick a probe · ↑/↓ · enter · esc cancel")
}

// probeSpecs returns the probe kinds, sorted (probe.Specs is already sorted).
func probeSpecs() []probe.Spec { return probe.Specs() }

// paletteMove clamps the kind selection.
func (m *model) paletteMove(d int) {
	n := len(probeSpecs())
	m.chatPaletteCur += d
	if m.chatPaletteCur < 0 {
		m.chatPaletteCur = 0
	}
	if m.chatPaletteCur >= n {
		m.chatPaletteCur = n - 1
	}
}

// paletteSelect moves from the kind picker to args editing, prefilling a JSON
// skeleton of the probe's parameters.
func (m *model) paletteSelect() tea.Cmd {
	specs := probeSpecs()
	if m.chatPaletteCur < 0 || m.chatPaletteCur >= len(specs) {
		return nil
	}
	spec := specs[m.chatPaletteCur]
	m.chatProbeTool = spec.Name
	m.chatProbePalette = false
	m.chatProbeCompose = true
	ti := textinput.New()
	ti.Prompt = stTitle.Render("args ▶ ")
	ti.SetValue(argsSkeleton(spec))
	ti.CursorEnd()
	ti.Focus()
	m.chatInput = ti
	m.chatStatus = stMuted.Render("fill args JSON for " + spec.Name + " · enter run · esc back")
	return textinput.Blink
}

// composeToQuestion validates the composed args and advances to the optional
// question step (what the operator wants the monitor to focus on).
func (m *model) composeToQuestion() tea.Cmd {
	args := strings.TrimSpace(m.chatInput.Value())
	if !json.Valid([]byte(args)) {
		m.chatStatus = stErr.Render("args must be valid JSON — fix and enter, or esc")
		return nil
	}
	m.chatProbeArgs = args
	m.chatProbeCompose = false
	m.chatProbeAsk = true
	ti := textinput.New()
	ti.Prompt = stTitle.Render("ask ▶ ")
	ti.Placeholder = "what should the monitor look at? (optional — enter to just interpret)"
	ti.Focus()
	m.chatInput = ti
	m.chatStatus = stMuted.Render("optional question for " + m.chatProbeTool + " · enter run · esc back")
	return textinput.Blink
}

// questionToArgs steps back from the question to editing the args.
func (m *model) questionToArgs() {
	m.chatProbeAsk = false
	m.chatProbeCompose = true
	ti := textinput.New()
	ti.Prompt = stTitle.Render("args ▶ ")
	ti.SetValue(m.chatProbeArgs)
	ti.CursorEnd()
	ti.Focus()
	m.chatInput = ti
	m.chatStatus = stMuted.Render("fill args JSON for " + m.chatProbeTool + " · enter run · esc back")
}

// runManualProbe captures the optional question and runs the probe locally.
func (m *model) runManualProbe() tea.Cmd {
	tool, args := m.chatProbeTool, m.chatProbeArgs
	m.chatProbeQuestion = strings.TrimSpace(m.chatInput.Value())
	m.chatProbeAsk = false
	m.restoreChatInput()
	m.chatBusy = true
	m.chatElapsed = 0
	m.chatScroll = 0
	note := "run " + tool + " " + stMuted.Render(compactArgs(args))
	if m.chatProbeQuestion != "" {
		note += stMuted.Render("  — " + m.chatProbeQuestion)
	}
	m.appendChatLine(stOK.Render("you ▶ ") + note)
	m.appendChatLine(stMuted.Render("  ▸ running " + tool + " externally …"))
	m.chatStatus = stMuted.Render("running " + tool + " …")
	return tea.Batch(m.runProbeCmd(tool, args, true), chatTickCmd())
}

// argsSkeleton builds an editable JSON object from a probe spec's required params
// (or all properties if none are marked required), values left empty for the operator.
func argsSkeleton(spec probe.Spec) string {
	props, _ := spec.Params["properties"].(map[string]any)
	keys := []string{}
	if req, ok := spec.Params["required"].([]string); ok && len(req) > 0 {
		keys = append(keys, req...)
	} else if req, ok := spec.Params["required"].([]any); ok && len(req) > 0 {
		for _, k := range req {
			keys = append(keys, fmt.Sprint(k))
		}
	} else {
		for k := range props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	m := map[string]string{}
	for _, k := range keys {
		m[k] = ""
	}
	// Marshal deterministically (map marshals sorted keys).
	b, _ := json.Marshal(m)
	return string(b)
}

// composeProbeResult builds the resume payload for an LLM-proposed probe. The
// model's history holds its own tool call, so when the operator edited the
// arguments before approving, a bare result would be read as answering the
// model's OWN args and it mis-correlates (observed live: it insisted its args
// were right and blamed the user for the mismatch). Disclose the actually-used
// args up front; unedited runs pass through verbatim.
func composeProbeResult(proposed, actual, result string) string {
	if proposed == actual {
		return result
	}
	return "NOTE: the operator edited the probe arguments before approving. The probe actually ran with " +
		actual + " — not the arguments you requested. Base your interpretation on these actual arguments.\n\n" + result
}

// approveProbe is the single approval decision point. Today it is always manual (the
// operator pressed approve); a future auto-approve policy plugs in here without
// touching the tool_request/resume wire path.
func (m *model) approveProbe() tea.Cmd {
	m.chatAwaitProbe = false
	m.chatProbeEdit = false
	m.restoreChatInput()
	m.chatStatus = stMuted.Render("running " + m.chatProbeTool + " externally …")
	m.appendChatLine(stMuted.Render("  ▸ running " + m.chatProbeTool + " " + compactArgs(m.chatProbeArgs)))
	return m.runProbeCmd(m.chatProbeTool, m.chatProbeArgs, false)
}

// beginTurn starts an LLM turn with hidden message text, showing note in the
// transcript (the manual-probe path uses a note different from the raw message).
func (m *model) beginTurn(message, note string) tea.Cmd {
	m.chatBusy = true
	m.chatElapsed = 0
	m.chatScroll = 0
	m.chatStatus = ""
	if note != "" {
		m.appendChatLine(note)
	}
	return tea.Batch(m.startChatTurn(message), chatTickCmd())
}

// denyProbe resumes the turn telling the model the operator declined.
func (m *model) denyProbe() tea.Cmd {
	tool := m.chatProbeTool
	m.chatAwaitProbe = false
	m.chatProbeEdit = false
	m.restoreChatInput()
	m.appendChatLine(stErr.Render("  ✗ denied " + tool))
	m.chatStatus = stMuted.Render("resuming …")
	return m.startChatResume(tool, "", "operator denied the probe")
}

// firstSentence trims a tool description to its first sentence for the palette list.
func firstSentence(s string) string {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return s[:i+1]
	}
	return s
}

// compactArgs renders a probe's args JSON as a short one-line summary.
func compactArgs(args string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil || len(m) == 0 {
		return strings.TrimSpace(args)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, m[k]))
	}
	return strings.Join(parts, " ")
}

// newChatSessionID mints the client-chosen conversation key (see nodewatch's
// ChatServer: a fresh id is a fresh history on the agent).
func newChatSessionID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// restoreChatInput reinstalls the normal chat prompt ("you ▶") after a probe
// flow replaced m.chatInput with an args/ask editor. Every probe-flow exit path
// must call it: startChat only builds an input when none exists, so a leftover
// editor would otherwise stick as "args ▶" forever.
func (m *model) restoreChatInput() {
	ti := textinput.New()
	ti.Prompt = stTitle.Render("you ▶ ")
	ti.Placeholder = "ask the monitor about the fleet…"
	ti.Focus()
	m.chatInput = ti
}

// startChat prepares the chat screen state (idempotent across re-entry: the
// transcript and session are kept — the history lives on the agent anyway).
func (m *model) startChat() tea.Cmd {
	if m.chatSession == "" {
		m.chatSession = newChatSessionID()
	}
	if m.chatInput.Prompt == "" {
		m.restoreChatInput()
	}
	m.chatInput.Focus()
	return textinput.Blink
}

// resetChat abandons the current conversation: new session id, empty transcript.
func (m *model) resetChat() {
	m.stopChatTurn()
	m.chatBusy = false
	m.chatSession = newChatSessionID()
	m.chatLines = nil
	m.chatPendThink, m.chatPendAns = "", ""
	m.chatAwaitProbe, m.chatProbeEdit = false, false
	m.chatProbePalette, m.chatProbeCompose, m.chatProbeAsk = false, false, false
	m.chatProbeQuestion = ""
	m.restoreChatInput()
	m.chatScroll = 0
	m.chatStatus = stMuted.Render("— new session —")
}

// chatRecvStream is the common surface of the Send and Resume client streams.
type chatRecvStream interface {
	Recv(context.Context) (*pb.ChatEvent, error)
}

// startChatTurn sends one user message and streams the turn's events back.
func (m *model) startChatTurn(text string) tea.Cmd {
	dto := &pbaccess.ResourceMonitorChatActionSendArgsDTO{SessionId: m.chatSession, Message: text}
	return m.streamChat(func(ctx context.Context, c pb.MonitorChatServiceClient) (chatRecvStream, error) {
		return c.Send(ctx, dto)
	})
}

// startChatResume feeds a client-side tool result (or a denial) back into the
// suspended turn and streams the continued events.
func (m *model) startChatResume(tool, result, denied string) tea.Cmd {
	dto := &pbaccess.ResourceMonitorChatActionResumeArgsDTO{
		SessionId: m.chatSession, Tool: tool, Result: result, Denied: denied,
	}
	return m.streamChat(func(ctx context.Context, c pb.MonitorChatServiceClient) (chatRecvStream, error) {
		return c.Resume(ctx, dto)
	})
}

// streamChat opens a chat stream (Send or Resume) and pumps its events into chatCh
// (channel → tea.Msg → re-arm, the monitor.go pattern), plus the elapsed-seconds tick.
func (m *model) streamChat(open func(context.Context, pb.MonitorChatServiceClient) (chatRecvStream, error)) tea.Cmd {
	sctx, cancel := context.WithCancel(m.ctx)
	m.chatCancel = cancel
	ch := make(chan chatEvMsg, 16)
	m.chatCh = ch
	src := m.src
	go func() {
		stream, err := open(sctx, pb.NewMonitorChatServiceClient(src))
		if err != nil {
			trySend(ch, chatEvMsg{err: err, done: true}, sctx)
			return
		}
		for {
			ev, err := stream.Recv(sctx)
			if errors.Is(err, io.EOF) {
				trySend(ch, chatEvMsg{done: true}, sctx)
				return
			}
			if err != nil {
				trySend(ch, chatEvMsg{err: err, done: true}, sctx)
				return
			}
			if !trySend(ch, chatEvMsg{ev: ev}, sctx) {
				return
			}
		}
	}()
	return waitChatEv(ch)
}

func waitChatEv(ch chan chatEvMsg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

func (m *model) stopChatTurn() {
	if m.chatCancel != nil {
		m.chatCancel()
		m.chatCancel = nil
	}
	m.chatCh = nil
}

// onChatEvent folds one streamed event into the transcript + activity status. The
// *_delta events accumulate into the live buffers (rendered in-progress by
// chatView); the full thinking / assistant events commit them to the transcript.
func (m *model) onChatEvent(ev *pb.ChatEvent) {
	switch ev.Kind {
	case "thinking_delta":
		m.chatPendThink += ev.Text
		m.chatStatus = stMuted.Render("thinking …")
	case "model_call":
		// The agent is about to (or now) call the LLM — proves the request reached
		// the monitor and any further wait is model latency, not a lost call.
		m.chatStatus = stMuted.Render("monitor is querying the model …")
	case "tool_request":
		// The monitor proposes an external probe to run on THIS frontend (outside
		// the lab). Freeze any streamed text and enter approval — the turn is
		// suspended on the agent until we resume with a result.
		m.commitPending()
		m.chatAwaitProbe = true
		m.chatProbeTool = ev.Tool
		m.chatProbeArgs = ev.Args
		m.chatProbeProposed = ev.Args // kept verbatim to detect an operator edit at resume
		m.chatProbeEdit = false
		m.appendChatLine(stWarn.Render("  ⚑ monitor wants to run: ") + stTitle.Render(ev.Tool) + " " + stMuted.Render(compactArgs(ev.Args)))
		m.chatStatus = stWarn.Render("approve external probe? a approve · e edit · d deny")
	case "delta":
		m.chatPendAns += ev.Text
		m.chatStatus = stMuted.Render("answering …")
	case "thinking":
		// Reasoning finalized: commit the (authoritative) full text as muted lines,
		// one per reasoning line, so the answer stays visually primary.
		m.chatPendThink = ""
		for _, l := range strings.Split(strings.TrimSpace(ev.Text), "\n") {
			m.appendChatLine(stMuted.Render("  ┆ " + l))
		}
		m.chatStatus = stMuted.Render("thinking …")
	case "tool_call":
		m.commitPending() // freeze any streamed-but-unfinalized text before the tool line
		args := strings.Join(strings.Fields(ev.Args), " ")
		if len(args) > 60 {
			args = args[:60] + "…"
		}
		m.appendChatLine(stMuted.Render("  ⚙ " + ev.Tool + " " + args))
		m.chatStatus = stMuted.Render("running " + ev.Tool + " …")
	case "tool_result":
		m.appendChatLine(stMuted.Render(fmt.Sprintf("  ↳ %s → %dB", ev.Tool, len(ev.Text))))
		m.chatStatus = stMuted.Render("thinking …")
	case "assistant":
		m.chatPendAns = ""
		m.appendChatLine(stTitle.Render("monitor ▶ ") + ev.Text)
		m.appendChatLine("")
		m.chatStatus = ""
	case "error":
		m.commitPending()
		m.appendChatLine(stErr.Render("✗ " + ev.Text))
		m.appendChatLine("")
		m.chatStatus = ""
	}
}

// commitPending flushes any in-progress streamed text to permanent transcript lines
// (used when a step boundary arrives without its finalizing full-text event, e.g. a
// cancelled turn or a tool call mid-stream).
func (m *model) commitPending() {
	if m.chatPendThink != "" {
		for _, l := range strings.Split(strings.TrimSpace(m.chatPendThink), "\n") {
			m.appendChatLine(stMuted.Render("  ┆ " + l))
		}
		m.chatPendThink = ""
	}
	if m.chatPendAns != "" {
		m.appendChatLine(stTitle.Render("monitor ▶ ") + m.chatPendAns)
		m.appendChatLine("")
		m.chatPendAns = ""
	}
}

// appendChatLine adds one logical transcript line, keeping scrollback anchored while
// streaming (same from-the-newest-end offset discipline as the monitor log feed).
func (m *model) appendChatLine(l string) {
	m.chatLines = append(m.chatLines, l)
	if m.chatScroll > 0 {
		m.chatScroll++
	}
	if len(m.chatLines) > chatLineLimit {
		m.chatLines = m.chatLines[len(m.chatLines)-chatLineLimit:]
	}
	m.clampChatScroll()
}

func (m *model) clampChatScroll() {
	if max := len(m.chatLines) - 1; m.chatScroll > max {
		m.chatScroll = max
	}
	if m.chatScroll < 0 {
		m.chatScroll = 0
	}
}

// chatView renders the full-width conversation: transcript filling the body, an
// activity/status line, and the input prompt pinned at the bottom.
func (m model) chatView() string {
	var b strings.Builder
	b.WriteString("\n  " + stTitle.Render("CHAT — monitor LLM") +
		stMuted.Render("    session "+m.chatSession) + "\n\n")

	// Manual probe palette: pick a kind (the compose step reuses the normal input
	// line, so only the picker needs its own body).
	if m.chatProbePalette {
		b.WriteString("  " + stTitle.Render("run an external probe") +
			stMuted.Render("  (from this frontend = outside the lab)") + "\n\n")
		for i, s := range probeSpecs() {
			cursor, name := "   ", s.Name
			if i == m.chatPaletteCur {
				cursor, name = stTitle.Render(" ▶ "), stSel.Render(s.Name)
			}
			b.WriteString(cursor + padTrunc(name, 18) + stMuted.Render(firstSentence(s.Description)) + "\n")
		}
		b.WriteString("\n  " + m.chatStatus + "\n")
		return b.String()
	}

	used := 3

	// Bottom lines we must leave room for: status + input (+ scroll marker).
	reserved := 2
	sc := m.chatScroll
	if sc > 0 {
		reserved++
	}
	budget := m.bodyH() - used - reserved
	if budget < 1 {
		budget = 1
	}

	// Hide the newest `sc` logical lines (scrollback), soft-wrap the rest, and keep
	// the last `budget` physical rows so the tail hugs the input line.
	visible := m.chatLines
	if sc > 0 && sc < len(visible) {
		visible = visible[:len(visible)-sc]
	}
	// Live in-progress block: the streamed-but-not-yet-finalized reasoning/answer,
	// shown at the tail (a cursor ▌ marks it as still growing) so tokens appear as
	// the model emits them instead of landing all at once on finalize.
	live := visible
	if sc == 0 {
		if m.chatPendThink != "" {
			for _, l := range strings.Split(m.chatPendThink, "\n") {
				live = append(live, stMuted.Render("  ┆ "+l))
			}
		}
		if m.chatPendAns != "" {
			live = append(live, stTitle.Render("monitor ▶ ")+m.chatPendAns+stMuted.Render("▌"))
		}
	}

	wrapW := m.w - 4
	rows := make([]string, 0, len(live))
	for _, l := range live {
		for _, part := range strings.Split(l, "\n") {
			if wrapW > 0 {
				rows = append(rows, strings.Split(ansiWrap(part, wrapW), "\n")...)
			} else {
				rows = append(rows, part)
			}
		}
	}
	if len(rows) == 0 {
		rows = []string{stMuted.Render("(ask anything — the monitor answers with live fleet state via its read tools)")}
	}
	if len(rows) > budget {
		rows = rows[len(rows)-budget:]
	}
	for _, r := range rows {
		b.WriteString("  " + r + "\n")
	}
	for i := len(rows); i < budget; i++ {
		b.WriteString("\n")
	}

	if sc > 0 {
		b.WriteString("  " + stWarn.Render(fmt.Sprintf("── scrolled ↑%d — PgUp/PgDn scroll ──", sc)) + "\n")
	}
	status := m.chatStatus
	if m.chatBusy {
		if status == "" {
			// Before the first event: still contacting the monitor over the relay
			// (a persistent "contacting" with a climbing timer means the request is
			// in flight; a fast red error below means it failed to reach one).
			status = stMuted.Render("contacting the monitor …")
		}
		status += stMuted.Render(fmt.Sprintf("  %ds", m.chatElapsed))
	}
	b.WriteString("  " + status + "\n")
	b.WriteString("  " + m.chatInput.View() + "\n")
	return b.String()
}
