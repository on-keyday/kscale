// Command workloadagent is the container-workload dataplane agent (dp_type
// "workload"). Like the other dp agents it enrolls with the control plane and
// SERVES over the connection — the substrate's common DataplaneService plus a
// WorkloadService the control plane calls to push the desired container set. The
// agent reconciles that set onto the node's container runtime (containerd) over
// CRI. See notes/ai/2026_07_07_container_workload_cri_design.md.
//
// The l4lb-shaped Hooks (VIP / secret / interface / MTU / eBPF) are all no-ops
// here; the container lifecycle lives entirely in the workload.Engine behind
// WorkloadService and a periodic converge that honours restart policy and heals
// crashes without a fresh Apply.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/netip"
	"os"
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
	_ = fs.Parse(os.Args[1:])
	domain = *domainFlag

	ctx, stop := sigctx.Context()
	defer stop()

	// Lazy CRI client — containerd may come up alongside/after the agent; the first
	// RPC that finds it down errors and the next converge retries.
	criClient := cri.NewClient(*criSocket, logger)
	engine := workload.NewEngine(criClient, logger)

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
			}
		}
	})

	lc := stat.NewAppLifecycle()
	lc.SetRunning() // operational from boot — the converge loop runs immediately (no start gate)
	hooks := &workloadHooks{logger: logger, promMetrics: &stat.PromMetrics{}, lc: lc}
	if err := dataplane.Run(ctx, dataplane.Config{
		Addr:    *addr,
		DataDir: *dataDir,
		Node:    *node,
		App:     "workload",
		Domain:  domain,
		FileDir: *fileDir,
		Hooks:   hooks,
		RegisterExtra: func(mgr *rpc.RPCManager) {
			pb.RegisterWorkloadServiceServer(mgr, &workloadService{engine: engine, logger: logger})
		},
	}, logger); err != nil && ctx.Err() == nil {
		logger.Error("workloadagent exited", "error", err)
		os.Exit(1)
	}
}

// workloadService adapts the workload.Engine to the WorkloadService RPC surface.
type workloadService struct {
	pb.UnimplementedWorkloadServiceServer
	engine *workload.Engine
	logger *slog.Logger
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
		})
	}
	if err := s.engine.Apply(ctx, specs); err != nil {
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

// workloadHooks satisfies dataplane.Hooks. The workload agent has no l4lb/popcache
// dataplane, so the traffic-plane methods are no-ops; container lifecycle is
// entirely in the workload.Engine.
type workloadHooks struct {
	logger *slog.Logger
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
func (h *workloadHooks) UpdateVip(netip.Addr, bool) error          { return nil }
func (h *workloadHooks) SyncSecret([]byte) error                   { return nil }
func (h *workloadHooks) BindInterface(string) error                { return nil }
func (h *workloadHooks) UpdateMtu(uint16) error                    { return nil }
func (h *workloadHooks) SetServerID(uint32)                        {}
func (h *workloadHooks) UpdateDestinations([]stat.DestEntry) error { return nil }
func (h *workloadHooks) UpdateRemote([]stat.DestEntry) error       { return nil }
func (h *workloadHooks) Stats() []*pbstat.Stats                    { return []*pbstat.Stats{h.lc.Stat()} }
func (h *workloadHooks) PromMetrics() *stat.PromMetrics            { return h.promMetrics }
