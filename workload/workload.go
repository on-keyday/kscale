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
	"sort"
	"strings"
	"sync"

	criapi "github.com/on-keyday/kscale/protobuf/proto/cri"
)

// Label keys stamped on every managed sandbox + container so converge can find
// and reconcile only what it owns, and detect drift. Unprefixed names (valid CRI
// label keys). The container/sandbox NAME is carried in CRI metadata.name, not a
// label, since resource keys may not be valid label values.
const (
	labelManaged  = "kscale-managed"
	labelSpecHash = "kscale-spec-hash"
	sandboxNS     = "kscale"
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
}

// Spec is one desired container (the WorkloadService wire spec, decoupled from
// protobuf so the engine and its tests don't depend on the pb types).
type Spec struct {
	Name    string
	Image   string
	Command []string
	Args    []string
	Env     []string // "KEY=VALUE"
	Mounts  []string // "hostPath:containerPath[:ro]"
	Restart string   // "always" | "never"
}

// Status is the observed state of one managed container.
type Status struct {
	Name         string
	ContainerID  string
	Image        string
	State        string
	PodSandboxID string
}

// Engine converges a desired container set onto a Runtime. Safe for concurrent
// use; Apply and Converge serialize on a single mutex so a timer-driven converge
// never races an Apply.
type Engine struct {
	rt     Runtime
	logger *slog.Logger

	mu      sync.Mutex
	desired map[string]Spec
}

func NewEngine(rt Runtime, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Engine{rt: rt, logger: logger, desired: map[string]Spec{}}
}

// Apply replaces the desired set with specs and converges once.
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

	if len(errs) > 0 {
		return fmt.Errorf("converge: %w", errors.Join(errs...))
	}
	return nil
}

// ensure reconciles one desired container given its current sandbox/container (or
// nil). A container that matches the desired spec hash is left alone when running
// (or when exited under restart=never); everything else is recreated.
func (e *Engine) ensure(ctx context.Context, spec Spec, pod *criapi.PodSandbox, ctr *criapi.Container) error {
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

	if _, err := e.rt.PullImage(ctx, &criapi.PullImageRequest{
		Image: &criapi.ImageSpec{Image: spec.Image},
	}); err != nil {
		return fmt.Errorf("pull image %q: %w", spec.Image, err)
	}

	sandboxConfig := e.sandboxConfig(spec)
	run, err := e.rt.RunPodSandbox(ctx, &criapi.RunPodSandboxRequest{Config: sandboxConfig})
	if err != nil {
		return fmt.Errorf("run sandbox: %w", err)
	}

	created, err := e.rt.CreateContainer(ctx, &criapi.CreateContainerRequest{
		PodSandboxId:  run.PodSandboxId,
		Config:        e.containerConfig(spec, hash),
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
	return &criapi.PodSandboxConfig{
		Metadata: &criapi.PodSandboxMetadata{Name: spec.Name, Uid: spec.Name, Namespace: sandboxNS},
		Labels:   map[string]string{labelManaged: "true"},
		Linux: &criapi.LinuxPodSandboxConfig{
			SecurityContext: &criapi.LinuxSandboxSecurityContext{
				// Host network (stage 1): no CNI, container shares the node netns.
				NamespaceOptions: &criapi.NamespaceOption{Network: criapi.NamespaceMode_NODE},
			},
		},
	}
}

func (e *Engine) containerConfig(spec Spec, hash string) *criapi.ContainerConfig {
	return &criapi.ContainerConfig{
		Metadata: &criapi.ContainerMetadata{Name: spec.Name},
		Image:    &criapi.ImageSpec{Image: spec.Image},
		Command:  spec.Command,
		Args:     spec.Args,
		Envs:     parseEnv(spec.Env),
		Mounts:   parseMounts(spec.Mounts),
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

// List reports the observed state of every managed container.
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
		out = append(out, Status{
			Name:         name,
			ContainerID:  c.Id,
			Image:        image,
			State:        stateString(c.State),
			PodSandboxID: c.PodSandboxId,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
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

// parseMounts turns "hostPath:containerPath[:ro]" into CRI Mounts. Entries that
// don't have at least host:container are skipped.
func parseMounts(mounts []string) []*criapi.Mount {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]*criapi.Mount, 0, len(mounts))
	for _, m := range mounts {
		parts := strings.Split(m, ":")
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		mount := &criapi.Mount{HostPath: parts[0], ContainerPath: parts[1]}
		if len(parts) >= 3 && parts[2] == "ro" {
			mount.Readonly = true
		}
		out = append(out, mount)
	}
	return out
}
