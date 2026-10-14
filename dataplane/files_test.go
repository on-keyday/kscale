package dataplane

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteFileAtomic: the file is replaced whole, with the requested mode, and no
// temp file is left behind.
func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lb.o")
	for _, content := range []string{"v1-longer-content", "v2"} {
		if err := writeFileAtomic(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(p)
		if err != nil || string(got) != content {
			t.Fatalf("content = %q (%v), want %q", got, err, content)
		}
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
}
