package iface

import (
	"reflect"
	"testing"

	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
)

func items(sel ...string) []*pbaccess.ResourceInterfaceActionGetResponseDTO {
	var out []*pbaccess.ResourceInterfaceActionGetResponseDTO
	for i := 0; i < len(sel); i += 2 {
		out = append(out, &pbaccess.ResourceInterfaceActionGetResponseDTO{Node: sel[i], Interface: sel[i+1]})
	}
	return out
}

func applied(its []*pbaccess.ResourceInterfaceActionGetResponseDTO) map[string][]string {
	m := map[string][]string{}
	for _, it := range its {
		m[it.Node] = it.AppliedOn
	}
	return m
}

// The live case: popcache and workload share hosts and NIC name. Each node must
// count only under the declaration that targets it.
func TestAppliedOnFollowsSelector(t *testing.T) {
	its := items(
		"popcache/*", "enp2s0f1",
		"s1.workload.dp.system.kscale.local", "enp2s0f1",
		"s2.workload.dp.system.kscale.local", "enp2s0f1",
	)
	observed := map[string][]string{
		"s1.popcache.dp.system.kscale.local": {"enp2s0f1"},
		"s2.popcache.dp.system.kscale.local": {"enp2s0f1"},
		"s1.workload.dp.system.kscale.local": {"enp2s0f1"},
	}
	dp := map[string]string{
		"s1.popcache.dp.system.kscale.local": "popcache",
		"s2.popcache.dp.system.kscale.local": "popcache",
		"s1.workload.dp.system.kscale.local": "workload",
	}
	attributeAppliedOn(its, observed, dp)
	want := map[string][]string{
		"popcache/*":                         {"s1.popcache.dp.system.kscale.local", "s2.popcache.dp.system.kscale.local"},
		"s1.workload.dp.system.kscale.local": {"s1.workload.dp.system.kscale.local"},
		"s2.workload.dp.system.kscale.local": nil, // s2's agent reports nothing bound
	}
	if got := applied(its); !reflect.DeepEqual(got, want) {
		t.Fatalf("applied_on = %v\nwant %v", got, want)
	}
}

func TestAppliedOnMostSpecificWinsAndRequiresMatchingIface(t *testing.T) {
	its := items("*", "eth0", "l4lb/*", "enp4s0", "l4lb/s3", "enp9s0")
	observed := map[string][]string{
		"s3.l4lb.dp.system.kscale.local": {"enp4s0"}, // bound the group's NIC, not its own declaration's
		"s4.l4lb.dp.system.kscale.local": {"enp4s0"},
		"s1.dns.dp.system.kscale.local":  {"eth0"},
	}
	dp := map[string]string{
		"s3.l4lb.dp.system.kscale.local": "l4lb",
		"s4.l4lb.dp.system.kscale.local": "l4lb",
		"s1.dns.dp.system.kscale.local":  "dns",
	}
	attributeAppliedOn(its, observed, dp)
	want := map[string][]string{
		"*":       {"s1.dns.dp.system.kscale.local"},
		"l4lb/*":  {"s4.l4lb.dp.system.kscale.local"},
		"l4lb/s3": nil, // s3 is governed by l4lb/s3 and has not bound enp9s0
	}
	if got := applied(its); !reflect.DeepEqual(got, want) {
		t.Fatalf("applied_on = %v\nwant %v", got, want)
	}
}
