// Chat screen: an adaptation of katui's chat mode (cmd/katui/chat.go) with the
// transport swapped — instead of MonitorChatService over the control-plane relay,
// turns run in-process against the nodewatch ChatServer (SendTo/ResumeTo + EventSink),
// and LLM-proposed external probes are answered from the scenario instead of
// probe.Run. The interaction surface (streamed events, approval flow, manual probe
// palette, scrollback) is kept identical so the rented-GPU evaluation exercises the
// same UX as production. Deliberately duplicated from katui rather than shared: this
// is a temporary evaluation harness and the production TUI stays untouched.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wordwrap"
	"github.com/muesli/reflow/wrap"
	"github.com/on-keyday/kscale/nodewatch"
	"github.com/on-keyday/kscale/probe"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
)

// --- styles (the katui palette subset the chat screen uses) ---
var (
	cFocus  = lipgloss.Color("69")
	cOK     = lipgloss.Color("42")
	cErr    = lipgloss.Color("196")
	cMuted  = lipgloss.Color("245")
	stSel   = lipgloss.NewStyle().Foreground(cFocus).Bold(true)
	stMuted = lipgloss.NewStyle().Foreground(cMuted)
	stOK    = lipgloss.NewStyle().Foreground(cOK)
	stErr   = lipgloss.NewStyle().Foreground(cErr)
	stTitle = lipgloss.NewStyle().Foreground(cFocus).Bold(true)
	stWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
)

const chatLineLimit = 400 // transcript ring: plenty of scrollback, bounded memory
const chatLogPage = 10    // logical lines per PgUp/PgDn step

// model is the mock TUI: katui's chat-screen state without the other screens.
type model struct {
	ctx  context.Context
	chat *nodewatch.ChatServer // in-process agent (no relay)
	scen *Scenario
	head string // header line: scenario + model identification
	w, h int

	chatSession string   // client-chosen conversation key (history lives on the agent)
	chatLines   []string // transcript ring (logical lines, pre-styled)
	chatScroll  int      // logical lines hidden from the newest end (0 = tail)
	chatStatus  string   // activity line (running tool / thinking / errors)
	chatBusy    bool     // a turn is in flight (enter disabled, esc cancels the turn)
	chatInput   textinput.Model
	chatCh      chan chatEvMsg
	chatCancel  context.CancelFunc

	// Live in-progress buffers for the streaming turn.
	chatPendThink string
	chatPendAns   string
	chatElapsed   int // seconds the in-flight turn has been running (a liveness tick)

	// LLM-proposed probe approval state.
	chatAwaitProbe    bool
	chatProbeTool     string
	chatProbeArgs     string
	chatProbeProposed string // args as the model proposed them (detects an operator edit)
	chatProbeEdit     bool

	// Manual probe palette state.
	chatProbePalette  bool
	chatProbeCompose  bool
	chatProbeAsk      bool
	chatProbeQuestion string
	chatPaletteCur    int
}

func newModel(ctx context.Context, chat *nodewatch.ChatServer, scen *Scenario, head string) *model {
	m := &model{ctx: ctx, chat: chat, scen: scen, head: head, w: 100, h: 30}
	m.chatSession = newChatSessionID()
	m.restoreChatInput()
	return m
}

func (m *model) Init() tea.Cmd { return textinput.Blink }

// chatEvMsg carries one streamed ChatEvent of the in-flight turn, or its termination.
type chatEvMsg struct {
	ev   *pb.ChatEvent
	err  error
	done bool
}

// chatTickMsg drives the in-flight turn's elapsed-seconds counter, so the status
// line visibly advances while the model generates.
type chatTickMsg struct{}

func chatTickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return chatTickMsg{} })
}

// chatProbeDoneMsg carries a probe's (scenario-supplied) output back to the Update
// loop. manual distinguishes an operator-initiated probe (fed to the model as a new
// turn) from an LLM-proposed one (resumed into the suspended turn).
type chatProbeDoneMsg struct {
	tool   string
	args   string
	result string
	err    error
	manual bool
}

// runProbeCmd answers the probe from the scenario (the mock's external vantage).
func (m *model) runProbeCmd(tool, args string, manual bool) tea.Cmd {
	scen := m.scen
	return func() tea.Msg {
		out := scen.ProbeResult(tool, json.RawMessage(args))
		return chatProbeDoneMsg{tool: tool, args: args, result: out, manual: manual}
	}
}

// --- in-process turn transport (replaces katui's RPC streamChat) ---

// sinkFunc adapts a closure to the nodewatch.EventSink a turn streams into.
type sinkFunc func(*pb.ChatEvent) error

func (f sinkFunc) Send(ev *pb.ChatEvent) error { return f(ev) }

// streamChat drives one in-process (sub)turn in a goroutine, pumping its events into
// chatCh (channel → tea.Msg → re-arm, the katui pattern). run is SendTo or ResumeTo.
func (m *model) streamChat(run func(ctx context.Context, sink nodewatch.EventSink) error) tea.Cmd {
	sctx, cancel := context.WithCancel(m.ctx)
	m.chatCancel = cancel
	ch := make(chan chatEvMsg, 16)
	m.chatCh = ch
	go func() {
		err := run(sctx, sinkFunc(func(ev *pb.ChatEvent) error {
			if !trySend(ch, chatEvMsg{ev: ev}, sctx) {
				return context.Canceled // viewer cancelled the turn
			}
			return nil
		}))
		trySend(ch, chatEvMsg{err: err, done: true}, sctx)
	}()
	return waitChatEv(ch)
}

func (m *model) startChatTurn(text string) tea.Cmd {
	srv := m.chat
	dto := &pbaccess.ResourceMonitorChatActionSendArgsDTO{SessionId: m.chatSession, Message: text}
	return m.streamChat(func(ctx context.Context, sink nodewatch.EventSink) error {
		return srv.SendTo(ctx, dto, sink)
	})
}

func (m *model) startChatResume(tool, result, denied string) tea.Cmd {
	srv := m.chat
	dto := &pbaccess.ResourceMonitorChatActionResumeArgsDTO{
		SessionId: m.chatSession, Tool: tool, Result: result, Denied: denied,
	}
	return m.streamChat(func(ctx context.Context, sink nodewatch.EventSink) error {
		return srv.ResumeTo(ctx, dto, sink)
	})
}

func waitChatEv(ch chan chatEvMsg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

func trySend[T any](ch chan T, msg T, ctx context.Context) bool {
	select {
	case ch <- msg:
		return true
	case <-ctx.Done():
		return false
	}
}

func (m *model) stopChatTurn() {
	if m.chatCancel != nil {
		m.chatCancel()
		m.chatCancel = nil
	}
	m.chatCh = nil
}

// --- manual probe palette (operator-initiated) ---

func (m *model) openProbePalette() {
	m.chatProbePalette = true
	m.chatProbeCompose = false
	m.chatPaletteCur = 0
	m.chatStatus = stMuted.Render("pick a probe · ↑/↓ · enter · esc cancel")
}

func probeSpecs() []probe.Spec { return probe.Specs() }

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
	m.appendChatLine(stMuted.Render("  ▸ running " + tool + " (scenario) …"))
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
	sk := map[string]string{}
	for _, k := range keys {
		sk[k] = ""
	}
	b, _ := json.Marshal(sk)
	return string(b)
}

// composeProbeResult discloses an operator edit of the probe args to the model (its
// history holds its own proposed call; see katui chat.go for the observed failure
// this prevents). Unedited runs pass through verbatim.
func composeProbeResult(proposed, actual, result string) string {
	if proposed == actual {
		return result
	}
	return "NOTE: the operator edited the probe arguments before approving. The probe actually ran with " +
		actual + " — not the arguments you requested. Base your interpretation on these actual arguments.\n\n" + result
}

func (m *model) approveProbe() tea.Cmd {
	m.chatAwaitProbe = false
	m.chatProbeEdit = false
	m.restoreChatInput()
	m.chatStatus = stMuted.Render("running " + m.chatProbeTool + " (scenario) …")
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

func (m *model) denyProbe() tea.Cmd {
	tool := m.chatProbeTool
	m.chatAwaitProbe = false
	m.chatProbeEdit = false
	m.restoreChatInput()
	m.appendChatLine(stErr.Render("  ✗ denied " + tool))
	m.chatStatus = stMuted.Render("resuming …")
	return m.startChatResume(tool, "", "operator denied the probe")
}

func firstSentence(s string) string {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return s[:i+1]
	}
	return s
}

func compactArgs(args string) string {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil || len(parsed) == 0 {
		return strings.TrimSpace(args)
	}
	keys := make([]string, 0, len(parsed))
	for k := range parsed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, parsed[k]))
	}
	return strings.Join(parts, " ")
}

// newChatSessionID mints the client-chosen conversation key (a fresh id is a fresh
// history on the agent).
func newChatSessionID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (m *model) restoreChatInput() {
	ti := textinput.New()
	ti.Prompt = stTitle.Render("you ▶ ")
	ti.Placeholder = "ask the monitor about the fleet…"
	ti.Focus()
	m.chatInput = ti
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

// --- update loop ---

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil
	case chatEvMsg:
		if msg.done {
			if m.chatAwaitProbe {
				// The turn suspended for a probe; stay busy and keep the tick running —
				// it continues after approval.
				return m, nil
			}
			m.chatBusy = false
			m.chatStatus = ""
			m.commitPending() // flush any streamed text the turn ended before finalizing
			if msg.err != nil && !errors.Is(msg.err, context.Canceled) {
				m.appendChatLine(stErr.Render("✗ " + msg.err.Error()))
				m.appendChatLine("")
			}
			return m, nil
		}
		m.onChatEvent(msg.ev)
		return m, waitChatEv(m.chatCh)
	case chatProbeDoneMsg:
		// Show the probe output regardless of who initiated it.
		if msg.err != nil {
			m.appendChatLine(stErr.Render("  ✗ probe error: " + msg.err.Error()))
		} else {
			for _, l := range strings.Split(strings.TrimRight(msg.result, "\n"), "\n") {
				m.appendChatLine(stMuted.Render("  │ " + l))
			}
		}
		if msg.manual {
			// Operator-initiated: feed the result to the monitor as a new turn.
			res := msg.result
			if msg.err != nil {
				res = "the probe failed to run: " + msg.err.Error()
			}
			ask := m.chatProbeQuestion
			if ask == "" {
				ask = "Interpret this and correlate it with the fleet's internal state."
			}
			hidden := fmt.Sprintf("I ran an external probe myself:\n%s %s\nResult:\n%s\n\n%s",
				msg.tool, msg.args, res, ask)
			return m, m.beginTurn(hidden, stMuted.Render("  asking the monitor to interpret …"))
		}
		// LLM-proposed: resume the suspended turn, disclosing an operator edit.
		result := msg.result
		if msg.err != nil {
			result = "probe failed to run on the frontend: " + msg.err.Error()
		}
		result = composeProbeResult(m.chatProbeProposed, msg.args, result)
		m.chatStatus = stMuted.Render("resuming …")
		return m, m.startChatResume(msg.tool, result, "")
	case chatTickMsg:
		if !m.chatBusy {
			return m, nil
		}
		m.chatElapsed++
		return m, chatTickCmd()
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m *model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.String() == "ctrl+c" {
		m.stopChatTurn()
		return m, tea.Quit
	}
	// Manual probe palette: pick a kind, then edit args, then run.
	if m.chatProbePalette {
		switch k.String() {
		case "esc":
			m.chatProbePalette = false
			m.restoreChatInput()
			m.chatStatus = ""
			return m, nil
		case "up", "k":
			m.paletteMove(-1)
			return m, nil
		case "down", "j":
			m.paletteMove(1)
			return m, nil
		case "enter":
			return m, m.paletteSelect()
		}
		return m, nil
	}
	if m.chatProbeCompose {
		switch k.String() {
		case "esc":
			m.chatProbeCompose = false
			m.chatInput.SetValue("")
			m.openProbePalette()
			return m, nil
		case "enter":
			return m, m.composeToQuestion()
		default:
			var tc tea.Cmd
			m.chatInput, tc = m.chatInput.Update(k)
			return m, tc
		}
	}
	if m.chatProbeAsk {
		switch k.String() {
		case "esc":
			m.questionToArgs()
			return m, nil
		case "enter":
			return m, m.runManualProbe()
		default:
			var tc tea.Cmd
			m.chatInput, tc = m.chatInput.Update(k)
			return m, tc
		}
	}
	// Probe approval mode owns the keys while a probe is pending (a/e/d), unless
	// editing the args JSON (then the input owns them until enter/esc).
	if m.chatAwaitProbe && !m.chatProbeEdit {
		switch k.String() {
		case "a":
			return m, m.approveProbe()
		case "d":
			return m, m.denyProbe()
		case "e":
			m.chatProbeEdit = true
			ti := textinput.New()
			ti.Prompt = stTitle.Render("args ▶ ")
			ti.SetValue(m.chatProbeArgs)
			ti.CursorEnd()
			ti.Focus()
			m.chatInput = ti
			m.chatStatus = stWarn.Render("edit args JSON · enter approve · esc back")
			return m, textinput.Blink
		case "esc":
			return m, m.denyProbe()
		}
		return m, nil
	}
	if m.chatAwaitProbe && m.chatProbeEdit {
		switch k.String() {
		case "enter":
			m.chatProbeArgs = strings.TrimSpace(m.chatInput.Value())
			m.chatInput.SetValue("")
			return m, m.approveProbe()
		case "esc":
			m.chatProbeEdit = false
			m.chatInput.SetValue("")
			m.chatStatus = stWarn.Render("approve external probe? a approve · e edit · d deny")
			return m, nil
		default:
			var tc tea.Cmd
			m.chatInput, tc = m.chatInput.Update(k)
			return m, tc
		}
	}
	// The input owns almost every key; the exceptions are turn control and scrollback.
	switch k.String() {
	case "esc":
		if m.chatBusy {
			// Cancel the in-flight turn but stay on the screen. The agent-side
			// session keeps the pre-turn history (see nodewatch ChatServer).
			m.stopChatTurn()
			m.chatBusy = false
			m.chatStatus = ""
			m.commitPending()
			m.appendChatLine(stMuted.Render("— turn cancelled —"))
			m.appendChatLine("")
		}
		return m, nil
	case "ctrl+l":
		m.resetChat()
		return m, nil
	case "ctrl+p":
		if !m.chatBusy {
			m.openProbePalette()
		}
		return m, nil
	case "pgup":
		m.chatScroll += chatLogPage
		m.clampChatScroll()
		return m, nil
	case "pgdown":
		m.chatScroll -= chatLogPage
		m.clampChatScroll()
		return m, nil
	case "enter":
		if m.chatBusy {
			return m, nil // one turn at a time
		}
		text := strings.TrimSpace(m.chatInput.Value())
		if text == "" {
			return m, nil
		}
		m.chatInput.SetValue("")
		m.chatBusy = true
		m.chatElapsed = 0
		m.chatScroll = 0
		m.chatStatus = ""
		m.appendChatLine(stOK.Render("you ▶ ") + text)
		return m, tea.Batch(m.startChatTurn(text), chatTickCmd())
	default:
		var tc tea.Cmd
		m.chatInput, tc = m.chatInput.Update(k)
		return m, tc
	}
}

// onChatEvent folds one streamed event into the transcript + activity status.
func (m *model) onChatEvent(ev *pb.ChatEvent) {
	switch ev.Kind {
	case "thinking_delta":
		m.chatPendThink += ev.Text
		m.chatStatus = stMuted.Render("thinking …")
	case "model_call":
		m.chatStatus = stMuted.Render("monitor is querying the model …")
	case "tool_request":
		// The monitor proposes an external probe. Freeze any streamed text and enter
		// approval — the turn is suspended on the agent until we resume with a result.
		m.commitPending()
		m.chatAwaitProbe = true
		m.chatProbeTool = ev.Tool
		m.chatProbeArgs = ev.Args
		m.chatProbeProposed = ev.Args
		m.chatProbeEdit = false
		m.appendChatLine(stWarn.Render("  ⚑ monitor wants to run: ") + stTitle.Render(ev.Tool) + " " + stMuted.Render(compactArgs(ev.Args)))
		m.chatStatus = stWarn.Render("approve external probe? a approve · e edit · d deny")
	case "delta":
		m.chatPendAns += ev.Text
		m.chatStatus = stMuted.Render("answering …")
	case "thinking":
		m.chatPendThink = ""
		for _, l := range strings.Split(strings.TrimSpace(ev.Text), "\n") {
			m.appendChatLine(stMuted.Render("  ┆ " + l))
		}
		m.chatStatus = stMuted.Render("thinking …")
	case "tool_call":
		m.commitPending()
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

// commitPending flushes any in-progress streamed text to permanent transcript lines.
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

// --- view ---

func (m *model) View() string {
	var b strings.Builder
	b.WriteString("\n  " + stTitle.Render("CHAT — nodewatch mock") + stMuted.Render("    "+m.head) +
		stMuted.Render("    session "+m.chatSession) + "\n\n")

	// Manual probe palette: pick a kind.
	if m.chatProbePalette {
		b.WriteString("  " + stTitle.Render("run an external probe") +
			stMuted.Render("  (answered from the scenario)") + "\n\n")
		for i, s := range probeSpecs() {
			cursor, name := "   ", s.Name
			if i == m.chatPaletteCur {
				cursor, name = stTitle.Render(" ▶ "), stSel.Render(s.Name)
			}
			b.WriteString(cursor + padTrunc(name, 18) + stMuted.Render(firstSentence(s.Description)) + "\n")
		}
		b.WriteString("\n  " + m.chatStatus + "\n")
		b.WriteString("  " + stMuted.Render(m.hints()) + "\n")
		return b.String()
	}

	used := 3

	// Bottom lines to leave room for: status + input + hints (+ scroll marker).
	reserved := 3
	sc := m.chatScroll
	if sc > 0 {
		reserved++
	}
	budget := m.h - used - reserved
	if budget < 1 {
		budget = 1
	}

	visible := m.chatLines
	if sc > 0 && sc < len(visible) {
		visible = visible[:len(visible)-sc]
	}
	// Live in-progress block at the tail (cursor ▌ marks it as still growing).
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
		rows = []string{stMuted.Render("(ask anything — the monitor answers with the scenario's fleet state via its read tools)")}
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
			status = stMuted.Render("contacting the monitor …")
		}
		status += stMuted.Render(fmt.Sprintf("  %ds", m.chatElapsed))
	}
	b.WriteString("  " + status + "\n")
	b.WriteString("  " + m.chatInput.View() + "\n")
	b.WriteString("  " + stMuted.Render(m.hints()) + "\n")
	return b.String()
}

func (m *model) hints() string {
	switch {
	case m.chatProbePalette:
		return "↑/↓ pick probe · enter compose · esc cancel"
	case m.chatProbeCompose:
		return "edit args JSON · enter next · esc back to picker"
	case m.chatProbeAsk:
		return "optional question · enter run probe · esc back to args"
	case m.chatProbeEdit:
		return "edit probe args JSON · enter approve · esc back"
	case m.chatAwaitProbe:
		return stWarn.Render("external probe pending") + " · a approve · e edit · d deny · esc deny"
	case m.chatBusy:
		return "● turn in flight · esc cancel turn · PgUp/PgDn scroll · ctrl+c quit"
	}
	return "type · enter send · ctrl+p probe · ctrl+l new session · PgUp/PgDn scroll · ctrl+c quit"
}

// ansiWrap soft-wraps s to width w, ANSI-aware (word-wrap on spaces, then hard-wrap
// any still-too-long run), so styled content fits without splitting escape codes.
func ansiWrap(s string, w int) string {
	if w <= 0 {
		return s
	}
	return wrap.String(wordwrap.String(s, w), w)
}

func padTrunc(s string, w int) string {
	r := []rune(s)
	if len(r) > w {
		if w <= 1 {
			return string(r[:w])
		}
		return string(r[:w-1]) + "…"
	}
	return s + strings.Repeat(" ", w-len(r))
}
