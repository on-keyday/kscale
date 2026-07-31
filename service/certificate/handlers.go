// Package certificate holds the hand-written business behind CertificateService
// — the read+delete resource carved from the ca_management facade. Certificates
// are issued via the bootstrap flow (not created through this API), so the
// resource exposes only list/get/delete (a verb subset). Kept separate from the
// generated gate (service.CertificateGated), which wires Handlers in as Inner.
package certificate

import (
	"context"
	"fmt"

	"github.com/on-keyday/kscale/ca"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
)

// Handlers is the business implementation of CertificateService, backed by the
// CA's certificate store. It embeds the Unimplemented server so it satisfies the
// full (list/get/delete) interface.
type Handlers struct {
	pb.UnimplementedCertificateServiceServer
	Certs  func() []ca.CertificateInfo   // list backing (ca.ListCertificates)
	Revoke func(commonName string) error // delete backing (ca.RevokeCertificate)
	Prune  func() error                  // garbage_collect: delete expired certs/tokens
	Count  func() int                    // certificate count after GC
}

var _ pb.CertificateServiceServer = (*Handlers)(nil)

func certToObject(c ca.CertificateInfo) *pbaccess.ResourceCertificateActionGetResponseDTO {
	return &pbaccess.ResourceCertificateActionGetResponseDTO{
		CommonName:   c.CommonName,
		SerialNumber: c.SerialNumber,
		IssuedAt:     c.IssuedAt,
		ExpiresAt:    c.ExpiresAt,
	}
}

func (h *Handlers) List(ctx context.Context, _ *pbaccess.ResourceCertificateActionListArgsDTO) (*pbaccess.ResourceCertificateActionListResponseDTO, error) {
	certs := h.Certs()
	items := make([]*pbaccess.ResourceCertificateActionGetResponseDTO, 0, len(certs))
	for _, c := range certs {
		items = append(items, certToObject(c))
	}
	return &pbaccess.ResourceCertificateActionListResponseDTO{Items: items}, nil
}

func (h *Handlers) Get(ctx context.Context, req *pbaccess.ResourceCertificateActionGetArgsDTO) (*pbaccess.ResourceCertificateActionGetResponseDTO, error) {
	for _, c := range h.Certs() {
		if c.CommonName == req.CommonName {
			return certToObject(c), nil
		}
	}
	return nil, fmt.Errorf("certificate %q not found", req.CommonName)
}

func (h *Handlers) Delete(ctx context.Context, req *pbaccess.ResourceCertificateActionDeleteArgsDTO) (*pbaccess.ResourceCertificateActionDeleteResponseDTO, error) {
	if err := h.Revoke(req.CommonName); err != nil {
		return nil, err
	}
	return &pbaccess.ResourceCertificateActionDeleteResponseDTO{CommonName: req.CommonName}, nil
}

// GarbageCollect is the hand-declared CA-maintenance operation (alongside the
// synthesized CRUD): prune expired certificates/tokens and report the remaining
// certificate count.
func (h *Handlers) GarbageCollect(ctx context.Context, _ *pbaccess.ResourceCertificateActionGarbageCollectArgsDTO) (*pbaccess.ResourceCertificateActionGarbageCollectResponseDTO, error) {
	if err := h.Prune(); err != nil {
		return nil, err
	}
	return &pbaccess.ResourceCertificateActionGarbageCollectResponseDTO{Status: "ok", CertificateCount: uint64(h.Count())}, nil
}
