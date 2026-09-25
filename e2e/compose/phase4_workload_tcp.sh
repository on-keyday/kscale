#!/bin/sh
# Phase 4: VIP -> host-network workload container over plain TCP (no popcache in the
# L7 path). client -> VIP:8080 -> l4lb XDP (TCP: hash pick, every dst port) -> IPIP
# -> popcache node (decap, VIP on lo) -> the `workload` service sharing popcache's
# netns (stand-in for a CRI NamespaceMode_NODE container) -> DSR reply.
#
# Also probes VIP:8081 (nothing listening): a fast "connection refused" rather than a
# timeout shows l4lb forwards ANY TCP port to the node — port filtering is only the
# router ACL today (notes/ai/2026_09_25_workload_l4_ingress_tcp_quic.md B7).
#
# Builds on phase3_dummy.sh (fresh CP, datapath, dummy.o on veth peers, checksum
# offload off). SYSTEM docker only.
set -e
cd "$(dirname "$0")"
export DOCKER_HOST=unix:///var/run/docker.sock
DC="docker compose -p kscale"
VIP=192.0.2.10
ex() { $DC exec -T "$1" sh -c "$2"; }

sh ./phase3_dummy.sh
echo "=== start workload (shares popcache netns, listens :8080) ==="
$DC --profile workload up -d workload
sleep 2
ex popcache "ss -ltn 'sport = :8080' | tail -n +2"

echo "=== client -> VIP:8080 (expect 200 workload-ok) ==="
ok=0
for i in 1 2 3; do
	body=$(ex client "curl -s -m 3 http://$VIP:8080/" || true)
	echo "try $i: ${body:-<no response>}"
	[ "$body" = "workload-ok" ] && ok=$((ok+1))
done

echo "=== client -> VIP:8081 (no listener; expect refused, not timeout) ==="
# curl exit 7 = connection refused (the RST came back from the node), 28 = timeout.
ex client "curl -s -m 3 -o /dev/null http://$VIP:8081/; echo \"curl exit=\$? (7=refused by node, 28=timeout)\"" || true

echo "=== result ==="
if [ "$ok" -eq 3 ]; then echo "PASS: 3/3 VIP:8080 reached the workload"; else echo "FAIL: $ok/3"; exit 1; fi
