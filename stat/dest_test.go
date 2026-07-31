package stat

import (
	"net"
	"net/netip"
	"testing"

	"github.com/on-keyday/kscale/consts"
	pbstat "github.com/on-keyday/kscale/protobuf/proto/stat"
)

// nodeStat builds a reported Stats for a node with one interface eth0.
func nodeStat(lbID uint32, bound string, status consts.AppStatus, mac string, ip string) *pbstat.Stats {
	hw, _ := net.ParseMAC(mac)
	ns := &NetworkSpecStat{
		Interfaces: []InterfaceSpecStat{
			{Name: "lo"},
			{Name: "eth0", Mac: hw, IPs: []netip.Prefix{netip.MustParsePrefix(ip)}},
		},
	}
	app := &CdnAppRealtimeStat{LoadBalancerID: lbID, BoundInterfaces: nil, AppStatus: status}
	if bound != "" {
		app.BoundInterfaces = []string{bound}
	}
	return &pbstat.Stats{NetworkSpec: ns.ToProto(), CdnAppRealtime: app.ToProto()}
}

func TestGetDestEntryFromStat_MembershipGate(t *testing.T) {
	// Ready backend: lbID set, bound eth0, running -> a dest with eth0's MAC/IP.
	d, err := GetDestEntryFromStat(nodeStat(7, "eth0", consts.AppStatusRunning, "02:00:00:00:00:07", "10.0.0.7/24"))
	if err != nil {
		t.Fatalf("ready backend: %v", err)
	}
	if d.ServerID != 7 || d.IPAddr.String() != "10.0.0.7" || d.HardwareAddr.String() != "02:00:00:00:00:07" {
		t.Fatalf("wrong dest: %+v", d)
	}
	// Not running / no bound interface -> skipped (ErrNoBoundInterface), not fatal.
	if _, err := GetDestEntryFromStat(nodeStat(7, "eth0", consts.AppStatus(0), "02:00:00:00:00:07", "10.0.0.7/24")); err != ErrNoBoundInterface {
		t.Fatalf("not-running: want ErrNoBoundInterface, got %v", err)
	}
	if _, err := GetDestEntryFromStat(nodeStat(7, "", consts.AppStatusRunning, "02:00:00:00:00:07", "10.0.0.7/24")); err != ErrNoBoundInterface {
		t.Fatalf("no-bound: want ErrNoBoundInterface, got %v", err)
	}
	// Zero LbId -> hard error (missing assignment).
	if _, err := GetDestEntryFromStat(nodeStat(0, "eth0", consts.AppStatusRunning, "02:00:00:00:00:07", "10.0.0.7/24")); err == nil {
		t.Fatal("zero LbId: want error")
	}
}

func TestDestManager_DiffFires(t *testing.T) {
	self := DestEntry{ServerID: 1, HardwareAddr: net.HardwareAddr{0x02, 0, 0, 0, 0, 1}, IPAddr: netip.MustParseAddr("10.0.0.1")}
	peers := []*pbstat.Stats{nodeStat(7, "eth0", consts.AppStatusRunning, "02:00:00:00:00:07", "10.0.0.7/24")}
	var m DestManager
	fires := 0
	push := func([]DestEntry) error { fires++; return nil }

	if err := m.UpdateFromStats(self, peers, push); err != nil {
		t.Fatal(err)
	}
	if fires != 1 || len(m.DestList) != 2 { // self + 1 backend
		t.Fatalf("first apply: fires=%d list=%d", fires, len(m.DestList))
	}
	// Identical report -> no fire (idempotent).
	if err := m.UpdateFromStats(self, peers, push); err != nil {
		t.Fatal(err)
	}
	if fires != 1 {
		t.Fatalf("idempotent re-apply fired again: fires=%d", fires)
	}
	// A new backend -> fire.
	peers = append(peers, nodeStat(8, "eth0", consts.AppStatusRunning, "02:00:00:00:00:08", "10.0.0.8/24"))
	if err := m.UpdateFromStats(self, peers, push); err != nil {
		t.Fatal(err)
	}
	if fires != 2 || len(m.DestList) != 3 {
		t.Fatalf("added backend: fires=%d list=%d", fires, len(m.DestList))
	}
}
