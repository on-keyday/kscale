package workload

import (
	"fmt"
	"regexp"

	criapi "github.com/on-keyday/kscale/protobuf/proto/cri"
)

// Devices are CDI (Container Device Interface) fully-qualified names,
// "vendor/class=name" (e.g. "nvidia.com/gpu=all"), handed to CRI as
// ContainerConfig.CDI_devices. containerd resolves each name against the CDI
// specs on the node (/etc/cdi, /var/run/cdi) and injects that spec's device
// nodes, mounts, env and hooks into the OCI spec.
//
// Only the name's syntax is checked here. What a name can grant is bounded by the
// node: a CDI spec only exists if the node's root installed it (for NVIDIA,
// `nvidia-ctk cdi generate`), so unlike a raw hostPath a spec cannot reach a
// device the node did not publish. A well-formed name with no spec on the node
// fails at CreateContainer and shows up in `container diff`.
// See notes/ai/2026_11_06_container_cdi_devices.md.

// The CDI name grammar (container-device-interface pkg/parser): vendor and class
// start with a letter, the device name with a letter or digit, and all three end
// with a letter or digit.
var cdiDeviceName = regexp.MustCompile(
	`^[A-Za-z]([A-Za-z0-9_.-]*[A-Za-z0-9])?` + // vendor (domain-like: nvidia.com)
		`/[A-Za-z]([A-Za-z0-9_-]*[A-Za-z0-9])?` + // class (gpu)
		`=[A-Za-z0-9]([A-Za-z0-9_.:-]*[A-Za-z0-9])?$`) // device (all, 0, GPU-<uuid>, 0:1)

// ValidateDevices rejects anything that is not a well-formed CDI name, and
// duplicates.
func ValidateDevices(devs []string) error {
	seen := make(map[string]bool, len(devs))
	for _, d := range devs {
		if !cdiDeviceName.MatchString(d) {
			return fmt.Errorf("device %q: want a CDI name vendor/class=name (e.g. nvidia.com/gpu=all)", d)
		}
		if seen[d] {
			return fmt.Errorf("device %q: listed twice", d)
		}
		seen[d] = true
	}
	return nil
}

func cdiDevices(devs []string) []*criapi.CDIDevice {
	if len(devs) == 0 {
		return nil
	}
	out := make([]*criapi.CDIDevice, 0, len(devs))
	for _, d := range devs {
		out = append(out, &criapi.CDIDevice{Name: d})
	}
	return out
}
