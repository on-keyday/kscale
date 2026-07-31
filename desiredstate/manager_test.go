package desiredstate

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeStore is a minimal desiredstate.Store backed by a string→string map.
type fakeStore struct {
	data map[string]string
}

func (f *fakeStore) Snapshot() (json.RawMessage, error) { return json.Marshal(f.data) }
func (f *fakeStore) Restore(raw json.RawMessage) error  { return json.Unmarshal(raw, &f.data) }

func key32() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "desired.dat")

	src := &fakeStore{data: map[string]string{"acl": "tcp:80", "vip": "192.0.2.1"}}
	m := NewManager(path, key32(), discardLogger())
	m.Register("open_port", src)
	if err := m.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not written: %v", err)
	}

	// A fresh manager + empty store restores the same data.
	dst := &fakeStore{data: map[string]string{}}
	m2 := NewManager(path, key32(), discardLogger())
	m2.Register("open_port", dst)
	if err := m2.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if dst.data["acl"] != "tcp:80" || dst.data["vip"] != "192.0.2.1" {
		t.Fatalf("restore mismatch: %+v", dst.data)
	}
}

func TestLoadMissingFileIsNoError(t *testing.T) {
	m := NewManager(filepath.Join(t.TempDir(), "absent.dat"), key32(), discardLogger())
	m.Register("x", &fakeStore{data: map[string]string{}})
	if err := m.Load(); err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
}

func TestSaveSkipsUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desired.dat")
	src := &fakeStore{data: map[string]string{"a": "1"}}
	m := NewManager(path, key32(), discardLogger())
	m.Register("r", src)
	if err := m.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	fi1, _ := os.Stat(path)

	// No change → save writes nothing (mtime unchanged).
	if err := m.save(); err != nil {
		t.Fatalf("save2: %v", err)
	}
	fi2, _ := os.Stat(path)
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Fatalf("unchanged state should not rewrite the file")
	}

	// Mutate → next save rewrites.
	src.data["b"] = "2"
	if err := m.save(); err != nil {
		t.Fatalf("save3: %v", err)
	}
	if got := loadDecoded(t, path); got["b"] != "2" {
		t.Fatalf("change not persisted: %+v", got)
	}
}

func TestRunFinalSaveOnCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desired.dat")
	src := &fakeStore{data: map[string]string{"k": "v"}}
	m := NewManager(path, key32(), discardLogger())
	m.Register("r", src)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx, time.Hour); close(done) }()
	cancel()
	<-done

	if got := loadDecoded(t, path); got["k"] != "v" {
		t.Fatalf("final save did not persist: %+v", got)
	}
}

func loadDecoded(t *testing.T, path string) map[string]string {
	t.Helper()
	m := NewManager(path, key32(), discardLogger())
	dst := &fakeStore{data: map[string]string{}}
	m.Register("r", dst)
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			if err := m.Load(); err != nil {
				t.Fatalf("reload: %v", err)
			}
		}
	}
	return dst.data
}
