// Command katui is the kscale admin TUI. It is driven entirely by the generated
// client.ResourceSpecs metadata (resources -> actions -> args) + client.
// DispatchResource — the same single source as the CLI and yaml-apply, so it has
// NO per-resource code: a new resource appears in the TUI for free.
//
// Layout (two panes, modeled on remote-agent-harness/tui): a persistent resource
// list on the left, a contextual right pane (the selected resource's actions ->
// an arg form -> the result), a bold header status line and a muted footer of
// context-sensitive key hints. Focus moves left<->right with enter/esc; the
// focused pane gets an accent border.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wordwrap"
	"github.com/muesli/reflow/wrap"
	"github.com/on-keyday/kscale/ca"
	"github.com/on-keyday/kscale/client"
	"github.com/on-keyday/kscale/internal/demo"
	"github.com/on-keyday/kscale/logbuf"
	"github.com/on-keyday/kscale/peer"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/kscale/protobuf/wire"
	"github.com/on-keyday/objtrsf/objproto"
)

// --- styles (palette + panels mirror the harness TUI) ---
var (
	cBorder = lipgloss.Color("241")
	cFocus  = lipgloss.Color("69")
	cOK     = lipgloss.Color("42")
	cErr    = lipgloss.Color("196")
	cMuted  = lipgloss.Color("245")

	stPanel  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cBorder).Padding(0, 1)
	stPanelF = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cFocus).Padding(0, 1)
	stHeader = lipgloss.NewStyle().Bold(true)
	stFooter = lipgloss.NewStyle().Foreground(cMuted)
	stSel    = lipgloss.NewStyle().Foreground(cFocus).Bold(true)
	stMuted  = lipgloss.NewStyle().Foreground(cMuted)
	stOK     = lipgloss.NewStyle().Foreground(cOK)
	stErr    = lipgloss.NewStyle().Foreground(cErr)
	stTitle  = lipgloss.NewStyle().Foreground(cFocus).Bold(true)
	stConnOK = lipgloss.NewStyle().Foreground(cOK).Bold(true)
	stWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true) // compose banner / confirm
	stSelRow = lipgloss.NewStyle().Foreground(lipgloss.Color("231")).Background(cFocus).Bold(true)

	// JSON syntax colors for the result pane.
	stJKey = lipgloss.NewStyle().Foreground(cFocus)
	stJStr = lipgloss.NewStyle().Foreground(cOK)
	stJNum = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	stJLit = lipgloss.NewStyle().Foreground(cMuted)
)

// colorizeJSON syntax-highlights an (already indented) JSON string: keys in the
// accent color, string values green, numbers amber, true/false/null muted. A
// char-level pass so it survives nested quotes; non-JSON text passes through.
func colorizeJSON(s string) string {
	r := []rune(s)
	var b strings.Builder
	for i := 0; i < len(r); {
		c := r[i]
		switch {
		case c == '"':
			j := i + 1
			for j < len(r) {
				if r[j] == '\\' {
					j += 2
					continue
				}
				if r[j] == '"' {
					break
				}
				j++
			}
			if j >= len(r) {
				j = len(r) - 1
			}
			tok := string(r[i : j+1])
			k := j + 1
			for k < len(r) && (r[k] == ' ' || r[k] == '\t') {
				k++
			}
			if k < len(r) && r[k] == ':' {
				b.WriteString(stJKey.Render(tok))
			} else {
				b.WriteString(stJStr.Render(tok))
			}
			i = j + 1
		case c == '-' || (c >= '0' && c <= '9'):
			j := i
			for j < len(r) && (r[j] == '-' || r[j] == '+' || r[j] == '.' || r[j] == 'e' || r[j] == 'E' || (r[j] >= '0' && r[j] <= '9')) {
				j++
			}
			b.WriteString(stJNum.Render(string(r[i:j])))
			i = j
		default:
			if lit := matchLit(r[i:]); lit != "" {
				b.WriteString(stJLit.Render(lit))
				i += len([]rune(lit))
			} else {
				b.WriteRune(c)
				i++
			}
		}
	}
	return b.String()
}

func matchLit(r []rune) string {
	for _, lit := range []string{"true", "false", "null"} {
		lr := []rune(lit)
		if len(r) >= len(lr) && string(r[:len(lr)]) == lit {
			return lit
		}
	}
	return ""
}

// prettyJSON renders JSON as an indented YAML-like tree (key/value, [i] for array
// elements), order-preserved via the token stream and colorized — far more legible
// than a raw brace/quote dump. Falls back to colorized raw JSON on a parse error.
func prettyJSON(s string) string {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return colorizeJSON(s)
	}
	var b strings.Builder
	if d, ok := tok.(json.Delim); ok {
		if err := renderBody(dec, d, 0, &b); err != nil {
			return colorizeJSON(s)
		}
	} else {
		b.WriteString(scalarTok(tok))
	}
	return strings.TrimLeft(b.String(), "\n")
}

// renderBody renders the body of a composite whose opening delim was already read.
func renderBody(dec *json.Decoder, open json.Delim, indent int, b *strings.Builder) error {
	pad := strings.Repeat("  ", indent)
	switch open {
	case '{':
		any := false
		for dec.More() {
			any = true
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			b.WriteString("\n" + pad + stJKey.Render(fmt.Sprint(keyTok)) + ":")
			if err := renderAfterLabel(dec, indent, b); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // closing '}'
			return err
		}
		if !any {
			b.WriteString(" " + stMuted.Render("{}"))
		}
	case '[':
		any := false
		for i := 0; dec.More(); i++ {
			any = true
			b.WriteString("\n" + pad + stMuted.Render(fmt.Sprintf("[%d]", i)) + ":")
			if err := renderAfterLabel(dec, indent, b); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // closing ']'
			return err
		}
		if !any {
			b.WriteString(" " + stMuted.Render("[]"))
		}
	}
	return nil
}

// renderAfterLabel renders the value following a "key:" / "[i]:" label: scalars
// inline, composites nested one level deeper.
func renderAfterLabel(dec *json.Decoder, indent int, b *strings.Builder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); ok {
		return renderBody(dec, d, indent+1, b)
	}
	b.WriteString(" " + scalarTok(tok))
	return nil
}

func scalarTok(t json.Token) string {
	switch v := t.(type) {
	case string:
		if v == "" {
			return stMuted.Render(`""`)
		}
		return stJStr.Render(v)
	case json.Number:
		return stJNum.Render(v.String())
	case bool:
		return stJLit.Render(fmt.Sprint(v))
	case nil:
		return stJLit.Render("null")
	}
	return fmt.Sprint(t)
}

type stage int

const (
	stgMode      stage = iota // entry screen: choose Live apply vs Document mode
	stgResources              // focus left: resource list (live apply)
	stgActions                // focus right: action list
	stgForm                   // focus right: arg form
	stgResult                 // focus right: result
	stgDoc                    // focus right: accumulated yaml-apply document
	stgSave                   // focus right: save-path input for the document
	stgLoad                   // focus right: load-path input (read a yaml file into the doc)
	stgConfirm                // focus right: confirm a live mutating action (apply/delete)
	stgMonitor                // full-width live fleet dashboard (its own mode)
	stgChat                   // full-width chat with the monitor agent's LLM — see chat.go
)

type resultMsg struct {
	out string
	err error
}

// streamMsg carries one item of a live server-streaming action (watch/logs), or
// its termination (done).
type streamMsg struct {
	line string
	done bool
	err  error
}

// dumpMsg carries the live desired-state dump (a yaml-apply document) fetched from the
// CP for the Document mode, or the error from fetching it.
type dumpMsg struct {
	doc string
	err error
}

// disconnectedMsg fires when the current peer's lifecycle context ends (liveness
// timeout, CP restart, network loss); the reconnect loop then redials until it
// succeeds. reconnectedMsg / reconnectFailedMsg carry one attempt's outcome;
// retryReconnectMsg is the tick between attempts.
type disconnectedMsg struct{}
type reconnectedMsg struct {
	p   *peer.Peer
	src *wire.StreamSource
}
type reconnectFailedMsg struct{ err error }
type retryReconnectMsg struct{}

// watchConn arms the disconnect watcher on the current peer: it blocks on the
// peer's Done channel and reports the loss to the Update loop. Re-armed on every
// successful (re)connect.
func watchConn(p *peer.Peer) tea.Cmd {
	return func() tea.Msg {
		<-p.Done()
		return disconnectedMsg{}
	}
}

// redialCmd runs one reconnect attempt. The handshake self-bounds (~5s receive
// timeout inside ca.HandshakeProtocol), so the long-lived ctx inside redial is
// safe — a per-attempt timeout ctx would kill the freshly-wrapped peer instead.
func (m model) redialCmd() tea.Cmd {
	redial := m.redial
	return func() tea.Msg {
		p, src, err := redial()
		if err != nil {
			return reconnectFailedMsg{err: err}
		}
		return reconnectedMsg{p: p, src: src}
	}
}

type model struct {
	ctx context.Context
	src *wire.StreamSource
	// p is the live connection, watched by watchConn; redial re-establishes it
	// (fresh connection id, reusing the startup enrollment) when it dies.
	p             *peer.Peer
	redial        func() (*peer.Peer, *wire.StreamSource, error)
	reconnecting  bool
	reconnAttempt int
	reconnErr     string // last redial error, shown in the header while reconnecting
	// dialShell opens a NEW short-lived admin connection for a Monitor remote shell.
	// The remote_shell ABAC policy gates on $user.login_time.since < 10s and the CP
	// captures login_time per connection auth, so the long-lived admin peer that drives
	// the monitor is always past that window — a shell must ride a freshly-authenticated
	// connection (opened here, closed when the shell exits). See openShell.
	dialShell func() (*peer.Peer, error)
	role      string
	addr      string
	w, h      int

	stage   stage
	modeCur int // stgMode selection: 0 = Live apply, 1 = Document
	resCur  int
	curRes  client.ResourceSpec
	actCur  int
	curAct  client.ActionSpec
	inputs  []textinput.Model
	formCur int

	result    string
	resultErr bool
	running   bool
	vp        viewport.Model

	// yaml-apply document builder (Document mode): apply-form entries are added with the
	// form's enter, viewed/managed in stgDoc (its own viewport so it doesn't clobber the
	// result pane) and written out in stgSave. docMsg is a transient status.
	doc      []string
	docCur   int // selected entry in the document (stgDoc), for delete
	docMsg   string
	savePath textinput.Model

	// compose: Document mode is active (chosen on the stgMode entry screen). In it,
	// selecting a resource jumps straight to the apply form and enter ADDS the entry to
	// the document (never a live apply). pendingArgs holds a live mutating action's args
	// while stgConfirm asks for confirmation.
	compose     bool
	pendingArgs map[string]string
	pendingDoc  bool // stgConfirm is confirming a whole-document live apply
	editIdx     int  // doc entry being edited via the form (-1 = adding a new one)

	// live streaming (watch/logs)
	streaming    bool // a stream is actively running (footer "● live", re-arm)
	streamResult bool // result holds pre-styled stream output — render it raw, do
	// NOT re-run prettyJSON/colorizeJSON over it (stays true after the stream ends)
	streamCh     chan streamMsg
	streamCancel context.CancelFunc

	// monitor mode (live fleet dashboard) — see monitor.go
	monRows      map[string]*fleetRow
	monOrder     []string
	monCur       int // selected fleet row (for start/stop)
	monStatCh    chan monStatsMsg
	monLogCh     chan monLogMsg
	monLog       []string // tailing CP log lines (ring)
	monLogScroll int      // logical lines hidden from the newest end (0 = live tail at bottom)
	monAlerts    []*pbaccess.ResourceAlertActionGetResponseDTO
	monStatus    string // last start/stop action result/error
	monCancel    context.CancelFunc
	monErr       string

	// shell prompt (Monitor "e"): an inline input for the command to run on the
	// selected node, pre-filled with "sh". While shellPrompt is set, monitor keys are
	// routed to shellInput; enter opens the shell with the typed command, esc cancels.
	shellPrompt bool
	shellInput  textinput.Model

	// chat mode (conversation with the monitor LLM) — see chat.go
	chatSession string   // client-chosen conversation key (history lives on the agent)
	chatLines   []string // transcript ring (logical lines, pre-styled)
	chatScroll  int      // logical lines hidden from the newest end (0 = tail)
	chatStatus  string   // activity line (running tool / thinking / errors)
	chatBusy    bool     // a turn is in flight (enter disabled, esc cancels the turn)
	chatInput   textinput.Model
	chatCh      chan chatEvMsg
	chatCancel  context.CancelFunc
	// Live token-streaming buffers: thinking_delta / delta fragments accumulate here
	// and render as an in-progress block below the committed transcript; the full
	// thinking / assistant events commit them as permanent lines and clear these.
	chatPendThink string
	chatPendAns   string
	chatElapsed   int // seconds the in-flight turn has been running (a liveness tick)
	// Client-side probe approval: when the monitor proposes an external probe
	// (tool_request), the turn suspends and these hold the pending proposal until
	// the operator approves / edits / denies it. chatProbeEdit routes input to
	// editing the args JSON.
	chatAwaitProbe bool
	chatProbeTool  string
	chatProbeArgs  string
	// chatProbeProposed is the args as the model proposed them (chatProbeArgs
	// mutates on operator edit); composeProbeResult compares the two at resume.
	chatProbeProposed string
	chatProbeEdit     bool
	// Manual probe palette: the operator composes and runs a probe themselves
	// (p key), independent of an LLM proposal; its result is fed to the monitor to
	// interpret. chatProbePalette = choosing a kind; chatProbeCompose = editing args.
	chatProbePalette  bool
	chatProbeCompose  bool
	chatProbeAsk      bool   // entering the optional question that accompanies the result
	chatProbeQuestion string // operator's question (empty = the generic interpret prompt)
	chatPaletteCur    int
}

func (m model) Init() tea.Cmd {
	if m.p == nil { // tests construct the model without a live connection
		return nil
	}
	return watchConn(m.p)
}

func (m *model) leftW() int {
	w := 22
	if m.w > 0 && w > m.w/3 {
		w = m.w / 3
	}
	return w
}
func (m *model) bodyH() int {
	h := m.h - 4 // header + footer + 2 gaps
	if h < 4 {
		h = 4
	}
	return h
}
func (m *model) rightW() int {
	w := m.w - m.leftW() - 6 // both panel borders + gap
	if w < 10 {
		w = 10
	}
	return w
}

func (m model) dispatchCmd(args map[string]string) tea.Cmd {
	ctx, src := m.ctx, m.src
	res, act := m.curRes.Command, m.curAct.Command
	return func() tea.Msg {
		out, err := client.DispatchResource(ctx, src, res, act, args)
		return resultMsg{out: out, err: err}
	}
}

// applyDocCmd applies the whole yaml-apply document to the live CP via
// client.ApplyConfig, returning a per-entry ✓/✗ summary (ApplyConfig halts at the first
// failure, which is appended).
func (m model) applyDocCmd() tea.Cmd {
	ctx, src := m.ctx, m.src
	data := []byte(strings.Join(m.doc, ""))
	n := len(m.doc)
	return func() tea.Msg {
		results, err := client.ApplyConfig(ctx, src, data)
		var b strings.Builder
		for _, r := range results {
			if r.Err != nil {
				b.WriteString("✗ " + r.Resource + ": " + r.Err.Error() + "\n")
			} else {
				b.WriteString("✓ " + r.Resource + "\n")
			}
		}
		if err != nil {
			b.WriteString("\nstopped: " + err.Error())
		} else {
			b.WriteString(fmt.Sprintf("\napplied %d/%d entries", len(results), n))
		}
		return resultMsg{out: b.String(), err: nil}
	}
}

func (m model) startAction() (model, tea.Cmd) {
	m.editIdx = -1 // a normal action is not an in-place doc edit
	m.curAct = m.curRes.Actions[m.actCur]
	if len(m.curAct.Args) == 0 {
		return m.run(map[string]string{})
	}
	m.inputs = make([]textinput.Model, len(m.curAct.Args))
	for i, a := range m.curAct.Args {
		ti := textinput.New()
		ti.Placeholder = client.ArgTypeHint(a.Type)
		ti.SetValue(a.Default)
		ti.Prompt = "  "
		if i == 0 {
			ti.Focus()
		}
		m.inputs[i] = ti
	}
	m.formCur = 0
	m.stage = stgForm
	return m, nil
}

// collectArgs gathers the filled form fields, skipping blank ones so they're treated as
// unset — matching the CLI, which only sends explicitly-set flags. The dispatch then
// applies each arg's declared default, and the typed binders never receive "" (which
// strconv for int/duration would reject).
func (m model) collectArgs() map[string]string {
	args := map[string]string{}
	for i, a := range m.curAct.Args {
		if strings.TrimSpace(m.inputs[i].Value()) == "" {
			continue
		}
		args[a.Name] = m.inputs[i].Value()
	}
	return args
}

func (m model) submit() (model, tea.Cmd) {
	return m.run(m.collectArgs())
}

// needsConfirm reports whether a live action mutates CP state and so should be
// confirmed before running (read-only ops run immediately).
func needsConfirm(cmd string) bool {
	return cmd == "apply" || cmd == "delete"
}

// applyActionIdx is the index of a resource's "apply" action, or -1 if it has none.
func applyActionIdx(r client.ResourceSpec) int {
	for i, a := range r.Actions {
		if a.Command == "apply" {
			return i
		}
	}
	return -1
}

// visibleResources is the left-pane list: every resource normally, or only the
// declarative (apply-able) ones in compose mode (a doc only holds applies).
func (m model) visibleResources() []client.ResourceSpec {
	if !m.compose {
		return client.ResourceSpecs
	}
	var out []client.ResourceSpec
	for _, r := range client.ResourceSpecs {
		if applyActionIdx(r) >= 0 {
			out = append(out, r)
		}
	}
	return out
}

// run executes the selected action: a server-streaming action (watch/logs) opens a
// live stream into the viewport; a unary action returns a single result.
func (m model) run(args map[string]string) (model, tea.Cmd) {
	if m.curAct.Stream {
		return m.startStream(args)
	}
	m.running = true
	m.streamResult = false
	m.result = "running…"
	m.setResult()
	m.stage = stgResult
	return m, m.dispatchCmd(args)
}

func (m model) startStream(args map[string]string) (model, tea.Cmd) {
	sctx, cancel := context.WithCancel(m.ctx)
	ch := make(chan streamMsg, 64)
	src, res, act := m.src, m.curRes.Command, m.curAct.Command
	go func() {
		err := client.StreamResource(sctx, src, res, act, args, func(line string) error {
			select {
			case ch <- streamMsg{line: line}:
				return nil
			case <-sctx.Done():
				return sctx.Err()
			}
		})
		select {
		case ch <- streamMsg{done: true, err: err}:
		case <-sctx.Done():
		}
	}()
	m.streamCancel = cancel
	m.streamCh = ch
	m.streaming = true
	m.streamResult = true
	m.result = stMuted.Render("● streaming "+res+" "+act+" … (esc to stop)") + "\n\n"
	m.setResult()
	m.stage = stgResult
	return m, waitStream(ch)
}

func waitStream(ch chan streamMsg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

func (m *model) stopStream() {
	if m.streamCancel != nil {
		m.streamCancel()
		m.streamCancel = nil
	}
	m.streaming = false
}

func (m *model) setResult() {
	m.vp.Width = m.rightW() - 2 // panel Padding(0,1) — keep vp content within the
	// panel's inner width so its ANSI lines aren't re-wrapped (which splits escapes)
	m.vp.Height = m.bodyH() - 2
	body := m.result
	switch {
	case m.streamResult:
		// pre-styled accumulated stream output (header + each item already
		// colorized) — render raw; re-running prettyJSON/colorizeJSON over
		// existing ANSI would mangle the escape sequences.
	case m.running:
		body = stMuted.Render(m.result)
	case m.resultErr:
		body = stErr.Render(m.result)
	case m.result == "OK":
		body = stOK.Render("OK")
	default:
		body = prettyJSON(m.result)
	}
	if !m.streamResult {
		// Soft-wrap to the pane width (ANSI-aware) so a long line — e.g. a long
		// reconcile / apply error — shows in full instead of being truncated at the
		// right edge. (Stream output is pre-styled; leave it raw.)
		body = ansiWrap(body, m.vp.Width)
	}
	m.vp.SetContent(body)
}

// ansiWrap soft-wraps s to width w, ANSI-aware (word-wrap on spaces, then hard-wrap any
// still-too-long run), so styled content fits the pane without splitting escape codes.
func ansiWrap(s string, w int) string {
	if w <= 0 {
		return s
	}
	return wrap.String(wordwrap.String(s, w), w)
}

// setDoc keeps the selected-entry cursor in range (called when opening the doc and
// after edits).
func (m *model) setDoc() {
	if m.docCur >= len(m.doc) {
		m.docCur = len(m.doc) - 1
	}
	if m.docCur < 0 {
		m.docCur = 0
	}
}

// docLabel is the one-line list label for a document entry: its resource name plus the
// value of its first field (the key — node / name / vip / …), so per-node entries of
// the same resource (e.g. mtu for s1, s2, s3) are distinguishable and individually
// deletable.
func docLabel(entry string) string {
	res, key := "entry", ""
	for _, line := range strings.Split(entry, "\n") {
		t := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(t, "- resource:"); ok {
			res = strings.TrimSpace(rest)
		} else if key == "" {
			if _, v, ok := strings.Cut(t, ":"); ok {
				key = strings.Trim(strings.TrimSpace(v), `"`)
			}
		}
	}
	if key != "" {
		return res + "  " + key
	}
	return res
}

// addToDoc appends the current apply form (resource + filled fields) to the yaml-apply
// document as one entry, formatted so client.ApplyConfig re-parses it.
func (m *model) addToDoc() {
	args := m.collectArgs()
	entry := client.ApplyEntryYAML(m.curRes.Command, args)
	if entry == "" {
		return
	}
	m.doc = append(m.doc, entry)
	m.docMsg = fmt.Sprintf("added %s — %d in doc", m.curRes.Command, len(m.doc))
}

// replaceEdited writes the current form back over the entry at editIdx (set by
// editEntry), then clears the edit marker and refreshes the doc selection.
func (m *model) replaceEdited() {
	if e := client.ApplyEntryYAML(m.curRes.Command, m.collectArgs()); e != "" && m.editIdx >= 0 && m.editIdx < len(m.doc) {
		m.doc[m.editIdx] = e
		m.docMsg = "updated " + m.curRes.Command
	}
	m.editIdx = -1
	m.setDoc()
}

// saveDoc writes the accumulated document to the save-path as a yaml-apply file.
func (m *model) saveDoc() {
	path := strings.TrimSpace(m.savePath.Value())
	if path == "" {
		path = "kscale_apply.yaml"
	}
	content := "# kscale yaml-apply — authored in katui\n" + strings.Join(m.doc, "")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		m.docMsg = "save failed: " + err.Error()
		return
	}
	abs, _ := filepath.Abs(path)
	m.docMsg = fmt.Sprintf("saved %d entries → %s", len(m.doc), abs)
}

// loadDoc reads a yaml-apply file and appends its (normalized, re-validated) entries to
// the document.
func (m *model) loadDoc() {
	path := strings.TrimSpace(m.savePath.Value())
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		m.docMsg = "load failed: " + err.Error()
		return
	}
	parsed, err := client.ParseApplyEntries(data)
	if err != nil {
		m.docMsg = "load failed: " + err.Error()
		return
	}
	n := 0
	for _, p := range parsed {
		if e := client.ApplyEntryYAML(p.Resource, p.Args); e != "" {
			m.doc = append(m.doc, e)
			n++
		}
	}
	m.docMsg = fmt.Sprintf("loaded %d entries", n)
}

// dumpDesiredCmd fetches the live desired state of every declarative resource from the
// CP as a yaml-apply document (the same client.DumpDesired the cli's --dump uses). Async
// because it does a list RPC per resource; the result arrives as a dumpMsg.
func (m model) dumpDesiredCmd() tea.Cmd {
	ctx, src := m.ctx, m.src
	return func() tea.Msg {
		out, err := client.DumpDesired(ctx, src)
		return dumpMsg{doc: out, err: err}
	}
}

// loadDumped parses a fetched yaml-apply dump and appends its (normalized, re-validated)
// entries to the document — the live-CP counterpart of loadDoc's file read. Sensitive
// fields are already blanked by DumpDesired (sourced from List), so nothing secret lands
// in the doc.
func (m *model) loadDumped(doc string) {
	parsed, err := client.ParseApplyEntries([]byte(doc))
	if err != nil {
		m.docMsg = "dump failed: " + err.Error()
		return
	}
	n := 0
	for _, p := range parsed {
		if e := client.ApplyEntryYAML(p.Resource, p.Args); e != "" {
			m.doc = append(m.doc, e)
			n++
		}
	}
	m.docMsg = fmt.Sprintf("dumped %d entries from live CP", n)
}

// resourceByCmd finds the ResourceSpec with the given command_name.
func resourceByCmd(cmd string) (client.ResourceSpec, bool) {
	for _, r := range client.ResourceSpecs {
		if r.Command == cmd {
			return r, true
		}
	}
	return client.ResourceSpec{}, false
}

// editEntry re-opens the selected document entry in the apply form, pre-filled, so its
// fields can be changed; submitting then REPLACES that entry (editIdx) rather than
// appending a new one.
func (m model) editEntry() (model, tea.Cmd) {
	if m.docCur >= len(m.doc) {
		return m, nil
	}
	parsed, err := client.ParseApplyEntries([]byte(m.doc[m.docCur]))
	if err != nil || len(parsed) == 0 {
		m.docMsg = "can't edit this entry"
		return m, nil
	}
	p := parsed[0]
	spec, ok := resourceByCmd(p.Resource)
	if !ok {
		m.docMsg = "unknown resource " + p.Resource
		return m, nil
	}
	ai := applyActionIdx(spec)
	if ai < 0 {
		return m, nil
	}
	m.curRes = spec
	m.actCur = ai
	m.curAct = spec.Actions[ai]
	m.inputs = make([]textinput.Model, len(m.curAct.Args))
	for i, a := range m.curAct.Args {
		ti := textinput.New()
		ti.Placeholder = client.ArgTypeHint(a.Type)
		ti.Prompt = "  "
		ti.SetValue(p.Args[a.Name])
		if i == 0 {
			ti.Focus()
		}
		m.inputs[i] = ti
	}
	m.formCur = 0
	m.editIdx = m.docCur
	m.stage = stgForm
	return m, nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case disconnectedMsg:
		if m.reconnecting { // already redialing; a late watcher firing must not reset the count
			return m, nil
		}
		m.reconnecting, m.reconnAttempt, m.reconnErr = true, 1, ""
		return m, m.redialCmd()
	case reconnectFailedMsg:
		if !m.reconnecting {
			return m, nil
		}
		m.reconnAttempt++
		m.reconnErr = msg.err.Error()
		// The attempt itself blocks up to ~5s when the CP is down; a short pause
		// between attempts is enough (no exponential backoff needed on a UDP dial).
		return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg { return retryReconnectMsg{} })
	case retryReconnectMsg:
		if !m.reconnecting {
			return m, nil
		}
		return m, m.redialCmd()
	case reconnectedMsg:
		if m.p != nil { // release the dead connection's endpoint state
			_ = m.p.Connection().Close()
		}
		m.p, m.src = msg.p, msg.src
		m.reconnecting, m.reconnAttempt, m.reconnErr = false, 0, ""
		cmds := []tea.Cmd{watchConn(m.p)}
		// The monitor dashboard's streams died with the old connection; if the
		// operator is sitting in it, restart them on the new one. Other live
		// streams (watch/logs, chat) surface their error and are re-run by hand.
		if m.stage == stgMonitor {
			m.stopMonitor()
			cmds = append(cmds, m.startMonitor())
		}
		return m, tea.Batch(cmds...)
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.vp.Width = m.rightW() - 2 // panel Padding(0,1) — keep vp content within the
		// panel's inner width so its ANSI lines aren't re-wrapped (which splits escapes)
		m.vp.Height = m.bodyH() - 2
		m.setResult()
		return m, nil
	case resultMsg:
		m.running = false
		if msg.err != nil {
			m.result, m.resultErr = "ERROR\n\n"+msg.err.Error(), true
		} else if msg.out == "" {
			m.result, m.resultErr = "OK", false
		} else {
			m.result, m.resultErr = msg.out, false
		}
		m.setResult()
		return m, nil
	case streamMsg:
		if msg.done {
			m.streaming = false
			end := "\n" + stMuted.Render("— stream ended —")
			if msg.err != nil && !errors.Is(msg.err, context.Canceled) {
				end = "\n" + stErr.Render("— stream error: "+msg.err.Error()+" —")
			}
			m.result += end
			m.setResult()
			m.vp.GotoBottom()
			return m, nil
		}
		if !m.streaming { // stopped/navigated away — drop late items, don't re-arm
			return m, nil
		}
		m.result += prettyJSON(msg.line) + "\n"
		m.setResult()
		m.vp.GotoBottom()
		return m, waitStream(m.streamCh)
	case monStatsMsg:
		if m.stage != stgMonitor {
			return m, nil // left the dashboard; drop late items, don't re-arm
		}
		if msg.err != nil || msg.done {
			if msg.err != nil {
				m.monErr = "stats stream: " + msg.err.Error()
			}
			return m, nil
		}
		m.onMonStats(msg)
		return m, waitMonStats(m.monStatCh)
	case monNodesMsg:
		if m.stage != stgMonitor {
			return m, nil
		}
		m.onMonNodes(msg)
		return m, nil
	case monTickMsg:
		if m.stage != stgMonitor {
			return m, nil // stop ticking once we leave
		}
		return m, tea.Batch(m.fetchMonNodes(), m.fetchMonAlerts(), monTickCmd())
	case monAlertsMsg:
		if m.stage != stgMonitor {
			return m, nil
		}
		if msg.err == nil {
			m.monAlerts = msg.alerts
		}
		return m, nil
	case monLogBatchMsg:
		if m.stage != stgMonitor {
			return m, nil
		}
		for _, lm := range msg.msgs {
			// done = one of the two streams (cp / nodes) ended; keep reading for
			// the other.
			if !lm.done {
				m.onMonLog(lm)
			}
		}
		return m, waitMonLog(m.monLogCh)
	case monActionMsg:
		if msg.err != nil {
			m.monStatus = stErr.Render("✗ " + msg.err.Error())
		} else {
			out := msg.out
			if out == "" {
				out = "OK"
			}
			m.monStatus = stOK.Render("✓ " + strings.ReplaceAll(out, "\n", " "))
		}
		return m, nil
	case execReadyMsg:
		if msg.err != nil {
			m.monStatus = stErr.Render("✗ shell: " + msg.err.Error())
			return m, nil
		}
		// Hand the terminal to the PTY shell: tea.Exec suspends bubbletea (leaves the
		// alt screen, restores cooked mode), runs RemoteShell, then restores the TUI.
		// The Monitor's background streams flow-control-pause while suspended and
		// resume from their queued msgs once the shell exits.
		m.monStatus = stMuted.Render("shell on " + msg.node + " — exit to return")
		return m, tea.Exec(&interactiveExec{ces: msg.ces, conn: msg.conn}, func(err error) tea.Msg {
			return execDoneMsg{node: msg.node, err: err}
		})
	case execDoneMsg:
		if msg.err != nil && !errors.Is(msg.err, context.Canceled) {
			m.monStatus = stErr.Render("✗ shell on " + msg.node + " ended: " + msg.err.Error())
		} else {
			m.monStatus = stOK.Render("✓ shell on " + msg.node + " closed")
		}
		return m, nil
	case chatEvMsg:
		if m.stage != stgChat {
			return m, nil // left the chat screen; drop late items, don't re-arm
		}
		if msg.done {
			if m.chatAwaitProbe {
				// The stream ended because the turn suspended for a probe; stay busy
				// and keep the tick running — the turn continues after approval.
				return m, nil
			}
			m.chatBusy = false
			m.chatStatus = ""
			m.commitPending() // flush any streamed text the stream ended before finalizing
			if msg.err != nil && !errors.Is(msg.err, context.Canceled) {
				m.appendChatLine(stErr.Render("✗ " + msg.err.Error()))
				m.appendChatLine("")
			}
			return m, nil
		}
		m.onChatEvent(msg.ev)
		return m, waitChatEv(m.chatCh)
	case chatProbeDoneMsg:
		if m.stage != stgChat {
			return m, nil
		}
		// Show the probe output regardless of who initiated it.
		if msg.err != nil {
			m.appendChatLine(stErr.Render("  ✗ probe error: " + msg.err.Error()))
		} else {
			for _, l := range strings.Split(strings.TrimRight(msg.result, "\n"), "\n") {
				m.appendChatLine(stMuted.Render("  │ " + l))
			}
		}
		if msg.manual {
			// Operator-initiated: feed the result to the monitor as a new turn so it
			// interprets and correlates with internal state (there is no suspended
			// turn to resume).
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
		// LLM-proposed: resume the suspended turn with the result, disclosing an
		// operator edit of the args so the model reasons from what actually ran.
		result := msg.result
		if msg.err != nil {
			result = "probe failed to run on the frontend: " + msg.err.Error()
		}
		result = composeProbeResult(m.chatProbeProposed, msg.args, result)
		m.chatStatus = stMuted.Render("resuming …")
		return m, m.startChatResume(msg.tool, result, "")
	case chatTickMsg:
		if m.stage != stgChat || !m.chatBusy {
			return m, nil // turn ended / left the screen — stop ticking
		}
		m.chatElapsed++
		return m, chatTickCmd()
	case dumpMsg:
		if msg.err != nil {
			m.docMsg = "dump failed: " + msg.err.Error()
		} else {
			m.loadDumped(msg.doc)
		}
		m.setDoc()
		return m, nil
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.String() == "ctrl+c" {
		m.stopStream()
		m.stopMonitor()
		m.stopChatTurn()
		return m, tea.Quit
	}
	switch m.stage {
	case stgMonitor:
		// While the shell-command prompt is open, route keys to it: enter opens the
		// shell with the typed command, esc cancels, everything else edits the input.
		if m.shellPrompt {
			switch k.String() {
			case "esc":
				m.shellPrompt = false
				m.monStatus = ""
				return m, nil
			case "enter":
				cmd := strings.TrimSpace(m.shellInput.Value())
				m.shellPrompt = false
				if m.monCur >= 0 && m.monCur < len(m.monOrder) {
					m.monStatus = stMuted.Render("opening shell on " + m.monRows[m.monOrder[m.monCur]].node + " …")
					return m, m.openShell(cmd)
				}
				return m, nil
			default:
				var tc tea.Cmd
				m.shellInput, tc = m.shellInput.Update(k)
				return m, tc
			}
		}
		switch k.String() {
		case "q":
			m.stopMonitor()
			return m, tea.Quit
		case "esc", "left", "h":
			m.stopMonitor()
			m.stage = stgMode
		case "up", "k":
			if m.monCur > 0 {
				m.monCur--
			}
		case "down", "j":
			if m.monCur < len(m.monOrder)-1 {
				m.monCur++
			}
		case "s": // start the selected node
			if m.monCur >= 0 && m.monCur < len(m.monOrder) {
				m.monStatus = stMuted.Render("starting " + m.monRows[m.monOrder[m.monCur]].node + " …")
				return m, m.startStopSelected(true)
			}
		case "x": // stop the selected node
			if m.monCur >= 0 && m.monCur < len(m.monOrder) {
				m.monStatus = stMuted.Render("stopping " + m.monRows[m.monOrder[m.monCur]].node + " …")
				return m, m.startStopSelected(false)
			}
		case "e": // prompt for a command, then open an interactive shell on the selected node
			if m.monCur >= 0 && m.monCur < len(m.monOrder) {
				ti := textinput.New()
				ti.Prompt = "  "
				ti.SetValue("sh")
				ti.CursorEnd()
				ti.Focus()
				m.shellInput = ti
				m.shellPrompt = true
				m.monStatus = ""
			}
		case "pgup", "b": // scroll the log feed back toward older lines
			m.monLogScroll += monLogPage
			m.clampMonLogScroll()
		case "pgdown", "f": // scroll the log feed forward toward newer lines
			m.monLogScroll -= monLogPage
			m.clampMonLogScroll()
		case "G": // jump the log feed back to the live tail
			m.monLogScroll = 0
		}
	case stgChat:
		// Manual probe palette: pick a kind, then edit args, then run.
		if m.chatProbePalette {
			switch k.String() {
			case "esc":
				m.chatProbePalette = false
				m.restoreChatInput() // may still hold an args editor from compose->esc
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
				m.openProbePalette() // back to the kind picker
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
				m.questionToArgs() // back to editing args
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
				// Deny-and-stay: cancel the probe request without approving.
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
		// The input owns almost every key (it's a text field); the exceptions are
		// turn control and scrollback.
		switch k.String() {
		case "esc":
			if m.chatBusy {
				// Cancel the in-flight turn but stay on the screen. The agent-side
				// session keeps the pre-turn history (see nodewatch ChatServer).
				m.stopChatTurn()
				m.chatBusy = false
				m.chatStatus = ""
				m.commitPending() // keep whatever streamed before the cancel
				m.appendChatLine(stMuted.Render("— turn cancelled —"))
				m.appendChatLine("")
				return m, nil
			}
			m.stage = stgMode
			return m, nil
		case "ctrl+l":
			m.resetChat()
			return m, nil
		case "ctrl+p":
			// Manually compose and run an external probe (fed to the monitor to
			// interpret). ctrl+p so plain 'p' still types into the message input.
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
	case stgMode:
		switch k.String() {
		case "q":
			return m, tea.Quit
		case "up", "k":
			if m.modeCur > 0 {
				m.modeCur--
			}
		case "down", "j":
			if m.modeCur < 3 {
				m.modeCur++
			}
		case "enter", "right", "l":
			switch m.modeCur {
			case 0: // Live apply: pick a resource and apply to the live CP
				m.compose = false
				m.stage = stgResources
			case 1: // Document: the yaml-apply doc is home
				m.compose = true
				m.docMsg = ""
				m.setDoc()
				m.stage = stgDoc
			case 2: // Monitor: live fleet dashboard
				m.compose = false
				m.stage = stgMonitor
				return m, m.startMonitor()
			case 3: // Chat: converse with the monitor agent's LLM
				m.compose = false
				m.stage = stgChat
				return m, m.startChat()
			}
		}
	case stgResources:
		switch k.String() {
		case "q":
			return m, tea.Quit
		case "esc", "left", "h":
			if m.compose { // adding to the doc → back to the document
				m.stage = stgDoc
			} else { // live apply → back to the mode menu
				m.stage = stgMode
			}
		case "up", "k":
			if m.resCur > 0 {
				m.resCur--
			}
		case "down", "j":
			if m.resCur < len(m.visibleResources())-1 {
				m.resCur++
			}
		case "enter", "right", "l":
			m.curRes = m.visibleResources()[m.resCur]
			m.actCur = 0
			// In compose mode, jump straight to the apply form (skip the action list) —
			// the only thing you do here is add apply entries to the document.
			if m.compose {
				if i := applyActionIdx(m.curRes); i >= 0 {
					m.actCur = i
					return m.startAction()
				}
			}
			m.stage = stgActions
		}
	case stgActions:
		switch k.String() {
		case "q":
			return m, tea.Quit
		case "esc", "left", "h":
			m.stage = stgResources
		case "up", "k":
			if m.actCur > 0 {
				m.actCur--
			}
		case "down", "j":
			if m.actCur < len(m.curRes.Actions)-1 {
				m.actCur++
			}
		case "enter", "right", "l":
			nm, cmd := m.startAction()
			return nm, cmd
		}
	case stgForm:
		switch k.String() {
		case "esc":
			if m.editIdx >= 0 {
				m.editIdx = -1
				m.stage = stgDoc
			} else if m.compose { // adding to the doc → back to the resource list
				m.stage = stgResources
			} else {
				m.stage = stgActions
			}
		case "tab", "down":
			m.inputs[m.formCur].Blur()
			m.formCur = (m.formCur + 1) % len(m.inputs)
			m.inputs[m.formCur].Focus()
		case "shift+tab", "up":
			m.inputs[m.formCur].Blur()
			m.formCur = (m.formCur - 1 + len(m.inputs)) % len(m.inputs)
			m.inputs[m.formCur].Focus()
		case "enter":
			// Reject malformed typed values (bad []string list/JSON, non-numeric int/duration)
			// on the spot — before editing/adding to the doc or touching the live CP.
			if err := client.ValidateActionArgs(m.curRes.Command, m.curAct.Command, m.collectArgs()); err != nil {
				m.docMsg = "✗ " + err.Error()
				return m, nil
			}
			// Editing an existing document entry → replace it in place, back to the doc.
			if m.editIdx >= 0 {
				m.replaceEdited()
				m.stage = stgDoc
				return m, nil
			}
			// Document mode: enter ADDS to the document (never a live apply), then
			// return to the document so the new entry is visible.
			if m.compose && m.curAct.Command == "apply" {
				m.addToDoc()
				m.stage = stgDoc
				return m, nil
			}
			// A live mutating action (apply/delete) confirms first.
			if needsConfirm(m.curAct.Command) {
				m.pendingArgs = m.collectArgs()
				m.stage = stgConfirm
				return m, nil
			}
			nm, cmd := m.submit()
			return nm, cmd
		default:
			var cmd tea.Cmd
			m.inputs[m.formCur], cmd = m.inputs[m.formCur].Update(k)
			return m, cmd
		}
	case stgResult:
		switch k.String() {
		case "q":
			m.stopStream()
			return m, tea.Quit
		case "esc", "left", "h":
			m.stopStream()
			m.stage = stgActions
		case "enter":
			if m.streaming { // stop but stay so the final output is readable
				m.stopStream()
			} else {
				m.stage = stgActions
			}
		default:
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(k)
			return m, cmd
		}
	case stgDoc:
		switch k.String() {
		case "q":
			m.stopStream()
			return m, tea.Quit
		case "esc", "left", "h":
			m.stage = stgMode
		case "up", "k":
			if m.docCur > 0 {
				m.docCur--
			}
		case "down", "j":
			if m.docCur < len(m.doc)-1 {
				m.docCur++
			}
		case "d", "x":
			// delete the selected entry
			if m.docCur < len(m.doc) {
				m.doc = append(m.doc[:m.docCur], m.doc[m.docCur+1:]...)
				if m.docCur >= len(m.doc) && m.docCur > 0 {
					m.docCur--
				}
				m.docMsg = fmt.Sprintf("deleted — %d left", len(m.doc))
			}
		case "s":
			if len(m.doc) > 0 {
				ti := textinput.New()
				ti.Prompt = "  "
				ti.SetValue("kscale_apply.yaml")
				ti.Focus()
				m.savePath = ti
				m.stage = stgSave
			}
		case "e":
			if len(m.doc) > 0 {
				nm, cmd := m.editEntry()
				return nm, cmd
			}
		case "l":
			ti := textinput.New()
			ti.Prompt = "  "
			ti.SetValue("kscale_apply.yaml")
			ti.Focus()
			m.savePath = ti
			m.stage = stgLoad
		case "D":
			// dump the live desired state from the CP into the document (view + round-trip:
			// edit / save / re-apply with the existing keys). Appends like a file load, so
			// press c first for a clean dump.
			m.docMsg = "dumping live desired state…"
			return m, m.dumpDesiredCmd()
		case "a":
			// add an entry: pick a resource (compose) → fill its apply form → it lands
			// back in the document.
			m.compose = true
			if m.resCur >= len(m.visibleResources()) {
				m.resCur = 0
			}
			m.stage = stgResources
		case "A":
			// apply the WHOLE document to the live control plane (confirmed first).
			if len(m.doc) > 0 {
				m.pendingDoc = true
				m.stage = stgConfirm
			}
		case "c":
			m.doc = nil
			m.docCur = 0
			m.docMsg = "cleared"
		}
	case stgSave:
		switch k.String() {
		case "esc":
			m.stage = stgDoc
		case "enter":
			m.saveDoc()
			m.setDoc()
			m.stage = stgDoc
		default:
			var cmd tea.Cmd
			m.savePath, cmd = m.savePath.Update(k)
			return m, cmd
		}
	case stgLoad:
		switch k.String() {
		case "esc":
			m.stage = stgDoc
		case "enter":
			m.loadDoc()
			m.setDoc()
			m.stage = stgDoc
		default:
			var cmd tea.Cmd
			m.savePath, cmd = m.savePath.Update(k)
			return m, cmd
		}
	case stgConfirm:
		switch k.String() {
		case "enter", "y":
			if m.pendingDoc {
				m.pendingDoc = false
				m.running = true
				m.streamResult = false
				m.result = "applying document…"
				m.setResult()
				m.stage = stgResult
				return m, m.applyDocCmd()
			}
			nm, cmd := m.run(m.pendingArgs) // run() moves to stgResult
			return nm, cmd
		case "esc", "n":
			if m.pendingDoc {
				m.pendingDoc = false
				m.stage = stgDoc
			} else {
				m.stage = stgForm
			}
		}
	}
	return m, nil
}

func (m model) View() string {
	if m.w == 0 {
		return "connecting…"
	}
	// Header.
	conn := stConnOK.Render("● connected")
	if m.reconnecting {
		conn = stWarn.Render(fmt.Sprintf("○ reconnecting… #%d", m.reconnAttempt))
		if m.reconnErr != "" {
			conn += stMuted.Render(" (" + m.reconnErr + ")")
		}
	}
	header := stHeader.Render(stTitle.Render("kscale")+"  admin TUI") +
		stMuted.Render(fmt.Sprintf("   role=%s  cp=%s  ", m.role, m.addr)) + conn
	if m.stage != stgMode {
		if m.compose {
			header += "   " + stWarn.Render("◆ Document — esc to menu")
		} else {
			header += "   " + stOK.Render("▶ Live apply — esc to menu")
		}
	}

	// Mode picker: a clean full-width entry screen (no two-pane).
	if m.stage == stgMode {
		return header + "\n" + m.modeView() + "\n" + stFooter.Render(m.hints())
	}

	// Monitor: a full-width live fleet dashboard (no two-pane).
	if m.stage == stgMonitor {
		header += "   " + stOK.Render("◉ Monitor — esc to menu")
		return header + "\n" + m.monitorView() + "\n" + stFooter.Render(m.hints())
	}

	// Chat: a full-width conversation with the monitor LLM (no two-pane).
	if m.stage == stgChat {
		header += "   " + stOK.Render("✦ Chat — esc to menu")
		return header + "\n" + m.chatView() + "\n" + stFooter.Render(m.hints())
	}

	// Left pane: resource list.
	var lb strings.Builder
	lb.WriteString(stMuted.Render("RESOURCES") + "\n")
	iw := m.leftW() - 2
	for i, r := range m.visibleResources() {
		switch {
		case m.stage == stgResources && i == m.resCur:
			lb.WriteString(stSelRow.Width(iw).Render(" "+r.Command) + "\n")
		case r.Command == m.curRes.Command && m.stage != stgResources:
			lb.WriteString(stTitle.Render(" · "+r.Command) + "\n")
		default:
			lb.WriteString("   " + r.Command + "\n")
		}
	}
	leftStyle := stPanel
	if m.stage == stgResources {
		leftStyle = stPanelF
	}
	left := leftStyle.Width(m.leftW()).Height(m.bodyH()).Render(lb.String())

	// Right pane: actions / form / result.
	right := rightStyle(m).Width(m.rightW()).Height(m.bodyH()).Render(m.rightBody())

	body := lipgloss.JoinHorizontal(lipgloss.Top, left, right)

	// Footer: contextual hints.
	footer := stFooter.Render(m.hints())

	return header + "\n" + body + "\n" + footer
}

// modeView renders the entry mode picker: Live apply vs Document.
func (m model) modeView() string {
	opts := []struct{ name, desc string }{
		{"Live apply", "pick a resource → action and apply it to the control plane now"},
		{"Document", fmt.Sprintf("build / load / edit a yaml-apply doc, then apply it all at once  (%d entries)", len(m.doc))},
		{"Monitor", "live fleet dashboard — one row per node: status, RTT, CPU/mem/load, app stats"},
		{"Chat", "ask the monitor agent's LLM about the fleet — it answers with live state via its read tools"},
	}
	var b strings.Builder
	b.WriteString("\n  " + stTitle.Render("choose a mode") + "\n\n")
	for i, o := range opts {
		if i == m.modeCur {
			b.WriteString(stSelRow.Render(" ▶ "+o.name+" ") + "\n")
		} else {
			b.WriteString("     " + stTitle.Render(o.name) + "\n")
		}
		b.WriteString("       " + stMuted.Render(o.desc) + "\n\n")
	}
	return b.String()
}

func rightStyle(m model) lipgloss.Style {
	if m.stage != stgResources {
		return stPanelF
	}
	return stPanel
}

func (m model) rightBody() string {
	switch m.stage {
	case stgResources:
		return stMuted.Render("Select a resource on the left\n(enter / → ), then choose an action.")
	case stgActions:
		var b strings.Builder
		b.WriteString(stTitle.Render(m.curRes.Command) + stMuted.Render(" — actions") + "\n\n")
		for i, a := range m.curRes.Actions {
			suffix := ""
			if a.Stream {
				suffix = stMuted.Render(" (stream)")
			}
			if i == m.actCur {
				b.WriteString(stSelRow.Render(" "+a.Command+" ") + suffix + "\n")
			} else {
				b.WriteString("   " + a.Command + suffix + "\n")
			}
		}
		return b.String()
	case stgForm:
		var b strings.Builder
		b.WriteString(stTitle.Render(m.curRes.Command+" "+m.curAct.Command) + "\n\n")
		for i, a := range m.curAct.Args {
			name := a.Name
			if i == m.formCur {
				name = stSel.Render(name)
			}
			b.WriteString(name + stMuted.Render(" ("+client.ArgTypeHint(a.Type)+")") + "\n")
			b.WriteString(m.inputs[i].View() + "\n\n")
		}
		return b.String()
	case stgResult:
		head := stTitle.Render(m.curRes.Command+" "+m.curAct.Command) + stMuted.Render(" →") + "\n"
		return head + m.vp.View()
	case stgDoc:
		head := stTitle.Render("yaml-apply document") + stMuted.Render(fmt.Sprintf("  %d entries", len(m.doc))) + "\n\n"
		if len(m.doc) == 0 {
			return head + stMuted.Render("empty — press a to add an entry, l to load a yaml-apply file,\nor D to dump the live desired state from the control plane.")
		}
		var b strings.Builder
		b.WriteString(head)
		for i, e := range m.doc {
			if i == m.docCur {
				b.WriteString(stSelRow.Render(" "+docLabel(e)+" ") + "\n")
			} else {
				b.WriteString("   " + docLabel(e) + "\n")
			}
		}
		if m.docCur < len(m.doc) {
			b.WriteString("\n" + stMuted.Render("— selected —") + "\n" + m.doc[m.docCur])
		}
		return b.String()
	case stgSave:
		return stTitle.Render("save yaml-apply document") + "\n\n" +
			stMuted.Render("path") + "\n" + m.savePath.View() + "\n\n" +
			stMuted.Render(fmt.Sprintf("%d entries → enter to write, esc to cancel.", len(m.doc)))
	case stgLoad:
		return stTitle.Render("load yaml-apply file") + "\n\n" +
			stMuted.Render("path") + "\n" + m.savePath.View() + "\n\n" +
			stMuted.Render("enter to read + append its entries, esc to cancel.")
	case stgConfirm:
		var b strings.Builder
		if m.pendingDoc {
			b.WriteString(stWarn.Render("⚠ confirm — apply document") + "\n\n")
			for _, e := range m.doc {
				b.WriteString("  " + docLabel(e) + "\n")
			}
			b.WriteString("\n" + stWarn.Render(fmt.Sprintf("apply these %d entries to the LIVE control plane?", len(m.doc))) + "\n")
			b.WriteString(stMuted.Render("enter / y = yes   ·   esc / n = no"))
			return b.String()
		}
		b.WriteString(stWarn.Render("⚠ confirm — live "+m.curAct.Command) + "\n\n")
		b.WriteString(stMuted.Render("resource  ") + m.curRes.Command + "\n")
		b.WriteString(stMuted.Render("action    ") + m.curAct.Command + "\n\n")
		for _, a := range m.curAct.Args {
			b.WriteString("  " + a.Name + ": " + m.pendingArgs[a.Name] + "\n")
		}
		b.WriteString("\n" + stWarn.Render("apply to the LIVE control plane?") + "\n")
		b.WriteString(stMuted.Render("enter / y = yes   ·   esc / n = no"))
		return b.String()
	}
	return ""
}

// docMsgStyle colours a transient status: red for a rejection (✗ prefix), green else.
func (m model) docMsgStyle() lipgloss.Style {
	if strings.HasPrefix(m.docMsg, "✗") {
		return stWarn
	}
	return stOK
}

func (m model) hints() string {
	switch m.stage {
	case stgMode:
		return "↑/↓ select · enter choose · q quit"
	case stgMonitor:
		if m.shellPrompt {
			return "type command (sh/bash/…) · enter run · esc cancel"
		}
		return "↑/↓ select · s start · x stop · e shell · PgUp/PgDn log · G live · esc menu · q quit"
	case stgChat:
		if m.chatProbePalette {
			return "↑/↓ pick probe · enter compose · esc cancel"
		}
		if m.chatProbeCompose {
			return "edit args JSON · enter next · esc back to picker"
		}
		if m.chatProbeAsk {
			return "optional question · enter run probe · esc back to args"
		}
		if m.chatProbeEdit {
			return "edit probe args JSON · enter approve · esc back"
		}
		if m.chatAwaitProbe {
			return stWarn.Render("external probe pending") + " · a approve · e edit · d deny · esc deny"
		}
		if m.chatBusy {
			return "● turn in flight · esc cancel turn · PgUp/PgDn scroll · ctrl+c quit"
		}
		return "type · enter send · ctrl+p probe · ctrl+l new session · PgUp/PgDn scroll · esc menu"
	case stgResources:
		if m.compose { // adding an entry to the document
			h := "↑/↓ move · enter/→ fill form · esc document"
			if m.docMsg != "" {
				h += "   " + m.docMsgStyle().Render(m.docMsg)
			}
			return h
		}
		return "↑/↓ move · enter/→ actions · esc menu · q quit"
	case stgActions:
		return "↑/↓ move · enter/→ run/form · esc/← back"
	case stgForm:
		var h string
		if m.editIdx >= 0 {
			h = "tab/↑↓ field · enter save edit · esc cancel"
		} else if m.compose {
			h = "tab/↑↓ field · enter → add to doc · esc back"
		} else {
			h = "tab/↑↓ field · enter submit · esc back"
		}
		if m.docMsg != "" {
			h += "   " + m.docMsgStyle().Render(m.docMsg)
		}
		return h
	case stgConfirm:
		return stWarn.Render("enter/y = apply to LIVE") + " · esc/n = cancel"
	case stgResult:
		if m.streaming {
			return "● live · esc/enter stop · ↑/↓ scroll · q quit"
		}
		return "↑/↓ scroll · esc/← back · q quit"
	case stgDoc:
		h := "↑/↓ sel · a add · A apply · e edit · d del · l load · D dump-live · s save · c clear · esc menu · q"
		if m.docMsg != "" {
			h += "   " + m.docMsgStyle().Render(m.docMsg)
		}
		return h
	case stgSave:
		return "type path · enter write · esc back"
	case stgLoad:
		return "type path · enter load · esc back"
	}
	return ""
}

func main() {
	logger := logbuf.NewStderrLogger(os.Stderr, slog.LevelWarn)
	fs := flag.NewFlagSet("katui", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:9443", "control plane address: host:port (udp) or a connection id like ws:127.0.0.1:9444-* (over an SSH tunnel)")
	dataDir := fs.String("data", "/tmp/kscale-ca", "dir holding bootstrap tokens / saved certs")
	role := fs.String("role", "admin", "identity role: admin | viewer")
	domainFlag := fs.String("ca-domain", demo.Domain, "CA domain — must match the control plane's --ca-domain")
	commonNameFlag := fs.String("common-name", "", "cert CommonName to enroll as (default <role>.manager.ca.admin.<ca-domain>)")
	_ = fs.Parse(os.Args[1:])
	demo.Domain = *domainFlag
	ctx := context.Background()

	// --addr is a connection id ("<transport>:<host>:<port>-<id>", e.g.
	// "ws:127.0.0.1:9444-*") or a bare "host:port" (defaults to udp), so the TUI can
	// reach a CP over an SSH-forwarded WebSocket port.
	cid, ep, err := client.Endpoint(logger, *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	boot, err := enroll(ctx, ep, cid, *dataDir, *role, *commonNameFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "enroll:", err)
		os.Exit(1)
	}
	p, src, err := client.Connect(ctx, ep, cid, *role, boot, demo.PingInterval, logger)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer p.Connection().Close()

	// dialShell opens a fresh admin connection for a Monitor remote shell, reusing the
	// already-enrolled bootstrap (boot) so it never re-enrolls — only a new handshake,
	// which resets login_time within the remote_shell 10s window. It resolves a NEW
	// connection id each call (the same endpoint multiplexes both): reusing the startup
	// cid collides on the endpoint ("connection already exists for …"). The stream
	// source is unused (shells ride raw EXEC streams, not RPC), so it is dropped.
	dialShell := func() (*peer.Peer, error) {
		shellCid, err := client.ResolveConnectionID(*addr)
		if err != nil {
			return nil, err
		}
		sp, _, err := client.Connect(ctx, ep, shellCid, *role, boot, demo.PingInterval, logger)
		return sp, err
	}

	// redial re-establishes the main connection after a loss (CP restart, network
	// blip), reusing the startup enrollment. Like dialShell it resolves a NEW
	// connection id each attempt — the endpoint may still hold the dead conn's id.
	redial := func() (*peer.Peer, *wire.StreamSource, error) {
		rcid, err := client.ResolveConnectionID(*addr)
		if err != nil {
			return nil, nil, err
		}
		return client.Connect(ctx, ep, rcid, *role, boot, demo.PingInterval, logger)
	}

	m := model{ctx: ctx, src: src, p: p, redial: redial, dialShell: dialShell, role: *role, addr: *addr, vp: viewport.New(0, 0), editIdx: -1}
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func enroll(ctx context.Context, ep objproto.Endpoint, cid objproto.ConnectionID, dataDir, role, commonName string) (*ca.BootstrapInfo, error) {
	savePath := filepath.Join(dataDir, "client."+role+".boot")
	var token []byte
	if _, err := os.Stat(savePath); os.IsNotExist(err) {
		t, err := os.ReadFile(filepath.Join(dataDir, "bootstrap."+role+".token"))
		if err != nil {
			return nil, fmt.Errorf("read %s bootstrap token: %w", role, err)
		}
		token = t
	}
	cn := commonName
	if cn == "" {
		cn = demo.ClientCN(role)
	}
	return client.Enroll(ctx, ep, cid, cn, token, role, savePath)
}
