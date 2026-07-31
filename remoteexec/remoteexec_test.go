package remoteexec

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/kscale/consts"
	pbexec "github.com/on-keyday/kscale/protobuf/proto/exec"
	execlib "github.com/on-keyday/objtrsf/exec"
	"github.com/on-keyday/objtrsf/trsf/mock"
)

// TestExecRoundTrip drives the full kscale exec path over a connected transport
// pair: the client writes the EXEC magic + a (length-prefixed) ExecCommand, the
// server consumes the magic and runs ServeDp (ReadCommand + objtrsf/exec), and the
// command's stdout flows back through the frame protocol. Exercises the preamble
// framing AND the objtrsf/exec round-trip with a real subprocess.
func TestExecRoundTrip(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	clientT, serverT := mock.SetupClientServerEx(t, slog.LevelWarn)
	mock.BackgroundIO(t, clientT, serverT)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Server (the dataplane end): accept the EXEC stream, consume the leading
	// magic (as the accept loop's rpc.DecodeMagic would), then ServeDp.
	go func() {
		conn, err := serverT.AcceptBidirectionalStream(ctx)
		if err != nil {
			return
		}
		var magic [len(consts.StreamMagicEXEC)]byte
		if _, err := io.ReadFull(conn, magic[:]); err != nil || string(magic[:]) != consts.StreamMagicEXEC {
			conn.CloseBoth()
			return
		}
		_ = ServeDp(ctx, conn, logger)
	}()

	// Client (the admin end): open the stream, send the magic + a non-pty command,
	// then read its stdout via the objtrsf/exec client wrapper.
	stream := clientT.CreateBidirectionalStream()
	if err := WriteMagic(stream); err != nil {
		t.Fatalf("WriteMagic: %v", err)
	}
	if err := WriteCommand(stream, &pbexec.ExecCommand{Cmd: "echo", Args: []string{"kscale-exec-ok"}}); err != nil {
		t.Fatalf("WriteCommand: %v", err)
	}

	ces := execlib.NewCommandExecutionStream(stream)
	// Signal stdin EOF (0-length Stdin frame) so the server's stdin reader finishes
	// and the (already-exited) echo's Cmd.Wait() returns, closing the stream. In the
	// interactive RemoteShell path the local terminal does this on Ctrl-D / detach.
	if c, ok := ces.Stdin().(io.Closer); ok {
		_ = c.Close()
	}
	out, err := io.ReadAll(ces.Stdout())
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if !strings.Contains(string(out), "kscale-exec-ok") {
		t.Fatalf("stdout = %q, want it to contain kscale-exec-ok", out)
	}
}
