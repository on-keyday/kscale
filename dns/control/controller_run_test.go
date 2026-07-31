package control_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"

	"codeberg.org/miekg/dns"
	"github.com/on-keyday/kscale/dns/control"
	"github.com/on-keyday/kscale/dns/server"
)

// TestBuiltinServerEndToEnd drives the dns controller exactly as the dnsagent's
// DnsControlService handlers would (SetDomain / SetPort / UpdateVip / Start), then
// sends a real A query over UDP to the live built-in authoritative server and
// asserts the configured VIP comes back — plus that the request was counted in the
// controller's Stats() (the same snapshot the substrate streams northbound).
func TestBuiltinServerEndToEnd(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := control.New(logger, server.NewServer(logger))

	const port = 15353
	const dom = "example.test"
	vip := netip.MustParseAddr("192.0.2.10")

	if err := c.SetDomain(dom); err != nil {
		t.Fatalf("SetDomain: %v", err)
	}
	if err := c.SetPort(port); err != nil {
		t.Fatalf("SetPort: %v", err)
	}
	if err := c.UpdateVip(vip, false); err != nil {
		t.Fatalf("UpdateVip: %v", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer c.Stop()
	time.Sleep(50 * time.Millisecond) // let the listener come up

	// Craft an A query for the apex domain using the codeberg/miekg API.
	q := &dns.Msg{}
	q.ID = 0x1234
	q.RecursionDesired = true
	q.Question = []dns.RR{&dns.A{Hdr: dns.Header{Name: dom + ".", Class: dns.ClassINET}}}
	if err := q.Pack(); err != nil {
		t.Fatalf("pack query: %v", err)
	}

	conn, err := net.Dial("udp", net.JoinHostPort("127.0.0.1", "15353"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(q.Data); err != nil {
		t.Fatalf("write query: %v", err)
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	resp := &dns.Msg{}
	resp.Data = buf[:n]
	if err := resp.Unpack(); err != nil {
		t.Fatalf("unpack response: %v", err)
	}
	if len(resp.Answer) == 0 {
		t.Fatalf("no answer records; rcode=%d", resp.Rcode)
	}
	a, ok := resp.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("answer[0] is %T, want *dns.A", resp.Answer[0])
	}
	if want := net.IP(vip.AsSlice()); !a.A.Equal(want) {
		t.Fatalf("A record = %v, want %v", a.A, want)
	}

	// The served request must show up in the controller's stat snapshot — now a
	// typed proto field (requests_total), not an opaque bytes blob — exactly as the
	// substrate streams it northbound.
	var found bool
	for _, s := range c.Stats() {
		if s.Dns != nil && s.Dns.RequestsTotal >= 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("Stats() did not report the served request as a typed requests_total")
	}
}

// TestStatsReportAppliedVip asserts the dns dp reports the VIPs it applied at
// CdnAppRealtime.Vip — the CP's VipObserver derives vip.applied_on from exactly
// that stat entry, so without it dns nodes never show up even though UpdateVip
// configured the A-record answer. popcache/l4lb/router all report it.
func TestStatsReportAppliedVip(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := control.New(logger, server.NewServer(logger))

	vip := netip.MustParseAddr("192.0.2.10")
	if err := c.UpdateVip(vip, false); err != nil {
		t.Fatalf("UpdateVip: %v", err)
	}

	var got []string
	for _, s := range c.Stats() {
		if s.CdnAppRealtime != nil && s.CdnAppRealtime.Vip != nil {
			got = append(got, s.CdnAppRealtime.Vip.Items...)
		}
	}
	if len(got) != 1 || got[0] != vip.String() {
		t.Fatalf("Stats() CdnAppRealtime.Vip = %v, want [%s]", got, vip)
	}
}
