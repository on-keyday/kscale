package dataplane

import (
	"context"
	"log/slog"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbstat "github.com/on-keyday/kscale/protobuf/proto/stat"
	"github.com/on-keyday/kscale/stat"
)

// nopHooks is a Hooks that does nothing (an agent with no dp_object slots).
type nopHooks struct{}

func (nopHooks) DpType() string                            { return "test" }
func (nopHooks) Start(context.Context) error               { return nil }
func (nopHooks) Stop() error                               { return nil }
func (nopHooks) UpdateVip(netip.Addr, bool) error          { return nil }
func (nopHooks) SyncSecret([]byte) error                   { return nil }
func (nopHooks) BindInterface(string) error                { return nil }
func (nopHooks) UpdateMtu(uint16) error                    { return nil }
func (nopHooks) SetServerID(uint32)                        {}
func (nopHooks) UpdateDestinations([]stat.DestEntry) error { return nil }
func (nopHooks) UpdateRemote([]stat.DestEntry) error       { return nil }
func (nopHooks) Stats() []*pbstat.Stats                    { return nil }
func (nopHooks) PromMetrics() *stat.PromMetrics            { return nil }

// slotHooks is a Hooks whose agent offers dp_object slots.
type slotHooks struct {
	nopHooks
	slots map[string]ObjectSlot
}

func (h *slotHooks) ObjectSlots() map[string]ObjectSlot { return h.slots }

func newObjectService(t *testing.T, hooks Hooks) *dpService {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	return &dpService{hooks: hooks, objects: newAgentObjects(filepath.Join(t.TempDir(), "dp_objects"), hooks, logger), logger: logger}
}

func TestSyncObjectsRPCLoadsAndReportsTheSlot(t *testing.T) {
	lb := &fakeSlot{}
	s := newObjectService(t, &slotHooks{slots: map[string]ObjectSlot{"balancer": lb}})

	_, err := s.SyncObjects(context.Background(), &pb.DataplaneServiceSyncObjectsRequest{
		Objects: []*pb.DesiredObject{obj("balancer", "main", "lb.o", "LB1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lb.loads) != 1 {
		t.Fatalf("loads = %d, want 1", len(lb.loads))
	}
	st := s.objectStat()
	if st == nil || st.DpObject == nil || st.DpObject.LoadedObjects == nil {
		t.Fatalf("objectStat = %v, want a dp_object entry", st)
	}
	if items := st.DpObject.LoadedObjects.Items; len(items) != 1 || !strings.HasPrefix(items[0], "balancer/main=lb.o@sha256:") {
		t.Fatalf("loaded_objects = %v", items)
	}
}

func TestObjectStatIsAbsentWhileNothingIsLoaded(t *testing.T) {
	s := newObjectService(t, &slotHooks{slots: map[string]ObjectSlot{"balancer": &fakeSlot{}}})
	if st := s.objectStat(); st != nil {
		t.Fatalf("objectStat = %v, want nil before any load", st)
	}
}

func TestSyncObjectsRPCWithoutSlotsAcceptsOnlyAnEmptySet(t *testing.T) {
	s := newObjectService(t, &nopHooks{})

	if _, err := s.SyncObjects(context.Background(), &pb.DataplaneServiceSyncObjectsRequest{}); err != nil {
		t.Fatalf("empty set on a slot-less agent: %v, want nil", err)
	}
	_, err := s.SyncObjects(context.Background(), &pb.DataplaneServiceSyncObjectsRequest{
		Objects: []*pb.DesiredObject{obj("balancer", "main", "lb.o", "LB1")},
	})
	if err == nil {
		t.Fatal("non-empty set on a slot-less agent succeeded, want an error")
	}
}
