package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/on-keyday/kscale/access"
	"github.com/on-keyday/kscale/dpbroker"
	"github.com/on-keyday/kscale/peer"
	"github.com/on-keyday/kscale/remoteexec"
	"github.com/on-keyday/kscale/service"
	execlib "github.com/on-keyday/objtrsf/exec"
	"github.com/on-keyday/objtrsf/trsf"
)

// handleExecStream is the control-plane end of a remote-shell EXEC stream (the
// magic was already consumed by rpc.DecodeMagic). It reads the ExecCommand
// preamble, runs the remote_shell ABAC check against the authenticated caller,
// then either runs the command on the CP (cplane_execute_command, no address) or
// relays the stream transparently to the target dataplane peer (dp_execute_command).
func handleExecStream(ctx context.Context, logger *slog.Logger, gd service.GateDeps, broker *dpbroker.Broker, stream trsf.BidirectionalStream) {
	cmd, err := remoteexec.ReadCommand(stream)
	if err != nil {
		logger.Warn("exec: read command", "error", err)
		stream.CloseBoth()
		return
	}

	action := "dp_execute_command"
	if cmd.Address == "" {
		action = "cplane_execute_command"
	}
	attrs := []access.Attribute{
		access.NewAttribute("dp_type", cmd.DpType),
		access.NewAttribute("address", cmd.Address),
		access.NewAttribute("command", cmd.Cmd),
	}
	if err := authorizeExec(ctx, gd, action, attrs); err != nil {
		logger.Warn("exec: authorization denied", "action", action, "error", err)
		stream.CloseBoth()
		return
	}

	if cmd.Address == "" {
		// cplane_execute_command: run on the control plane itself. The preamble is
		// already consumed, so call objtrsf/exec directly (not remoteexec.ServeDp).
		// The CP knows the caller here, so the audit record carries the identity.
		audit := remoteexec.NewLogAuditor(logger, "cplane cn="+callerCN(ctx)+" cmd="+cmd.Cmd)
		if err := execlib.ExecuteCommandWithOption(ctx, stream, logger, cmd.Cmd, cmd.Args, "", cmd.Pty, nil,
			execlib.ExecuteOption{Audit: audit}); err != nil {
			logger.Warn("exec: control-plane shell", "error", err)
		}
		return
	}

	// dp_execute_command: resolve the target peer by its transport ConnectionID
	// (== the connection resource's connection_id) and relay.
	target, ok := resolveByAddress(broker, cmd.DpType, cmd.Address)
	if !ok {
		logger.Warn("exec: target dataplane not connected", "dp_type", cmd.DpType, "address", cmd.Address)
		stream.CloseBoth()
		return
	}
	dpStream := target.Streams().CreateBidirectionalStream()
	if err := remoteexec.WriteMagic(dpStream); err != nil {
		logger.Warn("exec: relay magic", "error", err)
		stream.CloseBoth()
		dpStream.CloseBoth()
		return
	}
	if err := remoteexec.WriteCommand(dpStream, cmd); err != nil {
		logger.Warn("exec: relay command", "error", err)
		stream.CloseBoth()
		dpStream.CloseBoth()
		return
	}
	// Audit record for the dp session's who/what/where: the CP is the ABAC
	// gatekeeper and knows the caller. The session content (stdin/stdout) is
	// audited on the dp that runs the command (remoteexec.ServeDp), correlatable
	// by target + time — the CP relays the frames opaquely.
	logger.Info("remote-shell relay to dataplane", "caller", callerCN(ctx),
		"target", target.CommonName(), "dp_type", cmd.DpType, "address", cmd.Address,
		"command", cmd.Cmd, "args", cmd.Args)
	remoteexec.Relay(stream, dpStream)
}

// callerCN returns the authenticated caller's certificate CommonName for the exec
// stream's context, or "?" if unauthenticated (should not happen past the gate).
func callerCN(ctx context.Context) string {
	if p := peer.GetPeer(ctx); p != nil {
		if cn := p.CommonName(); cn != "" {
			return cn
		}
	}
	return "?"
}

// authorizeExec runs the remote_shell ABAC check for the authenticated caller,
// mirroring the generated *Gated.authorize (EXEC is a raw stream, not a gated RPC).
func authorizeExec(ctx context.Context, gd service.GateDeps, action string, args []access.Attribute) error {
	p := peer.GetPeer(ctx)
	if p == nil || p.Policy() == nil || p.Policy().User == nil {
		return fmt.Errorf("unauthenticated caller")
	}
	res, ok := gd.Resources["remote_shell"]
	if !ok {
		return fmt.Errorf("remote_shell resource not registered")
	}
	envAttrs := []access.Attribute{access.NewAuthorityAttributeMapper(gd.Root)}
	if len(args) > 0 {
		envAttrs = append(envAttrs, access.NewAttributeManager("args", args))
	}
	policyCtx := p.Policy().
		CollectResource(res).
		CollectEnvironment(access.NewEnvironment(envAttrs)).
		CollectAction(access.NewAction(action, nil)).
		Build(ctx, gd.Audit)
	if err := gd.Controller.CanAccess(policyCtx); err != nil {
		return fmt.Errorf("permission denied: %w", err)
	}
	return nil
}

// resolveByAddress finds the connected dataplane peer whose transport ConnectionID
// matches address (and dp_type, when given).
func resolveByAddress(broker *dpbroker.Broker, dpType, address string) (*peer.Peer, bool) {
	for _, n := range broker.Nodes() {
		if n.ConnID == address && (dpType == "" || n.DpType == dpType) {
			return broker.Find(n.CommonName)
		}
	}
	return nil, false
}
