package vip

import (
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
)

type RemoteInfo struct {
	Address    netip.Addr
	MacAddress [6]byte
}

type Remote struct {
	Remote RemoteInfo
	Link   *netlink.Iptun
}

type VIPManager struct {
	mgrLock                   sync.Mutex
	VIPDevice                 string
	vipDeviceRPFilterDisabled bool
	RemoteList                []*Remote
	VIP                       netip.Prefix
	LocalAddr                 netip.Addr
	PhyDev                    int
}

func DisableRPFilter(ifName string) error {
	return os.WriteFile(fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/rp_filter", ifName), []byte("0"), 0644)
}

func NewVIPManager() (*VIPManager, error) {
	return &VIPManager{VIPDevice: "lo"}, nil
}

func (m *VIPManager) BindToDevice(dev string, logger *slog.Logger) error {
	m.mgrLock.Lock()
	defer m.mgrLock.Unlock()
	link, err := netlink.LinkByName(dev)
	if err != nil {
		return fmt.Errorf("failed to find device %s: %w", dev, err)
	}
	addresses, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("failed to list addresses on device %s: %w", dev, err)
	}
	var found bool
	var boundAddr netip.Addr
	for _, addr := range addresses {
		ipAddr, ok := netip.AddrFromSlice(addr.IP)
		if !ok {
			continue
		}
		boundAddr = ipAddr
		found = true
		break
	}
	if !found {
		return fmt.Errorf("no address found on device %s", dev)
	}
	if err := DisableRPFilter(dev); err != nil {
		return fmt.Errorf("failed to disable rp_filter on %s: %w", dev, err)
	}
	// reset rp_filter on old device if changed
	var old []RemoteInfo
	if m.LocalAddr != boundAddr { // remove old bindings
		old = make([]RemoteInfo, len(m.RemoteList))
		for i, r := range m.RemoteList {
			old[i] = r.Remote
		}
		err := m.updateRemote([]RemoteInfo{}, logger)
		if err != nil {
			return fmt.Errorf("failed to clear old remote bindings: %w", err)
		}
	}
	m.LocalAddr = boundAddr
	m.PhyDev = link.Attrs().Index
	logger.Info("Bound VIP manager to device", "device", dev, "index", m.PhyDev, "address", m.LocalAddr)
	if len(old) > 0 {
		err := m.updateRemote(old, logger)
		if err != nil {
			return fmt.Errorf("failed to restore old remote bindings: %w", err)
		}
	}
	return nil
}
func (m *VIPManager) UpdateRemote(remotes []RemoteInfo, logger *slog.Logger) error {
	m.mgrLock.Lock()
	defer m.mgrLock.Unlock()
	return m.updateRemote(remotes, logger)
}

func (m *VIPManager) updateRemote(remotes []RemoteInfo, logger *slog.Logger) error {
	newRemoteList := make([]*Remote, len(remotes))
	for i, remote := range remotes {
		hexMac := hex.EncodeToString(remote.MacAddress[:])
		ipTun := &netlink.Iptun{
			LinkAttrs: netlink.LinkAttrs{
				Name:        hexMac,
				ParentIndex: m.PhyDev,
			},
			Local:  m.LocalAddr.AsSlice(),
			Remote: remote.Address.AsSlice(),
		}
		if err := netlink.LinkAdd(ipTun); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("failed to add iptun link %s: %w", hexMac, err)
			}
			existingLink, err := netlink.LinkByName(hexMac)
			if err != nil {
				return fmt.Errorf("failed to find existing iptun link %s: %w", hexMac, err)
			}
			// delete and recreate
			if err := netlink.LinkDel(existingLink); err != nil {
				return fmt.Errorf("failed to delete existing iptun link %s: %w", hexMac, err)
			}
			time.Sleep(100 * time.Millisecond) // Give some time for the link to be removed
			if err := netlink.LinkAdd(ipTun); err != nil {
				return fmt.Errorf("failed to recreate iptun link %s: %w", hexMac, err)
			}
		}
		if err := netlink.LinkSetUp(ipTun); err != nil {
			return fmt.Errorf("failed to set up iptun link %s: %w", hexMac, err)
		}
		time.Sleep(100 * time.Millisecond) // Give some time for the link to be up
		if err := DisableRPFilter(hexMac); err != nil {
			return fmt.Errorf("failed to disable rp_filter on %s: %w", hexMac, err)
		}
		newRemoteList[i] = &Remote{
			Remote: remote,
			Link:   ipTun,
		}
	}
	// Remove old links
	for _, oldRemote := range m.RemoteList {
		found := false
		for _, newRemote := range newRemoteList {
			if oldRemote.Remote.Address == newRemote.Remote.Address {
				found = true
				break
			}
		}
		if !found {
			if err := netlink.LinkDel(oldRemote.Link); err != nil {
				logger.Error("Failed to delete old iptun link", "link", oldRemote.Link.Name, "error", err)
			}
		}
	}
	m.RemoteList = newRemoteList
	logger.Info("Updated remote list", "count", len(m.RemoteList))
	return nil
}

func (m *VIPManager) UpdateVIP(vip netip.Prefix, logger *slog.Logger) error {
	m.mgrLock.Lock()
	defer m.mgrLock.Unlock()
	if !m.vipDeviceRPFilterDisabled {
		if err := DisableRPFilter(m.VIPDevice); err != nil {
			return fmt.Errorf("failed to disable rp_filter on %s: %w", m.VIPDevice, err)
		}
		m.vipDeviceRPFilterDisabled = true
	}
	link, err := netlink.LinkByName(m.VIPDevice)
	if err != nil {
		return err
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return err
	}
	var alreadySet bool
	for _, addr := range addrs {
		curAddr, ok := netip.AddrFromSlice(addr.IP)
		if !ok {
			continue
		}
		if vip.Addr() == curAddr {
			alreadySet = true
			continue
		}
		if curAddr.IsLoopback() {
			continue
		}
		if err := netlink.AddrDel(link, &netlink.Addr{IPNet: &net.IPNet{
			IP:   curAddr.AsSlice(),
			Mask: net.CIDRMask(curAddr.BitLen(), curAddr.BitLen()),
		}}); err != nil {
			logger.Error("Failed to remove addr", "addr", addr, "dev", m.VIPDevice, "error", err)
		}
	}
	if alreadySet {
		return nil
	}
	if err := netlink.AddrAdd(link, &netlink.Addr{IPNet: &net.IPNet{
		IP:   vip.Addr().AsSlice(),
		Mask: net.CIDRMask(vip.Bits(), vip.Addr().BitLen()),
	}}); err != nil {
		return fmt.Errorf("failed to add VIP %s to device %s: %w", vip, m.VIPDevice, err)
	}

	m.VIP = vip
	logger.Info("VIP updated", "vip", vip, "dev", m.VIPDevice)
	return nil
}
