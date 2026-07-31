package main

import (
	"fmt"
	"io"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	pbexec "github.com/on-keyday/kscale/protobuf/proto/exec"
	"github.com/on-keyday/kscale/remoteexec"
	execlib "github.com/on-keyday/objtrsf/exec"
	"github.com/on-keyday/objtrsf/objproto"
)

// Remote-shell integration for the Monitor view: open an interactive PTY shell on
// the selected dataplane node over the live admin peer. Mirrors the harness TUI's
// tui/interactive.go — a background tea.Cmd opens the EXEC stream and posts
// execReadyMsg, whose Update handler returns tea.Exec to suspend bubbletea and run
// the shell (terminal handoff), and execDoneMsg lands when it exits.

// execReadyMsg lands once openShell has opened the EXEC stream and written the
// ExecCommand preamble. On success Update returns tea.Exec(&interactiveExec{...});
// on error it surfaces the message on the monitor status line. conn is the fresh
// short-lived admin connection the stream rides — carried through so interactiveExec
// can close it when the shell exits (nil on the error path, already closed there).
type execReadyMsg struct {
	ces  *execlib.CommandExecutionStream
	conn objproto.Connection
	node string
	err  error
}

// execDoneMsg lands after tea.Exec returns (the shell exited or the peer dropped).
type execDoneMsg struct {
	node string
	err  error
}

// openShell opens an interactive remote shell on the currently selected monitor
// node: a FRESH admin connection (dialShell), the EXEC magic, then an ExecCommand
// preamble routed by the node's dp_type + connection_id (the CP ABAC-gates
// remote_shell and relays to that dataplane peer). The connection must be fresh
// because the remote_shell policy requires $user.login_time.since < 10s and the CP
// captures login_time per connection auth — the long-lived monitor peer is always
// past that window. The actual tea.Exec terminal handoff happens in Update on
// execReadyMsg — Cmds run outside the Update loop, but tea.Exec must be returned
// FROM Update. command is what the user typed in the Monitor shell prompt; it is
// split on whitespace into the executable + args (so "bash", "bash -l", or a
// one-off "ls -la" all work) and defaults to "sh" when blank.
func (m model) openShell(command string) tea.Cmd {
	if m.monCur < 0 || m.monCur >= len(m.monOrder) {
		return nil
	}
	row := m.monRows[m.monOrder[m.monCur]]
	node, dpType, connID := row.node, row.dpType, row.connID
	fields := strings.Fields(command)
	if len(fields) == 0 {
		fields = []string{"sh"}
	}
	cmdName, cmdArgs := fields[0], fields[1:]
	dial := m.dialShell
	return func() tea.Msg {
		if connID == "" {
			return execReadyMsg{node: node, err: fmt.Errorf("node %q has no live connection to open a shell on", node)}
		}
		sp, err := dial()
		if err != nil {
			return execReadyMsg{node: node, err: fmt.Errorf("dial shell connection: %w", err)}
		}
		conn := sp.Connection()
		stream := sp.Streams().CreateBidirectionalStream()
		if err := remoteexec.WriteMagic(stream); err != nil {
			stream.CloseBoth()
			conn.Close()
			return execReadyMsg{node: node, err: fmt.Errorf("open exec stream: %w", err)}
		}
		// PTY interactive session running the requested command, routed to the target
		// dataplane by dp_type + the connection_id (== the `connection` resource's
		// connection_id; the CP's resolveByAddress matches on it).
		if err := remoteexec.WriteCommand(stream, &pbexec.ExecCommand{
			Cmd:     cmdName,
			Args:    cmdArgs,
			Pty:     true,
			DpType:  dpType,
			Address: connID,
		}); err != nil {
			stream.CloseBoth()
			conn.Close()
			return execReadyMsg{node: node, err: fmt.Errorf("send command: %w", err)}
		}
		return execReadyMsg{node: node, ces: execlib.NewCommandExecutionStream(stream), conn: conn}
	}
}

// interactiveExec adapts CommandExecutionStream.RemoteShell to bubbletea's
// ExecCommand interface. SetStdin/Stdout/Stderr are no-ops because RemoteShell
// drives os.Stdin/os.Stdout directly (tea.Exec has released the terminal). Verbatim
// port of the harness TUI's interactiveExec.
type interactiveExec struct {
	ces  *execlib.CommandExecutionStream
	conn objproto.Connection // fresh admin connection this shell rides; closed on exit
}

func (e *interactiveExec) SetStdin(io.Reader)  {}
func (e *interactiveExec) SetStdout(io.Writer) {}
func (e *interactiveExec) SetStderr(io.Writer) {}

func (e *interactiveExec) Run() error {
	defer func() {
		e.ces.Close()
		if e.conn != nil {
			e.conn.Close() // tear down the short-lived shell connection
		}
	}()
	return e.ces.RemoteShell()
}
