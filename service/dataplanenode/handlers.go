// Package dataplanenode holds the hand-written business behind
// DataplaneNodeService — the connected dataplane agents (the broker inventory)
// exposed northbound. list is a read-only query over the broker; start/stop are
// sugar over the declared run-state (expected_node.desired_run): they record
// intent and bump run_generation, and the node_run reconcile loop performs the
// actual Start/StopDataplane — inline for immediate feedback, and again on
// reconnect/retry so the intent survives agent and control-plane restarts.
// Without a SetDesiredRun hook they fall back to the original imperative
// one-shot push.
package dataplanenode

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/on-keyday/kscale/dpbroker"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/kscale/protobuf/wkt"
	"github.com/on-keyday/kscale/rpc"
)

type Handlers struct {
	pb.UnimplementedDataplaneNodeServiceServer
	Broker *dpbroker.Broker
	Logger *slog.Logger
	// AppStatus returns a node's dataplane running state (e.g. "Running"/"Stopped") by
	// common name, read from the control plane's stat cache so start/stop and the current
	// state show together. Optional — nil yields an empty status.
	AppStatus func(commonName string) string
	// ReconcileErrors returns the declarative resources whose last reconcile push to a
	// node failed ("<resource>: <error>"). Optional — nil yields none.
	ReconcileErrors func(commonName string) []string
	// ExpectedNodes returns the DECLARED node inventory (expected_node resource,
	// common_name -> dp_type). Declared nodes with no live connection are folded into
	// list/get/watch with app_status "NotConnected", making absence visible — both a
	// dropped node (previously vanished with all traces) and one that never enrolled
	// (previously undetectable in principle). Optional — nil disables the fold-in.
	ExpectedNodes func() map[string]string
	// SetDesiredRun records the declared run-state ("running"/"stopped") for the given
	// nodes (common_name -> dp_type) — expectednode.Handlers.SetDesiredRun. When set,
	// start/stop become declarative: they write intent (bumping run_generation, which
	// clears a reconcile hold) and the node_run reconcile pushes it, inline via the
	// store's OnChange. Optional — nil keeps the imperative one-shot behavior.
	SetDesiredRun func(nodes map[string]string, run string)
	// ConfirmRunNode returns one node's current node_run reconcile error (nil = its
	// last push succeeded, or nothing was pushed yet), read back right after
	// SetDesiredRun for synchronous per-node feedback. Optional.
	ConfirmRunNode func(commonName string) error
	// ForceRun marks a node's next run-state push to skip the reconcile-errors gate
	// (`node start --force`). Optional.
	ForceRun func(commonName string)
}

var _ pb.DataplaneNodeServiceServer = (*Handlers)(nil)

// AppStatusNotConnected marks a DECLARED node (expected_node) with no live broker
// connection — down, or never enrolled. Distinct from the stat-cache app states
// (Running/Stopped/Unknown), which all imply a connection exists.
const AppStatusNotConnected = "NotConnected"

// snapshot lists the connected dataplane nodes (broker.Nodes()), each with its logical
// identity (common_name, dp_type) AND its live transport connection (connection_id,
// remote_address) — the latter folds in the former `connection` resource. Declared
// nodes (ExpectedNodes) that are NOT connected are appended with app_status
// "NotConnected", so absence is a visible row rather than a missing one.
func (h *Handlers) snapshot() []*pbaccess.ResourceDataplaneNodeActionGetResponseDTO {
	nodes := h.Broker.Nodes()
	out := make([]*pbaccess.ResourceDataplaneNodeActionGetResponseDTO, 0, len(nodes))
	connected := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		connected[n.CommonName] = true
		status := ""
		if h.AppStatus != nil {
			status = h.AppStatus(n.CommonName)
		}
		var rerrs []string
		if h.ReconcileErrors != nil {
			rerrs = h.ReconcileErrors(n.CommonName)
		}
		out = append(out, &pbaccess.ResourceDataplaneNodeActionGetResponseDTO{
			CommonName:      n.CommonName,
			DpType:          n.DpType,
			ConnectionId:    n.ConnID,
			RemoteAddress:   n.RemoteAddr,
			AppStatus:       status,
			ReconcileErrors: rerrs,
		})
	}
	if h.ExpectedNodes != nil {
		declared := h.ExpectedNodes()
		missing := make([]string, 0, len(declared))
		for cn := range declared {
			if !connected[cn] {
				missing = append(missing, cn)
			}
		}
		sort.Strings(missing)
		for _, cn := range missing {
			out = append(out, &pbaccess.ResourceDataplaneNodeActionGetResponseDTO{
				CommonName: cn,
				DpType:     declared[cn],
				AppStatus:  AppStatusNotConnected,
			})
		}
	}
	return out
}

func (h *Handlers) List(ctx context.Context, _ *pbaccess.ResourceDataplaneNodeActionListArgsDTO) (*pbaccess.ResourceDataplaneNodeActionListResponseDTO, error) {
	return &pbaccess.ResourceDataplaneNodeActionListResponseDTO{Items: h.snapshot()}, nil
}

func (h *Handlers) Get(ctx context.Context, req *pbaccess.ResourceDataplaneNodeActionGetArgsDTO) (*pbaccess.ResourceDataplaneNodeActionGetResponseDTO, error) {
	for _, n := range h.snapshot() {
		if n.CommonName == req.CommonName {
			return n, nil
		}
	}
	return nil, fmt.Errorf("dataplane node %q not found", req.CommonName)
}

func (h *Handlers) Watch(ctx context.Context, _ *pbaccess.ResourceDataplaneNodeActionWatchArgsDTO, stream *pb.DataplaneNodeServiceWatchServerStream) error {
	for _, n := range h.snapshot() {
		if err := stream.Send(&pbaccess.ResourceDataplaneNodeActionWatchResponseDTO{
			CommonName:      n.CommonName,
			DpType:          n.DpType,
			ConnectionId:    n.ConnectionId,
			RemoteAddress:   n.RemoteAddress,
			AppStatus:       n.AppStatus,
			ReconcileErrors: n.ReconcileErrors,
		}); err != nil {
			return err
		}
	}
	return nil
}

// matchSelector mirrors the broker's selector grammar for a node known only by
// CommonName + dp_type (a declared node need not be connected): exact CommonName,
// short label, "<dp_type>/<label>", "<dp_type>/*", or "*"/"*/*".
func matchSelector(selector, dpType, cn string) bool {
	if selector == "*" || selector == "*/*" || selector == dpType+"/*" || selector == cn {
		return true
	}
	label := cn
	if i := strings.IndexByte(cn, '.'); i >= 0 {
		label = cn[:i]
	}
	return selector == label || selector == dpType+"/"+label
}

// resolveTargets resolves the selector against declared ∪ connected nodes
// (common_name -> dp_type). Declarative start/stop must reach DECLARED nodes even
// while disconnected — the intent is recorded now and converged on connect.
func (h *Handlers) resolveTargets(selector string) (map[string]string, error) {
	targets := map[string]string{}
	for _, n := range h.Broker.Nodes() {
		if matchSelector(selector, n.DpType, n.CommonName) {
			targets[n.CommonName] = n.DpType
		}
	}
	if h.ExpectedNodes != nil {
		for cn, dpType := range h.ExpectedNodes() {
			if _, ok := targets[cn]; !ok && matchSelector(selector, dpType, cn) {
				targets[cn] = dpType
			}
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no declared or connected node matches %q", selector)
	}
	return targets, nil
}

// setDesiredRun is the declarative start/stop: record the run-state intent for
// every matched node (declared or connected) and read back the inline reconcile
// push's per-node outcome. A disconnected declared node succeeds silently — the
// intent is stored and converged when it connects. It errors only if EVERY
// matched node failed (partial failure returns the converged ones plus the
// failures in the error, mirroring the imperative behavior).
func (h *Handlers) setDesiredRun(selector string, start, force bool) ([]string, error) {
	targets, err := h.resolveTargets(selector)
	if err != nil {
		return nil, err
	}
	run := "stopped"
	if start {
		run = "running"
	}
	if force && h.ForceRun != nil {
		// Marked BEFORE the desired write: the generation bump preserves the
		// force flag and the inline push consumes it.
		for cn := range targets {
			h.ForceRun(cn)
		}
	}
	h.SetDesiredRun(targets, run)
	var done, failed []string
	for cn := range targets {
		if h.ConfirmRunNode != nil {
			if err := h.ConfirmRunNode(cn); err != nil {
				failed = append(failed, fmt.Sprintf("%s [%v]", cn, err))
				continue
			}
		}
		done = append(done, cn)
	}
	sort.Strings(done)
	sort.Strings(failed)
	if len(done) == 0 && len(failed) > 0 {
		return nil, fmt.Errorf("all %d node(s) failed: %s", len(failed), strings.Join(failed, "; "))
	}
	if len(failed) > 0 {
		return done, fmt.Errorf("desired run-state recorded but %d node(s) failed to converge (reconcile keeps retrying): %s", len(failed), strings.Join(failed, "; "))
	}
	return done, nil
}

// startStop resolves the selector to one or more nodes (a node, "<dp_type>/*", or
// "*"/"*/*" for all) and starts/stops each, returning the nodes acted on. It errors only
// if NOTHING matched or EVERY matched node failed (a partial failure returns the ones
// that succeeded plus a logged warning). The imperative fallback for a control plane
// wired without SetDesiredRun; the declarative path is setDesiredRun.
func (h *Handlers) startStop(ctx context.Context, selector string, start, force bool) ([]string, error) {
	if h.SetDesiredRun != nil {
		return h.setDesiredRun(selector, start, force)
	}
	peers, err := h.Broker.ResolveAll(selector)
	if err != nil {
		return nil, err
	}
	var done, failed, blocked []string
	for _, p := range peers {
		cn := p.CommonName()
		// Don't start a node whose declarative config last failed to reconcile — unless
		// forced — so a misconfigured node isn't started unknowingly.
		if start && !force && h.ReconcileErrors != nil {
			if rerrs := h.ReconcileErrors(cn); len(rerrs) > 0 {
				blocked = append(blocked, fmt.Sprintf("%s [%s]", cn, strings.Join(rerrs, "; ")))
				continue
			}
		}
		c := pb.NewDataplaneServiceClient(rpc.NewTrsfStreamSource(p.Streams(), h.Logger))
		if start {
			_, err = c.StartDataplane(ctx, &wkt.Empty{})
		} else {
			_, err = c.StopDataplane(ctx, &wkt.Empty{})
		}
		if err != nil {
			h.Logger.Error("dataplane node start/stop", "node", cn, "start", start, "error", err)
			failed = append(failed, cn)
			continue
		}
		done = append(done, cn)
	}
	if len(blocked) > 0 {
		return done, fmt.Errorf("refused %d misconfigured node(s) — fix the config or pass force=true (started %d): %s",
			len(blocked), len(done), strings.Join(blocked, "; "))
	}
	if len(done) == 0 && len(failed) > 0 {
		return nil, fmt.Errorf("all %d node(s) failed: %s", len(failed), strings.Join(failed, ", "))
	}
	return done, nil
}

func (h *Handlers) Start(ctx context.Context, req *pbaccess.ResourceDataplaneNodeActionStartArgsDTO) (*pbaccess.ResourceDataplaneNodeActionStartResponseDTO, error) {
	nodes, err := h.startStop(ctx, req.CommonName, true, req.Force)
	if err != nil {
		return nil, err
	}
	return &pbaccess.ResourceDataplaneNodeActionStartResponseDTO{Nodes: nodes}, nil
}

func (h *Handlers) Stop(ctx context.Context, req *pbaccess.ResourceDataplaneNodeActionStopArgsDTO) (*pbaccess.ResourceDataplaneNodeActionStopResponseDTO, error) {
	nodes, err := h.startStop(ctx, req.CommonName, false, false)
	if err != nil {
		return nil, err
	}
	return &pbaccess.ResourceDataplaneNodeActionStopResponseDTO{Nodes: nodes}, nil
}
