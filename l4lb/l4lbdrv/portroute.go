package l4lbdrv

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"syscall"

	"github.com/cilium/ebpf"
)

// PortRoute pins VIP-destined traffic for one (Proto, Port) to Dest instead of
// spreading it over the popcache backends (lb.c port_route_map). It applies to
// every VIP this l4lb serves; the map key carries the VIP so that several VIPs
// can later get different routes (B8).
type PortRoute struct {
	Proto uint8  // syscall.IPPROTO_TCP or syscall.IPPROTO_UDP
	Port  uint16 // host byte order
	Dest  DestinationEntry
}

func (r PortRoute) String() string {
	proto := "tcp"
	if r.Proto == syscall.IPPROTO_UDP {
		proto = "udp"
	}
	return fmt.Sprintf("%s:%d->%s", proto, r.Port, r.Dest.IPAddr)
}

// ValidatePortRoutes rejects routes the XDP program cannot honour.
func ValidatePortRoutes(routes []PortRoute) error {
	seen := map[[2]uint16]bool{}
	for _, r := range routes {
		if r.Proto != syscall.IPPROTO_TCP && r.Proto != syscall.IPPROTO_UDP {
			return fmt.Errorf("port route %v: protocol %d is not tcp or udp", r, r.Proto)
		}
		if r.Port == 0 {
			return fmt.Errorf("port route %v: port 0", r)
		}
		if !r.Dest.IPAddr.Is4() {
			return fmt.Errorf("port route %v: destination must be an IPv4 address", r)
		}
		if len(r.Dest.HardwareAddr) != 6 {
			return fmt.Errorf("port route %v: destination MAC %v is not 6 bytes", r, r.Dest.HardwareAddr)
		}
		k := [2]uint16{uint16(r.Proto), r.Port}
		if seen[k] {
			return fmt.Errorf("port route %v: (proto, port) listed twice", r)
		}
		seen[k] = true
	}
	if len(routes) > PORT_ROUTE_MAP_SIZE {
		return fmt.Errorf("%d port routes exceed the map size %d", len(routes), PORT_ROUTE_MAP_SIZE)
	}
	return nil
}

// portRouteValue is struct port_route_value: a destination_entry followed by the
// action, packed (PORT_ROUTE_VALUE_SIZE bytes).
type portRouteValue struct {
	dest   DestinationEntry
	action uint32
}

func (v portRouteValue) MarshalBinary() ([]byte, error) {
	b, err := DestinationEntries{v.dest}.MarshalBinary()
	if err != nil {
		return nil, err
	}
	b = binary.NativeEndian.AppendUint32(b, v.action)
	if len(b) != PORT_ROUTE_VALUE_SIZE {
		return nil, fmt.Errorf("port route value is %d bytes, want %d", len(b), PORT_ROUTE_VALUE_SIZE)
	}
	return b, nil
}

// portRouteKey builds the map key for vip: IPv4 is stored v4-mapped, the port in
// network byte order.
func portRouteKey(vip netip.Addr, proto uint8, port uint16) PortRouteKey {
	var be [2]byte
	binary.BigEndian.PutUint16(be[:], port)
	return PortRouteKey{
		Vip:   netip.AddrFrom16(vip.As16()).As16(),
		Port:  binary.NativeEndian.Uint16(be[:]),
		Proto: proto,
	}
}

// desiredPortRoutes expands routes over the configured VIPs.
func desiredPortRoutes(cfg *DynamicConfig) map[PortRouteKey]portRouteValue {
	out := map[PortRouteKey]portRouteValue{}
	for _, vip := range []netip.Addr{cfg.VIP, cfg.VIPv6} {
		if !vip.IsValid() || vip.IsUnspecified() {
			continue
		}
		for _, r := range cfg.PortRoutes {
			out[portRouteKey(vip, r.Proto, r.Port)] = portRouteValue{
				dest:   DestinationEntry{IPAddr: r.Dest.IPAddr, HardwareAddr: r.Dest.HardwareAddr},
				action: PORT_ROUTE_ACTION_FORWARD,
			}
		}
	}
	return out
}

// syncPortRoutes makes port_route_map hold exactly the desired entries: stale
// keys (a removed route, or an old VIP) are deleted, the rest written.
func (lb *L4LB) syncPortRoutes() error {
	want := desiredPortRoutes(lb.cfg)
	var stale []PortRouteKey
	var key PortRouteKey
	var raw []byte
	it := lb.bindings.PortRouteMap.Iterate()
	for it.Next(&key, &raw) {
		if _, ok := want[key]; !ok {
			stale = append(stale, key)
		}
	}
	if err := it.Err(); err != nil {
		return fmt.Errorf("iterate port_route_map: %w", err)
	}
	for _, k := range stale {
		if err := lb.bindings.PortRouteMap.Delete(k); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			return fmt.Errorf("delete port route: %w", err)
		}
	}
	for k, v := range want {
		if err := lb.bindings.PortRouteMap.Put(k, v); err != nil {
			return fmt.Errorf("put port route: %w", err)
		}
	}
	return nil
}

// UpdatePortRoutes replaces the port routes and programs them.
func (lb *L4LB) UpdatePortRoutes(routes []PortRoute) error {
	if err := ValidatePortRoutes(routes); err != nil {
		return err
	}
	lb.cfg.PortRoutes = routes
	return lb.syncPortRoutes()
}
