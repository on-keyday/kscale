#!/bin/sh
# Phase 3: full datapath. client -> (VIP) -> l4lb XDP (QUIC-LB pick + IPIP encap,
# XDP_TX) -> popcache (IPIP decap, VIP on lo, HTTP serve) -> DSR response -> client.
# Builds on phase 2 (peering already programs l4lb dests + popcache IPIP tunnels).
# Even if the full curl doesn't complete, the l4lb eBPF stat counters show how far
# each packet got (rx -> tcp -> xdp_tx vs dropped on no_dest / mtu).
#
# SYSTEM docker only (privileged + memlock=infinity).
set -e
cd "$(dirname "$0")"
export DOCKER_HOST=unix:///var/run/docker.sock
DC="docker compose -p kscale"
VIP=192.0.2.10
L4LB=node1.l4lb.dp.system.kscale.local
POP=node1.popcache.dp.system.kscale.local
cli() { $DC run --rm cli cli --addr 10.5.0.2:9443 --data /data --role admin "$@" 2>&1 | grep -vE "level=INFO|Container kscale"; }
# must: run a setup op and abort if the CLI did not ALLOW it (a silently rejected op —
# e.g. a stale flag — otherwise surfaces much later as an opaque "curl did not complete").
must() { out=$(cli "$@"); echo "$out" | grep -q '^ALLOWED' || { echo "setup op failed: $*" >&2; echo "$out" >&2; exit 1; }; }
ex() { $DC exec -T "$1" sh -c "$2"; }

./build.sh
# Fresh CP state each run: desired state persists in the shared volume.
$DC --profile datapath down -v >/dev/null 2>&1 || true
$DC --profile datapath up --build -d
echo "=== wait connect ==="; sleep 12
# interface is per-node and must be bound BEFORE start (no post-start rebind).
must --resource interface --op apply --node $L4LB --interface eth0
must --resource interface --op apply --node $POP --interface eth0
must --resource vip --op apply --vip $VIP
# QUIC-LB shared secret: the CP generates the value (default 16 bytes); l4lb start
# fails without one.
must --resource secret --op apply --name quiclb
must --resource node --op start --common_name $POP
must --resource node --op start --common_name $L4LB
echo "=== wait peering (dests + IPIP tunnels) ==="; sleep 9

# popcache: accept the decapped inner packets (dst=VIP) and serve them.
ex popcache "ip addr add $VIP/32 dev lo 2>/dev/null; sysctl -wq net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.lo.rp_filter=0 net.ipv4.conf.eth0.rp_filter=0; true"

L4LB_IP=$(ex l4lb "ip -4 -o addr show eth0 | awk '{print \$4}' | cut -d/ -f1" | tr -d '\r')
POP_IP=$(ex popcache "ip -4 -o addr show eth0 | awk '{print \$4}' | cut -d/ -f1" | tr -d '\r')
echo "l4lb front=$L4LB_IP  popcache=$POP_IP  vip=$VIP"

# client: reach the VIP via the l4lb node.
ex client "ip route replace $VIP/32 via $L4LB_IP dev eth0"
echo "=== client -> curl http://VIP/ (full datapath) ==="
ex client "curl -s -m 5 -o /dev/null -w 'HTTP %{http_code} in %{time_total}s\n' http://$VIP/ || echo 'curl did not complete'"

echo "=== l4lb eBPF stat counters (how far did packets get?) ==="
cli --resource stats --op get --common_name $L4LB 2>&1 | grep -iE "rx_packet|tcp_packet|tcp_syn|xdp|encap|_tx|no_dest|mtu_exceed" | head -12
echo "=== popcache request arrival ==="
$DC logs --tail 25 popcache 2>&1 | grep -iE "GET |request|http|cache|origin" | tail -5 || echo "(no request logged)"
