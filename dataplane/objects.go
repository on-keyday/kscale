package dataplane

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"

	pb "github.com/on-keyday/kscale/protobuf/proto"
)

// ObjectSlot is one named load point an agent offers to the dp_object resource
// (l4lb "balancer", workload "netdp", observer "capture"). A slot loads a set of
// files TOGETHER: one dp_object entry is one load unit, so its files never switch
// separately.
type ObjectSlot interface {
	// Load replaces what the slot runs with files (role -> path) at once. On error
	// the slot must keep running what it ran before.
	Load(files map[string]string) error
	// Unload stops running the slot's objects (no dp_object declares it any more).
	Unload() error
}

// ObjectSlots is an optional Hooks capability: the slots this agent loads dp_object
// sets into, by name. Agents without it accept only an empty SyncObjects.
type ObjectSlots interface {
	ObjectSlots() map[string]ObjectSlot
}

// objectManager converges an agent's slots to the dp_object set the control plane
// pushes (SyncObjects). Files land in dir under content-addressed names, so a new
// set never overwrites the files a running set was loaded from; after each sync,
// files no loaded set references are removed (dir is owned by the manager).
type objectManager struct {
	dir    string
	slots  map[string]ObjectSlot
	logger *slog.Logger

	mu     sync.Mutex
	loaded map[string]map[string]objectFile // slot -> role -> file
}

type objectFile struct {
	name string // the CP store name
	sum  string // hex sha256 of the content
	path string // where it lives in dir
}

func newObjectManager(dir string, slots map[string]ObjectSlot, logger *slog.Logger) *objectManager {
	return &objectManager{dir: dir, slots: slots, logger: logger, loaded: map[string]map[string]objectFile{}}
}

// Sync loads every desired slot whose set differs from what it runs, unloads loaded
// slots the set no longer declares, then removes unreferenced files. A failing slot
// does not stop the others; all errors are returned joined.
func (m *objectManager) Sync(objects []*pb.DesiredObject) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error
	desired := map[string]bool{}
	for _, d := range objects {
		if desired[d.Slot] {
			errs = append(errs, fmt.Errorf("dp_object slot %q: declared more than once", d.Slot))
			continue
		}
		desired[d.Slot] = true
	}
	for _, d := range objects {
		if err := m.syncSlotLocked(d, objects); err != nil {
			errs = append(errs, err)
		}
	}
	for slot := range m.loaded {
		if desired[slot] {
			continue
		}
		if err := m.slots[slot].Unload(); err != nil {
			errs = append(errs, fmt.Errorf("dp_object slot %q: unload: %w", slot, err))
			continue
		}
		delete(m.loaded, slot)
		m.logger.Info("dp_object: slot unloaded", "slot", slot)
	}
	if err := m.collectLocked(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (m *objectManager) syncSlotLocked(d *pb.DesiredObject, all []*pb.DesiredObject) error {
	n := 0
	for _, o := range all {
		if o.Slot == d.Slot {
			n++
		}
	}
	if n > 1 {
		return nil // reported once by Sync
	}
	slot, ok := m.slots[d.Slot]
	if !ok {
		return fmt.Errorf("dp_object slot %q: this node has no such slot", d.Slot)
	}
	want, err := m.validateSet(d)
	if err != nil {
		return err
	}
	if sameSet(m.loaded[d.Slot], want) {
		return nil
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return fmt.Errorf("dp_object slot %q: %w", d.Slot, err)
	}
	paths := map[string]string{}
	for _, f := range d.Files {
		of := want[f.Role]
		if err := writeFileAtomic(of.path, f.Content, 0o644); err != nil {
			return fmt.Errorf("dp_object slot %q: write %s: %w", d.Slot, f.Name, err)
		}
		paths[f.Role] = of.path
	}
	if err := slot.Load(paths); err != nil {
		return fmt.Errorf("dp_object slot %q: load: %w", d.Slot, err)
	}
	m.loaded[d.Slot] = want
	m.logger.Info("dp_object: slot loaded", "slot", d.Slot, "files", len(want))
	return nil
}

// validateSet checks one load unit and resolves each file's digest and path.
func (m *objectManager) validateSet(d *pb.DesiredObject) (map[string]objectFile, error) {
	if len(d.Files) == 0 {
		return nil, fmt.Errorf("dp_object slot %q: no files", d.Slot)
	}
	set := map[string]objectFile{}
	for _, f := range d.Files {
		if f.Role == "" {
			return nil, fmt.Errorf("dp_object slot %q: file %q has no role", d.Slot, f.Name)
		}
		if _, dup := set[f.Role]; dup {
			return nil, fmt.Errorf("dp_object slot %q: role %q given twice", d.Slot, f.Role)
		}
		if !filepath.IsLocal(f.Name) || filepath.Base(f.Name) != f.Name {
			return nil, fmt.Errorf("dp_object slot %q: %q is not a plain file name", d.Slot, f.Name)
		}
		h := sha256.Sum256(f.Content)
		sum := hex.EncodeToString(h[:])
		set[f.Role] = objectFile{name: f.Name, sum: sum, path: filepath.Join(m.dir, sum[:16]+"-"+f.Name)}
	}
	return set, nil
}

func sameSet(a, b map[string]objectFile) bool {
	if len(a) != len(b) {
		return false
	}
	for role, f := range a {
		if g, ok := b[role]; !ok || g.name != f.name || g.sum != f.sum {
			return false
		}
	}
	return true
}

// collectLocked removes every file in dir that no loaded set references —
// including the files of a set that failed to load (the next sync re-sends them).
func (m *objectManager) collectLocked() error {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	keep := map[string]bool{}
	for _, set := range m.loaded {
		for _, f := range set {
			keep[filepath.Base(f.path)] = true
		}
	}
	var errs []error
	for _, e := range entries {
		if keep[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(m.dir, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// LoadedItems reports what each slot runs as "<slot>/<role>=<name>@sha256:<12 hex>",
// sorted — the dp_object stat the control plane surfaces per node.
func (m *objectManager) LoadedItems() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var items []string
	for slot, set := range m.loaded {
		for role, f := range set {
			items = append(items, slot+"/"+role+"="+f.name+"@sha256:"+f.sum[:12])
		}
	}
	sort.Strings(items)
	return items
}

// newAgentObjects builds the manager for an agent's Hooks: its slots when the Hooks
// offer ObjectSlots, none otherwise (then only an empty SyncObjects succeeds).
func newAgentObjects(dir string, hooks Hooks, logger *slog.Logger) *objectManager {
	slots := map[string]ObjectSlot{}
	if s, ok := hooks.(ObjectSlots); ok {
		slots = s.ObjectSlots()
	}
	return newObjectManager(dir, slots, logger)
}
