package access_test

import (
	"strings"
	"testing"

	"github.com/on-keyday/kscale/access"
)

// TestAuthoritySuffixAnchoring guards the descendant-check anchoring after the domain-last
// naming flip. Policies now check `$user.authority.full_name suffix $env.authority.<path>.`,
// so `$env.authority.<path>.` MUST resolve to a LEADING-dot value (".manager.ca.admin.example.com",
// not "manager.ca.admin.example.com"). Without the leading dot a suffix match leaks across a
// label boundary — "evilmanager.ca.admin.example.com" would wrongly match.
func TestAuthoritySuffixAnchoring(t *testing.T) {
	root := access.NewRootAuthority("example.com")
	admin, err := root.CreateChild("admin")
	if err != nil {
		t.Fatal(err)
	}
	ca, err := admin.CreateChild("ca")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := ca.CreateChild("manager")
	if err != nil {
		t.Fatal(err)
	}

	// "$env.authority.admin.ca.manager." — the trailing dot resolves GetAttribute("").
	attr, ok := access.NewAuthorityAttributeMapper(manager).(interface {
		GetAttribute(string) (access.Attribute, bool)
	}).GetAttribute("")
	if !ok {
		t.Fatal(`GetAttribute("") not found`)
	}
	got, _ := attr.Value().(string)
	const want = ".manager.ca.admin.example.com"
	if got != want {
		t.Fatalf("descendant-check value = %q, want %q (leading dot required)", got, want)
	}
	if !strings.HasPrefix(got, ".") {
		t.Fatalf("value %q lacks the leading-dot anchor", got)
	}

	// Real descendant matches; a non-boundary near-match must NOT (the whole point of the dot).
	if d := "monitor.manager.ca.admin.example.com"; !strings.HasSuffix(d, got) {
		t.Errorf("descendant %q should suffix-match %q", d, got)
	}
	if e := "evilmanager.ca.admin.example.com"; strings.HasSuffix(e, got) {
		t.Errorf("non-descendant %q must NOT suffix-match %q (anchor leak)", e, got)
	}
	// The authority itself is not a strict descendant of itself.
	if s := "manager.ca.admin.example.com"; strings.HasSuffix(s, got) {
		t.Errorf("authority %q should not match its own descendant anchor %q", s, got)
	}
}
