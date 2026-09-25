package podnet

import (
	"net/netip"
	"sync"
	"testing"
)

const sbx = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestNames(t *testing.T) {
	host, err := HostVethName(sbx)
	if err != nil || host != "ksc0123456789ab" || len(host) != 15 {
		t.Fatalf("HostVethName = %q, %v", host, err)
	}
	mac, err := PodMAC(sbx)
	if err != nil || mac.String() != "02:01:23:45:67:89" {
		t.Fatalf("PodMAC = %v, %v", mac, err)
	}
	if mac[0]&0x01 != 0 || mac[0]&0x02 == 0 {
		t.Fatalf("PodMAC %v must be unicast + locally administered", mac)
	}
	for _, bad := range []string{"", "0123", "zzzzzzzzzzzzzzzz"} {
		if _, err := HostVethName(bad); err == nil {
			t.Errorf("HostVethName(%q): expected error", bad)
		}
	}
}

func newTestIPAM(t *testing.T, subnet string) *IPAM {
	t.Helper()
	m, err := NewIPAM(t.TempDir(), netip.MustParsePrefix(subnet))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestIPAMAllocateReuseRelease(t *testing.T) {
	m := newTestIPAM(t, "10.200.0.0/24")
	a, err := m.Allocate("A")
	if err != nil || a.String() != "10.200.0.2" {
		t.Fatalf("first = %v, %v (want .2: .0 network, .1 skipped)", a, err)
	}
	b, _ := m.Allocate("B")
	if b.String() != "10.200.0.3" {
		t.Fatalf("second = %v", b)
	}
	if again, _ := m.Allocate("A"); again != a {
		t.Fatalf("retried ADD for A got %v, want %v", again, a)
	}
	if err := m.Release("A"); err != nil {
		t.Fatal(err)
	}
	if err := m.Release("A"); err != nil {
		t.Fatalf("double release must be a no-op: %v", err)
	}
	if _, ok, _ := m.Lookup("A"); ok {
		t.Fatal("A still holds an IP after release")
	}
	if c, _ := m.Allocate("C"); c != a {
		t.Fatalf("freed .2 not reused: got %v", c)
	}
}

func TestIPAMExhaustion(t *testing.T) {
	m := newTestIPAM(t, "10.200.0.0/29") // .2-.6 usable
	for i := 0; i < 5; i++ {
		if _, err := m.Allocate(string(rune('a' + i))); err != nil {
			t.Fatalf("alloc %d: %v", i, err)
		}
	}
	if ip, err := m.Allocate("f"); err == nil {
		t.Fatalf("expected exhaustion, got %v (broadcast .7 must not be handed out)", ip)
	}
}

func TestIPAMConcurrent(t *testing.T) {
	m := newTestIPAM(t, "10.200.0.0/24")
	var wg sync.WaitGroup
	got := make([]netip.Addr, 20)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ip, err := m.Allocate(string(rune('A' + i)))
			if err != nil {
				t.Error(err)
			}
			got[i] = ip
		}(i)
	}
	wg.Wait()
	seen := map[netip.Addr]bool{}
	for _, ip := range got {
		if seen[ip] {
			t.Fatalf("IP %v handed out twice", ip)
		}
		seen[ip] = true
	}
}

func TestIPAMRejectsBadSubnet(t *testing.T) {
	for _, s := range []string{"fd00::/64", "10.0.0.0/31"} {
		if _, err := NewIPAM(t.TempDir(), netip.MustParsePrefix(s)); err == nil {
			t.Errorf("%s: expected rejection", s)
		}
	}
}
