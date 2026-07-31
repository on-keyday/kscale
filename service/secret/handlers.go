// Package secret holds the hand-written business behind SecretService — the dataplane
// QUIC-LB shared secret. The operator does NOT supply the value (it must be
// cryptographic): apply names a secret + gives a length, and the control plane
// GENERATES a random, HKDF-derived value (ported from ksdk's sync-secret), holds it
// internally keyed by name, and the reconcile loop pushes the value to every dataplane
// via DataplaneService.SyncSecret. The value is never a schema field, so it is never
// apply-able, returned, or logged.
package secret

import (
	"context"
	"crypto/rand"
	"fmt"
	"sort"
	"sync"

	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/objtrsf/objproto"
)

// quicLBSecretLen is the only valid QUIC-LB shared-secret length: both dataplane
// consumers are AES-128 (popcache's conn-id generator checks against the AES block
// size; the l4lb XDP crypto is hard-wired to 16-byte keys). The old default of 32
// generated values every dataplane rejected.
const quicLBSecretLen = 16

type entry struct {
	value  string
	length uint32
}

// Store holds the GENERATED shared secrets keyed by name; Desired() exposes only the
// values for the reconcile loop (the operator never sees or sets them).
type Store struct {
	mu     sync.Mutex
	byName map[string]entry
	notify func()
}

func NewStore() *Store { return &Store{byName: map[string]entry{}} }

func (s *Store) OnChange(f func()) { s.notify = f }

func (s *Store) changed() {
	if s.notify != nil {
		s.notify()
	}
}

// Desired returns the generated secret VALUES to reconcile onto the dataplane.
func (s *Store) Desired() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.byName))
	for _, e := range s.byName {
		out = append(out, e.value)
	}
	sort.Strings(out)
	return out
}

func (s *Store) set(name string, e entry) {
	s.mu.Lock()
	s.byName[name] = e
	s.mu.Unlock()
	s.changed()
}

func (s *Store) del(name string) {
	s.mu.Lock()
	delete(s.byName, name)
	s.mu.Unlock()
	s.changed()
}

func (s *Store) snapshot() map[string]entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]entry, len(s.byName))
	for k, v := range s.byName {
		out[k] = v
	}
	return out
}

type Handlers struct {
	pb.UnimplementedSecretServiceServer
	Store *Store
	// ConfirmPush, set by the control plane, returns the resource's current per-node
	// reconcile push error (nil if all good). Called right after notify() so apply
	// reports a failed push synchronously instead of a silent OK. Optional.
	ConfirmPush func() error
}

var _ pb.SecretServiceServer = (*Handlers)(nil)

// generateSecret makes a fresh random QUIC-LB shared secret, HKDF-derived from 32
// random bytes (the "quic-lb-conn-id" context, ported from ksdk's sync-secret). The
// length is fixed at 16 (0 defaults to it): the dataplane consumers are AES-128 and
// reject anything else, so accepting other lengths only manufactures reconcile
// failures.
func generateSecret(length uint32) (string, error) {
	if length == 0 {
		length = quicLBSecretLen
	}
	if length != quicLBSecretLen {
		return "", fmt.Errorf("QUIC-LB shared secret must be %d bytes (AES-128), got length=%d", quicLBSecretLen, length)
	}
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return "", err
	}
	key, err := objproto.DeriveKey(seed, "quic-lb-conn-id", int(length))
	if err != nil {
		return "", fmt.Errorf("derive secret: %w", err)
	}
	return string(key), nil
}

func (h *Handlers) Apply(ctx context.Context, req *pbaccess.ResourceSecretActionApplyArgsDTO) (*pbaccess.ResourceSecretActionApplyResponseDTO, error) {
	val, err := generateSecret(req.Length)
	if err != nil {
		return nil, err
	}
	h.Store.set(req.Name, entry{value: val, length: uint32(len(val))})
	// Write-only: the generated value is never returned.
	resp := &pbaccess.ResourceSecretActionApplyResponseDTO{Name: req.Name, Length: uint32(len(val))}
	if h.ConfirmPush != nil {
		return resp, h.ConfirmPush()
	}
	return resp, nil
}

func (h *Handlers) List(ctx context.Context, _ *pbaccess.ResourceSecretActionListArgsDTO) (*pbaccess.ResourceSecretActionListResponseDTO, error) {
	all := h.Store.snapshot()
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)
	items := make([]*pbaccess.ResourceSecretActionGetResponseDTO, 0, len(names))
	for _, n := range names {
		items = append(items, &pbaccess.ResourceSecretActionGetResponseDTO{Name: n, Length: all[n].length})
	}
	return &pbaccess.ResourceSecretActionListResponseDTO{Items: items}, nil
}

func (h *Handlers) Delete(ctx context.Context, req *pbaccess.ResourceSecretActionDeleteArgsDTO) (*pbaccess.ResourceSecretActionDeleteResponseDTO, error) {
	h.Store.del(req.Name)
	return &pbaccess.ResourceSecretActionDeleteResponseDTO{Name: req.Name}, nil
}
