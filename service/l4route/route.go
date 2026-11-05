// Package l4route is the l4_route resource: a VIP (proto, port) pinned to one node
// (B6). The generated store holds the declarations; this file resolves a
// declaration to the destination the l4lb encapsulates to, shared by the
// reconcile loop (reconcile/l4route.go) and the Diff action.
package l4route

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/on-keyday/kscale/dpbroker"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	pbstat "github.com/on-keyday/kscale/protobuf/proto/stat"
	"github.com/on-keyday/kscale/stat"
)

// ParsePort parses "tcp:8443" / "udp:9443" (the open_port syntax, tcp/udp only).
func ParsePort(s string) (proto string, port uint16, err error) {
	proto, num, ok := strings.Cut(s, ":")
	if !ok {
		return "", 0, fmt.Errorf("port %q: want proto:port (e.g. tcp:8443)", s)
	}
	if proto != "tcp" && proto != "udp" {
		return "", 0, fmt.Errorf("port %q: protocol must be tcp or udp", s)
	}
	n, err := strconv.ParseUint(num, 10, 16)
	if err != nil || n == 0 {
		return "", 0, fmt.Errorf("port %q: bad port number", s)
	}
	return proto, uint16(n), nil
}

// Validate checks one declaration on its own (the CP refuses a bad one at apply).
func Validate(name, port, node string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if _, _, err := ParsePort(port); err != nil {
		return err
	}
	if node == "" || node == "*" || strings.HasSuffix(node, "/*") {
		return fmt.Errorf("node %q: a route targets exactly one node (no wildcards)", node)
	}
	return nil
}

// ValidateSet rejects two declarations claiming the same proto:port — the XDP
// table has one destination per (VIP, proto, port).
func ValidateSet(items []*pbaccess.ResourceL4RouteActionGetResponseDTO) error {
	owner := map[string]string{}
	for _, it := range items {
		proto, port, err := ParsePort(it.Port)
		if err != nil {
			return fmt.Errorf("l4_route %q: %w", it.Name, err)
		}
		k := fmt.Sprintf("%s:%d", proto, port)
		if other, dup := owner[k]; dup {
			return fmt.Errorf("l4_route %q: %s is already routed by %q", it.Name, k, other)
		}
		owner[k] = it.Name
	}
	return nil
}

// StatSource is the CP's per-node stat cache (stats.Cache).
type StatSource interface {
	Get(commonName string) *pbstat.StatBatch
}

// Target is a declaration resolved to the node and address the l4lb sends to.
type Target struct {
	Name  string
	Proto string
	Port  uint16
	Node  string // the resolved node's CommonName
	Dest  stat.DestEntry
}

// Resolve resolves one declaration: the node selector must name exactly one
// connected node, and that node must report a bound interface with an address
// (the interface resource, plus --host-ingress on a workload node). Unlike an
// l4lb backend it needs no ServerID: the route bypasses the consistent hash.
func Resolve(it *pbaccess.ResourceL4RouteActionGetResponseDTO, broker *dpbroker.Broker, cache StatSource) (Target, error) {
	proto, port, err := ParsePort(it.Port)
	if err != nil {
		return Target{}, err
	}
	p, err := broker.Resolve(it.Node)
	if err != nil {
		return Target{}, err
	}
	dest, err := NodeAddr(cache.Get(p.CommonName()))
	if err != nil {
		return Target{}, fmt.Errorf("node %s: %w", p.CommonName(), err)
	}
	return Target{Name: it.Name, Proto: proto, Port: port, Node: p.CommonName(), Dest: dest}, nil
}

// NodeAddr reads a node's bound interface MAC/IP out of its reported stats: the
// NetworkSpec (base entry) and the first bound interface any entry reports.
func NodeAddr(batch *pbstat.StatBatch) (stat.DestEntry, error) {
	if batch == nil {
		return stat.DestEntry{}, fmt.Errorf("no stats reported yet")
	}
	var spec *pbstat.NetworkSpecStat
	iface := ""
	for _, s := range batch.Stats {
		if s.NetworkSpec != nil {
			spec = s.NetworkSpec
		}
		if iface == "" && s.CdnAppRealtime != nil && s.CdnAppRealtime.BoundIfaces != nil && len(s.CdnAppRealtime.BoundIfaces.Items) > 0 {
			iface = s.CdnAppRealtime.BoundIfaces.Items[0]
		}
	}
	if iface == "" {
		return stat.DestEntry{}, fmt.Errorf("no bound interface (apply the interface resource; a workload node also needs --host-ingress)")
	}
	if spec == nil {
		return stat.DestEntry{}, fmt.Errorf("no network spec reported yet")
	}
	var ns stat.NetworkSpecStat
	ns.FromProto(spec)
	d, err := stat.GetDestEntry(0, iface, &ns)
	if err != nil {
		return stat.DestEntry{}, err
	}
	if !d.IPAddr.Is4() || len(d.HardwareAddr) != 6 {
		return stat.DestEntry{}, fmt.Errorf("interface %s: need an IPv4 address and a 6-byte MAC (got %v / %v)", iface, d.IPAddr, d.HardwareAddr)
	}
	return d, nil
}
