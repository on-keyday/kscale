// Package userhierarchyauthority holds the hand-written business behind
// UserHierarchyAuthorityService — the genuine resource carved out of the
// ca_management facade (kscale untangle). It is kept separate from the generated
// ABAC gate (service.UserHierarchyAuthorityGated) so the CA / RootAuthority
// dependencies stay local to this resource; the gate decorates the
// pb.UserHierarchyAuthorityServiceServer interface and wires Handlers in as its
// Inner at the composition root.
package userhierarchyauthority

import (
	"context"

	"github.com/on-keyday/kscale/access"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
)

// Handlers is the business implementation of UserHierarchyAuthorityService: the
// standard Create / Delete / List verbs backed by the CA RootAuthority tree. It
// embeds pb.UnimplementedUserHierarchyAuthorityServiceServer so it satisfies the
// full interface (and can be the gate's Inner).
type Handlers struct {
	pb.UnimplementedUserHierarchyAuthorityServiceServer
	Authorities func() ([]string, error) // list backing (ca.ListCommonNames)
	Root        access.RootAuthority     // create/delete backing
	Domain      string                   // CN suffix, to resolve a name to its tree node
}

var _ pb.UserHierarchyAuthorityServiceServer = (*Handlers)(nil)

func (h *Handlers) Create(ctx context.Context, req *pbaccess.ResourceUserHierarchyAuthorityActionCreateArgsDTO) (*pbaccess.ResourceUserHierarchyAuthorityActionCreateResponseDTO, error) {
	if _, err := h.Root.CreateLeafDescendant(access.ParseCommonNameToFullName(req.AuthorityName, h.Domain), true); err != nil {
		return nil, err
	}
	// A freshly created descendant is a leaf.
	return &pbaccess.ResourceUserHierarchyAuthorityActionCreateResponseDTO{AuthorityName: req.AuthorityName, IsLeaf: true}, nil
}

// Apply is the declarative upsert: it ensures the authority exists (creating a
// leaf when absent, a no-op when present) and returns the resulting object. It
// is idempotent — applying the same desired state twice succeeds both times,
// unlike Create which fails if the authority already exists.
func (h *Handlers) Apply(ctx context.Context, req *pbaccess.ResourceUserHierarchyAuthorityActionApplyArgsDTO) (*pbaccess.ResourceUserHierarchyAuthorityActionApplyResponseDTO, error) {
	fn := access.ParseCommonNameToFullName(req.AuthorityName, h.Domain)
	a, ok := h.Root.GetDescendant(fn)
	if !ok {
		var err error
		if a, err = h.Root.CreateLeafDescendant(fn, true); err != nil {
			return nil, err
		}
	}
	return &pbaccess.ResourceUserHierarchyAuthorityActionApplyResponseDTO{AuthorityName: req.AuthorityName, IsLeaf: a.IsLeaf()}, nil
}

// isLeafOf resolves a listed common name to its tree node and reports whether it
// is a leaf (false if the name does not resolve).
func (h *Handlers) isLeafOf(commonName string) bool {
	fn, err := access.ParseCommonNameToFullNameStrict(commonName, h.Domain)
	if err != nil {
		return false
	}
	a, ok := h.Root.GetDescendant(fn)
	return ok && a.IsLeaf()
}

func (h *Handlers) Delete(ctx context.Context, req *pbaccess.ResourceUserHierarchyAuthorityActionDeleteArgsDTO) (*pbaccess.ResourceUserHierarchyAuthorityActionDeleteResponseDTO, error) {
	if err := h.Root.RemoveDescendant(access.ParseCommonNameToFullName(req.AuthorityName, h.Domain)); err != nil {
		return nil, err
	}
	return &pbaccess.ResourceUserHierarchyAuthorityActionDeleteResponseDTO{AuthorityName: req.AuthorityName}, nil
}

func (h *Handlers) List(ctx context.Context, _ *pbaccess.ResourceUserHierarchyAuthorityActionListArgsDTO) (*pbaccess.ResourceUserHierarchyAuthorityActionListResponseDTO, error) {
	names, err := h.Authorities()
	if err != nil {
		return nil, err
	}
	items := make([]*pbaccess.ResourceUserHierarchyAuthorityActionGetResponseDTO, 0, len(names))
	for _, name := range names {
		items = append(items, &pbaccess.ResourceUserHierarchyAuthorityActionGetResponseDTO{
			AuthorityName: name,
			IsLeaf:        h.isLeafOf(name),
		})
	}
	return &pbaccess.ResourceUserHierarchyAuthorityActionListResponseDTO{Items: items}, nil
}

// Watch streams the current authorities as a snapshot, then returns (closing the
// stream). A real change-feed would keep the stream open and emit subsequent
// events; the snapshot proves the server-streaming path end to end.
func (h *Handlers) Watch(ctx context.Context, _ *pbaccess.ResourceUserHierarchyAuthorityActionWatchArgsDTO, stream *pb.UserHierarchyAuthorityServiceWatchServerStream) error {
	names, err := h.Authorities()
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := stream.Send(&pbaccess.ResourceUserHierarchyAuthorityActionWatchResponseDTO{
			AuthorityName: name,
			IsLeaf:        h.isLeafOf(name),
		}); err != nil {
			return err
		}
	}
	return nil
}

// Get is left to the embedded Unimplemented for now: the synthesized verb proves
// the CRUD surface is generated end to end (proto + gate + dispatch); its
// business backing can be added when a read-by-key is needed.
