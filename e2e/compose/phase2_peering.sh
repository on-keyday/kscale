#!/bin/sh
# Phase 2: CP-driven l4lb<->popcache peering (l4lb encap direction).
#   apply interface/vip/secret, start BOTH popcache (reports as a backend) and
#   l4lb (real XDP); the CP's membership reconcile builds the dest set from the
#   popcache node's reported MAC/IP/ServerID and pushes UpdateDestinations to l4lb.
# Proof: CP logs "peering: pushed node=<l4lb> count=1" and the l4lb node
#   accepts it (real XDP dest table programmed, no error).
#
# Must run against the SYSTEM (rootful) docker (privileged + memlock=infinity).
set -e
cd "$(dirname "$0")"
export DOCKER_HOST=unix:///var/run/docker.sock
DC="docker compose -p kscale"
L4LB=node1.l4lb.dp.system.kscale.local
POP=node1.popcache.dp.system.kscale.local
cli() { $DC run --rm cli cli --addr 10.5.0.2:9443 --data /data --role admin "$@" 2>&1 | grep -vE "level=INFO|Container kscale"; }

./build.sh
$DC up --build -d
echo "=== wait for agents to enroll+connect ==="
sleep 12
echo "=== apply desired: interface=eth0, vip, secret ==="
cli --resource interface --op apply --interface eth0 >/dev/null
cli --resource vip --op apply --vip 192.0.2.10 >/dev/null
cli --resource secret --op apply --value 0123456789abcdef >/dev/null
echo "=== start popcache (backend) then l4lb (XDP) ==="
cli --resource node --op start --common_name "$POP" | tail -2
cli --resource node --op start --common_name "$L4LB" | tail -2
echo "=== wait for popcache stat report + dest reconcile tick ==="
sleep 9
echo "=== PROOF 1 (encap): CP pushed destinations to l4lb ==="
$DC logs cp 2>&1 | grep -iE "peering: pushed" | tail -2 || echo "(no push logged)"
echo "=== PROOF 2 (encap): l4lb XDP attached ==="
$DC exec -T l4lb ip -d link show eth0 2>&1 | grep -ioE "prog/xdp|id [0-9]+" | head -2
echo "=== PROOF 3 (decap): CP pushed remotes to popcache ==="
$DC logs cp 2>&1 | grep -iE "peering: pushed" | tail -2 || echo "(no remote push logged)"
echo "=== PROOF 4 (decap): popcache created IPIP tunnel(s) to the l4lb front ==="
$DC exec -T popcache ip -d link show type ipip 2>&1 | grep -iE "ipip|remote " | head -4 || echo "(no ipip tunnel)"
echo "=== popcache VIP errors (if any) ==="
$DC logs popcache 2>&1 | grep -iE "BindToDevice|VIPManager|rp_filter|ipip|remote" | tail -4 || echo "(none)"
