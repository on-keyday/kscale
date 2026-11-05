#!/bin/sh
# Phase 9: l4_route (B6) — pin VIP proto:port to ONE node that is not a popcache
# backend. client -> VIP:8443 -> l4lb XDP (port_route_map hit, not the consistent
# hash) -> IPIP -> gpunode (own netns; workloadagent --host-ingress: kernel IPIP
# tunnel + VIP on lo) -> a HOST process listening on :8443 -> DSR reply.
#
# Checks:
#   - before the route, VIP:8443 goes to popcache (nothing listens there: refused);
#   - with the route, VIP:8443 reaches gpunode's host listener 3/3, and a UDP
#     datagram to VIP:7443 (not QUIC) reaches gpunode's UDP listener;
#   - VIP:80 still goes to popcache (other ports keep the hash);
#   - l4-route diff reports the ports open_port does not open, then is empty;
#   - deleting the route sends VIP:8443 back to popcache.
#
# Builds on phase3_dummy.sh (fresh CP, real XDP, dummy.o on veth peers, checksum
# offload off). SYSTEM docker only.
set -e
cd "$(dirname "$0")"
export DOCKER_HOST=unix:///var/run/docker.sock
DC="docker compose -p kscale"
VIP=192.0.2.10
GPU=gpu1.workload.dp.system.kscale.local
cli() { $DC run --rm cli cli --addr 10.5.0.2:9443 --data /data --role admin "$@" 2>&1 | grep -vE "level=INFO|Container kscale"; }
must() { out=$(cli "$@"); echo "$out" | grep -q '^ALLOWED' || { echo "setup op failed: $*" >&2; echo "$out" >&2; exit 1; }; }
ex() { $DC exec -T "$1" sh -c "$2"; }
fail() { echo "FAIL: $*" >&2; $DC logs --tail 30 gpunode l4lb >&2; exit 1; }
# curl exit codes: 0 = answered, 7 = refused (an RST came back), 28 = timeout,
# 56 = reset after the body (the nc stand-in closes without reading the request).
get() { ex client "curl -s -m 3 http://$VIP:$1/; echo \" exit=\$?\"" || true; }

sh ./phase3_dummy.sh

echo "=== start gpunode (workload, own netns, --host-ingress) ==="
# The demo seeds authorities for node1 only: a node with another name needs its
# authority before it can authenticate (as the Ansible deploy does).
must --resource authority --op apply --authority_name gpu1.workload.dp.system
$DC --profile routed up -d gpunode
i=0
until cli --resource node --op list | grep -q "\"$GPU\""; do
	i=$((i+1)); [ $i -gt 20 ] && fail "gpunode never joined"
	sleep 3
done
must --resource interface --op apply --node $GPU --interface eth0
# Host listeners: TCP :8443 answers "routed-ok"; UDP :7443 records what arrives.
ex gpunode "cat > /r.sh <<'R'
#!/bin/sh
printf 'HTTP/1.0 200 OK\r\n\r\nrouted-ok'
R
chmod +x /r.sh"
$DC exec -d gpunode sh -c "exec nc -lk -p 8443 -e /r.sh"
$DC exec -d gpunode sh -c "exec nc -lu -p 7443 > /tmp/udp.txt"
i=0
until ex gpunode "ip -4 addr show dev lo | grep -q $VIP"; do
	i=$((i+1)); [ $i -gt 20 ] && fail "host ingress never put the VIP on gpunode's lo"
	sleep 1
done
echo "gpunode: VIP on lo, tunnels: $(ex gpunode "ip -o link show type ipip | awk -F': ' '{print \$2}' | tr '\n' ' '")"

echo "=== before the route: VIP:8443 goes to popcache (no listener there) ==="
before=$(get 8443); echo "$before"
echo "$before" | grep -q 'exit=7' || fail "VIP:8443 before the route: expected refused by popcache, got: $before"

echo "=== l4-route apply: tcp:8443 and udp:7443 -> workload/gpu1 ==="
# gpunode's address reaches the CP with its next stats after the interface bind;
# until then apply stores the route but reports it as not programmed yet.
i=0
until cli --resource l4-route --op apply --name gpu-tcp --port tcp:8443 --node workload/gpu1 | grep -q '^ALLOWED'; do
	i=$((i+1)); [ $i -gt 10 ] && fail "l4-route gpu-tcp never resolved: $(cli --resource l4-route --op diff)"
	sleep 2
done
must --resource l4-route --op apply --name gpu-udp --port udp:7443 --node workload/gpu1
sleep 2

echo "=== client -> VIP:8443 (expect routed-ok from gpunode's host listener) ==="
ok=0
for i in 1 2 3; do
	out=$(get 8443); echo "try $i: $out"
	echo "$out" | grep -q '^routed-ok' && ok=$((ok+1))
done

echo "=== client -> VIP:7443/udp (not QUIC) ==="
ex client "echo hello-udp | nc -u -w 1 $VIP 7443; true"
sleep 1
udp=$(ex gpunode "cat /tmp/udp.txt" || true); echo "gpunode received: ${udp:-<nothing>}"

echo "=== VIP:80 still served by popcache ==="
pop=$(ex client "curl -s -m 3 -o /dev/null -w '%{http_code}' http://$VIP/" || true); echo "HTTP $pop"

echo "=== l4-route diff: no open_port yet ==="
d1=$(cli --resource l4-route --op diff); echo "$d1"
echo "$d1" | grep -q 'gpu-tcp tcp:8443: not opened by open_port' || fail "diff does not report tcp:8443 unopened"
must --resource open-port --op apply --acl_name e2e --ports '["tcp:8443","udp:7443"]'
d2=$(cli --resource l4-route --op diff); echo "$d2"
if echo "$d2" | grep -q 'gpu-'; then fail "diff still reports a route after open_port: $d2"; fi

echo "=== delete the tcp route: VIP:8443 goes back to popcache ==="
must --resource l4-route --op delete --name gpu-tcp
sleep 2
after=$(get 8443); echo "$after"

echo "=== result ==="
res=0
[ "$ok" -eq 3 ] && echo "PASS: 3/3 VIP:8443 reached gpunode's host listener" || { echo "FAIL: $ok/3 VIP:8443 via the route"; res=1; }
[ "$udp" = "hello-udp" ] && echo "PASS: VIP:7443/udp reached gpunode" || { echo "FAIL: udp got '${udp}'"; res=1; }
[ -n "$pop" ] && [ "$pop" != "000" ] && echo "PASS: VIP:80 still answered by popcache (HTTP $pop)" || { echo "FAIL: VIP:80 got '$pop'"; res=1; }
echo "$after" | grep -q 'exit=7' && echo "PASS: route removed -> VIP:8443 back to popcache (refused)" || { echo "FAIL: after delete: $after"; res=1; }
exit $res
