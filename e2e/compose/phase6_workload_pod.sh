#!/bin/sh
# Phase 6: VIP -> a CRI workload in its OWN netns (network "pod"). The kscale CNI
# plugin wires the pod (veth + 10.200.0.0/24 IP + gateway neighbor); the workload
# eBPF datapath (workloadagent --netdp-obj) decaps l4lb's IPIP on eth0 for the
# declared port, DNATs VIP -> pod IP and redirects into the pod; the pod's reply
# is SNATed back to the VIP on its host veth and leaves eth0 directly (DSR).
#
# Checks:
#   - VIP:8080 reaches the pod 3/3, and :8080 is NOT listening in the node netns
#     (it really is a separate netns);
#   - VIP:8081 (no pod owns it) still takes popcache's kernel path -> refused;
#   - IPIP from a non-l4lb source is not decapped (datapath counter in_not_lb_src).
# Also runs the netdp BPF_PROG_TEST_RUN unit tests inside the privileged node.
# SYSTEM docker only; needs registry access (busybox, pause).
set -e
cd "$(dirname "$0")"
export DOCKER_HOST=unix:///var/run/docker.sock
DC="docker compose -p kscale"
VIP=192.0.2.10
POP_IP=10.5.0.4
WL=node1.workload.dp.system.kscale.local
cli() { $DC run --rm cli cli --addr 10.5.0.2:9443 --data /data --role admin "$@" 2>&1 | grep -vE "level=INFO|Container kscale"; }
must() { out=$(cli "$@"); echo "$out" | grep -q '^ALLOWED' || { echo "setup op failed: $*" >&2; echo "$out" >&2; exit 1; }; }
ex() { $DC exec -T "$1" sh -c "$2"; }
counter() { # sum a netdp counter across CPUs (index per the C enum)
	ex workloadnode "bpftool -j map dump name netdp_counters" |
		python3 -c "import json,sys; d=json.load(sys.stdin); print(sum(v['value'] for e in d if e['formatted']['key']==$1 for v in e['formatted']['values']))"
}

sh ./phase3_dummy.sh
echo "=== start workload node (containerd + kscale-cni + workloadagent --netdp-obj) ==="
$DC --profile workload-cri up --build -d workloadnode
i=0
until cli --resource node --op list | grep -q "\"$WL\""; do
	i=$((i+1)); [ $i -gt 20 ] && { echo "workload node never joined" >&2; $DC logs --tail 30 workloadnode >&2; exit 1; }
	sleep 3
done
echo "workload node joined: $WL"
# The datapath attaches its ingress program to the bound interface.
must --resource interface --op apply --node $WL --interface eth0

echo "=== container apply: busybox httpd :8080, network pod ==="
must --resource container --op apply --name web --node workload/node1 \
	--image docker.io/library/busybox:1.37 --restart always --network pod --ports tcp:8080 \
	--command '["sh","-c","mkdir -p /www && echo workload-ok > /www/index.html && exec httpd -f -p 8080 -h /www"]'

i=0
until $DC logs workloadnode 2>&1 | grep -q 'netdp: steered ports.*tcp:8080->10.200.0'; do
	i=$((i+1)); [ $i -gt 40 ] && { echo "port never steered" >&2; $DC logs --tail 40 workloadnode >&2; exit 1; }
	sleep 3
done
$DC logs workloadnode 2>&1 | grep 'netdp: steered ports' | tail -1
echo "--- node netns must NOT have :8080 (the pod has its own netns) ---"
if ex popcache "ss -ltn 'sport = :8080' | grep -q LISTEN"; then echo "FAIL: :8080 listens in the node netns" >&2; exit 1; fi
echo "ok: no :8080 in the node netns"

echo "=== client -> VIP:8080 (expect workload-ok) ==="
ok=0
for i in 1 2 3; do
	body=$(ex client "curl -s -m 3 http://$VIP:8080/" || true)
	echo "try $i: ${body:-<no response>}"
	[ "$body" = "workload-ok" ] && ok=$((ok+1))
done

echo "=== client -> VIP:8081 (no pod owns it; expect refused via popcache's kernel path) ==="
ex client "curl -s -m 3 -o /dev/null http://$VIP:8081/; echo \"curl exit=\$? (7=refused, 28=timeout)\"" || true

echo "=== spoofed IPIP from the client (not an l4lb front) must not be decapped ==="
before=$(counter 1)
ex client "ip tunnel add spoof mode ipip remote $POP_IP local 10.5.0.5 && ip link set spoof up && ip route replace $VIP/32 dev spoof"
ex client "curl -s -m 2 -o /dev/null http://$VIP:8080/; true"
L4LB_IP=10.5.0.3
ex client "ip route replace $VIP/32 via $L4LB_IP dev eth0; ip tunnel del spoof"
after=$(counter 1)
echo "in_not_lb_src: $before -> $after"
spoof_ok=0; [ "$after" -gt "$before" ] && spoof_ok=1

echo "=== prometheus: netdp counters exported by the workload node ==="
# scrape-metrics returns the exposition text JSON-escaped in "metrics"; pick our series.
sleep 3 # one host-metrics tick (2s) so the exported values include the requests above
prom=$(cli --resource stats --op scrape-metrics --common_name $WL |
	python3 -c "import json,sys; t=sys.stdin.read(); t=t[t.index('{'):]; print(json.loads(t)['metrics'])" |
	grep -E '^ksdk_workload_netdp_(in_steered_total|in_not_lb_src_total|out_snat_total|steered_ports)')
echo "$prom"
prom_ok=0
echo "$prom" | grep -qE '^ksdk_workload_netdp_in_steered_total\{[^}]*\} [1-9]' &&
	echo "$prom" | grep -qE '^ksdk_workload_netdp_out_snat_total\{[^}]*\} [1-9]' &&
	echo "$prom" | grep -qE '^ksdk_workload_netdp_steered_ports\{[^}]*\} 1$' && prom_ok=1

echo "=== netdp unit tests (BPF_PROG_TEST_RUN) inside the privileged node ==="
unit_ok=0
if (cd ../.. && CGO_ENABLED=0 go test -c -o e2e/compose/stage/netdp.test ./workload/netdp); then
	docker cp stage/netdp.test "$($DC ps -q workloadnode)":/tmp/netdp.test
	docker cp ../../workload/netdp/c/netdp.o "$($DC ps -q workloadnode)":/tmp/netdp.o
	ex workloadnode "mkdir -p /tmp/netdp/c && mv /tmp/netdp.o /tmp/netdp/c/ && cd /tmp/netdp && /tmp/netdp.test -test.v 2>&1 | grep -E '^(--- |PASS|FAIL|ok)'" && unit_ok=1
fi

echo "=== result ==="
fail=0
[ "$ok" -eq 3 ] && echo "PASS: 3/3 VIP:8080 reached the pod-network workload" || { echo "FAIL: $ok/3 VIP:8080"; fail=1; }
[ "$spoof_ok" -eq 1 ] && echo "PASS: spoofed IPIP left alone" || { echo "FAIL: spoofed IPIP not counted"; fail=1; }
[ "$prom_ok" -eq 1 ] && echo "PASS: netdp counters on /metrics" || { echo "FAIL: netdp counters not exported"; fail=1; }
[ "$unit_ok" -eq 1 ] && echo "PASS: netdp unit tests" || { echo "FAIL: netdp unit tests"; fail=1; }
exit $fail
