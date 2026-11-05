package workload

import (
	"context"
	"testing"
)

// TestDevicesPassedAsCDI: each Spec.Devices entry reaches CRI as a CDI device,
// in order, so containerd resolves it against the node's CDI specs.
func TestDevicesPassedAsCDI(t *testing.T) {
	f := newFake()
	e := NewEngine(f, nil, WithVolumeRoot(t.TempDir()))
	defer e.Close()
	s := nginx()
	s.Devices = []string{"nvidia.com/gpu=all", "vendor.example/class=dev0"}
	if err := applySettled(context.Background(), e, []Spec{s}); err != nil {
		t.Fatal(err)
	}
	c := f.byName("web")
	if c == nil {
		t.Fatal("web not created")
	}
	got := f.cdi[c.Id]
	if len(got) != 2 || got[0].Name != "nvidia.com/gpu=all" || got[1].Name != "vendor.example/class=dev0" {
		t.Fatalf("CDI devices = %+v", got)
	}
}

// TestNoDevicesNoCDI: a spec without devices sends no CDI field at all.
func TestNoDevicesNoCDI(t *testing.T) {
	f := newFake()
	e := NewEngine(f, nil, WithVolumeRoot(t.TempDir()))
	defer e.Close()
	if err := applySettled(context.Background(), e, []Spec{nginx()}); err != nil {
		t.Fatal(err)
	}
	if got := f.cdi[f.byName("web").Id]; got != nil {
		t.Fatalf("CDI devices = %+v, want none", got)
	}
}

// TestDeviceChangeRecreates: devices are part of the spec hash, but only when
// set — a spec without them keeps its pre-devices hash (no recreate on upgrade;
// TestHashStableWithoutNetworkFields pins that older hash).
func TestDeviceChangeRecreates(t *testing.T) {
	s := nginx()
	withGPU := s
	withGPU.Devices = []string{"nvidia.com/gpu=all"}
	if specHash(s) == specHash(withGPU) {
		t.Fatal("adding a device must change the hash")
	}
	other := s
	other.Devices = []string{"nvidia.com/gpu=0"}
	if specHash(withGPU) == specHash(other) {
		t.Fatal("changing the device must change the hash")
	}
}

// TestInvalidDeviceRejected: a malformed device name is refused before the
// runtime is touched.
func TestInvalidDeviceRejected(t *testing.T) {
	f := newFake()
	e := NewEngine(f, nil, WithVolumeRoot(t.TempDir()))
	defer e.Close()
	s := nginx()
	s.Devices = []string{"/dev/nvidia0"}
	if err := applySettled(context.Background(), e, []Spec{s}); err == nil {
		t.Fatal("malformed device accepted")
	}
	if len(f.pods) != 0 || len(f.ctrs) != 0 || f.pullCount() != 0 {
		t.Fatalf("rejected spec touched the runtime: %d pods %d ctrs %d pulls", len(f.pods), len(f.ctrs), f.pullCount())
	}
}

func TestValidateDevices(t *testing.T) {
	for _, d := range []string{
		"", "nvidia.com/gpu", "nvidia.com=all", "/dev/nvidia0", "nvidia.com/gpu=",
		"nvidia.com//gpu=0", "nvidia.com/gpu=all=x", "-bad.com/gpu=0", "nvidia.com/gpu.x=0",
		"nvidia.com/gpu=-0", "nvidia.com/1gpu=0", "nvidia.com/gpu=a b", "nvidia.com/gpu=0/1",
	} {
		if err := ValidateDevices([]string{d}); err == nil {
			t.Errorf("%q accepted", d)
		}
	}
	for _, d := range []string{
		"nvidia.com/gpu=all", "nvidia.com/gpu=0", "nvidia.com/gpu=GPU-1a2b",
		"nvidia.com/mig=0:1", "kscale.test/null=dev0", "a/b=c",
	} {
		if err := ValidateDevices([]string{d}); err != nil {
			t.Errorf("%q rejected: %v", d, err)
		}
	}
	if err := ValidateDevices([]string{"nvidia.com/gpu=0", "nvidia.com/gpu=0"}); err == nil {
		t.Error("duplicate device accepted")
	}
}
