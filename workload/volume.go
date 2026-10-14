package workload

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	criapi "github.com/on-keyday/kscale/protobuf/proto/cri"
)

// Volumes replace raw hostPath mounts. Containers run as root, so a hostPath of
// the caller's choosing ("/" included) handed out the whole node; a volume is
// only ever a directory the agent itself created under its volume root, named by
// a flat token, so a spec cannot point anywhere else
// (notes/ai/2026_10_14_container_hostpath_root.md).
//
// Only the volume directory itself is bind-mounted, never a path inside it: a
// container can plant symlinks within its volume, but nothing resolves them on
// the host. The volume directory's own entry lives in the volume root, which no
// container sees, and is Lstat-checked to be a real directory before every use.
//
// Volumes outlive their containers (a model cache survives a recreate) and are
// shared by name across containers on the same node. Nothing removes them yet.

// DefaultVolumeRoot is where volumes live unless WithVolumeRoot says otherwise.
const DefaultVolumeRoot = "/var/lib/kscale-workload/volumes"

// WithVolumeRoot sets the directory volumes are created under. It must not be
// reachable from inside any container (never mount it, or a parent of it).
func WithVolumeRoot(dir string) Option { return func(e *Engine) { e.volumeRoot = dir } }

var volumeName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)

// Volume is one parsed Spec.Volumes entry.
type Volume struct {
	Name          string
	ContainerPath string
	ReadOnly      bool
}

// ParseVolumes parses "name:/containerPath[:ro]" entries, rejecting anything
// that is not a flat volume name or a clean absolute container path.
func ParseVolumes(vols []string) ([]Volume, error) {
	out := make([]Volume, 0, len(vols))
	for _, v := range vols {
		parts := strings.Split(v, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, fmt.Errorf("volume %q: want name:/containerPath[:ro]", v)
		}
		name, cpath := parts[0], parts[1]
		if !volumeName.MatchString(name) || name == "." || name == ".." || strings.Contains(name, "..") {
			return nil, fmt.Errorf("volume %q: name must match %s", v, volumeName)
		}
		if !path.IsAbs(cpath) || path.Clean(cpath) != cpath || cpath == "/" {
			return nil, fmt.Errorf("volume %q: container path must be a clean absolute path other than /", v)
		}
		ro := false
		if len(parts) == 3 {
			if parts[2] != "ro" {
				return nil, fmt.Errorf("volume %q: the only option is ro", v)
			}
			ro = true
		}
		out = append(out, Volume{Name: name, ContainerPath: cpath, ReadOnly: ro})
	}
	return out, nil
}

// errHostMounts is returned for any spec that still carries raw hostPath mounts.
var errHostMounts = errors.New("hostPath mounts are not allowed (a root container with an arbitrary hostPath owns the node); use volumes")

// ensureVolumes makes each volume's directory exist under the volume root and
// returns the CRI mounts for them. A volume entry that exists but is not a plain
// directory (a symlink in particular) is refused rather than followed.
func (e *Engine) ensureVolumes(vols []Volume) ([]*criapi.Mount, error) {
	if len(vols) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(e.volumeRoot, 0o755); err != nil {
		return nil, fmt.Errorf("volume root: %w", err)
	}
	if fi, err := os.Lstat(e.volumeRoot); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("volume root %s is not a plain directory", e.volumeRoot)
	}
	mounts := make([]*criapi.Mount, 0, len(vols))
	for _, v := range vols {
		dir := filepath.Join(e.volumeRoot, v.Name)
		fi, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return nil, fmt.Errorf("volume %s: %w", v.Name, err)
			}
			if fi, err = os.Lstat(dir); err != nil {
				return nil, fmt.Errorf("volume %s: %w", v.Name, err)
			}
		case err != nil:
			return nil, fmt.Errorf("volume %s: %w", v.Name, err)
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("volume %s: %s is not a plain directory (symlink or file); refusing to mount it", v.Name, dir)
		}
		mounts = append(mounts, &criapi.Mount{HostPath: dir, ContainerPath: v.ContainerPath, Readonly: v.ReadOnly})
	}
	return mounts, nil
}
