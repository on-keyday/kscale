package l4route

import (
	"context"
	"strings"
	"testing"

	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	pbstat "github.com/on-keyday/kscale/protobuf/proto/stat"
)

func TestParsePort(t *testing.T) {
	for _, bad := range []string{"", "8443", "tcp", "tcp:", "tcp:0", "tcp:65536", "icmp:echo", "sctp:1", "tcp:x"} {
		if _, _, err := ParsePort(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	proto, port, err := ParsePort("udp:9443")
	if err != nil || proto != "udp" || port != 9443 {
		t.Fatalf("udp:9443 -> %s %d %v", proto, port, err)
	}
}

func TestValidate(t *testing.T) {
	if err := Validate("r", "tcp:8443", "workload/g1"); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][3]string{
		{"", "tcp:8443", "g1"},
		{"r", "tcp:8443", ""},
		{"r", "tcp:8443", "*"},
		{"r", "tcp:8443", "workload/*"},
		{"r", "8443", "g1"},
	} {
		if err := Validate(c[0], c[1], c[2]); err == nil {
			t.Errorf("%v accepted", c)
		}
	}
}

func TestValidateSetRejectsSharedPort(t *testing.T) {
	items := []*pbaccess.ResourceL4RouteActionGetResponseDTO{
		{Name: "a", Port: "tcp:8443", Node: "g1"},
		{Name: "b", Port: "udp:8443", Node: "g1"}, // same number, other protocol: fine
	}
	if err := ValidateSet(items); err != nil {
		t.Fatal(err)
	}
	items = append(items, &pbaccess.ResourceL4RouteActionGetResponseDTO{Name: "c", Port: "tcp:8443", Node: "s2"})
	if err := ValidateSet(items); err == nil || !strings.Contains(err.Error(), `already routed by "a"`) {
		t.Fatalf("shared tcp:8443 not refused: %v", err)
	}
}

func spec(iface, mac, ip string) *pbstat.Stats {
	return &pbstat.Stats{NetworkSpec: &pbstat.NetworkSpecStat{Interfaces: []*pbstat.InterfaceSpecStat{
		{Name: "lo", Mac: "", Ips: []string{"127.0.0.1/8"}},
		{Name: iface, Mac: mac, Ips: []string{ip}},
	}}}
}

func bound(iface string) *pbstat.Stats {
	return &pbstat.Stats{CdnAppRealtime: &pbstat.CdnAppRealtimeStat{BoundIfaces: &pbstat.StringList{Items: []string{iface}}}}
}

// TestNodeAddr: the address comes from the bound interface in the NetworkSpec,
// whichever entries carry them, and needs no ServerID (unlike an l4lb backend).
func TestNodeAddr(t *testing.T) {
	d, err := NodeAddr(&pbstat.StatBatch{Stats: []*pbstat.Stats{
		spec("eth0", "02:00:00:00:00:09", "198.51.100.9/24"),
		{CdnAppRealtime: &pbstat.CdnAppRealtimeStat{}}, // e.g. a lifecycle entry without ifaces
		bound("eth0"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if d.IPAddr.String() != "198.51.100.9" || d.HardwareAddr.String() != "02:00:00:00:00:09" {
		t.Fatalf("dest = %v / %v", d.IPAddr, d.HardwareAddr)
	}
	for name, b := range map[string]*pbstat.StatBatch{
		"nothing reported": nil,
		"no bound iface":   {Stats: []*pbstat.Stats{spec("eth0", "02:00:00:00:00:09", "198.51.100.9/24")}},
		"no network spec":  {Stats: []*pbstat.Stats{bound("eth0")}},
		"bound iface unknown": {Stats: []*pbstat.Stats{
			spec("eth0", "02:00:00:00:00:09", "198.51.100.9/24"), bound("eth1")}},
		"ipv6 only": {Stats: []*pbstat.Stats{
			spec("eth0", "02:00:00:00:00:09", "2001:db8::9/64"), bound("eth0")}},
	} {
		if _, err := NodeAddr(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestApplyRefusesBeforeStoring: a malformed or port-sharing declaration is
// refused and nothing is stored.
func TestApplyRefusesBeforeStoring(t *testing.T) {
	ctx := context.Background()
	s := &Service{Handlers: New()}
	if _, err := s.Handlers.Apply(ctx, &pbaccess.ResourceL4RouteActionApplyArgsDTO{Name: "a", Port: "tcp:8443", Node: "g1"}); err != nil {
		t.Fatal(err)
	}
	for _, req := range []*pbaccess.ResourceL4RouteActionApplyArgsDTO{
		{Name: "b", Port: "tcp:8443", Node: "s2"},
		{Name: "c", Port: "tcp", Node: "s2"},
		{Name: "d", Port: "tcp:9000", Node: "*"},
	} {
		if _, err := s.Apply(ctx, req); err == nil {
			t.Errorf("%+v accepted", req)
		}
	}
	if d := s.Handlers.Desired(); len(d) != 1 || d[0].Name != "a" {
		t.Fatalf("store = %+v", d)
	}
}
