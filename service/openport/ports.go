package openport

import (
	"fmt"
	"strconv"
	"strings"

	pb "github.com/on-keyday/kscale/protobuf/proto"
)

// ParsePorts converts the resource's "proto:port" strings (e.g. "tcp:80") into the
// southbound PortInfo representation. "icmp:echo" (no port; Port-0 sentinel) permits
// ICMP echo requests to the VIP — the router-side gate for l4lb's vip.icmp_echo.
// Protocols the router client can't render/parse back are rejected here: a synced
// line the ACL parser can't see is never removed and re-added on every sync.
//
// This is the ONE parser for the open_port value syntax — both the reconcile loop
// and the Diff action bind through it (a past local copy in Diff drifted and
// rejected icmp:echo).
func ParsePorts(ss []string) ([]*pb.PortInfo, error) {
	out := make([]*pb.PortInfo, 0, len(ss))
	for _, s := range ss {
		proto, portStr, ok := strings.Cut(s, ":")
		if !ok || proto == "" {
			return nil, fmt.Errorf("bad port %q (want proto:port, e.g. tcp:80 or icmp:echo)", s)
		}
		switch proto {
		case "icmp":
			if portStr != "echo" {
				return nil, fmt.Errorf("bad port %q (icmp supports only icmp:echo)", s)
			}
			out = append(out, &pb.PortInfo{Protocol: "icmp", Port: 0})
		case "tcp", "udp":
			port, err := strconv.ParseUint(portStr, 10, 16)
			if err != nil {
				return nil, fmt.Errorf("bad port %q: %w", s, err)
			}
			out = append(out, &pb.PortInfo{Protocol: proto, Port: uint16(port)})
		default:
			return nil, fmt.Errorf("bad port %q (protocol must be tcp, udp or icmp)", s)
		}
	}
	return out, nil
}
