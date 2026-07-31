#!/bin/sh
# Phase 1: CP-driven declarative XDP bring-up.
#   apply interface=eth0 (reconcile -> BindInterface), vip (-> UpdateVip), and a
#   16-byte secret (-> SyncSecret); then `node start` (-> StartDataplane ->
#   l4lbdrv.New: InitCrypto + BindBalancer + AttachToLink(eth0)).
# Proof: node start returns ALLOWED AND `ip link show eth0` reports an xdp prog.
#
# Must run against the SYSTEM (rootful) docker (privileged + memlock=infinity).
set -e
cd "$(dirname "$0")"
export DOCKER_HOST=unix:///var/run/docker.sock
DC="docker compose -p kscale"
VIP=192.0.2.10
SECRET=0123456789abcdef # exactly 16 bytes — InitCrypto requires a 16-byte key
CN=node1.l4lb.dp.system.kscale.local
# NB: action arg flags use the resource.yaml field names verbatim, so it is
# --common_name (underscore), not --common-name.
cli() { $DC run --rm cli cli --addr 10.5.0.2:9443 --data /data --role admin "$@" 2>&1 | grep -vE "level=INFO|Container kscale"; }

./build.sh
$DC up --build -d
echo "=== wait for agents to enroll+connect ==="
sleep 12
echo "=== apply desired: interface=eth0, vip=$VIP, secret ==="
cli --resource interface --op apply --interface eth0 | tail -3
cli --resource vip --op apply --vip "$VIP" | tail -3
cli --resource secret --op apply --value "$SECRET" | tail -3
echo "=== wait for reconcile to push BindInterface + UpdateVip + SyncSecret ==="
sleep 6
echo "=== node start (-> StartDataplane -> New -> XDP attach) ==="
cli --resource node --op start --common_name "$CN" | tail -5
echo "=== PROOF: xdp prog attached on l4lb eth0? ==="
if $DC exec -T l4lb ip -d link show eth0 2>&1 | grep -qiE "prog/xdp|xdp "; then
	$DC exec -T l4lb ip -d link show eth0 2>&1 | grep -ioE "xdp[a-z]*|prog/xdp|id [0-9]+" | head
	echo "XDP ATTACHED OK"
else
	echo "(no xdp line — attach may have failed)"
fi
