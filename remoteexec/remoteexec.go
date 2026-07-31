// Package remoteexec wires the remote-shell EXEC stream onto kscale's transport.
//
// An EXEC stream is a raw trsf bidirectional stream that begins with the
// consts.StreamMagicEXEC magic (consumed by the accept loop's rpc.DecodeMagic),
// then a length-prefixed exec.ExecCommand preamble, then the objtrsf/exec/frame
// I/O (stdin/stdout/stderr/control). The command is run via objtrsf/exec — the
// evolved, shared implementation — never re-implemented here.
//
// Routing (chosen design): admin -> control plane -> dataplane peer. The CP is the
// ABAC gatekeeper and a TRANSPARENT byte relay (Relay); it never parses frames.
// The dp runs the command (ServeDp). A cplane_execute_command runs on the CP via
// ServeDp directly (no relay).
package remoteexec

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/on-keyday/kscale/consts"
	pbexec "github.com/on-keyday/kscale/protobuf/proto/exec"
	"github.com/on-keyday/kscale/remoteexec/wire"
	"github.com/on-keyday/objtrsf/exec"
	"github.com/on-keyday/objtrsf/trsf"
)

// maxCommandSize caps the ExecCommand preamble body (defensive bound; a real
// command is a few hundred bytes). Enforced in ReadCommand via io.LimitReader.
const maxCommandSize = 1 << 16

// WriteMagic writes the EXEC stream magic. The ExecCommand preamble follows.
func WriteMagic(stream trsf.BidirectionalStream) error {
	return stream.AppendData(false, []byte(consts.StreamMagicEXEC))
}

// WriteCommand writes the length-prefixed ExecCommand preamble (the EXEC magic is
// written separately by the opener via WriteMagic). The framing (u32 big-endian
// length + body) is the brgen-generated wire.ExecCommandPreamble.
func WriteCommand(stream trsf.BidirectionalStream, cmd *pbexec.ExecCommand) error {
	body, err := cmd.Append(nil)
	if err != nil {
		return fmt.Errorf("encode ExecCommand: %w", err)
	}
	var frame wire.ExecCommandPreamble
	if !frame.SetBody(body) {
		return fmt.Errorf("ExecCommand too large: %d bytes", len(body))
	}
	buf, err := frame.Append(nil)
	if err != nil {
		return fmt.Errorf("encode ExecCommand preamble: %w", err)
	}
	return stream.AppendData(false, buf)
}

// ReadCommand reads the length-prefixed ExecCommand preamble. The magic must
// already be consumed (by rpc.DecodeMagic in the accept loop). The io.LimitReader
// enforces maxCommandSize: a body_len claiming more than the cap exhausts the
// limited reader before the body completes, so Read errors instead of trusting it.
func ReadCommand(stream trsf.BidirectionalStream) (*pbexec.ExecCommand, error) {
	var frame wire.ExecCommandPreamble
	if err := frame.Read(io.LimitReader(stream, 4+maxCommandSize)); err != nil {
		return nil, fmt.Errorf("read ExecCommand preamble: %w", err)
	}
	cmd := &pbexec.ExecCommand{}
	if err := cmd.Decode(frame.Body); err != nil {
		return nil, fmt.Errorf("decode ExecCommand: %w", err)
	}
	return cmd, nil
}

// ServeDp runs the command end of an EXEC stream: read the preamble, then hand the
// stream to objtrsf/exec for the frame I/O. Used on the dataplane (the dp that runs
// the command) and on the control plane for cplane_execute_command.
func ServeDp(ctx context.Context, stream trsf.BidirectionalStream, logger *slog.Logger) error {
	cmd, err := ReadCommand(stream)
	if err != nil {
		stream.CloseBoth()
		return err
	}
	// Audit the session on the executor (this dp runs the command). The caller
	// identity lives on the control plane (it ABAC-gated + relayed); the CP logs
	// that separately, correlatable by target + time.
	audit := NewLogAuditor(logger, "dp cmd="+cmd.Cmd)
	return exec.ExecuteCommandWithOption(ctx, stream, logger, cmd.Cmd, cmd.Args, "", cmd.Pty, nil,
		exec.ExecuteOption{Audit: audit})
}

// Relay transparently pipes raw bytes between two EXEC streams (admin <-> dp) until
// either side ends, then tears both down. The frame protocol stays end-to-end; the
// relay never parses it.
func Relay(a, b trsf.BidirectionalStream) {
	done := make(chan struct{}, 2)
	pipe := func(dst, src trsf.BidirectionalStream) {
		io.Copy(dst, src)
		dst.CloseBoth()
		src.CloseBoth()
		done <- struct{}{}
	}
	go pipe(a, b)
	go pipe(b, a)
	<-done
	<-done
}
