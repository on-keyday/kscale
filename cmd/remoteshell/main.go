// Command remoteshell is the kscale admin remote-shell client. It opens an EXEC
// stream to the control plane, which ABAC-gates it (remote_shell policy) and either
// runs the command on the CP (no --address) or relays it to the target dataplane
// peer (--address = that peer's `connection` connection_id). The interactive
// terminal (raw mode, stdin/stdout, window-size, signals) is driven entirely by
// objtrsf/exec's CommandExecutionStream.RemoteShell.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"

	"golang.org/x/term"

	"github.com/on-keyday/kscale/ca"
	"github.com/on-keyday/kscale/client"
	"github.com/on-keyday/kscale/internal/demo"
	"github.com/on-keyday/kscale/logbuf"
	pbexec "github.com/on-keyday/kscale/protobuf/proto/exec"
	"github.com/on-keyday/kscale/remoteexec"
	execlib "github.com/on-keyday/objtrsf/exec"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/transport"
)

func main() {
	logger := logbuf.NewStderrLogger(os.Stderr, slog.LevelInfo)
	fs := flag.NewFlagSet("remoteshell", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:9443", "control plane UDP address")
	dataDir := fs.String("data", "/tmp/kscale-ca", "dir holding bootstrap tokens / saved certs")
	role := fs.String("role", "admin", "identity role (must satisfy the remote_shell policy)")
	dpType := fs.String("dp-type", "", "target dataplane type, e.g. popcache (with -address)")
	address := fs.String("address", "", "target connection_id (from `cli connection list`); empty = run on the control plane")
	command := fs.String("command", "sh", "command to run")
	pty := fs.Bool("pty", true, "allocate a PTY (interactive)")
	domainFlag := fs.String("ca-domain", demo.Domain, "CA domain — must match the control plane's --ca-domain")
	commonNameFlag := fs.String("common-name", "", "cert CommonName to enroll as (default <role>.manager.ca.admin.<ca-domain>)")
	_ = fs.Parse(os.Args[1:])
	demo.Domain = *domainFlag
	ctx := context.Background()

	ap, err := netip.ParseAddrPort(*addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ep, err := transport.UDPEndpoint(logger, 0, objproto.EndpointModeClient)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Random connection id (not a fixed id=1): a fixed id wedges on restart — the CP keeps
	// the prior connection for that id until keepalive expires, so the next start fails with
	// "connection already exists for handshake".
	cid, err := objproto.NewRandomConnectionID("udp", ap)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	boot, err := enroll(ctx, ep, cid, *dataDir, *role, *commonNameFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "enroll:", err)
		os.Exit(1)
	}
	p, _, err := client.Connect(ctx, ep, cid, *role, boot, demo.PingInterval, logger)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer p.Connection().Close()

	stream := p.Streams().CreateBidirectionalStream()
	if err := remoteexec.WriteMagic(stream); err != nil {
		fmt.Fprintln(os.Stderr, "open exec stream:", err)
		os.Exit(1)
	}
	// A PTY only makes sense with an interactive terminal: with no TTY, closing
	// stdin (to let a non-reading command finish) tears down the PTY master, which
	// SIGHUPs the child before it flushes output. So force pty off when piped.
	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	if err := remoteexec.WriteCommand(stream, &pbexec.ExecCommand{
		Cmd:     *command,
		Args:    fs.Args(),
		Pty:     *pty && interactive,
		DpType:  *dpType,
		Address: *address,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "send command:", err)
		os.Exit(1)
	}

	ces := execlib.NewCommandExecutionStream(stream)
	if interactive {
		// Interactive: objtrsf/exec drives the local terminal (raw mode, window
		// size, signals, terminal-state restoration) end-to-end.
		if err := ces.RemoteShell(); err != nil {
			fmt.Fprintln(os.Stderr, "shell:", err)
			os.Exit(1)
		}
		return
	}
	// Non-interactive (piped / scripted): forward local stdin to the remote and
	// copy the remote stdout/stderr out. Closing stdin sends the EOF that lets a
	// non-reading command (echo, id, …) finish.
	go func() {
		_, _ = io.Copy(ces.Stdin(), os.Stdin)
		if c, ok := ces.Stdin().(io.Closer); ok {
			_ = c.Close()
		}
	}()
	go func() { _, _ = io.Copy(os.Stderr, ces.Stderr()) }()
	if _, err := io.Copy(os.Stdout, ces.Stdout()); err != nil {
		fmt.Fprintln(os.Stderr, "shell:", err)
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
