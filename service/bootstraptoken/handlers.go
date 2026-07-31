// Package bootstraptoken holds the hand-written business behind
// BootstrapTokenService — an operation-only resource carved from the
// ca_management facade. issue mints a token (returning the secret, base64); revoke
// presents a token to revoke it. The CA has no token list/get, so this resource
// has no synthesized CRUD — its verbs are hand-declared operation actions. Kept
// separate from the generated gate (service.BootstrapTokenGated).
package bootstraptoken

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
)

type Handlers struct {
	pb.UnimplementedBootstrapTokenServiceServer
	Generate    func(duration time.Duration, app, commonNameSuffix string, certExp time.Duration) ([]byte, error) // ca.GenerateBootstrapToken
	RevokeToken func(token []byte) error                                                                          // ca.RevokeBootstrapToken
	Domain      string                                                                                            // CA root domain, e.g. "kscale.local"
}

var _ pb.BootstrapTokenServiceServer = (*Handlers)(nil)

func (h *Handlers) Issue(ctx context.Context, req *pbaccess.ResourceBootstrapTokenActionIssueArgsDTO) (*pbaccess.ResourceBootstrapTokenActionIssueResponseDTO, error) {
	// After the domain-last unification req.Domain is already the DNS / authority full_name
	// form (e.g. "popcache.dp.system.kscale.local", "manager.ca.admin.kscale.local") — the
	// same order as the cert CN an agent presents ("s1.popcache.dp.system.kscale.local").
	// The CA's bootstrap handshake requires the token's commonNameSuffix to be a suffix of
	// that CN, so we just prepend a dot to anchor it at a label boundary (no reverse /
	// re-append needed anymore — that was for the old domain-first req.Domain).
	suffix := "." + req.Domain
	tok, err := h.Generate(req.Duration, req.AppName, suffix, req.ExpiresPeriod)
	if err != nil {
		return nil, err
	}
	return &pbaccess.ResourceBootstrapTokenActionIssueResponseDTO{Token: base64.StdEncoding.EncodeToString(tok)}, nil
}

func (h *Handlers) Revoke(ctx context.Context, req *pbaccess.ResourceBootstrapTokenActionRevokeArgsDTO) (*pbaccess.ResourceBootstrapTokenActionRevokeResponseDTO, error) {
	tok, err := base64.StdEncoding.DecodeString(req.Token)
	if err != nil {
		return nil, fmt.Errorf("invalid token encoding: %w", err)
	}
	if err := h.RevokeToken(tok); err != nil {
		return nil, err
	}
	return &pbaccess.ResourceBootstrapTokenActionRevokeResponseDTO{Token: req.Token}, nil
}
