package openport

import "testing"

func TestParsePorts(t *testing.T) {
	ports, err := ParsePorts([]string{"tcp:443", "udp:443", "icmp:echo"})
	if err != nil {
		t.Fatalf("ParsePorts: %v", err)
	}
	if len(ports) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(ports))
	}
	if ports[0].Protocol != "tcp" || ports[0].Port != 443 {
		t.Errorf("ports[0] = %+v, want tcp:443", ports[0])
	}
	if ports[1].Protocol != "udp" || ports[1].Port != 443 {
		t.Errorf("ports[1] = %+v, want udp:443", ports[1])
	}
	// icmp has no port; echo maps to the Port-0 sentinel the router client renders
	// as "permit icmp any host <vip> echo".
	if ports[2].Protocol != "icmp" || ports[2].Port != 0 {
		t.Errorf("ports[2] = %+v, want icmp (port 0)", ports[2])
	}

	for _, bad := range []string{"icmp:0", "icmp:1", "tcp:echo", "icmp", "sctp:100"} {
		if _, err := ParsePorts([]string{bad}); err == nil {
			t.Errorf("ParsePorts(%q) succeeded, want error", bad)
		}
	}
}
