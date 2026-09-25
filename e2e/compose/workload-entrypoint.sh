#!/bin/sh
# Workload node entrypoint: containerd (CRI) + workloadagent in one privileged
# container. Args are passed through to workloadagent.
set -e

# cgroup v2 nesting (same trick as docker's dind): a cgroup with member processes
# cannot delegate controllers to children, so move every process in the (private
# cgroupns) root into a leaf first, then enable the controllers for the subtree
# runc will create pod/container cgroups in.
if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
	mkdir -p /sys/fs/cgroup/init
	xargs -rn1 < /sys/fs/cgroup/cgroup.procs > /sys/fs/cgroup/init/cgroup.procs 2>/dev/null || true
	sed -e 's/ / +/g' -e 's/^/+/' < /sys/fs/cgroup/cgroup.controllers > /sys/fs/cgroup/cgroup.subtree_control
fi

# /var/lib/containerd is a docker volume (a real host fs), so the overlayfs
# snapshotter works — overlay-on-overlay (the container rootfs) would not.
mkdir -p /etc/containerd /var/lib/containerd /run/containerd
cat > /etc/containerd/config.toml <<'EOF'
version = 3
disabled_plugins = ["io.containerd.nri.v1.nri"]
[grpc]
  address = "/run/containerd/containerd.sock"
# Host network only (NamespaceMode NODE): no CNI config needed.
[plugins.'io.containerd.cri.v1.runtime'.containerd]
  default_runtime_name = "runc"
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc]
    runtime_type = "io.containerd.runc.v2"
    [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc.options]
      # No systemd in this container: plain cgroupfs.
      SystemdCgroup = false
EOF

containerd --config /etc/containerd/config.toml > /var/log/containerd.log 2>&1 &
i=0
until [ -S /run/containerd/containerd.sock ]; do
	i=$((i+1)); [ $i -gt 50 ] && { echo "containerd did not come up" >&2; cat /var/log/containerd.log >&2; exit 1; }
	sleep 0.2
done
echo "containerd up"
exec workloadagent --cri-socket /run/containerd/containerd.sock "$@"
