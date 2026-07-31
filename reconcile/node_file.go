package reconcile

import (
	"context"
	"log/slog"
	"time"

	"github.com/on-keyday/kscale/dpbroker"
	"github.com/on-keyday/kscale/peer"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/kscale/rpc"
)

// NodeFileDesired is the desired-state source the NodeFile controller reconciles
// (satisfied by the generated node_file store; schema.reconcile.custom keeps the loop
// hand-written here).
type NodeFileDesired interface {
	Desired() []*pbaccess.ResourceNodeFileActionGetResponseDTO
	OnChange(func())
}

// NodeFile wires the node_file reconcile loop: for each desired entry, read the named
// cplane_file from the CP file store and SendFile it to every matching node as save_as, on
// desired change, on node connect, and when the source cplane_file's content is updated
// (onCPFileUpload). Hand-written (custom) because the content is sourced from the CP file
// store rather than copied from a DTO field. Idempotent — SendFile overwrites — so a
// re-push is harmless. Note: re-shipping the bytes does NOT reload any consumer (l4lb keeps
// its already-loaded objects until its resource is re-applied); that timing is the
// operator's to control.
func NodeFile(ctx context.Context, store NodeFileDesired, broker *dpbroker.Broker, readCPFile func(string) ([]byte, error), onCPFileUpload func(func(name string)), status *Status, logger *slog.Logger) {
	send := func(item *pbaccess.ResourceNodeFileActionGetResponseDTO, p *peer.Peer) {
		content, err := readCPFile(item.CplaneFile)
		if err != nil {
			logger.Error("reconcile node_file: read cplane file", "cplane_file", item.CplaneFile, "error", err)
			status.Record("node_file", p.CommonName(), time.Now(), err)
			return
		}
		c := pb.NewDataplaneServiceClient(rpc.NewTrsfStreamSource(p.Streams(), logger))
		_, err = c.SendFile(ctx, &pb.DataplaneServiceSendFileRequest{FileName: item.SaveAs, SaveAs: item.SaveAs, Content: content})
		status.Record("node_file", p.CommonName(), time.Now(), err)
		if err != nil {
			logger.Error("reconcile node_file: SendFile failed", "node", p.CommonName(), "save_as", item.SaveAs, "error", err)
		}
	}
	store.OnChange(func() {
		for _, item := range store.Desired() {
			peers, err := broker.ResolveAll(item.Node)
			if err != nil {
				continue
			}
			for _, p := range peers {
				send(item, p)
			}
		}
	})
	broker.OnConnect(func(dpType string, p *peer.Peer) {
		for _, item := range store.Desired() {
			if broker.MatchSpecificity(item.Node, dpType, p) > 0 {
				send(item, p) // converge the (re)connected node to its desired files
			}
		}
	})
	onCPFileUpload(func(name string) {
		// The staged cplane_file `name` was (re)uploaded; re-ship it to every node whose
		// node_file entry sources from it so the new bytes land on the node.
		for _, item := range store.Desired() {
			if item.CplaneFile != name {
				continue
			}
			peers, err := broker.ResolveAll(item.Node)
			if err != nil {
				continue
			}
			for _, p := range peers {
				send(item, p)
			}
		}
	})
}
