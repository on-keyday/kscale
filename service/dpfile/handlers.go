// Package dpfile holds the hand-written business behind DpFileService — files on
// a dataplane node, managed over the southbound DataplaneService. list/delete map
// to ListFiles/RemoveFile on the named node; send reads a staged file from the
// cplane_file store and pushes its content inline via SendFile.
package dpfile

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/on-keyday/kscale/dpbroker"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/kscale/rpc"
	"github.com/on-keyday/kscale/service/cplanefile"
)

type Handlers struct {
	pb.UnimplementedDpFileServiceServer
	Broker      *dpbroker.Broker
	CplaneFiles *cplanefile.Store
	Logger      *slog.Logger
}

var _ pb.DpFileServiceServer = (*Handlers)(nil)

func (h *Handlers) client(commonName string) (pb.DataplaneServiceClient, error) {
	// Resolve (not Find): accept the same short forms as start/stop/logs — a full CN, a
	// bare node label ("s1"), or "<dp_type>/<node>" ("popcache/s1") — with Resolve's
	// not-found / ambiguous errors (single-node only; dp-file ops aren't a fan-out).
	p, err := h.Broker.Resolve(commonName)
	if err != nil {
		return nil, err
	}
	return pb.NewDataplaneServiceClient(rpc.NewTrsfStreamSource(p.Streams(), h.Logger)), nil
}

func (h *Handlers) List(ctx context.Context, req *pbaccess.ResourceDpFileActionListArgsDTO) (*pbaccess.ResourceDpFileActionListResponseDTO, error) {
	c, err := h.client(req.CommonName)
	if err != nil {
		return nil, err
	}
	resp, err := c.ListFiles(ctx, &pb.DataplaneServiceListFilesRequest{Dir: req.Dir})
	if err != nil {
		return nil, err
	}
	files := make([]string, 0, len(resp.Files))
	for _, f := range resp.Files {
		files = append(files, f.Name)
	}
	return &pbaccess.ResourceDpFileActionListResponseDTO{Files: files}, nil
}

func (h *Handlers) Send(ctx context.Context, req *pbaccess.ResourceDpFileActionSendArgsDTO) (*pbaccess.ResourceDpFileActionSendResponseDTO, error) {
	content, err := h.CplaneFiles.Read(req.CplaneFile)
	if err != nil {
		return nil, fmt.Errorf("staged file %q: %w", req.CplaneFile, err)
	}
	c, err := h.client(req.CommonName)
	if err != nil {
		return nil, err
	}
	if _, err := c.SendFile(ctx, &pb.DataplaneServiceSendFileRequest{
		FileName: req.CplaneFile, SaveAs: req.SaveAs, Content: content,
	}); err != nil {
		return nil, err
	}
	return &pbaccess.ResourceDpFileActionSendResponseDTO{SaveAs: req.SaveAs}, nil
}

func (h *Handlers) Delete(ctx context.Context, req *pbaccess.ResourceDpFileActionDeleteArgsDTO) (*pbaccess.ResourceDpFileActionDeleteResponseDTO, error) {
	c, err := h.client(req.CommonName)
	if err != nil {
		return nil, err
	}
	if _, err := c.RemoveFile(ctx, &pb.DataplaneServiceRemoveFileRequest{File: req.File}); err != nil {
		return nil, err
	}
	return &pbaccess.ResourceDpFileActionDeleteResponseDTO{File: req.File}, nil
}
