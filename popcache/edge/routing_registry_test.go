package edge

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero/api"
)

// fakeCompiledModule is a no-op wazero.CompiledModule so RoutingRegistry can be
// exercised without compiling real wasm. Close is a no-op, so re-register and
// Unregister (which Close the previous module) are safe.
type fakeCompiledModule struct{}

func (fakeCompiledModule) Name() string                                { return "" }
func (fakeCompiledModule) ImportedFunctions() []api.FunctionDefinition { return nil }
func (fakeCompiledModule) ExportedFunctions() map[string]api.FunctionDefinition {
	return nil
}
func (fakeCompiledModule) ImportedMemories() []api.MemoryDefinition          { return nil }
func (fakeCompiledModule) ExportedMemories() map[string]api.MemoryDefinition { return nil }
func (fakeCompiledModule) CustomSections() []api.CustomSection               { return nil }
func (fakeCompiledModule) Close(context.Context) error                       { return nil }

func mustRegister(t *testing.T, r *RoutingRegistry, id uint32, method, path, matchType string) {
	t.Helper()
	if err := r.Register(ModuleSpec{ID: id, Method: method, Path: path, MatchType: matchType, FilePath: path + ".wasm"}, 1, fakeCompiledModule{}); err != nil {
		t.Fatalf("Register(%d,%q,%q,%q): %v", id, method, path, matchType, err)
	}
}

// TestRoutingRegistryMatch covers the resolution contract: exact wins over any
// prefix, the longest prefix wins among prefixes, prefixes only match at path
// segment boundaries, and method is always matched exactly.
func TestRoutingRegistryMatch(t *testing.T) {
	r := NewRoutingRegistry()
	mustRegister(t, r, 1, "GET", "/", MatchTypeExact)               // exact root
	mustRegister(t, r, 2, "GET", "/api", MatchTypePrefix)           // prefix
	mustRegister(t, r, 3, "GET", "/api/v1", MatchTypePrefix)        // longer prefix
	mustRegister(t, r, 4, "GET", "/api/v1/special", MatchTypeExact) // exact under a prefix
	mustRegister(t, r, 5, "GET", "/", MatchTypePrefix)              // prefix root catch-all

	cases := []struct {
		method, path string
		wantID       uint32
		wantOK       bool
	}{
		{"GET", "/", 1, true},               // exact wins over prefix "/"
		{"GET", "/api", 2, true},            // prefix matches its own base
		{"GET", "/api/foo", 2, true},        // longest prefix "/api"
		{"GET", "/api/v1", 3, true},         // longer prefix "/api/v1" beats "/api"
		{"GET", "/api/v1/x", 3, true},       // longest prefix "/api/v1"
		{"GET", "/api/v1/special", 4, true}, // exact beats the "/api/v1" prefix
		{"GET", "/apixyz", 5, true},         // "/api" must NOT match; falls to root
		{"GET", "/other", 5, true},          // root prefix catch-all
		{"POST", "/api", 0, false},          // method is exact: no POST route
		{"POST", "/", 0, false},             // method is exact: no POST root
	}
	for _, c := range cases {
		id, ok := r.lookupID(c.method, c.path)
		if ok != c.wantOK || (ok && id != c.wantID) {
			t.Errorf("lookupID(%q,%q) = (%d,%v), want (%d,%v)", c.method, c.path, id, ok, c.wantID, c.wantOK)
		}
	}
}

// TestRoutingRegistryMultiMethod covers the method axis: a comma-separated spec
// serves several methods, "*" (and empty) match any method, and a method-exact
// entry wins over a wildcard on the same path.
func TestRoutingRegistryMultiMethod(t *testing.T) {
	r := NewRoutingRegistry()
	mustRegister(t, r, 1, "GET,HEAD", "/a", MatchTypeExact) // two methods, one module
	mustRegister(t, r, 2, "*", "/b", MatchTypeExact)        // any method
	mustRegister(t, r, 3, "", "/c", MatchTypeExact)         // empty == any method
	mustRegister(t, r, 4, "POST", "/d", MatchTypeExact)     // exact, beats wildcard below
	mustRegister(t, r, 5, "*", "/d", MatchTypeExact)        // wildcard on same path as id 4

	cases := []struct {
		method, path string
		wantID       uint32
		wantOK       bool
	}{
		{"GET", "/a", 1, true},    // comma-list member
		{"HEAD", "/a", 1, true},   // comma-list member
		{"POST", "/a", 0, false},  // not listed
		{"DELETE", "/b", 2, true}, // "*" matches anything
		{"GET", "/c", 3, true},    // empty spec matches anything
		{"POST", "/d", 4, true},   // method-exact wins over "*"
		{"GET", "/d", 5, true},    // falls back to "*" when no exact method
		{"get", "/a", 0, false},   // spec is uppercased; a lowercase request method does not match
	}
	for _, c := range cases {
		id, ok := r.lookupID(c.method, c.path)
		if ok != c.wantOK || (ok && id != c.wantID) {
			t.Errorf("lookupID(%q,%q) = (%d,%v), want (%d,%v)", c.method, c.path, id, ok, c.wantID, c.wantOK)
		}
	}

	// Unregister must drop every method key for a multi-method module.
	if err := r.Unregister(1); err != nil {
		t.Fatalf("Unregister(1): %v", err)
	}
	for _, m := range []string{"GET", "HEAD"} {
		if _, ok := r.lookupID(m, "/a"); ok {
			t.Errorf("after Unregister lookupID(%q,/a) still matched", m)
		}
	}
}

// TestRoutingRegistryMethodPrefixInteraction confirms the precedence matrix:
// exact path dominates prefix, and within a path candidate method-exact beats
// wildcard — so an exact-path wildcard can beat a prefix-path exact method.
func TestRoutingRegistryMethodPrefixInteraction(t *testing.T) {
	r := NewRoutingRegistry()
	mustRegister(t, r, 1, "GET", "/api", MatchTypePrefix)   // prefix, exact method
	mustRegister(t, r, 2, "*", "/api/v1/x", MatchTypeExact) // exact path, wildcard method
	mustRegister(t, r, 3, "*", "/api/v1", MatchTypePrefix)  // longer prefix, wildcard method

	cases := []struct {
		method, path string
		wantID       uint32
	}{
		{"GET", "/api/foo", 1},   // only the "/api" GET prefix matches
		{"GET", "/api/v1/x", 2},  // exact path (wildcard method) beats both prefixes
		{"POST", "/api/v1/x", 2}, // exact path wildcard also serves POST
		{"POST", "/api/v1/y", 3}, // longest prefix "/api/v1" (wildcard) — not the GET "/api"
		{"POST", "/api/foo", 0},  // "/api" is GET-only; no POST route -> miss
	}
	for _, c := range cases {
		id, ok := r.lookupID(c.method, c.path)
		if c.wantID == 0 {
			if ok {
				t.Errorf("lookupID(%q,%q) = (%d,true), want miss", c.method, c.path, id)
			}
			continue
		}
		if !ok || id != c.wantID {
			t.Errorf("lookupID(%q,%q) = (%d,%v), want (%d,true)", c.method, c.path, id, ok, c.wantID)
		}
	}
}

// TestRoutingRegistryPrefixNormalization checks that a trailing slash on a
// registered prefix does not change matching.
func TestRoutingRegistryPrefixNormalization(t *testing.T) {
	r := NewRoutingRegistry()
	mustRegister(t, r, 1, "GET", "/foo/", MatchTypePrefix) // trailing slash

	for _, path := range []string{"/foo", "/foo/bar", "/foo/bar/baz"} {
		if id, ok := r.lookupID("GET", path); !ok || id != 1 {
			t.Errorf("lookupID(GET,%q) = (%d,%v), want (1,true)", path, id, ok)
		}
	}
	if _, ok := r.lookupID("GET", "/foobar"); ok {
		t.Errorf("lookupID(GET,/foobar) matched; prefix /foo must not match /foobar")
	}
}

// TestRoutingRegistryUnregister confirms a prefix module is removed from the
// prefix index, so lookups fall through to the next-longest match.
func TestRoutingRegistryUnregister(t *testing.T) {
	r := NewRoutingRegistry()
	mustRegister(t, r, 1, "GET", "/api", MatchTypePrefix)
	mustRegister(t, r, 2, "GET", "/", MatchTypePrefix)

	if id, ok := r.lookupID("GET", "/api/x"); !ok || id != 1 {
		t.Fatalf("precondition lookupID(GET,/api/x) = (%d,%v), want (1,true)", id, ok)
	}
	if err := r.Unregister(1); err != nil {
		t.Fatalf("Unregister(1): %v", err)
	}
	if id, ok := r.lookupID("GET", "/api/x"); !ok || id != 2 {
		t.Errorf("after Unregister lookupID(GET,/api/x) = (%d,%v), want (2,true)", id, ok)
	}
}

// TestRoutingRegistryReRegisterSwitchesIndex confirms that re-registering an id
// with a different match_type moves it between the exact and prefix indexes
// without leaving a stale entry behind.
func TestRoutingRegistryReRegisterSwitchesIndex(t *testing.T) {
	r := NewRoutingRegistry()
	mustRegister(t, r, 1, "GET", "/api", MatchTypePrefix)
	if id, ok := r.lookupID("GET", "/api/x"); !ok || id != 1 {
		t.Fatalf("precondition lookupID(GET,/api/x) = (%d,%v), want (1,true)", id, ok)
	}
	// Re-register the same id as an exact match on a different path.
	mustRegister(t, r, 1, "GET", "/exact", MatchTypeExact)
	// The old prefix entry must be gone.
	if _, ok := r.lookupID("GET", "/api/x"); ok {
		t.Errorf("stale prefix entry survived re-register: lookupID(GET,/api/x) matched")
	}
	if id, ok := r.lookupID("GET", "/exact"); !ok || id != 1 {
		t.Errorf("lookupID(GET,/exact) = (%d,%v), want (1,true)", id, ok)
	}
	if id, ok := r.lookupID("GET", "/exact/deeper"); ok {
		t.Errorf("exact match must not prefix-match: lookupID(GET,/exact/deeper) = (%d,%v), want miss", id, ok)
	}
}
