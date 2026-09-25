#!/bin/sh
# Phase 5: VIP -> a REAL CRI workload container over plain TCP. Like phase 4, but the
# listener is created by kscale itself: `container apply` -> CP reconcile ->
# workloadagent -> containerd (CRI) -> runc, as a NamespaceMode_NODE (host network)
# sandbox. The workload node shares popcache's netns (the s1/s2 co-location), so the
# container sits behind the same VIP-on-lo that l4lb IPIP-forwards to.
#
# Needs registry access: the engine always PullImage's (busybox from docker.io) and
# containerd pulls its pause image for the sandbox. SYSTEM docker only.
set -e
cd "$(dirname "$0")"
export DOCKER_HOST=unix:///var/run/docker.sock
DC="docker compose -p kscale"
VIP=192.0.2.10
WL=node1.workload.dp.system.kscale.local
cli() { $DC run --rm cli cli --addr 10.5.0.2:9443 --data /data --role admin "$@" 2>&1 | grep -vE "level=INFO|Container kscale"; }
must() { out=$(cli "$@"); echo "$out" | grep -q '^ALLOWED' || { echo "setup op failed: $*" >&2; echo "$out" >&2; exit 1; }; }
ex() { $DC exec -T "$1" sh -c "$2"; }

sh ./phase3_dummy.sh
echo "=== start workload node (containerd + workloadagent in popcache netns) ==="
$DC --profile workload-cri up --build -d workloadnode
i=0
until cli --resource node --op list | grep -q "\"$WL\""; do
	i=$((i+1)); [ $i -gt 20 ] && { echo "workload node never joined" >&2; $DC logs --tail 30 workloadnode >&2; exit 1; }
	sleep 3
done
echo "workload node joined: $WL"

echo "=== container apply: busybox httpd on :8080, host network ==="
must --resource container --op apply --name web --node workload/node1 \
	--image docker.io/library/busybox:1.37 --restart always \
	--command '["sh","-c","mkdir -p /www && echo workload-ok > /www/index.html && exec httpd -f -p 8080 -h /www"]'

# Wait until the listener is up in the shared netns (pull + sandbox + start).
i=0
until ex popcache "ss -ltn 'sport = :8080' | grep -q LISTEN"; do
	i=$((i+1)); [ $i -gt 40 ] && { echo "container never listened on :8080" >&2; $DC logs --tail 40 workloadnode >&2; exit 1; }
	sleep 3
done
echo "--- container diff (empty = in sync) ---"
cli --resource container --op diff

echo "=== client -> VIP:8080 (expect workload-ok) ==="
ok=0
for i in 1 2 3; do
	body=$(ex client "curl -s -m 3 http://$VIP:8080/" || true)
	echo "try $i: ${body:-<no response>}"
	[ "$body" = "workload-ok" ] && ok=$((ok+1))
done

echo "=== result ==="
if [ "$ok" -eq 3 ]; then echo "PASS: 3/3 VIP:8080 reached the CRI workload"; else echo "FAIL: $ok/3"; exit 1; fi
