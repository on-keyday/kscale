package control

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"syscall"
	"testing"

	"github.com/on-keyday/kscale/dataplane"
	"github.com/on-keyday/kscale/l4lb/l4lbdrv"
	"github.com/on-keyday/kscale/stat"
)

type routeDriver struct {
	StubDriver
	got [][]l4lbdrv.PortRoute
}

func (d *routeDriver) UpdatePortRoutes(r []l4lbdrv.PortRoute) error {
	d.got = append(d.got, r)
	return nil
}

// TestSetPortRoutesStagedThenApplied: a push before StartDataplane is staged in
// the dynamic config (which the driver is built from); after start it is applied
// to the live driver. A malformed route is refused either way.
func TestSetPortRoutesStagedThenApplied(t *testing.T) {
	d := &routeDriver{StubDriver: StubDriver{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	var builtWith []l4lbdrv.PortRoute
	c := New(&l4lbdrv.FixedConfig{}, func(_ *l4lbdrv.FixedConfig, dyn *l4lbdrv.DynamicConfig) (Driver, error) {
		builtWith = dyn.PortRoutes
		return d, nil
	}, nil)
	dest := stat.DestEntry{IPAddr: netip.MustParseAddr("10.3.10.9"), HardwareAddr: net.HardwareAddr{2, 0, 0, 0, 0, 9}}
	routes := []dataplane.PortRoute{{Proto: "tcp", Port: 8443, Dest: dest}, {Proto: "udp", Port: 9443, Dest: dest}}

	if err := c.SetPortRoutes(routes); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(builtWith) != 2 || builtWith[0].Proto != syscall.IPPROTO_TCP || builtWith[1].Proto != syscall.IPPROTO_UDP || builtWith[1].Port != 9443 {
		t.Fatalf("driver built with %+v", builtWith)
	}
	if err := c.SetPortRoutes(routes[:1]); err != nil {
		t.Fatal(err)
	}
	if len(d.got) != 1 || len(d.got[0]) != 1 {
		t.Fatalf("live updates = %+v", d.got)
	}
	for _, bad := range [][]dataplane.PortRoute{
		{{Proto: "icmp", Port: 1, Dest: dest}},
		{{Proto: "tcp", Port: 8443, Dest: stat.DestEntry{IPAddr: netip.MustParseAddr("2001:db8::9"), HardwareAddr: dest.HardwareAddr}}},
		{routes[0], routes[0]},
	} {
		if err := c.SetPortRoutes(bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if len(d.got) != 1 {
		t.Fatalf("a refused set reached the driver: %+v", d.got)
	}
}
