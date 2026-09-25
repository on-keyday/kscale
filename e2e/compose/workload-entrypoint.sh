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
# Pod-network sandboxes are wired by the kscale CNI plugin (see below).
[plugins.'io.containerd.cri.v1.runtime'.cni]
  conf_dir = "/etc/cni/net.d"
  bin_dir = "/opt/cni/bin"
[plugins.'io.containerd.cri.v1.runtime'.containerd]
  default_runtime_name = "runc"
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc]
    runtime_type = "io.containerd.runc.v2"
    [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc.options]
      # No systemd in this container: plain cgroupfs.
      SystemdCgroup = false
EOF

# kscale CNI plugin + its network config (root-owned, as on a real node).
mkdir -p /etc/cni/net.d /opt/cni/bin
install -m 0755 /usr/local/bin/kscale-cni /opt/cni/bin/kscale-cni
# containerd also runs a "loopback" network per sandbox; kscale-cni serves it too.
ln -sf kscale-cni /opt/cni/bin/loopback
cat > /etc/cni/net.d/10-kscale.conflist <<'CONF'
{
  "cniVersion": "1.0.0",
  "name": "kscale",
  "plugins": [{"type": "kscale-cni", "subnet": "10.200.0.0/24"}]
}
CONF

containerd --config /etc/containerd/config.toml > /var/log/containerd.log 2>&1 &
i=0
until [ -S /run/containerd/containerd.sock ]; do
	i=$((i+1)); [ $i -gt 50 ] && { echo "containerd did not come up" >&2; cat /var/log/containerd.log >&2; exit 1; }
	sleep 0.2
done
echo "containerd up"
exec workloadagent --cri-socket /run/containerd/containerd.sock "$@"
