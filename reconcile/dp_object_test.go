package reconcile

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/kscale/rpc"
	framewire "github.com/on-keyday/kscale/rpc/wire"
)

func dpObj(name, node, slot string, files ...string) *pbaccess.ResourceDpObjectActionGetResponseDTO {
	return &pbaccess.ResourceDpObjectActionGetResponseDTO{Name: name, Node: node, Slot: slot, Files: files}
}

const s1Workload = "s1.workload.dp.system.kscale.local"

func TestDpObjectsForNodePicksMostSpecificPerSlot(t *testing.T) {
	items := []*pbaccess.ResourceDpObjectActionGetResponseDTO{
		dpObj("all", "*", "netdp", "main=a.o"),
		dpObj("group", "workload/*", "netdp", "main=b.o"),
		dpObj("node", s1Workload, "netdp", "main=c.o"),
		dpObj("cap", "*", "capture", "main=cap.o"),
		dpObj("other", "l4lb/*", "balancer", "main=lb.o"),
	}
	got, err := dpObjectsForNode(items, "workload", s1Workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["netdp"].Name != "node" || got["capture"].Name != "cap" {
		t.Fatalf("selected = %v, want netdp<-node and capture<-cap only", names(got))
	}
}

func TestDpObjectsForNodeRejectsEqualSpecificityConflict(t *testing.T) {
	items := []*pbaccess.ResourceDpObjectActionGetResponseDTO{
		dpObj("a", "workload/*", "netdp", "main=a.o"),
		dpObj("b", "workload/*", "netdp", "main=b.o"),
	}
	_, err := dpObjectsForNode(items, "workload", s1Workload)
	if err == nil || !strings.Contains(err.Error(), "netdp") || !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), `"b"`) {
		t.Fatalf("err = %v, want a conflict naming the slot and both entries", err)
	}
}

func TestBuildSyncObjectsReadsEachRoleFromTheStore(t *testing.T) {
	store := map[string]string{"lb.o": "LB", "c.o": "C"}
	read := func(name string) ([]byte, error) {
		if v, ok := store[name]; ok {
			return []byte(v), nil
		}
		return nil, fmt.Errorf("no %s", name)
	}
	sel := map[string]*pbaccess.ResourceDpObjectActionGetResponseDTO{
		"balancer": dpObj("lb", "*", "balancer", "main=lb.o", "crypto=c.o"),
	}
	req, err := buildSyncObjects(sel, read)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Objects) != 1 || req.Objects[0].Slot != "balancer" || len(req.Objects[0].Files) != 2 {
		t.Fatalf("request = %v", req)
	}
	for _, f := range req.Objects[0].Files {
		if string(f.Content) != store[f.Name] {
			t.Fatalf("file %s/%s content = %q", f.Role, f.Name, f.Content)
		}
	}
}

// A push is the node's FULL set: a slot left out is unloaded. So a slot the CP can't
// build (unreadable file, malformed entry) must fail the whole push — never ship a
// set that silently drops a running slot.
func TestBuildSyncObjectsFailsWholeWhenAnySlotIsBroken(t *testing.T) {
	read := func(name string) ([]byte, error) {
		if name == "missing.o" {
			return nil, errors.New("not found")
		}
		return []byte("X"), nil
	}
	sel := map[string]*pbaccess.ResourceDpObjectActionGetResponseDTO{
		"netdp":   dpObj("n", "*", "netdp", "main=missing.o"),
		"capture": dpObj("c", "*", "capture", "main=cap.o"),
		"bad":     dpObj("b", "*", "bad", "noequals"),
	}
	req, err := buildSyncObjects(sel, read)
	if err == nil || !strings.Contains(err.Error(), "missing.o") || !strings.Contains(err.Error(), "noequals") {
		t.Fatalf("err = %v, want both the read and the parse failure", err)
	}
	if req != nil {
		t.Fatalf("request = %v, want none (no partial set)", req)
	}
}

func TestDpObjectsForNodeConflictFailsTheNode(t *testing.T) {
	items := []*pbaccess.ResourceDpObjectActionGetResponseDTO{
		dpObj("a", "workload/*", "netdp", "main=a.o"),
		dpObj("b", "workload/*", "netdp", "main=b.o"),
		dpObj("c", "*", "capture", "main=c.o"),
	}
	if _, err := dpObjectsForNode(items, "workload", s1Workload); err == nil {
		t.Fatal("conflict on one slot must fail the node's selection (its push would unload netdp)")
	}
}

func TestDpObjectReferencesFindsEveryEntryUsingAFile(t *testing.T) {
	items := []*pbaccess.ResourceDpObjectActionGetResponseDTO{
		dpObj("lb", "*", "balancer", "main=lb.o", "crypto=c.o"),
		dpObj("cap", "*", "capture", "main=cap.o"),
		dpObj("lb2", "s3", "balancer", "main=lb2.o", "crypto=c.o"),
	}
	if got := DpObjectReferences(items, "c.o"); len(got) != 2 || got[0] != "lb" || got[1] != "lb2" {
		t.Fatalf("refs = %v, want [lb lb2]", got)
	}
	if got := DpObjectReferences(items, "unused.o"); len(got) != 0 {
		t.Fatalf("refs = %v, want none", got)
	}
}

func TestIsUnimplementedRecognizesTheRPCStatus(t *testing.T) {
	if !isUnimplemented(&rpc.RPCError{Status: framewire.RPCStatus_Unimplemented}) {
		t.Fatal("Unimplemented RPCError not recognized")
	}
	if isUnimplemented(errors.New("boom")) || isUnimplemented(nil) {
		t.Fatal("non-RPC errors must not count as unimplemented")
	}
}

func names(m map[string]*pbaccess.ResourceDpObjectActionGetResponseDTO) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v.Name
	}
	return out
}
