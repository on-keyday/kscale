package control

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/on-keyday/kscale/l4lb/l4lbdrv"
)

// countingFactory builds stub drivers and counts how many it built and how many
// of them were closed — a build after start is an XDP re-attach on a real host.
type countingFactory struct {
	builds, closes int
}

type countingDriver struct {
	StubDriver
	f *countingFactory
}

func (d *countingDriver) Close() error { d.f.closes++; return nil }

func (f *countingFactory) factory(*l4lbdrv.FixedConfig, *l4lbdrv.DynamicConfig) (Driver, error) {
	f.builds++
	return &countingDriver{StubDriver: StubDriver{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, f: f}, nil
}

func writeObj(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSetBalancerObjectIdempotent: re-pushing the objects the driver already runs
// (what every reconnect does) must not rebuild it; a changed path or changed file
// content must.
func TestSetBalancerObjectIdempotent(t *testing.T) {
	dir := t.TempDir()
	lb, crypto := filepath.Join(dir, "lb.o"), filepath.Join(dir, "init_crypto.o")
	writeObj(t, lb, "lb-v1")
	writeObj(t, crypto, "crypto-v1")

	f := &countingFactory{}
	c := New(&l4lbdrv.FixedConfig{}, f.factory, nil)
	// The CP's push may arrive before StartDataplane: staged, no build yet.
	if err := c.SetBalancerObject(lb, crypto, "/pin"); err != nil {
		t.Fatal(err)
	}
	if f.builds != 0 {
		t.Fatalf("built before start: %d", f.builds)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ { // reconnects re-push the same desired object
		if err := c.SetBalancerObject(lb, crypto, "/pin"); err != nil {
			t.Fatal(err)
		}
	}
	if f.builds != 1 || f.closes != 0 {
		t.Fatalf("identical re-push rebuilt the driver: builds=%d closes=%d", f.builds, f.closes)
	}

	writeObj(t, lb, "lb-v2") // new object shipped under the same name, then applied
	if err := c.SetBalancerObject(lb, crypto, "/pin"); err != nil {
		t.Fatal(err)
	}
	if f.builds != 2 || f.closes != 1 {
		t.Fatalf("changed content not reloaded: builds=%d closes=%d", f.builds, f.closes)
	}

	if err := c.SetBalancerObject(lb, crypto, "/pin2"); err != nil { // different pin dir
		t.Fatal(err)
	}
	if f.builds != 3 {
		t.Fatalf("changed pin dir not reloaded: builds=%d", f.builds)
	}
	if err := c.SetBalancerObject(lb, crypto, "/pin2"); err != nil {
		t.Fatal(err)
	}
	if f.builds != 3 {
		t.Fatalf("re-push after a reload rebuilt again: builds=%d", f.builds)
	}
}

// TestSetBalancerObjectAfterStopRestart: a stop clears what was loaded, so the
// next start builds and a following identical push stays a no-op.
func TestSetBalancerObjectAfterStopRestart(t *testing.T) {
	dir := t.TempDir()
	lb := filepath.Join(dir, "lb.o")
	writeObj(t, lb, "lb-v1")
	f := &countingFactory{}
	c := New(&l4lbdrv.FixedConfig{BinPath: lb}, f.factory, nil)
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := c.SetBalancerObject(lb, "", ""); err != nil { // staged while stopped
		t.Fatal(err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.SetBalancerObject(lb, "", ""); err != nil {
		t.Fatal(err)
	}
	if f.builds != 2 {
		t.Fatalf("builds = %d, want 2 (start, restart)", f.builds)
	}
}

// TestSetBalancerObjectUnreadableReloads: a file that cannot be hashed is not
// treated as unchanged — the reload runs and surfaces the real failure.
func TestSetBalancerObjectUnreadableReloads(t *testing.T) {
	dir := t.TempDir()
	lb := filepath.Join(dir, "lb.o")
	writeObj(t, lb, "lb-v1")
	f := &countingFactory{}
	c := New(&l4lbdrv.FixedConfig{BinPath: lb}, f.factory, nil)
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(lb); err != nil {
		t.Fatal(err)
	}
	if err := c.SetBalancerObject(lb, "", ""); err != nil {
		t.Fatal(err)
	}
	if f.builds != 2 {
		t.Fatalf("unreadable object treated as unchanged: builds=%d", f.builds)
	}
}
