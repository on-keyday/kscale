package podnet

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// IPAM hands out pod IPs from one subnet, node-locally, the way the reference
// host-local plugin does: one file per allocated IP (named by the IP, holding
// the owner's container ID) under dir, all changes under an flock so concurrent
// plugin invocations serialize. Pod IPs never leave the node (traffic enters
// through the VIP), so no cross-node coordination is needed.
type IPAM struct {
	dir    string
	subnet netip.Prefix
}

func NewIPAM(dir string, subnet netip.Prefix) (*IPAM, error) {
	if !subnet.Addr().Is4() {
		return nil, fmt.Errorf("ipam: subnet %v: only IPv4 is supported", subnet)
	}
	if subnet.Bits() > 30 {
		return nil, fmt.Errorf("ipam: subnet %v: too small (need at least a /30)", subnet)
	}
	return &IPAM{dir: dir, subnet: subnet.Masked()}, nil
}

func (m *IPAM) lock() (func(), error) {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(m.dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil // closing drops the flock
}

// owned returns the IP already allocated to id, if any.
func (m *IPAM) owned(id string) (netip.Addr, bool, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return netip.Addr{}, false, err
	}
	for _, e := range entries {
		ip, err := netip.ParseAddr(e.Name())
		if err != nil {
			continue // the lock file
		}
		b, err := os.ReadFile(filepath.Join(m.dir, e.Name()))
		if err != nil {
			return netip.Addr{}, false, err
		}
		if strings.TrimSpace(string(b)) == id {
			return ip, true, nil
		}
	}
	return netip.Addr{}, false, nil
}

// Allocate returns id's IP, allocating the lowest free one if id has none yet.
// Calling it again for the same id returns the same IP (a retried ADD).
func (m *IPAM) Allocate(id string) (netip.Addr, error) {
	unlock, err := m.lock()
	if err != nil {
		return netip.Addr{}, err
	}
	defer unlock()
	if ip, ok, err := m.owned(id); err != nil || ok {
		return ip, err
	}
	// Skip the network address and .1; stop before the broadcast address.
	first := m.subnet.Addr().Next().Next()
	for ip := first; m.subnet.Contains(ip) && m.subnet.Contains(ip.Next()); ip = ip.Next() {
		f, err := os.OpenFile(filepath.Join(m.dir, ip.String()), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return netip.Addr{}, err
		}
		_, werr := f.WriteString(id)
		cerr := f.Close()
		if werr != nil || cerr != nil {
			os.Remove(f.Name())
			return netip.Addr{}, errors.Join(werr, cerr)
		}
		return ip, nil
	}
	return netip.Addr{}, fmt.Errorf("ipam: subnet %v exhausted", m.subnet)
}

// Release frees id's IP. Releasing an id that holds nothing is not an error (DEL
// must be idempotent).
func (m *IPAM) Release(id string) error {
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	defer unlock()
	ip, ok, err := m.owned(id)
	if err != nil || !ok {
		return err
	}
	return os.Remove(filepath.Join(m.dir, ip.String()))
}

// Lookup reports id's IP without allocating (CHECK).
func (m *IPAM) Lookup(id string) (netip.Addr, bool, error) {
	unlock, err := m.lock()
	if err != nil {
		return netip.Addr{}, false, err
	}
	defer unlock()
	return m.owned(id)
}
