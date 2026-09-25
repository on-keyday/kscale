#!/bin/sh
# Measure how fresh the control plane's stat cache is for the dataplanes whose
# Hooks.Stats() is delta-reported (l4lb eBPF counters, dns request counters),
# against the node's own /metrics as ground truth.
#
# Hypothesis (notes/ai/2026_09_25_workload_pod_netns_ebpf_design.md): the dp
# returns those entries only when they changed since the last Stats() call, the
# substrate calls Stats() from two goroutines (2s host-metrics ticker +
# StreamStats), and the CP's cache REPLACES a node's batch on every StreamStats
# message. So the CP should see the entry only in the batch right after a change
# that StreamStats (not the ticker) happened to observe, and lose it again in the
# next batch.
#
# Method: a 1s `stats watch` per node records, for each CP snapshot, whether the
# l4lb / dns entry is present; traffic runs for TRAFFIC seconds, then IDLE seconds
# of silence. /metrics is scraped at the end of each phase for comparison.
# SYSTEM docker only.
set -e
cd "$(dirname "$0")"
export DOCKER_HOST=unix:///var/run/docker.sock
DC="docker compose -p kscale"
VIP=192.0.2.10
L4LB=node1.l4lb.dp.system.kscale.local
DNS=node1.dns.dp.system.kscale.local
TRAFFIC=${TRAFFIC:-20}
IDLE=${IDLE:-12}
OUT=stage/freshness
cli() { $DC run --rm -T cli cli --addr 10.5.0.2:9443 --data /data --role admin "$@" 2>&1 | grep -vE "level=INFO|Container kscale"; }
prom() { # $1 node, $2 metric -> value from the node's /metrics
	cli --resource stats --op scrape-metrics --common_name "$1" |
		python3 -c "import json,sys; t=sys.stdin.read(); t=t[t.index('{'):]; print(json.loads(t)['metrics'])" |
		awk -v m="$2" '$1 ~ "^"m"[{]" {print $2}'
}

sh ./phase3_dummy.sh >/dev/null
$DC --profile dns up -d dns
sleep 8
mkdir -p "$OUT"; rm -f "$OUT"/*.json

echo "=== watch CP snapshots (1s) for $((TRAFFIC + IDLE))s: ${TRAFFIC}s traffic, then ${IDLE}s idle ==="
secs=$((TRAFFIC + IDLE + 4))
( timeout $secs $DC run --rm -T cli cli --addr 10.5.0.2:9443 --data /data --role admin \
	--resource stats --op watch --common_name $L4LB --interval 1s > "$OUT/l4lb.json" 2>/dev/null || true ) &
( timeout $secs $DC run --rm -T cli cli --addr 10.5.0.2:9443 --data /data --role admin \
	--resource stats --op watch --common_name $DNS --interval 1s > "$OUT/dns.json" 2>/dev/null || true ) &
sleep 3 # let both watches connect

$DC exec -T client sh -c "end=\$((\$(date +%s)+$TRAFFIC)); while [ \$(date +%s) -lt \$end ]; do
	curl -s -m 1 -o /dev/null http://$VIP/ ; nslookup a.example.test 10.5.0.7 >/dev/null 2>&1; sleep 0.2; done"
echo "--- end of traffic: node /metrics ---"
echo "l4lb tcp_packet_total=$(prom $L4LB ksdk_l4lb_ebpf_tcp_packet_total)  dns requests_total=$(prom $DNS ksdk_dns_requests_total)"
wait
echo "--- end of idle: node /metrics ---"
echo "l4lb tcp_packet_total=$(prom $L4LB ksdk_l4lb_ebpf_tcp_packet_total)  dns requests_total=$(prom $DNS ksdk_dns_requests_total)"

echo "=== CP snapshots: is the delta-reported entry present? (1 char per 1s snapshot: # = present, . = absent) ==="
python3 - "$OUT/l4lb.json" l4lb "$OUT/dns.json" dns "$TRAFFIC" <<'PY'
import json, sys
def snapshots(path):
    t = open(path).read()
    dec, i, out = json.JSONDecoder(), 0, []
    while True:
        j = t.find('{', i)
        if j < 0:
            return out
        try:
            obj, i = dec.raw_decode(t, j)
        except ValueError:
            i = j + 1
            continue
        if isinstance(obj, dict) and 'stats' in obj:
            out.append(obj)
args = sys.argv[1:]
traffic = int(args[4])
for path, key in ((args[0], args[1]), (args[2], args[3])):
    snaps = snapshots(path)
    marks = ''.join('#' if any(key in s for s in snap['stats']) else '.' for snap in snaps)
    # watch starts ~3s before traffic; split the strip at the traffic/idle boundary.
    cut = min(len(marks), 3 + traffic)
    busy, idle = marks[3:cut], marks[cut:]
    pct = lambda m: f"{100 * m.count('#') // max(1, len(m))}%"
    print(f"{key:5} traffic [{busy}] present {pct(busy)}   idle [{idle}] present {pct(idle)}   ({len(snaps)} snapshots)")
PY
