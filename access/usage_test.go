package access_test

import (
	"testing"

	"github.com/on-keyday/kscale/access"
)

func TestUsage(t *testing.T) {
	auth := access.NewRootAuthority("root")
	_ = access.NewResourceTemplate("file", nil, []access.Attribute{
		access.NewAttribute("type", "file"),
	}, []access.Action{
		access.NewAction("create", []string{"path", "mode"}),
		access.NewAction("delete", []string{"path"}),
	})
	auth2, err := auth.CreateChild("admin")
	if err != nil {
		t.Fatalf("failed to create child authority: %v", err)
	}
	if auth2.Name() != "admin" {
		t.Errorf("expected authority name 'admin', got '%s'", auth2.Name())
	}
}
