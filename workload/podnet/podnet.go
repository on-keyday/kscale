// Package podnet holds what the kscale CNI plugin (cmd/kscale-cni, root, run
// once per sandbox) and the workload eBPF datapath (in workloadagent, non-root)
// must agree on without talking to each other: the naming rules that turn a
// sandbox ID into the host-side veth name and the pod's MAC, the fixed gateway
// address, and the node-local IPAM. Because both sides derive the same names
// from the sandbox ID (which the agent reads back from CRI), the plugin never
// needs a channel to the agent.
// See notes/ai/2026_09_25_workload_pod_netns_ebpf_design.md.
package podnet

import (
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
)

// GatewayIP is the pod's default gateway. It is never assigned to any interface:
// the plugin installs a permanent neighbor entry for it pointing at the host-side
// veth's MAC, so every packet the pod sends leaves through that veth (where the
// datapath's egress program sees it) without ARP.
var GatewayIP = netip.MustParseAddr("169.254.1.1")

// idPrefixLen is how many hex chars of the sandbox ID the names use: "ksc" + 12
// = 15 chars, the Linux interface-name limit (IFNAMSIZ-1).
const idPrefixLen = 12

func idPrefix(sandboxID string) (string, error) {
	if len(sandboxID) < idPrefixLen {
		return "", fmt.Errorf("sandbox id %q: shorter than %d chars", sandboxID, idPrefixLen)
	}
	p := sandboxID[:idPrefixLen]
	if _, err := hex.DecodeString(p); err != nil {
		return "", fmt.Errorf("sandbox id %q: not hex", sandboxID)
	}
	return p, nil
}

// HostVethName is the host-side end of the pod's veth pair.
func HostVethName(sandboxID string) (string, error) {
	p, err := idPrefix(sandboxID)
	if err != nil {
		return "", err
	}
	return "ksc" + p, nil
}

// PodMAC is the pod-side interface's MAC: 02 (locally administered, unicast)
// followed by the first 5 bytes of the sandbox ID.
func PodMAC(sandboxID string) (net.HardwareAddr, error) {
	p, err := idPrefix(sandboxID)
	if err != nil {
		return nil, err
	}
	b, _ := hex.DecodeString(p[:10])
	return append(net.HardwareAddr{0x02}, b...), nil
}
