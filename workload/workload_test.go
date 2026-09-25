package workload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	criapi "github.com/on-keyday/kscale/protobuf/proto/cri"
)

// fakeRuntime models a containerd-ish CRI: sandboxes and containers as maps,
// mutated by the lifecycle calls and filtered by label on the List calls. It
// records call counts so tests can assert idempotency (no needless recreate).
type fakeRuntime struct {
	nextID   int
	pods     map[string]*criapi.PodSandbox
	ctrs     map[string]*criapi.Container
	pulled   []string
	creates  int
	removes  int
	startErr error                           // if set, StartContainer fails once
	modes    map[string]criapi.NamespaceMode // sandbox id -> requested network mode
	podIPs   map[string]string               // sandbox id -> IP PodSandboxStatus reports
}

func newFake() *fakeRuntime {
	return &fakeRuntime{pods: map[string]*criapi.PodSandbox{}, ctrs: map[string]*criapi.Container{},
		modes: map[string]criapi.NamespaceMode{}, podIPs: map[string]string{}}
}

func (f *fakeRuntime) id(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s-%d", prefix, f.nextID)
}

func labelMatch(have map[string]string, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func (f *fakeRuntime) PullImage(_ context.Context, r *criapi.PullImageRequest) (*criapi.PullImageResponse, error) {
	f.pulled = append(f.pulled, r.Image.Image)
	return &criapi.PullImageResponse{ImageRef: r.Image.Image}, nil
}

func (f *fakeRuntime) RunPodSandbox(_ context.Context, r *criapi.RunPodSandboxRequest) (*criapi.RunPodSandboxResponse, error) {
	id := f.id("pod")
	f.pods[id] = &criapi.PodSandbox{Id: id, Metadata: &criapi.PodSandboxMetadata{Name: r.Config.Metadata.Name}, Labels: r.Config.Labels, State: criapi.PodSandboxState_SANDBOX_READY}
	f.modes[id] = r.Config.Linux.SecurityContext.NamespaceOptions.Network
	if f.modes[id] == criapi.NamespaceMode_POD {
		f.podIPs[id] = fmt.Sprintf("10.200.0.%d", f.nextID)
	}
	return &criapi.RunPodSandboxResponse{PodSandboxId: id}, nil
}

func (f *fakeRuntime) PodSandboxStatus(_ context.Context, r *criapi.PodSandboxStatusRequest) (*criapi.PodSandboxStatusResponse, error) {
	st := &criapi.PodSandboxStatus{Id: r.PodSandboxId}
	if ip, ok := f.podIPs[r.PodSandboxId]; ok {
		st.Network = &criapi.PodSandboxNetworkStatus{Ip: ip}
	}
	return &criapi.PodSandboxStatusResponse{Status: st}, nil
}

// podOf returns the sandbox a named container runs in.
func (f *fakeRuntime) podOf(name string) string {
	if c := f.byName(name); c != nil {
		return c.PodSandboxId
	}
	return ""
}

func (f *fakeRuntime) StopPodSandbox(_ context.Context, r *criapi.StopPodSandboxRequest) (*criapi.StopPodSandboxResponse, error) {
	if p, ok := f.pods[r.PodSandboxId]; ok {
		p.State = criapi.PodSandboxState_SANDBOX_NOTREADY
	}
	return &criapi.StopPodSandboxResponse{}, nil
}

func (f *fakeRuntime) RemovePodSandbox(_ context.Context, r *criapi.RemovePodSandboxRequest) (*criapi.RemovePodSandboxResponse, error) {
	delete(f.pods, r.PodSandboxId)
	return &criapi.RemovePodSandboxResponse{}, nil
}

func (f *fakeRuntime) ListPodSandbox(_ context.Context, r *criapi.ListPodSandboxRequest) (*criapi.ListPodSandboxResponse, error) {
	var out []*criapi.PodSandbox
	for _, p := range f.pods {
		if r.Filter == nil || labelMatch(p.Labels, r.Filter.LabelSelector) {
			out = append(out, p)
		}
	}
	return &criapi.ListPodSandboxResponse{Items: out}, nil
}

func (f *fakeRuntime) CreateContainer(_ context.Context, r *criapi.CreateContainerRequest) (*criapi.CreateContainerResponse, error) {
	f.creates++
	id := f.id("ctr")
	f.ctrs[id] = &criapi.Container{
		Id:           id,
		PodSandboxId: r.PodSandboxId,
		Metadata:     &criapi.ContainerMetadata{Name: r.Config.Metadata.Name},
		Image:        r.Config.Image,
		Labels:       r.Config.Labels,
		State:        criapi.ContainerState_CONTAINER_CREATED,
	}
	return &criapi.CreateContainerResponse{ContainerId: id}, nil
}

func (f *fakeRuntime) StartContainer(_ context.Context, r *criapi.StartContainerRequest) (*criapi.StartContainerResponse, error) {
	if f.startErr != nil {
		err := f.startErr
		f.startErr = nil
		return nil, err
	}
	if c, ok := f.ctrs[r.ContainerId]; ok {
		c.State = criapi.ContainerState_CONTAINER_RUNNING
	}
	return &criapi.StartContainerResponse{}, nil
}

func (f *fakeRuntime) StopContainer(_ context.Context, r *criapi.StopContainerRequest) (*criapi.StopContainerResponse, error) {
	if c, ok := f.ctrs[r.ContainerId]; ok {
		c.State = criapi.ContainerState_CONTAINER_EXITED
	}
	return &criapi.StopContainerResponse{}, nil
}

func (f *fakeRuntime) RemoveContainer(_ context.Context, r *criapi.RemoveContainerRequest) (*criapi.RemoveContainerResponse, error) {
	f.removes++
	delete(f.ctrs, r.ContainerId)
	return &criapi.RemoveContainerResponse{}, nil
}

func (f *fakeRuntime) ListContainers(_ context.Context, r *criapi.ListContainersRequest) (*criapi.ListContainersResponse, error) {
	var out []*criapi.Container
	for _, c := range f.ctrs {
		if r.Filter == nil || labelMatch(c.Labels, r.Filter.LabelSelector) {
			out = append(out, c)
		}
	}
	return &criapi.ListContainersResponse{Containers: out}, nil
}

// byName returns the single managed container with metadata.name == name.
func (f *fakeRuntime) byName(name string) *criapi.Container {
	for _, c := range f.ctrs {
		if c.Metadata.Name == name {
			return c
		}
	}
	return nil
}

func nginx() Spec {
	return Spec{Name: "web", Image: "nginx:1", Args: []string{"-g", "daemon off;"}, Env: []string{"TZ=UTC"}, Restart: "always"}
}

func TestApplyCreatesAndRuns(t *testing.T) {
	f := newFake()
	e := NewEngine(f, nil)
	if err := e.Apply(context.Background(), []Spec{nginx()}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	c := f.byName("web")
	if c == nil || c.State != criapi.ContainerState_CONTAINER_RUNNING {
		t.Fatalf("web not running: %+v", c)
	}
	if len(f.pods) != 1 {
		t.Fatalf("expected 1 sandbox, got %d", len(f.pods))
	}
	if len(f.pulled) != 1 || f.pulled[0] != "nginx:1" {
		t.Fatalf("image not pulled: %v", f.pulled)
	}
}

func TestReapplySameSpecIsNoop(t *testing.T) {
	f := newFake()
	e := NewEngine(f, nil)
	ctx := context.Background()
	if err := e.Apply(ctx, []Spec{nginx()}); err != nil {
		t.Fatal(err)
	}
	createsAfterFirst := f.creates
	// Re-apply identical spec several times; nothing should be recreated.
	for i := 0; i < 3; i++ {
		if err := e.Apply(ctx, []Spec{nginx()}); err != nil {
			t.Fatal(err)
		}
	}
	if f.creates != createsAfterFirst {
		t.Fatalf("idempotency broken: %d creates after first, %d after re-applies", createsAfterFirst, f.creates)
	}
	if err := e.Converge(ctx); err != nil {
		t.Fatal(err)
	}
	if f.creates != createsAfterFirst {
		t.Fatalf("Converge recreated a matching running container")
	}
}

func TestChangedSpecRecreates(t *testing.T) {
	f := newFake()
	e := NewEngine(f, nil)
	ctx := context.Background()
	if err := e.Apply(ctx, []Spec{nginx()}); err != nil {
		t.Fatal(err)
	}
	oldID := f.byName("web").Id

	changed := nginx()
	changed.Image = "nginx:2"
	if err := e.Apply(ctx, []Spec{changed}); err != nil {
		t.Fatal(err)
	}
	c := f.byName("web")
	if c == nil || c.Id == oldID {
		t.Fatalf("container not recreated on image change (id %q)", c.Id)
	}
	if c.Image.Image != "nginx:2" || c.State != criapi.ContainerState_CONTAINER_RUNNING {
		t.Fatalf("recreated container wrong: %+v", c)
	}
	if len(f.ctrs) != 1 || len(f.pods) != 1 {
		t.Fatalf("stale container/sandbox left behind: %d ctrs %d pods", len(f.ctrs), len(f.pods))
	}
}

func TestUndesiredIsRemoved(t *testing.T) {
	f := newFake()
	e := NewEngine(f, nil)
	ctx := context.Background()
	two := []Spec{nginx(), {Name: "api", Image: "api:1", Restart: "never"}}
	if err := e.Apply(ctx, two); err != nil {
		t.Fatal(err)
	}
	if len(f.ctrs) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(f.ctrs))
	}
	// Drop "api" from the desired set.
	if err := e.Apply(ctx, []Spec{nginx()}); err != nil {
		t.Fatal(err)
	}
	if f.byName("api") != nil {
		t.Fatalf("api should have been removed")
	}
	if f.byName("web") == nil || len(f.ctrs) != 1 || len(f.pods) != 1 {
		t.Fatalf("web should remain alone: %d ctrs %d pods", len(f.ctrs), len(f.pods))
	}
}

func TestRestartAlwaysRecreatesExited(t *testing.T) {
	f := newFake()
	e := NewEngine(f, nil)
	ctx := context.Background()
	if err := e.Apply(ctx, []Spec{nginx()}); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash: the container exits out from under us.
	f.byName("web").State = criapi.ContainerState_CONTAINER_EXITED
	oldID := f.byName("web").Id

	if err := e.Converge(ctx); err != nil {
		t.Fatal(err)
	}
	c := f.byName("web")
	if c == nil || c.State != criapi.ContainerState_CONTAINER_RUNNING || c.Id == oldID {
		t.Fatalf("restart=always did not recreate exited container: %+v", c)
	}
}

func TestRestartNeverLeavesExited(t *testing.T) {
	f := newFake()
	e := NewEngine(f, nil)
	ctx := context.Background()
	spec := Spec{Name: "job", Image: "job:1", Restart: "never"}
	if err := e.Apply(ctx, []Spec{spec}); err != nil {
		t.Fatal(err)
	}
	f.byName("job").State = criapi.ContainerState_CONTAINER_EXITED
	oldID := f.byName("job").Id

	if err := e.Converge(ctx); err != nil {
		t.Fatal(err)
	}
	c := f.byName("job")
	if c == nil || c.Id != oldID || c.State != criapi.ContainerState_CONTAINER_EXITED {
		t.Fatalf("restart=never should leave the exited container alone: %+v", c)
	}
}

func TestListReportsState(t *testing.T) {
	f := newFake()
	e := NewEngine(f, nil)
	ctx := context.Background()
	if err := e.Apply(ctx, []Spec{nginx(), {Name: "api", Image: "api:1"}}); err != nil {
		t.Fatal(err)
	}
	list, err := e.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "api" || list[1].Name != "web" {
		t.Fatalf("unexpected list (want sorted api,web): %+v", list)
	}
	if list[1].State != "RUNNING" || list[1].Image != "nginx:1" {
		t.Fatalf("web status wrong: %+v", list[1])
	}
}

func TestCreateFailureRollsBackSandbox(t *testing.T) {
	f := newFake()
	e := NewEngine(f, nil)
	ctx := context.Background()
	// Make the first StartContainer fail; the container is created but never runs,
	// so Apply returns an error and the next converge must heal it.
	f.startErr = fmt.Errorf("boom")
	err := e.Apply(ctx, []Spec{nginx()})
	if err == nil {
		t.Fatal("expected Apply to surface the start failure")
	}
	// Healing converge: the created-but-not-running container is recreated and started.
	if err := e.Converge(ctx); err != nil {
		t.Fatalf("healing converge: %v", err)
	}
	c := f.byName("web")
	if c == nil || c.State != criapi.ContainerState_CONTAINER_RUNNING {
		t.Fatalf("web not healed to running: %+v", c)
	}
	if len(f.pods) != 1 || len(f.ctrs) != 1 {
		t.Fatalf("leaked sandbox/container: %d pods %d ctrs", len(f.pods), len(f.ctrs))
	}
}

func TestNetworkModeSelectsNamespace(t *testing.T) {
	f := newFake()
	e := NewEngine(f, nil)
	pod := Spec{Name: "pod", Image: "a:1", Network: NetworkPod, Ports: []string{"tcp:8080"}}
	host := Spec{Name: "host", Image: "a:1"}
	if err := e.Apply(context.Background(), []Spec{pod, host}); err != nil {
		t.Fatal(err)
	}
	if m := f.modes[f.podOf("pod")]; m != criapi.NamespaceMode_POD {
		t.Fatalf("network pod -> %v, want POD", m)
	}
	if m := f.modes[f.podOf("host")]; m != criapi.NamespaceMode_NODE {
		t.Fatalf("network \"\" -> %v, want NODE", m)
	}
}

func TestInvalidNetworkSpecRejected(t *testing.T) {
	for _, s := range []Spec{
		{Name: "a", Image: "a:1", Network: "bridge"},
		{Name: "b", Image: "a:1", Ports: []string{"tcp:80"}}, // ports without pod network
		{Name: "c", Image: "a:1", Network: NetworkPod, Ports: []string{"udp:53"}},
		{Name: "d", Image: "a:1", Network: NetworkPod, Ports: []string{"tcp:0"}},
		{Name: "e", Image: "a:1", Network: NetworkPod, Ports: []string{"8080"}},
	} {
		f := newFake()
		if err := NewEngine(f, nil).Apply(context.Background(), []Spec{s}); err == nil {
			t.Errorf("%+v: expected rejection", s)
		}
		if len(f.pods) != 0 {
			t.Errorf("%+v: rejected spec still created a sandbox", s)
		}
	}
}

func TestEndpointsReportPodNetworkOnly(t *testing.T) {
	f := newFake()
	e := NewEngine(f, nil)
	ctx := context.Background()
	pod := Spec{Name: "web", Image: "a:1", Network: NetworkPod, Ports: []string{"tcp:8080", "tcp:9090"}}
	if err := e.Apply(ctx, []Spec{pod, {Name: "host", Image: "a:1"}}); err != nil {
		t.Fatal(err)
	}
	eps, err := e.Endpoints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 || eps[0].Name != "web" || eps[0].SandboxID != f.podOf("web") {
		t.Fatalf("want only web's endpoint, got %+v", eps)
	}
	if want := f.podIPs[f.podOf("web")]; eps[0].PodIP.String() != want {
		t.Fatalf("pod IP %v, want %s", eps[0].PodIP, want)
	}
	if len(eps[0].Ports) != 2 || eps[0].Ports[0] != (Port{"tcp", 8080}) || eps[0].Ports[1] != (Port{"tcp", 9090}) {
		t.Fatalf("ports %+v", eps[0].Ports)
	}
}

func TestHashStableWithoutNetworkFields(t *testing.T) {
	// A spec that never sets Network/Ports must hash as before the fields existed,
	// or an agent upgrade would recreate every running host-network container.
	preNetworkHash := func(s Spec) string { // specHash as it was before Network/Ports
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
		return hex.EncodeToString(h.Sum(nil))[:32]
	}
	s := nginx()
	if specHash(s) != preNetworkHash(s) {
		t.Fatal("a spec without network fields no longer hashes as before")
	}
	withPod := s
	withPod.Network = NetworkPod
	if specHash(s) == specHash(withPod) {
		t.Fatal("network change must change the hash")
	}
}
