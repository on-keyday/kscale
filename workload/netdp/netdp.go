// Package netdp is the workload node's eBPF datapath (the userspace half of
// workload/netdp/c/netdp.c): it loads the object, attaches the ingress program
// to the bound NIC and the egress program to each pod's host-side veth (tcx),
// and keeps the maps in step with what the agent learns — the VIP (UpdateVip),
// the l4lb fronts allowed to send IPIP (UpdateRemote), and the pod endpoints and
// their ports (the workload engine). It runs in workloadagent, non-root, with
// CAP_BPF + CAP_NET_ADMIN; it never enters a pod netns (the CNI plugin wired
// that), so it needs no CAP_SYS_ADMIN.
// See notes/ai/2026_09_25_workload_pod_netns_ebpf_design.md.
package netdp

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"sync"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/vishvananda/netlink"

	"github.com/on-keyday/kscale/workload"
	"github.com/on-keyday/kscale/workload/netdp/netdpmetrics"
	"github.com/on-keyday/kscale/workload/podnet"
)

// Go mirrors of the C map types. Network-order fields are byte arrays so the
// host's endianness never enters into it.
type config struct {
	VIP        [4]byte
	NICIfindex uint32
}

type portKey struct {
	Proto uint8
	Pad   uint8
	Port  [2]byte
}

type podDest struct {
	Ifindex uint32
	PodIP   [4]byte
	PodMAC  [6]byte
	HostMAC [6]byte
}

type outKey struct {
	PodIP [4]byte
	Proto uint8
	Pad   uint8
	Port  [2]byte
}

const ipprotoTCP = 6

// Counter names, in the C enum's order.
var counterNames = []string{"in_steered", "in_not_lb_src", "in_no_port", "in_err", "out_snat", "out_err"}

type Datapath struct {
	logger *slog.Logger

	mu        sync.Mutex
	coll      *ebpf.Collection
	cfg       config
	nic       string
	nicLink   link.Link
	vethLinks map[string]link.Link // host veth name -> egress tcx link
}

// Load loads the datapath object at objPath. Nothing is attached until
// BindInterface / SetEndpoints.
func Load(objPath string, logger *slog.Logger) (*Datapath, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	spec, err := ebpf.LoadCollectionSpec(objPath)
	if err != nil {
		return nil, fmt.Errorf("netdp: load %s: %w", objPath, err)
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		return nil, fmt.Errorf("netdp: create collection: %w", err)
	}
	return &Datapath{logger: logger, coll: coll, vethLinks: map[string]link.Link{}}, nil
}

func (d *Datapath) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var errs []error
	if d.nicLink != nil {
		errs = append(errs, d.nicLink.Close())
	}
	for _, l := range d.vethLinks {
		errs = append(errs, l.Close())
	}
	d.coll.Close()
	return errors.Join(errs...)
}

func (d *Datapath) writeConfig() error {
	return d.coll.Maps["netdp_config"].Put(uint32(0), d.cfg)
}

// BindInterface attaches the ingress program to the NIC l4lb's IPIP arrives on
// (and replies leave by). Rebinding to another NIC moves it.
func (d *Datapath) BindInterface(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if name == d.nic && d.nicLink != nil {
		return nil
	}
	l, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("netdp: interface %q: %w", name, err)
	}
	tl, err := link.AttachTCX(link.TCXOptions{
		Interface: l.Attrs().Index,
		Program:   d.coll.Programs["netdp_ingress"],
		Attach:    ebpf.AttachTCXIngress,
	})
	if err != nil {
		return fmt.Errorf("netdp: attach ingress on %s: %w", name, err)
	}
	if d.nicLink != nil {
		_ = d.nicLink.Close()
	}
	d.nic, d.nicLink = name, tl
	d.cfg.NICIfindex = uint32(l.Attrs().Index)
	d.logger.Info("netdp: ingress attached", "interface", name, "ifindex", l.Attrs().Index)
	return d.writeConfig()
}

// SetVIP sets the address the datapath steers (and SNATs replies to).
func (d *Datapath) SetVIP(vip netip.Addr) error {
	if !vip.Is4() {
		return fmt.Errorf("netdp: VIP %v: only IPv4 is steered", vip)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cfg.VIP = vip.As4()
	return d.writeConfig()
}

// SetLBSources replaces the set of l4lb fronts whose IPIP the ingress program
// will decap. IPIP from any other source is left to the kernel.
func (d *Datapath) SetLBSources(srcs []netip.Addr) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	want := map[[4]byte]bool{}
	for _, a := range srcs {
		if a.Is4() {
			want[a.As4()] = true
		}
	}
	m := d.coll.Maps["netdp_lb_srcs"]
	return syncSet(m, want, uint8(1))
}

// SetEndpoints makes the port maps and egress attachments match eps. A port
// claimed by two endpoints goes to the first by name (eps is sorted by name) and
// the other claim is reported in the returned error; an endpoint whose host veth
// is not there yet is skipped and reported (the next sync retries).
func (d *Datapath) SetEndpoints(eps []workload.Endpoint) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var errs []error
	in := map[portKey]podDest{}
	out := map[outKey]bool{}
	veths := map[string]int{}
	owner := map[portKey]string{}
	for _, ep := range eps {
		name, err := podnet.HostVethName(ep.SandboxID)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ep.Name, err))
			continue
		}
		l, err := netlink.LinkByName(name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: host veth %s: %w", ep.Name, name, err))
			continue
		}
		podMAC, _ := podnet.PodMAC(ep.SandboxID)
		dest := podDest{Ifindex: uint32(l.Attrs().Index), PodIP: ep.PodIP.As4()}
		copy(dest.PodMAC[:], podMAC)
		copy(dest.HostMAC[:], l.Attrs().HardwareAddr)
		veths[name] = l.Attrs().Index
		for _, p := range ep.Ports {
			k := portKey{Proto: ipprotoTCP, Port: be16(p.Port)}
			if prev, taken := owner[k]; taken {
				errs = append(errs, fmt.Errorf("%s: %s:%d already owned by %s", ep.Name, p.Proto, p.Port, prev))
				continue
			}
			owner[k] = ep.Name
			in[k] = dest
			out[outKey{PodIP: ep.PodIP.As4(), Proto: ipprotoTCP, Port: be16(p.Port)}] = true
		}
	}

	// Egress attachments first, so a pod never receives traffic it can't answer.
	for name, idx := range veths {
		if _, ok := d.vethLinks[name]; ok {
			continue
		}
		tl, err := link.AttachTCX(link.TCXOptions{
			Interface: idx,
			Program:   d.coll.Programs["netdp_egress"],
			Attach:    ebpf.AttachTCXIngress,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("attach egress on %s: %w", name, err))
			continue
		}
		d.vethLinks[name] = tl
	}
	if err := syncSet(d.coll.Maps["netdp_ports_out"], out, uint8(1)); err != nil {
		errs = append(errs, err)
	}
	if err := syncMap(d.coll.Maps["netdp_ports_in"], in); err != nil {
		errs = append(errs, err)
	}
	for name, l := range d.vethLinks {
		if _, ok := veths[name]; !ok {
			_ = l.Close() // the veth is usually gone already (CNI DEL)
			delete(d.vethLinks, name)
		}
	}
	return errors.Join(errs...)
}

// Counters sums the per-CPU counters.
func (d *Datapath) Counters() (map[string]uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]uint64{}
	for i, name := range counterNames {
		var per []uint64
		if err := d.coll.Maps["netdp_counters"].Lookup(uint32(i), &per); err != nil {
			return nil, err
		}
		var sum uint64
		for _, v := range per {
			sum += v
		}
		out[name] = sum
	}
	return out, nil
}

// Metrics snapshots the counters and the steered-port count in the generated
// stat shape (reported via Stats and exported to prometheus). The counters live
// in unpinned maps, so they restart from zero with the agent.
func (d *Datapath) Metrics() (netdpmetrics.NetdpMetrics, error) {
	c, err := d.Counters()
	if err != nil {
		return netdpmetrics.NetdpMetrics{}, err
	}
	ports, err := d.Ports()
	if err != nil {
		return netdpmetrics.NetdpMetrics{}, err
	}
	return netdpmetrics.NetdpMetrics{
		InSteeredTotal:  c["in_steered"],
		InNotLbSrcTotal: c["in_not_lb_src"],
		InNoPortTotal:   c["in_no_port"],
		InErrTotal:      c["in_err"],
		OutSnatTotal:    c["out_snat"],
		OutErrTotal:     c["out_err"],
		SteeredPorts:    uint64(len(ports)),
	}, nil
}

// Ports lists the steered ports (for logs/tests), sorted.
func (d *Datapath) Ports() ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	var k portKey
	var v podDest
	it := d.coll.Maps["netdp_ports_in"].Iterate()
	for it.Next(&k, &v) {
		out = append(out, fmt.Sprintf("tcp:%d->%v", uint16(k.Port[0])<<8|uint16(k.Port[1]), net.IP(v.PodIP[:])))
	}
	sort.Strings(out)
	return out, it.Err()
}

func be16(v uint16) [2]byte { return [2]byte{byte(v >> 8), byte(v)} }

// syncSet makes a map's key set equal want, every value = val.
func syncSet[K comparable, V any](m *ebpf.Map, want map[K]bool, val V) error {
	vals := make(map[K]V, len(want))
	for k := range want {
		vals[k] = val
	}
	return syncMap(m, vals)
}

// syncMap makes a map's contents equal want: stale keys deleted, the rest put.
func syncMap[K comparable, V any](m *ebpf.Map, want map[K]V) error {
	var stale []K
	var k K
	var v V
	it := m.Iterate()
	for it.Next(&k, &v) {
		if _, ok := want[k]; !ok {
			stale = append(stale, k)
		}
	}
	if err := it.Err(); err != nil {
		return err
	}
	var errs []error
	for _, k := range stale {
		if err := m.Delete(k); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			errs = append(errs, err)
		}
	}
	for k, v := range want {
		if err := m.Put(k, v); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
