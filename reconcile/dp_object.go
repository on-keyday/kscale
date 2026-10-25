package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/on-keyday/kscale/dpbroker"
	"github.com/on-keyday/kscale/peer"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/kscale/rpc"
	framewire "github.com/on-keyday/kscale/rpc/wire"
)

// DpObjectDesired is the desired-state source the DpObject controller reconciles
// (satisfied by the generated dp_object store; schema.reconcile.custom keeps the loop
// hand-written here).
type DpObjectDesired interface {
	Desired() []*pbaccess.ResourceDpObjectActionGetResponseDTO
	OnChange(func())
}

// DpObject wires the dp_object reconcile loop: each node gets its FULL desired set —
// per slot, the most specific matching entry, file contents read from the CP store —
// via DataplaneService.SyncObjects, on desired change, on node connect, and when a
// referenced cplane_file is re-uploaded. The agent unloads slots the set omits, so a
// node whose set can't be built completely (an ambiguous slot, an unreadable file)
// gets NO push and keeps running what it runs; the failure goes to status. An empty
// set to an agent that predates SyncObjects is not an error (nothing to converge).
func DpObject(ctx context.Context, store DpObjectDesired, broker *dpbroker.Broker, readCPFile func(string) ([]byte, error), onCPFileUpload func(func(name string)), status *Status, logger *slog.Logger) {
	push := func(dpType string, p *peer.Peer) {
		sel, err := dpObjectsForNode(store.Desired(), dpType, p.CommonName())
		var req *pb.DataplaneServiceSyncObjectsRequest
		if err == nil {
			req, err = buildSyncObjects(sel, readCPFile)
		}
		if err == nil {
			c := pb.NewDataplaneServiceClient(rpc.NewTrsfStreamSource(p.Streams(), logger))
			_, err = c.SyncObjects(ctx, req)
			if err != nil && len(req.Objects) == 0 && isUnimplemented(err) {
				err = nil
			}
		}
		status.Record("dp_object", p.CommonName(), time.Now(), err)
		if err != nil {
			logger.Error("reconcile dp_object: push failed", "node", p.CommonName(), "error", err)
		}
	}
	store.OnChange(func() { broker.ForEachNode(push) })
	broker.OnConnect(push)
	onCPFileUpload(func(name string) {
		if len(DpObjectReferences(store.Desired(), name)) > 0 {
			broker.ForEachNode(push)
		}
	})
}

// dpObjectsForNode selects, per slot, the most specific entry whose node selector
// matches the node. Two entries tying for a slot is an error for the whole node.
func dpObjectsForNode(items []*pbaccess.ResourceDpObjectActionGetResponseDTO, dpType, commonName string) (map[string]*pbaccess.ResourceDpObjectActionGetResponseDTO, error) {
	type pick struct {
		item *pbaccess.ResourceDpObjectActionGetResponseDTO
		spec int
		ties []string
	}
	best := map[string]*pick{}
	for _, item := range items {
		spec := dpbroker.MatchSelector(item.Node, dpType, commonName)
		if spec == 0 {
			continue
		}
		cur, ok := best[item.Slot]
		switch {
		case !ok || spec > cur.spec:
			best[item.Slot] = &pick{item: item, spec: spec, ties: []string{item.Name}}
		case spec == cur.spec:
			cur.ties = append(cur.ties, item.Name)
		}
	}
	out := map[string]*pbaccess.ResourceDpObjectActionGetResponseDTO{}
	var errs []error
	for slot, p := range best {
		if len(p.ties) > 1 {
			sort.Strings(p.ties)
			errs = append(errs, fmt.Errorf("dp_object slot %q: entries %q match equally specifically", slot, p.ties))
			continue
		}
		out[slot] = p.item
	}
	return out, errors.Join(errs...)
}

// buildSyncObjects turns the selected entries into the node's push, reading each
// "<role>=<cplane_file>" from the CP store. Any broken slot fails the whole push.
func buildSyncObjects(sel map[string]*pbaccess.ResourceDpObjectActionGetResponseDTO, readCPFile func(string) ([]byte, error)) (*pb.DataplaneServiceSyncObjectsRequest, error) {
	slots := make([]string, 0, len(sel))
	for slot := range sel {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	req := &pb.DataplaneServiceSyncObjectsRequest{}
	var errs []error
	for _, slot := range slots {
		item := sel[slot]
		d := &pb.DesiredObject{Slot: slot}
		for _, spec := range item.Files {
			role, name, ok := strings.Cut(spec, "=")
			if !ok || role == "" || name == "" {
				errs = append(errs, fmt.Errorf("dp_object %q: file %q: want <role>=<cplane_file>", item.Name, spec))
				continue
			}
			content, err := readCPFile(name)
			if err != nil {
				errs = append(errs, fmt.Errorf("dp_object %q: read cplane file %s: %w", item.Name, name, err))
				continue
			}
			d.Files = append(d.Files, &pb.ObjectFile{Role: role, Name: name, Content: content})
		}
		req.Objects = append(req.Objects, d)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return req, nil
}

// DpObjectReferences returns the (sorted) names of the dp_object entries whose files
// use the cplane_file name — the guard against deleting a file still in use.
func DpObjectReferences(items []*pbaccess.ResourceDpObjectActionGetResponseDTO, name string) []string {
	var refs []string
	for _, item := range items {
		for _, spec := range item.Files {
			if _, file, ok := strings.Cut(spec, "="); ok && file == name {
				refs = append(refs, item.Name)
				break
			}
		}
	}
	sort.Strings(refs)
	return refs
}

func isUnimplemented(err error) bool {
	var rpcErr *rpc.RPCError
	return errors.As(err, &rpcErr) && rpcErr.Status == framewire.RPCStatus_Unimplemented
}
