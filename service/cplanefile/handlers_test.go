package cplanefile

import (
	"context"
	"os"
	"strings"
	"testing"

	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
)

func newTestHandlers(t *testing.T, inUse func(string) []string) *Handlers {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &Handlers{Store: store, InUse: inUse}
	if _, err := h.Upload(context.Background(), &pbaccess.ResourceCplaneFileActionUploadArgsDTO{FileName: "lb.o", Content: []byte("LB")}); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestDeleteRefusesAFileInUse(t *testing.T) {
	h := newTestHandlers(t, func(name string) []string {
		if name == "lb.o" {
			return []string{"dp_object lb"}
		}
		return nil
	})
	_, err := h.Delete(context.Background(), &pbaccess.ResourceCplaneFileActionDeleteArgsDTO{FileName: "lb.o"})
	if err == nil || !strings.Contains(err.Error(), "dp_object lb") {
		t.Fatalf("err = %v, want a refusal naming the user", err)
	}
	if _, err := os.Stat(h.Store.Path("lb.o")); err != nil {
		t.Fatalf("file gone after a refused delete: %v", err)
	}
}

func TestDeleteRemovesAnUnusedFile(t *testing.T) {
	h := newTestHandlers(t, func(string) []string { return nil })
	if _, err := h.Delete(context.Background(), &pbaccess.ResourceCplaneFileActionDeleteArgsDTO{FileName: "lb.o"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.Store.Path("lb.o")); !os.IsNotExist(err) {
		t.Fatalf("stat after delete: %v, want not-exist", err)
	}
}
