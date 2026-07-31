package main

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/on-keyday/kscale/client"
	"github.com/on-keyday/kscale/peer"
	"github.com/on-keyday/kscale/protobuf/wire"
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// TestStreamDoneNoReReprocess guards the bug where, after a stream ended, setResult
// fell through to prettyJSON/colorizeJSON and re-processed the already-styled stream
// buffer — mangling its escape sequences (signature: a CSI introducer immediately
// followed by another, "\x1b[\x1b["). With streamResult the buffer renders raw.
func TestStreamDoneNoReReprocess(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256) // force color so the bug can manifest
	if len(client.ResourceSpecs) == 0 {
		t.Skip("no generated ResourceSpecs")
	}
	m := model{ctx: context.Background(), role: "admin", addr: "x", vp: viewport.New(0, 0)}
	m.w, m.h = 120, 30
	m.stage, m.streaming, m.streamResult = stgResult, true, true
	m.curRes = client.ResourceSpecs[0]
	m.curAct = client.ActionSpec{Command: "watch", Stream: true}
	m.streamCh = make(chan streamMsg, 4)
	m.result = stMuted.Render("● streaming x watch … (esc to stop)") + "\n\n"

	m = step(m, streamMsg{line: `{"connection_id":"c-1","dp_type":"l4lb","remote_address":"203.0.113.7:51000"}`})
	m = step(m, streamMsg{done: true}) // ends the stream -> setResult must NOT reprocess

	out := m.View()
	if strings.Contains(out, "\x1b[\x1b[") {
		t.Errorf("stream buffer was re-processed after done (mangled ANSI: \\x1b[\\x1b[ present)")
	}
}

// TestPrettyJSON checks the YAML-like tree renderer: nested objects/arrays indent,
// array elements get [i] labels, scalars render inline, empty string shows as "".
func TestPrettyJSON(t *testing.T) {
	in := `{"items":[{"vip":"1.2.3.4"}],"n":5,"up":true,"note":""}`
	got := ansiRe.ReplaceAllString(prettyJSON(in), "")
	for _, want := range []string{"items:", "[0]:", "vip: 1.2.3.4", "n: 5", "up: true", `note: ""`} {
		if !strings.Contains(got, want) {
			t.Errorf("prettyJSON output missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "{") || strings.Contains(got, "\"items\"") {
		t.Errorf("output still looks like raw JSON:\n%s", got)
	}
}

func step(m model, msg tea.Msg) model {
	nm, _ := m.Update(msg)
	return nm.(model)
}

// TestDumpMsgIntoDoc checks that a dumpMsg (the live desired-state dump) is parsed and
// its entries appended to the Document buffer, and that an error dump surfaces in docMsg
// without touching the doc.
func TestDumpMsgIntoDoc(t *testing.T) {
	if len(client.ResourceSpecs) == 0 {
		t.Skip("no generated ResourceSpecs")
	}
	// Build a one-entry yaml-apply dump for the first declarative resource, the same
	// shape client.DumpDesired emits (comment header + ApplyEntryYAML entries).
	var entry string
	for _, r := range client.ResourceSpecs {
		spec, ok := client.ActionSpecFor(r.Command, "apply")
		if !ok {
			continue
		}
		args := map[string]string{}
		for _, a := range spec.Args {
			args[a.Name] = "x"
		}
		if e := client.ApplyEntryYAML(r.Command, args); e != "" {
			entry = e
			break
		}
	}
	if entry == "" {
		t.Skip("no declarative resource with an apply action")
	}
	dump := "# kscale desired-state dump (live)\n" + entry

	m := model{ctx: context.Background(), role: "admin", vp: viewport.New(0, 0)}
	m.stage = stgDoc
	m = step(m, dumpMsg{doc: dump})
	if len(m.doc) != 1 {
		t.Fatalf("after dumpMsg: doc has %d entries, want 1", len(m.doc))
	}
	if !strings.Contains(m.docMsg, "dumped 1") {
		t.Errorf("docMsg=%q, want it to report 'dumped 1'", m.docMsg)
	}

	// An error dump must not append anything and must surface in docMsg.
	m = step(m, dumpMsg{err: context.Canceled})
	if len(m.doc) != 1 {
		t.Errorf("error dumpMsg changed doc len to %d, want 1", len(m.doc))
	}
	if !strings.Contains(m.docMsg, "dump failed") {
		t.Errorf("docMsg=%q, want 'dump failed'", m.docMsg)
	}
}

// TestModelFlow drives the metadata-driven navigation without a TTY: the resource
// list comes from client.ResourceSpecs, selecting one moves focus to its actions,
// and an action with args builds a form input per arg.
func TestModelFlow(t *testing.T) {
	if len(client.ResourceSpecs) == 0 {
		t.Skip("no generated ResourceSpecs")
	}
	m := model{ctx: context.Background(), role: "admin", vp: viewport.New(0, 0)}
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})

	// stgMode: enter chooses "Live apply" (modeCur 0) → the resource list.
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.stage != stgResources {
		t.Fatalf("after mode enter: want stgResources, got %d", m.stage)
	}

	// stgResources: enter selects ResourceSpecs[0] and focuses its actions.
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.stage != stgActions {
		t.Fatalf("after enter: want stgActions, got %d", m.stage)
	}
	if m.curRes.Command != client.ResourceSpecs[0].Command {
		t.Fatalf("curRes=%q want %q", m.curRes.Command, client.ResourceSpecs[0].Command)
	}

	idx := -1
	for i, a := range m.curRes.Actions {
		if len(a.Args) > 0 {
			idx = i
			break
		}
	}
	if idx < 0 {
		m = step(m, tea.KeyMsg{Type: tea.KeyEnter})
		if m.stage != stgResult {
			t.Fatalf("no-arg action: want stgResult, got %d", m.stage)
		}
		_ = m.View()
		return
	}
	for m.actCur < idx {
		m = step(m, tea.KeyMsg{Type: tea.KeyDown})
	}
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.stage != stgForm {
		t.Fatalf("arg action: want stgForm, got %d", m.stage)
	}
	if len(m.inputs) != len(m.curAct.Args) {
		t.Fatalf("form inputs=%d want %d", len(m.inputs), len(m.curAct.Args))
	}
	_ = m.View() // must render on every stage without panic
}

// TestMonLogBurstBatched guards the monitor-open freeze: the logs subscribe
// replays up to 200 backlog lines per source (cp + every node), and delivering
// them one bubbletea message (= one full-screen render) at a time froze the
// TUI right after entering the monitor. waitMonLog must drain the
// already-buffered burst into ONE message.
func TestMonLogBurstBatched(t *testing.T) {
	ch := make(chan monLogMsg, 256)
	for i := 0; i < 100; i++ {
		ch <- monLogMsg{line: `{"level":"INFO","message":"x"}`}
	}
	msg := waitMonLog(ch)()
	batch, ok := msg.(monLogBatchMsg)
	if !ok {
		t.Fatalf("waitMonLog returned %T, want monLogBatchMsg", msg)
	}
	if len(batch.msgs) != 100 {
		t.Fatalf("batch drained %d lines, want 100", len(batch.msgs))
	}
}

// TestEditedProbeArgsAreDisclosed guards the approve-after-edit blind spot: the
// model's history holds its own tool call, so when the operator edits the args
// before approving, the result must carry a note with the actually-used args —
// otherwise the model assumes the result answers ITS args and mis-correlates
// (observed live: it insisted its args were right and blamed the user).
func TestEditedProbeArgsAreDisclosed(t *testing.T) {
	proposed := `{"host":"a.test","port":8443}`
	edited := `{"host":"a.test","port":443}`
	if got := composeProbeResult(proposed, proposed, "ok"); got != "ok" {
		t.Fatalf("unedited args must pass the result through verbatim, got %q", got)
	}
	got := composeProbeResult(proposed, edited, "ok")
	if !strings.Contains(got, edited) || !strings.Contains(got, "edited") {
		t.Fatalf("edited args must be disclosed with the actual args, got %q", got)
	}
	if !strings.HasSuffix(got, "\n\nok") {
		t.Fatalf("result body must follow the note, got %q", got)
	}
}

// TestProbeFlowRestoresPrompt guards the stuck "args ▶" prompt: the probe
// edit/compose flows swap m.chatInput for an args editor, and every exit path
// must reinstall the normal "you ▶" input — startChat only builds one when the
// prompt is empty, so a leftover editor otherwise sticks forever.
func TestProbeFlowRestoresPrompt(t *testing.T) {
	m := model{ctx: context.Background(), vp: viewport.New(0, 0)}
	_ = m.startChat()
	want := m.chatInput.Prompt

	argsEditor := func() {
		ti := textinput.New()
		ti.Prompt = stTitle.Render("args ▶ ")
		m.chatInput = ti
	}

	m.chatProbeTool, m.chatProbeArgs = "probe_http", `{"url":"http://x/"}`
	argsEditor()
	_ = m.approveProbe()
	if m.chatInput.Prompt != want {
		t.Fatalf("approveProbe left prompt %q, want %q", m.chatInput.Prompt, want)
	}

	argsEditor()
	_ = m.denyProbe()
	if m.chatInput.Prompt != want {
		t.Fatalf("denyProbe left prompt %q, want %q", m.chatInput.Prompt, want)
	}

	argsEditor()
	m.chatProbeAsk = true
	_ = m.runManualProbe()
	if m.chatInput.Prompt != want {
		t.Fatalf("runManualProbe left prompt %q, want %q", m.chatInput.Prompt, want)
	}

	argsEditor()
	m.resetChat()
	if m.chatInput.Prompt != want {
		t.Fatalf("resetChat left prompt %q, want %q", m.chatInput.Prompt, want)
	}
}

// TestReconnectStateMachine drives the connection-loss lifecycle: disconnectedMsg
// flips the model into reconnecting (redial armed), a failed attempt bumps the
// counter and schedules a retry, and reconnectedMsg swaps in the new stream
// source, clears the state, and re-arms the watcher. The header must show the
// state while it lasts.
func TestReconnectStateMachine(t *testing.T) {
	m := model{ctx: context.Background(), role: "admin", addr: "x", vp: viewport.New(0, 0)}
	m.w, m.h = 120, 30
	m.redial = func() (*peer.Peer, *wire.StreamSource, error) { return nil, nil, nil }

	m = step(m, disconnectedMsg{})
	if !m.reconnecting || m.reconnAttempt != 1 {
		t.Fatalf("after disconnect: reconnecting=%v attempt=%d, want true/1", m.reconnecting, m.reconnAttempt)
	}
	if out := ansiRe.ReplaceAllString(m.View(), ""); !strings.Contains(out, "reconnecting") {
		t.Fatalf("header does not show reconnecting state:\n%s", out)
	}

	m = step(m, reconnectFailedMsg{err: context.DeadlineExceeded})
	if !m.reconnecting || m.reconnAttempt != 2 {
		t.Fatalf("after failed attempt: reconnecting=%v attempt=%d, want true/2", m.reconnecting, m.reconnAttempt)
	}

	// a duplicate disconnect while already reconnecting must not reset the counter
	m = step(m, disconnectedMsg{})
	if m.reconnAttempt != 2 {
		t.Fatalf("duplicate disconnect reset attempt to %d", m.reconnAttempt)
	}

	newSrc := &wire.StreamSource{}
	m = step(m, reconnectedMsg{src: newSrc})
	if m.reconnecting {
		t.Fatal("still reconnecting after reconnectedMsg")
	}
	if m.src != newSrc {
		t.Fatal("stream source was not swapped on reconnect")
	}
	if out := ansiRe.ReplaceAllString(m.View(), ""); !strings.Contains(out, "connected") {
		t.Fatalf("header does not show connected state:\n%s", out)
	}
}
