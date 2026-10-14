// Package workload reconciles a desired set of containers onto a node's
// container runtime (containerd) over CRI. It is the southbound half of the
// container-workload feature: the control plane pushes the desired set via
// WorkloadService.ApplyContainers, and this Engine converges the runtime to
// match — idempotently, and on a timer so crashes and restart policy are
// honoured. See notes/ai/2026_07_07_container_workload_cri_design.md.
package workload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	criapi "github.com/on-keyday/kscale/protobuf/proto/cri"
)

// Label keys stamped on every managed sandbox + container so converge can find
// and reconcile only what it owns, and detect drift. Unprefixed names (valid CRI
// label keys). The container/sandbox NAME is carried in CRI metadata.name, not a
// label, since resource keys may not be valid label values.
const (
	labelManaged  = "kscale-managed"
	labelSpecHash = "kscale-spec-hash"
	labelNetwork  = "kscale-network"
	sandboxNS     = "kscale"
)

// Network modes (Spec.Network). "" is host, so specs written before the field
// existed keep their behaviour.
const (
	NetworkHost = "host"
	NetworkPod  = "pod"
)

// Runtime is the subset of the CRI client the Engine uses. *cri.CRI satisfies it;
// tests supply a fake.
type Runtime interface {
	PullImage(context.Context, *criapi.PullImageRequest) (*criapi.PullImageResponse, error)
	RunPodSandbox(context.Context, *criapi.RunPodSandboxRequest) (*criapi.RunPodSandboxResponse, error)
	StopPodSandbox(context.Context, *criapi.StopPodSandboxRequest) (*criapi.StopPodSandboxResponse, error)
	RemovePodSandbox(context.Context, *criapi.RemovePodSandboxRequest) (*criapi.RemovePodSandboxResponse, error)
	ListPodSandbox(context.Context, *criapi.ListPodSandboxRequest) (*criapi.ListPodSandboxResponse, error)
	CreateContainer(context.Context, *criapi.CreateContainerRequest) (*criapi.CreateContainerResponse, error)
	StartContainer(context.Context, *criapi.StartContainerRequest) (*criapi.StartContainerResponse, error)
	StopContainer(context.Context, *criapi.StopContainerRequest) (*criapi.StopContainerResponse, error)
	RemoveContainer(context.Context, *criapi.RemoveContainerRequest) (*criapi.RemoveContainerResponse, error)
	ListContainers(context.Context, *criapi.ListContainersRequest) (*criapi.ListContainersResponse, error)
	PodSandboxStatus(context.Context, *criapi.PodSandboxStatusRequest) (*criapi.PodSandboxStatusResponse, error)
	ImageStatus(context.Context, *criapi.ImageStatusRequest) (*criapi.ImageStatusResponse, error)
}

// Spec is one desired container (the WorkloadService wire spec, decoupled from
// protobuf so the engine and its tests don't depend on the pb types).
type Spec struct {
	Name    string
	Image   string
	Command []string
	Args    []string
	Env     []string // "KEY=VALUE"
	Mounts  []string // raw "hostPath:containerPath" — always rejected (see volume.go)
	Restart string   // "always" | "never"
	Network string   // "" / "host" (node netns) | "pod" (own netns via the kscale CNI)
	Ports   []string // "tcp:8080"; only for Network "pod"
	Volumes []string // "name:/containerPath[:ro]" — agent-managed dirs (volume.go)
}

// Port is one parsed Spec.Ports entry.
type Port struct {
	Proto string // "tcp" (the only protocol the datapath steers so far)
	Port  uint16
}

// ParsePorts parses "proto:port" entries. Only tcp is accepted: the eBPF
// datapath does not steer UDP yet, and accepting it would declare a port that
// silently never receives traffic.
func ParsePorts(ports []string) ([]Port, error) {
	out := make([]Port, 0, len(ports))
	for _, s := range ports {
		proto, num, ok := strings.Cut(s, ":")
		if !ok {
			return nil, fmt.Errorf("port %q: want proto:port", s)
		}
		if proto != "tcp" {
			return nil, fmt.Errorf("port %q: only tcp is supported", s)
		}
		n, err := strconv.ParseUint(num, 10, 16)
		if err != nil || n == 0 {
			return nil, fmt.Errorf("port %q: bad port number", s)
		}
		out = append(out, Port{Proto: proto, Port: uint16(n)})
	}
	return out, nil
}

// Validate rejects specs the engine cannot (or must not) honour. The engine runs
// it before touching the runtime; the control plane runs it at apply time so a
// bad spec is refused up front instead of failing silently on the node.
func (s Spec) Validate() error {
	if len(s.Mounts) > 0 {
		return errHostMounts
	}
	if _, err := ParseVolumes(s.Volumes); err != nil {
		return err
	}
	switch s.Network {
	case "", NetworkHost:
		if len(s.Ports) > 0 {
			return fmt.Errorf("ports need network %q (host network is not steered)", NetworkPod)
		}
	case NetworkPod:
		if _, err := ParsePorts(s.Ports); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown network %q (want host or pod)", s.Network)
	}
	return nil
}

// Endpoint is a running pod-network container as the datapath needs it: where
// it lives (sandbox ID -> the CNI's veth/MAC naming) and which ports it owns.
type Endpoint struct {
	Name      string
	SandboxID string
	PodIP     netip.Addr
	Ports     []Port
}

// Status is the observed state of one managed container. A desired container
// whose image is still pulling gets one too: with no container yet (ContainerID
// and State empty), or on the container it will replace (Pull.Image != Image).
type Status struct {
	Name         string
	ContainerID  string
	Image        string
	State        string // CRI state (stateString); "" = no container yet
	PodSandboxID string
	Pull         PullInfo // zero = no pull pending
}

// Engine converges a desired container set onto a Runtime. Safe for concurrent
// use; Apply and Converge serialize on a single mutex so a timer-driven converge
// never races an Apply. Image pulls run in the background (pull.go) and are not
// under that mutex.
type Engine struct {
	rt     Runtime
	logger *slog.Logger

	mu      sync.Mutex
	desired map[string]Spec

	puller     Puller
	pullSem    chan struct{}
	pullCtx    context.Context
	pullCancel context.CancelFunc
	pullWG     sync.WaitGroup
	pullDone   chan struct{}
	pmu        sync.Mutex // guards pulls; never held across a CRI call
	pulls      map[string]*pullState
	now        func() time.Time
	volumeRoot string
}

// NewEngine builds an Engine over rt. Pulls go through rt unless WithPuller says
// otherwise; Close stops them.
func NewEngine(rt Runtime, logger *slog.Logger, opts ...Option) *Engine {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	e := &Engine{
		rt: rt, logger: logger, desired: map[string]Spec{},
		puller: rt, pullSem: make(chan struct{}, 2), pullDone: make(chan struct{}, 1),
		pulls: map[string]*pullState{}, now: time.Now, volumeRoot: DefaultVolumeRoot,
	}
	e.pullCtx, e.pullCancel = context.WithCancel(context.Background())
	for _, o := range opts {
		o(e)
	}
	return e
}

// Apply replaces the desired set with specs and converges once. It does not wait
// for image pulls: a container whose image is still downloading starts on a later
// converge (see PullDone).
func (e *Engine) Apply(ctx context.Context, specs []Spec) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	next := make(map[string]Spec, len(specs))
	for _, s := range specs {
		next[s.Name] = s
	}
	e.desired = next
	return e.convergeLocked(ctx)
}

// Converge reconciles the runtime to the current desired set. Called on a timer
// so exited containers with restart=always come back and crashed sandboxes are
// rebuilt without a fresh Apply.
func (e *Engine) Converge(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.convergeLocked(ctx)
}

// managed returns the current managed sandboxes and containers, keyed by the
// name stored in their CRI metadata.
func (e *Engine) managed(ctx context.Context) (pods map[string]*criapi.PodSandbox, ctrs map[string]*criapi.Container, err error) {
	podList, err := e.rt.ListPodSandbox(ctx, &criapi.ListPodSandboxRequest{
		Filter: &criapi.PodSandboxFilter{LabelSelector: map[string]string{labelManaged: "true"}},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list sandboxes: %w", err)
	}
	ctrList, err := e.rt.ListContainers(ctx, &criapi.ListContainersRequest{
		Filter: &criapi.ContainerFilter{LabelSelector: map[string]string{labelManaged: "true"}},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list containers: %w", err)
	}
	pods = map[string]*criapi.PodSandbox{}
	for _, p := range podList.Items {
		if p.Metadata != nil {
			pods[p.Metadata.Name] = p
		}
	}
	ctrs = map[string]*criapi.Container{}
	for _, c := range ctrList.Containers {
		if c.Metadata != nil {
			ctrs[c.Metadata.Name] = c
		}
	}
	return pods, ctrs, nil
}

func (e *Engine) convergeLocked(ctx context.Context) error {
	pods, ctrs, err := e.managed(ctx)
	if err != nil {
		return err
	}

	var errs []error

	// 1. Remove anything managed but no longer desired.
	for name, c := range ctrs {
		if _, ok := e.desired[name]; !ok {
			if err := e.teardownContainer(ctx, c.Id); err != nil {
				errs = append(errs, fmt.Errorf("remove container %q: %w", name, err))
			}
		}
	}
	for name, p := range pods {
		if _, ok := e.desired[name]; !ok {
			if err := e.teardownSandbox(ctx, p.Id); err != nil {
				errs = append(errs, fmt.Errorf("remove sandbox %q: %w", name, err))
			}
		}
	}

	// 2. Ensure each desired container exists, matches, and (per restart) runs.
	for name, spec := range e.desired {
		if err := e.ensure(ctx, spec, pods[name], ctrs[name]); err != nil {
			errs = append(errs, fmt.Errorf("ensure %q: %w", name, err))
		}
	}
	e.prunePulls()

	if len(errs) > 0 {
		return fmt.Errorf("converge: %w", errors.Join(errs...))
	}
	return nil
}

// ensure reconciles one desired container given its current sandbox/container (or
// nil). A container that matches the desired spec hash is left alone when running
// (or when exited under restart=never); everything else is recreated — once its
// image is present. Until then the current container (if any) is left untouched
// and a background pull is requested, so an image change never means downtime
// for the length of the download.
func (e *Engine) ensure(ctx context.Context, spec Spec, pod *criapi.PodSandbox, ctr *criapi.Container) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	hash := specHash(spec)
	if ctr != nil && ctr.Labels[labelSpecHash] == hash {
		switch ctr.State {
		case criapi.ContainerState_CONTAINER_RUNNING:
			return nil
		case criapi.ContainerState_CONTAINER_EXITED:
			if spec.Restart != "always" {
				return nil // intentionally stopped; leave it
			}
		}
		// CREATED, or EXITED+restart=always → recreate below.
	}

	if ok, err := e.imageReady(ctx, spec.Image); err != nil || !ok {
		return err // !ok: pulling in the background; a later converge recreates
	}
	vols, _ := ParseVolumes(spec.Volumes) // validated above
	mounts, err := e.ensureVolumes(vols)
	if err != nil {
		return err
	}

	// Recreate: tear down the stale container and sandbox for this name.
	if ctr != nil {
		if err := e.teardownContainer(ctx, ctr.Id); err != nil {
			return err
		}
	}
	if pod != nil {
		if err := e.teardownSandbox(ctx, pod.Id); err != nil {
			return err
		}
	}

	sandboxConfig := e.sandboxConfig(spec)
	run, err := e.rt.RunPodSandbox(ctx, &criapi.RunPodSandboxRequest{Config: sandboxConfig})
	if err != nil {
		return fmt.Errorf("run sandbox: %w", err)
	}

	created, err := e.rt.CreateContainer(ctx, &criapi.CreateContainerRequest{
		PodSandboxId:  run.PodSandboxId,
		Config:        e.containerConfig(spec, hash, mounts),
		SandboxConfig: sandboxConfig,
	})
	if err != nil {
		// Roll back the orphaned sandbox so a retry starts clean.
		_ = e.teardownSandbox(ctx, run.PodSandboxId)
		return fmt.Errorf("create container: %w", err)
	}

	if _, err := e.rt.StartContainer(ctx, &criapi.StartContainerRequest{ContainerId: created.ContainerId}); err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	e.logger.Info("workload: container converged", "name", spec.Name, "image", spec.Image, "container_id", created.ContainerId)
	return nil
}

func (e *Engine) sandboxConfig(spec Spec) *criapi.PodSandboxConfig {
	// Host network: no CNI, the container shares the node netns. Pod network: the
	// runtime creates a netns and the kscale CNI plugin wires it.
	mode, network := criapi.NamespaceMode_NODE, NetworkHost
	if spec.Network == NetworkPod {
		mode, network = criapi.NamespaceMode_POD, NetworkPod
	}
	return &criapi.PodSandboxConfig{
		Metadata: &criapi.PodSandboxMetadata{Name: spec.Name, Uid: spec.Name, Namespace: sandboxNS},
		Labels:   map[string]string{labelManaged: "true", labelNetwork: network},
		Linux: &criapi.LinuxPodSandboxConfig{
			SecurityContext: &criapi.LinuxSandboxSecurityContext{
				NamespaceOptions: &criapi.NamespaceOption{Network: mode},
			},
		},
	}
}

func (e *Engine) containerConfig(spec Spec, hash string, mounts []*criapi.Mount) *criapi.ContainerConfig {
	return &criapi.ContainerConfig{
		Metadata: &criapi.ContainerMetadata{Name: spec.Name},
		Image:    &criapi.ImageSpec{Image: spec.Image},
		Command:  spec.Command,
		Args:     spec.Args,
		Envs:     parseEnv(spec.Env),
		Mounts:   mounts,
		Labels:   map[string]string{labelManaged: "true", labelSpecHash: hash},
	}
}

func (e *Engine) teardownContainer(ctx context.Context, id string) error {
	if _, err := e.rt.StopContainer(ctx, &criapi.StopContainerRequest{ContainerId: id}); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	if _, err := e.rt.RemoveContainer(ctx, &criapi.RemoveContainerRequest{ContainerId: id}); err != nil {
		return fmt.Errorf("remove: %w", err)
	}
	return nil
}

func (e *Engine) teardownSandbox(ctx context.Context, id string) error {
	if _, err := e.rt.StopPodSandbox(ctx, &criapi.StopPodSandboxRequest{PodSandboxId: id}); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	if _, err := e.rt.RemovePodSandbox(ctx, &criapi.RemovePodSandboxRequest{PodSandboxId: id}); err != nil {
		return fmt.Errorf("remove: %w", err)
	}
	return nil
}

// List reports the observed state of every managed container, plus an entry for
// each desired container that has none yet but whose image is pulling. Pull
// carries a pending pull either way (see Status).
func (e *Engine) List(ctx context.Context) ([]Status, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ctrs, err := e.managed(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(ctrs))
	for name, c := range ctrs {
		image := ""
		if c.Image != nil {
			image = c.Image.Image
		}
		st := Status{
			Name:         name,
			ContainerID:  c.Id,
			Image:        image,
			State:        stateString(c.State),
			PodSandboxID: c.PodSandboxId,
		}
		if spec, ok := e.desired[name]; ok && spec.Image != image {
			st.Pull = e.pullInfo(spec.Image)
		}
		out = append(out, st)
	}
	for name, spec := range e.desired {
		if _, ok := ctrs[name]; ok {
			continue
		}
		if pi := e.pullInfo(spec.Image); pi.State != "" {
			out = append(out, Status{Name: name, Image: spec.Image, Pull: pi})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Endpoints reports every ready pod-network sandbox with its IP (from the CNI
// result, via PodSandboxStatus) and the ports its desired spec declares. A
// sandbox whose status read fails or that has no IP yet is skipped (the next
// converge retries); its error is returned alongside the rest.
func (e *Engine) Endpoints(ctx context.Context) ([]Endpoint, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	pods, _, err := e.managed(ctx)
	if err != nil {
		return nil, err
	}
	var out []Endpoint
	var errs []error
	for name, p := range pods {
		if p.Labels[labelNetwork] != NetworkPod || p.State != criapi.PodSandboxState_SANDBOX_READY {
			continue
		}
		spec, ok := e.desired[name]
		if !ok {
			continue
		}
		ports, err := ParsePorts(spec.Ports)
		if err != nil {
			continue // Validate() already failed this spec at ensure time
		}
		st, err := e.rt.PodSandboxStatus(ctx, &criapi.PodSandboxStatusRequest{PodSandboxId: p.Id})
		if err != nil {
			errs = append(errs, fmt.Errorf("sandbox status %q: %w", name, err))
			continue
		}
		var ip netip.Addr
		if st.Status != nil && st.Status.Network != nil {
			ip, _ = netip.ParseAddr(st.Status.Network.Ip)
		}
		if !ip.Is4() {
			errs = append(errs, fmt.Errorf("sandbox %q: no IPv4 pod IP yet", name))
			continue
		}
		out = append(out, Endpoint{Name: name, SandboxID: p.Id, PodIP: ip, Ports: ports})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errors.Join(errs...)
}

func stateString(s criapi.ContainerState) string {
	switch s {
	case criapi.ContainerState_CONTAINER_CREATED:
		return "CREATED"
	case criapi.ContainerState_CONTAINER_RUNNING:
		return "RUNNING"
	case criapi.ContainerState_CONTAINER_EXITED:
		return "EXITED"
	default:
		return "UNKNOWN"
	}
}

// specHash is a stable digest of the fields that affect the running container.
// Stored in a label so converge can detect drift without a status read. Env and
// mounts keep their given order (order is semantically meaningful for env).
func specHash(s Spec) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
		h.Write([]byte{1})
	}
	write(s.Image)
	write(s.Command...)
	write(s.Args...)
	write(s.Env...)
	write(s.Mounts...)
	write(s.Restart)
	// Only when set, so specs from before these fields existed keep their hash
	// (and their running containers) across an agent upgrade.
	if s.Network != "" || len(s.Ports) > 0 {
		write(s.Network)
		write(s.Ports...)
	}
	if len(s.Volumes) > 0 {
		write(s.Volumes...)
	}
	// 32 hex chars (128 bit) — collision-safe and within CRI's 63-char label limit.
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func parseEnv(env []string) []*criapi.KeyValue {
	if len(env) == 0 {
		return nil
	}
	out := make([]*criapi.KeyValue, 0, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		out = append(out, &criapi.KeyValue{Key: k, Value: v})
	}
	return out
}
