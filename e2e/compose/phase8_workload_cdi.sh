#!/bin/sh
# Phase 8: container devices (CDI) end to end, without a GPU.
#   container apply --devices -> CP reconcile -> workloadagent -> CRI CDI_devices
#   -> containerd resolves the name against /etc/cdi -> OCI runtime injects it.
# The workload node carries a test CDI spec (workload-entrypoint.sh):
# kscale.test/null=dev0 = /dev/null as /dev/kscale-null + an env var + a
# createContainer hook (the hook kind nvidia-ctk's spec uses).
#
# Checks:
#   - a malformed device name is refused at apply;
#   - the device node and env reach the container, and the hook ran;
#   - a well-formed name the node has no spec for never starts: apply reports
#     containerd's reason, container diff reports it missing, and it does not
#     disturb the other container.
#
#   ./phase8_workload_cdi.sh [runc|youki]     (default runc; youki = production)
#
# Lighter than phase 5: no datapath (no XDP), only cp + popcache (whose netns the
# workload node shares) + the workload node. SYSTEM docker only; needs registry
# access (busybox, pause).
set -e
cd "$(dirname "$0")"
export DOCKER_HOST=unix:///var/run/docker.sock
export E2E_OCI_RUNTIME="${1:-runc}"
DC="docker compose -p kscale"
WL=node1.workload.dp.system.kscale.local
cli() { $DC run --rm cli cli --addr 10.5.0.2:9443 --data /data --role admin "$@" 2>&1 | grep -vE "level=INFO|Container kscale"; }
must() { out=$(cli "$@"); echo "$out" | grep -q '^ALLOWED' || { echo "setup op failed: $*" >&2; echo "$out" >&2; exit 1; }; }
ex() { $DC exec -T "$1" sh -c "$2"; }
fail() { echo "FAIL: $*" >&2; $DC logs --tail 40 workloadnode >&2; exit 1; }

./build.sh
$DC --profile workload-cri --profile datapath down -v >/dev/null 2>&1 || true
$DC up --build -d cp popcache
echo "=== start workload node (OCI runtime: $E2E_OCI_RUNTIME) ==="
$DC --profile workload-cri up --build -d workloadnode
i=0
until cli --resource node --op list | grep -q "\"$WL\""; do
	i=$((i+1)); [ $i -gt 20 ] && fail "workload node never joined"
	sleep 3
done
ex workloadnode "grep -q 'default_runtime_name = \"$E2E_OCI_RUNTIME\"' /etc/containerd/config.toml" \
	|| fail "containerd is not configured for $E2E_OCI_RUNTIME"

echo "=== malformed device name: must be refused ==="
out=$(cli --resource container --op apply --name bad --node workload/node1 \
	--image docker.io/library/busybox:1.37 --devices '["/dev/null"]')
echo "$out" | grep -q 'CDI name' || { echo "$out" >&2; fail "malformed device was not refused"; }
echo "refused as expected"

echo "=== container with the test CDI device ==="
must --resource container --op apply --name gpu-sim --node workload/node1 \
	--image docker.io/library/busybox:1.37 --restart always \
	--devices '["kscale.test/null=dev0"]' --volumes '["cdi:/out"]' \
	--command '["sh","-c","if [ -c /dev/kscale-null ]; then t=char; else t=missing; fi; echo \"$t env=$KSCALE_CDI_DEVICE\" > /out/result; exec sleep 3600"]'

i=0
# Volumes live beside --data (/data -> /volumes) on the e2e node.
until ex workloadnode "test -s /volumes/cdi/result"; do
	i=$((i+1)); [ $i -gt 40 ] && fail "gpu-sim never wrote its result"
	sleep 3
done
result=$(ex workloadnode "cat /volumes/cdi/result")
echo "in-container: $result"
[ "$result" = "char env=dev0" ] || fail "device/env not injected (got: $result)"
ex workloadnode "test -s /tmp/kscale-cdi-hook" || fail "the CDI createContainer hook did not run"
echo "hook ran: $(ex workloadnode 'wc -l < /tmp/kscale-cdi-hook') time(s)"

echo "=== well-formed name with no CDI spec on the node: must not start ==="
# The spec is stored, but the synchronous push fails on the node, so the apply
# itself reports containerd's reason. Seeing that text at all needs the CRI
# error trailers to decode (hpack dynamic-table order, fixed alongside).
out=$(cli --resource container --op apply --name nodev-sim --node workload/node1 \
	--image docker.io/library/busybox:1.37 --restart always \
	--devices '["kscale.test/null=nope"]' --command '["sleep","3600"]')
echo "$out"
echo "$out" | grep -q 'unresolvable CDI devices kscale.test/null=nope' \
	|| fail "apply did not report containerd's reason for the unresolvable device"
sleep 15 # a few 5s converges
diff_out=$(cli --resource container --op diff)
echo "$diff_out"
echo "$diff_out" | grep -q 'nodev-sim: +missing' || fail "unresolvable device: container diff does not report nodev-sim missing"
if echo "$diff_out" | grep -q 'gpu-sim'; then fail "gpu-sim drifted while nodev-sim failed"; fi

must --resource container --op delete --name nodev-sim
must --resource container --op delete --name gpu-sim

echo "=== result ==="
echo "PASS: CDI device injection under $E2E_OCI_RUNTIME (device + env + createContainer hook; unknown device refused at create)"
