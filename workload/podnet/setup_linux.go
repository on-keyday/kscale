package podnet

import (
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// Wired describes a pod interface after Setup, for the CNI result.
type Wired struct {
	HostVeth    string
	HostMAC     net.HardwareAddr
	PodIfName   string
	PodMAC      net.HardwareAddr
	PodIP       netip.Addr
	NetnsPath   string
	GatewayAddr netip.Addr
}

// Setup wires a pod netns: a veth pair whose pod end (ifName, PodMAC) lives in
// the netns at netnsPath with podIP/32, a scope-link route to GatewayIP, a
// default route via it, and a permanent neighbor entry GatewayIP -> host-side
// veth MAC. Any leftover host veth of the same sandbox (a retried ADD) is
// removed first. Runs as root (the CNI plugin).
func Setup(sandboxID, netnsPath, ifName string, podIP netip.Addr, mtu int) (*Wired, error) {
	hostName, err := HostVethName(sandboxID)
	if err != nil {
		return nil, err
	}
	podMAC, err := PodMAC(sandboxID)
	if err != nil {
		return nil, err
	}
	ns, err := netns.GetFromPath(netnsPath)
	if err != nil {
		return nil, fmt.Errorf("open netns %s: %w", netnsPath, err)
	}
	defer ns.Close()
	nsh, err := netlink.NewHandleAt(ns)
	if err != nil {
		return nil, fmt.Errorf("netlink handle in %s: %w", netnsPath, err)
	}
	defer nsh.Close()

	if old, err := netlink.LinkByName(hostName); err == nil {
		if err := netlink.LinkDel(old); err != nil {
			return nil, fmt.Errorf("remove stale %s: %w", hostName, err)
		}
	}
	if old, err := nsh.LinkByName(ifName); err == nil {
		_ = nsh.LinkDel(old) // a half-wired pod end from an earlier attempt
	}

	veth := &netlink.Veth{
		LinkAttrs:        netlink.LinkAttrs{Name: hostName, MTU: mtu},
		PeerName:         ifName,
		PeerHardwareAddr: podMAC,
		PeerNamespace:    netlink.NsFd(int(ns)),
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return nil, fmt.Errorf("add veth %s: %w", hostName, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = netlink.LinkDel(veth) // takes the pod end with it
		}
	}()

	host, err := netlink.LinkByName(hostName)
	if err != nil {
		return nil, err
	}
	if err := netlink.LinkSetUp(host); err != nil {
		return nil, fmt.Errorf("up %s: %w", hostName, err)
	}

	pod, err := nsh.LinkByName(ifName)
	if err != nil {
		return nil, fmt.Errorf("pod end %s: %w", ifName, err)
	}
	if lo, err := nsh.LinkByName("lo"); err == nil {
		if err := nsh.LinkSetUp(lo); err != nil {
			return nil, fmt.Errorf("up lo: %w", err)
		}
	}
	addr := &netlink.Addr{IPNet: &net.IPNet{IP: podIP.AsSlice(), Mask: net.CIDRMask(32, 32)}}
	if err := nsh.AddrAdd(pod, addr); err != nil {
		return nil, fmt.Errorf("addr %v: %w", podIP, err)
	}
	if err := nsh.LinkSetUp(pod); err != nil {
		return nil, fmt.Errorf("up %s: %w", ifName, err)
	}
	gw := GatewayIP.AsSlice()
	if err := nsh.RouteAdd(&netlink.Route{
		LinkIndex: pod.Attrs().Index,
		Dst:       &net.IPNet{IP: gw, Mask: net.CIDRMask(32, 32)},
		Scope:     netlink.SCOPE_LINK,
	}); err != nil {
		return nil, fmt.Errorf("gateway route: %w", err)
	}
	if err := nsh.RouteAdd(&netlink.Route{LinkIndex: pod.Attrs().Index, Gw: gw}); err != nil {
		return nil, fmt.Errorf("default route: %w", err)
	}
	if err := nsh.NeighAdd(&netlink.Neigh{
		LinkIndex:    pod.Attrs().Index,
		Family:       netlink.FAMILY_V4,
		State:        netlink.NUD_PERMANENT,
		IP:           gw,
		HardwareAddr: host.Attrs().HardwareAddr,
	}); err != nil {
		return nil, fmt.Errorf("gateway neighbor: %w", err)
	}

	ok = true
	return &Wired{
		HostVeth:    hostName,
		HostMAC:     host.Attrs().HardwareAddr,
		PodIfName:   ifName,
		PodMAC:      podMAC,
		PodIP:       podIP,
		NetnsPath:   netnsPath,
		GatewayAddr: GatewayIP,
	}, nil
}

// Teardown removes the sandbox's host veth (its pod end goes with it). Missing is
// fine: DEL must be idempotent, and the netns may already be gone.
func Teardown(sandboxID string) error {
	hostName, err := HostVethName(sandboxID)
	if err != nil {
		return err
	}
	link, err := netlink.LinkByName(hostName)
	if err != nil {
		var nf netlink.LinkNotFoundError
		if errors.As(err, &nf) {
			return nil
		}
		return err
	}
	return netlink.LinkDel(link)
}

// Check verifies the host veth exists and is up (CNI CHECK).
func Check(sandboxID string) error {
	hostName, err := HostVethName(sandboxID)
	if err != nil {
		return err
	}
	link, err := netlink.LinkByName(hostName)
	if err != nil {
		return fmt.Errorf("host veth %s: %w", hostName, err)
	}
	if link.Attrs().Flags&net.FlagUp == 0 {
		return fmt.Errorf("host veth %s is down", hostName)
	}
	return nil
}

// LoopbackUp brings lo up in the netns at netnsPath (the "loopback" CNI plugin
// containerd runs for every pod sandbox).
func LoopbackUp(netnsPath string) error {
	ns, err := netns.GetFromPath(netnsPath)
	if err != nil {
		return fmt.Errorf("open netns %s: %w", netnsPath, err)
	}
	defer ns.Close()
	nsh, err := netlink.NewHandleAt(ns)
	if err != nil {
		return err
	}
	defer nsh.Close()
	lo, err := nsh.LinkByName("lo")
	if err != nil {
		return err
	}
	return nsh.LinkSetUp(lo)
}
