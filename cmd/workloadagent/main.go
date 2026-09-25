// Command workloadagent is the container-workload dataplane agent (dp_type
// "workload"). Like the other dp agents it enrolls with the control plane and
// SERVES over the connection — the substrate's common DataplaneService plus a
// WorkloadService the control plane calls to push the desired container set. The
// agent reconciles that set onto the node's container runtime (containerd) over
// CRI. See notes/ai/2026_07_07_container_workload_cri_design.md.
//
// The container lifecycle lives in the workload.Engine behind WorkloadService and
// a periodic converge that honours restart policy and heals crashes without a
// fresh Apply. The agent also runs the pod-network eBPF datapath (workload/netdp):
// the VIP / interface / remote (l4lb fronts) hooks feed it, and every converge
// re-syncs it with the running pod endpoints. Its eBPF object comes from the
// control plane (workload_netdp_object -> SetNetdpObject, the file delivered by
// node_file) and can be replaced at runtime; until one is loaded the datapath only
// records state. The secret / MTU / destination hooks stay no-ops.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/on-keyday/kscale/cri"
	"github.com/on-keyday/kscale/dataplane"
	"github.com/on-keyday/kscale/internal/safe"
	"github.com/on-keyday/kscale/internal/sigctx"
	"github.com/on-keyday/kscale/logbuf"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbstat "github.com/on-keyday/kscale/protobuf/proto/stat"
	wkt "github.com/on-keyday/kscale/protobuf/wkt"
	"github.com/on-keyday/kscale/rpc"
	"github.com/on-keyday/kscale/stat"
	"github.com/on-keyday/kscale/workload"
	"github.com/on-keyday/kscale/workload/netdp"
)

var domain = "kscale.local"

func main() {
	logger := logbuf.NewStderrLogger(os.Stderr, slog.LevelInfo)
	fs := flag.NewFlagSet("workloadagent", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:9443", "control-plane UDP address")
	dataDir := fs.String("data", "/tmp/kscale-ca", "dir holding the workload bootstrap token / saved cert")
	node := fs.String("node", "node1", "this workload node's name")
	criSocket := fs.String("cri-socket", "/run/containerd/containerd.sock", "CRI (containerd) unix socket")
	convergeEvery := fs.Duration("converge-interval", 30*time.Second, "periodic reconcile interval (restart policy + crash healing)")
	fileDir := fs.String("file-dir", "", "node-local file store (default <data>/dp_files)")
	domainFlag := fs.String("ca-domain", domain, "CA domain — must match the control plane's --ca-domain")
	netdpObj := fs.String("netdp-obj", "", "load this eBPF datapath object at boot (dev/test). Normally the object comes from the control plane (workload_netdp_object). Loading needs CAP_BPF + CAP_NET_ADMIN")
	_ = fs.Parse(os.Args[1:])
	domain = *domainFlag

	ctx, stop := sigctx.Context()
	defer stop()

	// Lazy CRI client — containerd may come up alongside/after the agent; the first
	// RPC that finds it down errors and the next converge retries.
	criClient := cri.NewClient(*criSocket, logger)
	engine := workload.NewEngine(criClient, logger)

	// The datapath records state from boot; its object arrives from the CP (or
	// --netdp-obj). A boot object that cannot load is fatal: silently running
	// without it would leave every pod-network port unreachable.
	dp := netdp.New(logger)
	defer dp.Close()
	if *netdpObj != "" {
		if err := dp.LoadObject(*netdpObj); err != nil {
			logger.Error("workloadagent: netdp", "error", err)
			os.Exit(1)
		}
	}
	objects := &netdpObjects{
		dp:      dp,
		fileDir: dataplane.ResolveFileDir(dataplane.Config{DataDir: *dataDir, FileDir: *fileDir}),
		logger:  logger,
	}
	epSync := &endpointSync{engine: engine, dp: dp, logger: logger}

	// Periodic converge: honour restart=always + heal crashed sandboxes using the
	// last-applied desired set, independent of control-plane pushes.
	safe.Go(logger, "workload-converge", func() {
		t := time.NewTicker(*convergeEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := engine.Converge(ctx); err != nil {
					logger.Warn("workload: periodic converge", "error", err)
				}
				epSync.run(ctx)
			}
		}
	})

	lc := stat.NewAppLifecycle()
	lc.SetRunning() // operational from boot — the converge loop runs immediately (no start gate)
	hooks := &workloadHooks{logger: logger, promMetrics: &stat.PromMetrics{}, lc: lc, dp: dp, objects: objects}
	if err := dataplane.Run(ctx, dataplane.Config{
		Addr:    *addr,
		DataDir: *dataDir,
		Node:    *node,
		App:     "workload",
		Domain:  domain,
		FileDir: *fileDir,
		Hooks:   hooks,
		RegisterExtra: func(mgr *rpc.RPCManager) {
			pb.RegisterWorkloadServiceServer(mgr, &workloadService{engine: engine, sync: epSync, objects: objects, logger: logger})
		},
	}, logger); err != nil && ctx.Err() == nil {
		logger.Error("workloadagent exited", "error", err)
		os.Exit(1)
	}
}

// workloadService adapts the workload.Engine to the WorkloadService RPC surface.
type workloadService struct {
	pb.UnimplementedWorkloadServiceServer
	engine  *workload.Engine
	sync    *endpointSync
	objects *netdpObjects
	logger  *slog.Logger
}

func (s *workloadService) SetNetdpObject(ctx context.Context, req *pb.WorkloadServiceSetNetdpObjectRequest) (*wkt.Empty, error) {
	if err := s.objects.set(req.Object); err != nil {
		return nil, err
	}
	return &wkt.Empty{}, nil
}

// netdpObjects resolves the object the CP selects (a name in the node's file
// store) and loads it — at once if the file is there, else when node_file
// delivers it (the two reconciles are independent, so the selection can win the
// race). A file arriving under the currently loaded name reloads it, so the
// datapath can also be replaced by re-sending the same name with new content.
type netdpObjects struct {
	dp      *netdp.Datapath
	fileDir string
	logger  *slog.Logger

	mu      sync.Mutex
	want    string // selected object name
	current string // loaded object name
}

func (o *netdpObjects) set(name string) error {
	if name == "" {
		return nil // nothing selected: keep whatever runs
	}
	// A bare file-store name only: the path is joined under fileDir and must not
	// climb out of it.
	if !filepath.IsLocal(name) || filepath.Base(name) != name {
		return fmt.Errorf("netdp object %q: want a file name in the node file store", name)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.want = name
	if name == o.current {
		return nil
	}
	path := filepath.Join(o.fileDir, name)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		o.logger.Info("netdp: object selected; waiting for the file", "object", name)
		return nil
	}
	return o.loadLocked(name)
}

func (o *netdpObjects) fileReceived(name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if name != o.want {
		return
	}
	if err := o.loadLocked(name); err != nil {
		o.logger.Error("netdp: load on file arrival", "object", name, "error", err)
	}
}

func (o *netdpObjects) loadLocked(name string) error {
	if err := o.dp.LoadObject(filepath.Join(o.fileDir, name)); err != nil {
		return err
	}
	o.current = name
	return nil
}

// endpointSync pushes the engine's running pod endpoints into the datapath. It
// runs after every Apply and periodic converge, so a pod that just got its IP
// (or went away) is reflected within one converge interval.
type endpointSync struct {
	engine *workload.Engine
	dp     *netdp.Datapath
	logger *slog.Logger

	mu   sync.Mutex // run is called from the converge loop and the RPC handler
	last string
}

func (s *endpointSync) run(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	eps, err := s.engine.Endpoints(ctx)
	if err != nil {
		s.logger.Warn("netdp: endpoints", "error", err)
	}
	if err := s.dp.SetEndpoints(eps); err != nil {
		s.logger.Warn("netdp: sync", "error", err)
	}
	if ports, err := s.dp.Ports(); err == nil {
		if cur := fmt.Sprint(ports); cur != s.last {
			s.logger.Info("netdp: steered ports", "ports", ports)
			s.last = cur
		}
	}
}

func (s *workloadService) ApplyContainers(ctx context.Context, req *pb.WorkloadServiceApplyContainersRequest) (*wkt.Empty, error) {
	specs := make([]workload.Spec, 0, len(req.Containers))
	for _, c := range req.Containers {
		specs = append(specs, workload.Spec{
			Name:    c.Name,
			Image:   c.Image,
			Command: c.Command,
			Args:    c.Args,
			Env:     c.Env,
			Mounts:  c.Mounts,
			Restart: c.Restart,
			Network: c.Network,
			Ports:   c.Ports,
		})
	}
	err := s.engine.Apply(ctx, specs)
	s.sync.run(ctx) // also after a partial failure: the converged part is live
	if err != nil {
		return nil, err
	}
	return &wkt.Empty{}, nil
}

func (s *workloadService) ListContainers(ctx context.Context, _ *wkt.Empty) (*pb.WorkloadServiceListContainersResponse, error) {
	statuses, err := s.engine.List(ctx)
	if err != nil {
		return nil, err
	}
	resp := &pb.WorkloadServiceListContainersResponse{}
	for _, st := range statuses {
		resp.Containers = append(resp.Containers, &pb.WorkloadServiceContainerStatus{
			Name:         st.Name,
			ContainerId:  st.ContainerID,
			Image:        st.Image,
			State:        st.State,
			PodSandboxId: st.PodSandboxID,
		})
	}
	return resp, nil
}

// workloadHooks satisfies dataplane.Hooks (and dataplane.FileReceiver). VIP /
// interface / remote feed the pod-network datapath's state (applied once an
// object is loaded); the rest are no-ops (no l4lb/popcache dataplane here).
type workloadHooks struct {
	logger  *slog.Logger
	dp      *netdp.Datapath
	objects *netdpObjects
	// promMetrics is the node's prometheus surface. The workload agent has no
	// app-specific counters, but a NON-NIL PromMetrics is what makes the substrate
	// register the /metrics (StreamMagicHTTP) handler; returning nil leaves the CP's
	// ScrapeMetrics dialing a stream that closes immediately (read metrics response:
	// unexpected EOF). It still carries the shared host/process/go telemetry the
	// substrate collects. (Same fix as the router controller, kscale 5b94c82.)
	promMetrics *stat.PromMetrics
	// lc carries the app-lifecycle status the CP reports as a node's app_status.
	// Unlike l4lb/popcache, the workload agent has no start gate (no --auto-start,
	// no StartDataplane), so its Start hook is never called — it's operational from
	// boot (the converge loop runs immediately). We therefore set it Running at
	// construction; without this Stats() reports nothing and the CP shows "Unknown".
	lc *stat.AppLifecycle
}

func (h *workloadHooks) DpType() string                            { return "workload" }
func (h *workloadHooks) Start(ctx context.Context) error           { h.lc.SetRunning(); return nil }
func (h *workloadHooks) Stop() error                               { h.lc.SetStopped(); return nil }
func (h *workloadHooks) SyncSecret([]byte) error                   { return nil }
func (h *workloadHooks) UpdateMtu(uint16) error                    { return nil }
func (h *workloadHooks) SetServerID(uint32)                        {}
func (h *workloadHooks) UpdateDestinations([]stat.DestEntry) error { return nil }

func (h *workloadHooks) UpdateVip(vip netip.Addr, _ bool) error {
	if !vip.IsValid() {
		return nil
	}
	return h.dp.SetVIP(vip)
}

func (h *workloadHooks) BindInterface(iface string) error { return h.dp.BindInterface(iface) }

// OnFileReceived lets a selected datapath object load once node_file delivers it.
func (h *workloadHooks) OnFileReceived(name string) { h.objects.fileReceived(name) }

// UpdateRemote receives the l4lb fronts (peering l4lb -> workload): the only
// sources whose IPIP the datapath will decap.
func (h *workloadHooks) UpdateRemote(remotes []stat.DestEntry) error {
	srcs := make([]netip.Addr, 0, len(remotes))
	for _, r := range remotes {
		srcs = append(srcs, r.IPAddr)
	}
	return h.dp.SetLBSources(srcs)
}
func (h *workloadHooks) PromMetrics() *stat.PromMetrics { return h.promMetrics }

// Stats reports the app lifecycle plus, when the datapath is loaded, its counters,
// folding them into the prometheus surface. Deliberately NOT delta-reported: the
// substrate calls Stats from two goroutines (the 2s host-metrics ticker and the
// StreamStats loop), so a "last reported" snapshot would let one caller swallow a
// change the other never sends — and would race. Seven counters per call is cheap.
func (h *workloadHooks) Stats() []*pbstat.Stats {
	entries := []*pbstat.Stats{h.lc.Stat()}
	loaded, _ := h.dp.Loaded()
	h.promMetrics.WorkloadNetdpEnabled = loaded
	if !loaded {
		return entries
	}
	m, err := h.dp.Metrics()
	if err != nil {
		h.logger.Warn("netdp: metrics", "error", err)
		return entries
	}
	ws := stat.WorkloadStat{NetdpStats: m}
	h.promMetrics.UpdateWorkloadStat(&ws)
	return append(entries, &pbstat.Stats{Workload: ws.ToProto()})
}
