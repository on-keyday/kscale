// Package desiredstate persists the control plane's declarative desired state across
// restarts. Every declarative resource's store is in-memory only; without this a CP
// restart drops all desired state (vip/secret/router_config/...), blanking `* list`,
// breaking drift detection, and leaving the fleet un-reconciled until a human re-applies
// (see notes/ai/2026_06_30_desired_state_persistence.md).
//
// The whole snapshot is written to ONE encrypted file (castorage AES, keyed by the CA
// secret): the desired state carries write-only secrets — secret VALUES, router
// passwords, dns api_tokens — that must round-trip verbatim and must not sit on disk in
// plaintext. Each store serializes its OWN internal state via Snapshot/Restore, so those
// write-only fields are captured by construction.
package desiredstate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	castorage "github.com/on-keyday/kscale/ca/storage"
)

// Store is a declarative resource's desired-state backing. Snapshot returns its full
// internal state (including write-only fields) as JSON; Restore replays such a snapshot.
// Both must be safe against concurrent Apply/Delete (lock the store's own mutex).
type Store interface {
	Snapshot() (json.RawMessage, error)
	Restore(json.RawMessage) error
}

type entry struct {
	name  string
	store Store
}

// Manager persists a set of registered stores to one encrypted file.
type Manager struct {
	path     string
	key      []byte
	logger   *slog.Logger
	entries  []entry
	lastHash [32]byte
}

// NewManager persists to path, encrypted with key (the CA secret).
func NewManager(path string, key []byte, logger *slog.Logger) *Manager {
	return &Manager{path: path, key: key, logger: logger}
}

// Register adds a store under a resource name (the snapshot's key). Order does not
// matter — resources are independent.
func (m *Manager) Register(name string, s Store) {
	m.entries = append(m.entries, entry{name: name, store: s})
}

// snapshot builds the combined {resource: store-snapshot} JSON. Deterministic
// (json.Marshal sorts map keys) so an unchanged state hashes identically.
func (m *Manager) snapshot() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(m.entries))
	for _, e := range m.entries {
		raw, err := e.store.Snapshot()
		if err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", e.name, err)
		}
		out[e.name] = raw
	}
	return json.Marshal(out)
}

// Load decrypts the persisted desired state and restores it into every registered
// store. A missing file is not an error (first boot). Must run before serving so the
// reconcile loops push the restored desired as nodes connect.
func (m *Manager) Load() error {
	if _, err := os.Stat(m.path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	data, err := castorage.LoadAESEncryptedDataFromFile(m.path, m.key)
	if err != nil {
		return fmt.Errorf("load desired state %s: %w", m.path, err)
	}
	var blob map[string]json.RawMessage
	if err := json.Unmarshal(data, &blob); err != nil {
		return fmt.Errorf("parse desired state %s: %w", m.path, err)
	}
	restored := 0
	for _, e := range m.entries {
		raw, ok := blob[e.name]
		if !ok {
			continue
		}
		if err := e.store.Restore(raw); err != nil {
			return fmt.Errorf("restore %s: %w", e.name, err)
		}
		restored++
	}
	// Anchor lastHash to the re-serialized current state (not the file bytes): if the
	// round-trip is byte-stable the first tick writes nothing; if not, one harmless
	// rewrite reconciles the on-disk form.
	if cur, err := m.snapshot(); err == nil {
		m.lastHash = sha256.Sum256(cur)
	}
	m.logger.Info("restored desired state", "resources", restored, "path", m.path)
	return nil
}

// save snapshots all stores and, only if the combined state changed since the last
// write, encrypts it to disk.
func (m *Manager) save() error {
	data, err := m.snapshot()
	if err != nil {
		return err
	}
	h := sha256.Sum256(data)
	if h == m.lastHash {
		return nil
	}
	if err := castorage.SaveAESEncryptedDataToFile(m.path, data, m.key); err != nil {
		return fmt.Errorf("save desired state %s: %w", m.path, err)
	}
	m.lastHash = h
	return nil
}

// Run persists the desired state on a ticker, writing only when the combined snapshot
// changed (an idle CP does no disk I/O). A final save runs when ctx is cancelled.
func (m *Manager) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := m.save(); err != nil {
				m.logger.Error("desired state final save failed", "error", err)
			}
			return
		case <-t.C:
			if err := m.save(); err != nil {
				m.logger.Error("desired state save failed", "error", err)
			}
		}
	}
}
