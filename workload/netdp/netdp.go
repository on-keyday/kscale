// Package netdp is the workload node's eBPF datapath (the userspace half of
// workload/netdp/c/netdp.c): it attaches the ingress program to the bound NIC and
// the egress program to each pod's host-side veth (tcx), and keeps the maps in
// step with what the agent learns — the VIP (UpdateVip), the l4lb fronts allowed
// to send IPIP (UpdateRemote), and the pod endpoints and their ports (the
// workload engine). It runs in workloadagent, non-root, with CAP_BPF +
// CAP_NET_ADMIN; it never enters a pod netns (the CNI plugin wired that), so it
// needs no CAP_SYS_ADMIN.
//
// The Datapath keeps that desired state itself, independent of any loaded
// object, so the object can arrive late and be replaced at runtime
// (workload_netdp_object): LoadObject builds the new collection, refills its maps
// from the state, then swaps the programs under the existing tcx links.
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

	mu sync.Mutex

	// Desired state, kept whether or not an object is loaded.
	cfg    config
	nic    string
	lbSrcs map[[4]byte]bool
	in     map[portKey]podDest
	out    map[outKey]bool
	veths  map[string]int // host veth name -> ifindex

	// Loaded object (nil until LoadObject) and its attachments.
	coll      *ebpf.Collection
	object    string
	nicLink   link.Link
	vethLinks map[string]link.Link // host veth name -> egress tcx link
}

// New returns a Datapath with no object loaded: it records state until
// LoadObject.
func New(logger *slog.Logger) *Datapath {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Datapath{
		logger:    logger,
		lbSrcs:    map[[4]byte]bool{},
		in:        map[portKey]podDest{},
		out:       map[outKey]bool{},
		veths:     map[string]int{},
		vethLinks: map[string]link.Link{},
	}
}

// Load is New + LoadObject.
func Load(objPath string, logger *slog.Logger) (*Datapath, error) {
	d := New(logger)
	if err := d.LoadObject(objPath); err != nil {
		return nil, err
	}
	return d, nil
}

// Loaded reports whether an object is loaded, and its path.
func (d *Datapath) Loaded() (bool, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.coll != nil, d.object
}

// LoadObject loads the object at objPath and makes it the live datapath: its maps
// are filled from the current state, then its programs replace the running ones
// under the existing tcx links (or get attached, on the first load). On any
// failure the running datapath is left as it was.
func (d *Datapath) LoadObject(objPath string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	spec, err := ebpf.LoadCollectionSpec(objPath)
	if err != nil {
		return fmt.Errorf("netdp: load %s: %w", objPath, err)
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		return fmt.Errorf("netdp: create collection from %s: %w", objPath, err)
	}
	for _, p := range []string{"netdp_ingress", "netdp_egress"} {
		if coll.Programs[p] == nil {
			coll.Close()
			return fmt.Errorf("netdp: %s has no program %s", objPath, p)
		}
	}
	if err := d.fillMaps(coll); err != nil {
		coll.Close()
		return fmt.Errorf("netdp: fill maps of %s: %w", objPath, err)
	}

	old := d.coll
	if err := d.swapPrograms(old, coll); err != nil {
		coll.Close()
		return err
	}
	d.coll, d.object = coll, objPath
	if old != nil {
		old.Close()
	}
	// First load with an interface already bound: attach now.
	if d.nicLink == nil && d.nic != "" {
		if err := d.attachNICLocked(); err != nil {
			return err
		}
	}
	if err := d.attachVethsLocked(); err != nil {
		return err
	}
	d.logger.Info("netdp: object loaded", "object", objPath, "replaced", old != nil)
	return nil
}

// swapPrograms points every existing link at next's programs. If one update
// fails, the ones already moved go back to prev's programs.
func (d *Datapath) swapPrograms(prev, next *ebpf.Collection) error {
	type moved struct {
		l    link.Link
		prog string
	}
	var done []moved
	update := func(l link.Link, prog string) error {
		if err := l.Update(next.Programs[prog]); err != nil {
			return err
		}
		done = append(done, moved{l, prog})
		return nil
	}
	fail := func(what string, err error) error {
		for _, m := range done {
			_ = m.l.Update(prev.Programs[m.prog])
		}
		return fmt.Errorf("netdp: swap %s: %w", what, err)
	}
	if d.nicLink != nil {
		if err := update(d.nicLink, "netdp_ingress"); err != nil {
			return fail("ingress on "+d.nic, err)
		}
	}
	for name, l := range d.vethLinks {
		if err := update(l, "netdp_egress"); err != nil {
			return fail("egress on "+name, err)
		}
	}
	return nil
}

func (d *Datapath) fillMaps(coll *ebpf.Collection) error {
	return errors.Join(
		coll.Maps["netdp_config"].Put(uint32(0), d.cfg),
		syncSet(coll.Maps["netdp_lb_srcs"], d.lbSrcs, uint8(1)),
		syncSet(coll.Maps["netdp_ports_out"], d.out, uint8(1)),
		syncMap(coll.Maps["netdp_ports_in"], d.in),
	)
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
	if d.coll != nil {
		d.coll.Close()
	}
	return errors.Join(errs...)
}

func (d *Datapath) writeConfig() error {
	if d.coll == nil {
		return nil
	}
	return d.coll.Maps["netdp_config"].Put(uint32(0), d.cfg)
}

// BindInterface sets the NIC l4lb's IPIP arrives on (and replies leave by) and,
// once an object is loaded, attaches the ingress program there. Rebinding to
// another NIC moves it.
func (d *Datapath) BindInterface(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if name == d.nic && (d.nicLink != nil || d.coll == nil) {
		return nil
	}
	l, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("netdp: interface %q: %w", name, err)
	}
	d.nic = name
	d.cfg.NICIfindex = uint32(l.Attrs().Index)
	if d.coll == nil {
		return nil
	}
	if d.nicLink != nil {
		_ = d.nicLink.Close()
		d.nicLink = nil
	}
	if err := d.attachNICLocked(); err != nil {
		return err
	}
	return d.writeConfig()
}

func (d *Datapath) attachNICLocked() error {
	tl, err := link.AttachTCX(link.TCXOptions{
		Interface: int(d.cfg.NICIfindex),
		Program:   d.coll.Programs["netdp_ingress"],
		Attach:    ebpf.AttachTCXIngress,
	})
	if err != nil {
		return fmt.Errorf("netdp: attach ingress on %s: %w", d.nic, err)
	}
	d.nicLink = tl
	d.logger.Info("netdp: ingress attached", "interface", d.nic, "ifindex", d.cfg.NICIfindex)
	return nil
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
	d.lbSrcs = want
	if d.coll == nil {
		return nil
	}
	return syncSet(d.coll.Maps["netdp_lb_srcs"], d.lbSrcs, uint8(1))
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
	d.in, d.out, d.veths = in, out, veths
	if d.coll == nil {
		return errors.Join(errs...)
	}

	// Egress attachments first, so a pod never receives traffic it can't answer.
	if err := d.attachVethsLocked(); err != nil {
		errs = append(errs, err)
	}
	if err := syncSet(d.coll.Maps["netdp_ports_out"], d.out, uint8(1)); err != nil {
		errs = append(errs, err)
	}
	if err := syncMap(d.coll.Maps["netdp_ports_in"], d.in); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// attachVethsLocked makes the egress attachments match d.veths.
func (d *Datapath) attachVethsLocked() error {
	var errs []error
	for name, idx := range d.veths {
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
	for name, l := range d.vethLinks {
		if _, ok := d.veths[name]; !ok {
			_ = l.Close() // the veth is usually gone already (CNI DEL)
			delete(d.vethLinks, name)
		}
	}
	return errors.Join(errs...)
}

// Counters sums the per-CPU counters (all zero with no object loaded).
func (d *Datapath) Counters() (map[string]uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]uint64{}
	for i, name := range counterNames {
		out[name] = 0
		if d.coll == nil {
			continue
		}
		var per []uint64
		if err := d.coll.Maps["netdp_counters"].Lookup(uint32(i), &per); err != nil {
			return nil, err
		}
		for _, v := range per {
			out[name] += v
		}
	}
	return out, nil
}

// Metrics snapshots the counters and the steered-port count in the generated
// stat shape (reported via Stats and exported to prometheus). The counters live
// in the loaded object's maps, so they restart from zero with the agent and on
// every object swap.
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

// Ports lists the steered ports (for logs/tests), sorted. It reports the desired
// state, so it is meaningful before an object is loaded too.
func (d *Datapath) Ports() ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.in))
	for k, v := range d.in {
		out = append(out, fmt.Sprintf("tcp:%d->%v", uint16(k.Port[0])<<8|uint16(k.Port[1]), net.IP(v.PodIP[:])))
	}
	sort.Strings(out)
	return out, nil
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
