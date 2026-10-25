package dataplane

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	pb "github.com/on-keyday/kscale/protobuf/proto"
)

// fakeSlot records each Load as role -> file content read at load time, so a test
// sees exactly what the slot was handed (and that the files existed when it ran).
type fakeSlot struct {
	loads    []map[string]string
	unloads  int
	loadErr  error
	loadedOK bool
}

func (s *fakeSlot) Load(files map[string]string) error {
	got := map[string]string{}
	for role, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		got[role] = string(data)
	}
	s.loads = append(s.loads, got)
	if s.loadErr != nil {
		return s.loadErr
	}
	s.loadedOK = true
	return nil
}

func (s *fakeSlot) Unload() error {
	s.unloads++
	s.loadedOK = false
	return nil
}

func obj(slot string, files ...string) *pb.DesiredObject {
	d := &pb.DesiredObject{Slot: slot}
	for i := 0; i+2 < len(files); i += 3 {
		d.Files = append(d.Files, &pb.ObjectFile{Role: files[i], Name: files[i+1], Content: []byte(files[i+2])})
	}
	return d
}

func dirFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func newTestManager(t *testing.T, slots map[string]ObjectSlot) (*objectManager, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "objects")
	return newObjectManager(dir, slots, slog.New(slog.DiscardHandler)), dir
}

func TestSyncObjectsLoadsSlotSetTogether(t *testing.T) {
	lb := &fakeSlot{}
	m, _ := newTestManager(t, map[string]ObjectSlot{"balancer": lb})

	err := m.Sync([]*pb.DesiredObject{obj("balancer", "main", "lb.o", "LB1", "crypto", "c.o", "C1")})
	if err != nil {
		t.Fatal(err)
	}
	if len(lb.loads) != 1 {
		t.Fatalf("loads = %d, want 1", len(lb.loads))
	}
	if got := lb.loads[0]; got["main"] != "LB1" || got["crypto"] != "C1" || len(got) != 2 {
		t.Fatalf("loaded %v, want main=LB1 crypto=C1 in one load", got)
	}
}

func TestSyncObjectsSkipsUnchangedSet(t *testing.T) {
	lb := &fakeSlot{}
	m, _ := newTestManager(t, map[string]ObjectSlot{"balancer": lb})
	set := []*pb.DesiredObject{obj("balancer", "main", "lb.o", "LB1")}

	if err := m.Sync(set); err != nil {
		t.Fatal(err)
	}
	if err := m.Sync(set); err != nil {
		t.Fatal(err)
	}
	if len(lb.loads) != 1 {
		t.Fatalf("loads = %d, want 1 (an unchanged set must not reload)", len(lb.loads))
	}
}

func TestSyncObjectsReplacesChangedSetAndCollectsOldFiles(t *testing.T) {
	lb := &fakeSlot{}
	m, dir := newTestManager(t, map[string]ObjectSlot{"balancer": lb})

	if err := m.Sync([]*pb.DesiredObject{obj("balancer", "main", "lb.o", "LB1")}); err != nil {
		t.Fatal(err)
	}
	// Same name, new content: the old file must not be overwritten in place.
	if err := m.Sync([]*pb.DesiredObject{obj("balancer", "main", "lb.o", "LB2")}); err != nil {
		t.Fatal(err)
	}
	if len(lb.loads) != 2 || lb.loads[1]["main"] != "LB2" {
		t.Fatalf("loads = %v, want a second load of LB2", lb.loads)
	}
	if files := dirFiles(t, dir); len(files) != 1 {
		t.Fatalf("object dir = %v, want only the loaded file", files)
	}
}

func TestSyncObjectsKeepsOldSetWhenLoadFails(t *testing.T) {
	lb := &fakeSlot{}
	m, dir := newTestManager(t, map[string]ObjectSlot{"balancer": lb})

	if err := m.Sync([]*pb.DesiredObject{obj("balancer", "main", "lb.o", "LB1")}); err != nil {
		t.Fatal(err)
	}
	lb.loadErr = errors.New("verifier rejected")
	if err := m.Sync([]*pb.DesiredObject{obj("balancer", "main", "lb.o", "BAD")}); err == nil {
		t.Fatal("Sync succeeded, want the load error")
	}
	if items := m.LoadedItems(); len(items) != 1 || !strings.HasPrefix(items[0], "balancer/main=lb.o@") {
		t.Fatalf("LoadedItems = %v, want the old set still reported", items)
	}
	files := dirFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("object dir = %v, want only the still-loaded old file", files)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, files[0])); string(data) != "LB1" {
		t.Fatalf("kept file content = %q, want LB1", data)
	}
	// A later sync of the same failed set retries the load.
	lb.loadErr = nil
	if err := m.Sync([]*pb.DesiredObject{obj("balancer", "main", "lb.o", "BAD")}); err != nil {
		t.Fatal(err)
	}
	if last := lb.loads[len(lb.loads)-1]["main"]; last != "BAD" {
		t.Fatalf("last load = %q, want the retried set", last)
	}
}

func TestSyncObjectsUnloadsUndeclaredSlot(t *testing.T) {
	lb := &fakeSlot{}
	m, dir := newTestManager(t, map[string]ObjectSlot{"balancer": lb})

	if err := m.Sync([]*pb.DesiredObject{obj("balancer", "main", "lb.o", "LB1")}); err != nil {
		t.Fatal(err)
	}
	if err := m.Sync(nil); err != nil {
		t.Fatal(err)
	}
	if lb.unloads != 1 {
		t.Fatalf("unloads = %d, want 1", lb.unloads)
	}
	if items := m.LoadedItems(); len(items) != 0 {
		t.Fatalf("LoadedItems = %v, want none", items)
	}
	if files := dirFiles(t, dir); len(files) != 0 {
		t.Fatalf("object dir = %v, want empty after unload", files)
	}
}

func TestSyncObjectsNeverUnloadsAnIdleSlot(t *testing.T) {
	lb := &fakeSlot{}
	m, _ := newTestManager(t, map[string]ObjectSlot{"balancer": lb})

	if err := m.Sync(nil); err != nil {
		t.Fatal(err)
	}
	if lb.unloads != 0 {
		t.Fatalf("unloads = %d, want 0 for a slot that never loaded", lb.unloads)
	}
}

func TestSyncObjectsRejectsUnknownSlotButAppliesOthers(t *testing.T) {
	lb := &fakeSlot{}
	m, _ := newTestManager(t, map[string]ObjectSlot{"balancer": lb})

	err := m.Sync([]*pb.DesiredObject{
		obj("capture", "main", "cap.o", "CAP"),
		obj("balancer", "main", "lb.o", "LB1"),
	})
	if err == nil || !strings.Contains(err.Error(), `"capture"`) {
		t.Fatalf("err = %v, want an unknown-slot error naming capture", err)
	}
	if len(lb.loads) != 1 {
		t.Fatalf("balancer loads = %d, want 1 despite the other slot's error", len(lb.loads))
	}
}

func TestSyncObjectsRejectsBadSets(t *testing.T) {
	cases := map[string]*pb.DesiredObject{
		"path escape":    obj("balancer", "main", "../lb.o", "X"),
		"subdir":         obj("balancer", "main", "a/lb.o", "X"),
		"empty role":     obj("balancer", "", "lb.o", "X"),
		"duplicate role": obj("balancer", "main", "a.o", "X", "main", "b.o", "Y"),
		"no files":       obj("balancer"),
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			lb := &fakeSlot{}
			m, _ := newTestManager(t, map[string]ObjectSlot{"balancer": lb})
			if err := m.Sync([]*pb.DesiredObject{d}); err == nil {
				t.Fatal("Sync succeeded, want a validation error")
			}
			if len(lb.loads) != 0 {
				t.Fatalf("loads = %d, want none for an invalid set", len(lb.loads))
			}
		})
	}
}

func TestSyncObjectsRejectsDuplicateSlot(t *testing.T) {
	lb := &fakeSlot{}
	m, _ := newTestManager(t, map[string]ObjectSlot{"balancer": lb})
	err := m.Sync([]*pb.DesiredObject{
		obj("balancer", "main", "a.o", "A"),
		obj("balancer", "main", "b.o", "B"),
	})
	if err == nil {
		t.Fatal("Sync succeeded, want a duplicate-slot error")
	}
	if len(lb.loads) != 0 {
		t.Fatalf("loads = %d, want none for an ambiguous slot", len(lb.loads))
	}
}

func TestLoadedItemsReportsSlotRoleNameAndDigest(t *testing.T) {
	lb, cap := &fakeSlot{}, &fakeSlot{}
	m, _ := newTestManager(t, map[string]ObjectSlot{"balancer": lb, "capture": cap})
	if err := m.Sync([]*pb.DesiredObject{
		obj("capture", "main", "cap.o", "CAP"),
		obj("balancer", "main", "lb.o", "LB1", "crypto", "c.o", "C1"),
	}); err != nil {
		t.Fatal(err)
	}
	items := m.LoadedItems()
	want := []string{"balancer/crypto=c.o@", "balancer/main=lb.o@", "capture/main=cap.o@"}
	if len(items) != len(want) {
		t.Fatalf("LoadedItems = %v, want %d items", items, len(want))
	}
	for i, prefix := range want {
		if !strings.HasPrefix(items[i], prefix) {
			t.Fatalf("LoadedItems[%d] = %q, want prefix %q (sorted)", i, items[i], prefix)
		}
	}
	// The digest lets an operator (or the CP) compare against the store's content.
	sum := sha256.Sum256([]byte("LB1"))
	if want := "balancer/main=lb.o@sha256:" + hex.EncodeToString(sum[:])[:12]; !slices.Contains(items, want) {
		t.Fatalf("LoadedItems = %v, want %q", items, want)
	}
}
