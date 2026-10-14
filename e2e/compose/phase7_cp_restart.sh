#!/bin/sh
# Phase 7: a control-plane restart must not break the datapath.
#
# Two regressions seen on the live fleet (2026-09-25):
#   - popcache's listeners were bound to the CP connection's ctx (StartDataplane),
#     so a CP restart closed them for good while the node still said Running
#     (notes/bugs/bug_2026_09_25_popcache_listeners_die_with_cp_connection.md);
#   - the fresh CP pushed l4lb a self-only dest table (no popcache backends)
#     until the popcache stats arrived (~21s)
#     (notes/bugs/bug_2026_09_25_cp_restart_l4lb_self_only_dests.md).
#
# A third: the CP re-pushes l4lb_object on every (re)connect, and l4lb rebuilt its
# driver (re-attached XDP) for each push even when nothing changed
# (notes/bugs/bug_2026_10_15_l4lb_object_reload_on_every_connect.md).
#
# Checks, across a `docker restart` of the CP:
#   - VIP:80 is served by popcache before AND after (502 = popcache answered; there
#     is no origin in this harness), i.e. the listener survived;
#   - the restarted CP never pushes l4lb a dest table of count=1 (self only);
#   - with l4lb_object declared, l4lb does not rebuild its driver on reconnect.
# SYSTEM docker only.
set -e
cd "$(dirname "$0")"
export DOCKER_HOST=unix:///var/run/docker.sock
DC="docker compose -p kscale"
VIP=192.0.2.10
ex() { $DC exec -T "$1" sh -c "$2"; }
cli() { $DC run --rm cli cli --addr 10.5.0.2:9443 --data /data --role admin "$@" 2>&1 | grep -vE "level=INFO|Container kscale"; }
must() { out=$(cli "$@"); echo "$out" | grep -q '^ALLOWED' || { echo "setup op failed: $*" >&2; echo "$out" >&2; exit 1; }; }
L4LB=node1.l4lb.dp.system.kscale.local
built() { $DC logs --no-log-prefix l4lb 2>&1 | grep -c 'l4lb: driver built' || true; }
code() { ex client "curl -s -m 3 -o /dev/null -w '%{http_code}' http://$VIP/" || true; }

sh ./phase3_dummy.sh >/dev/null

# Declare the objects l4lb already runs (same files, now via the file store), the
# way kscale_l4lb_deploy.sh does: upload -> node_file -> l4lb_object.
echo "=== declare l4lb_object (same lb.o / init_crypto.o, via the node file store) ==="
for f in lb.o init_crypto.o; do
	must --resource cplane-file --op upload --file-name "$f" --content "/objs/$f"
	must --resource node-file --op apply --name "l4lb-$f" --node "$L4LB" --cplane-file "$f" --save-as "$f"
done
i=0
until ex l4lb "test -s /data/l4lb_files/lb.o && test -s /data/l4lb_files/init_crypto.o"; do
	i=$((i+1)); [ $i -gt 30 ] && { echo "objects never reached the l4lb file store" >&2; exit 1; }
	sleep 1
done
b0=$(built)
must --resource l4lb-object --op apply --node "$L4LB" --object lb.o --crypto-object init_crypto.o --pin-dir /sys/fs/bpf
# The path changes (/objs -> the file store), so exactly one rebuild is expected here.
i=0
until [ "$(built)" -gt "$b0" ]; do
	i=$((i+1)); [ $i -gt 30 ] && { echo "l4lb_object apply never rebuilt the driver" >&2; exit 1; }
	sleep 1
done
b1=$(built)
echo "l4lb driver builds before the restart: $b1"
before=$(code)
echo "before CP restart: HTTP $before"

# Log lines before the restart (the restarted container keeps appending).
n0=$($DC logs --no-log-prefix cp 2>&1 | wc -l)
echo "=== restart the CP container (popcache held until 10s after l4lb rejoined) ==="
# Pausing popcache across the restart forces the race that emptied the l4lb dest
# table on the live fleet: the fresh CP sees l4lb before any popcache. 10s is
# inside the 30s startup grace (counted from l4lb's arrival), so no self-only push
# may happen.
docker pause "$($DC ps -q popcache)" >/dev/null
docker restart "$($DC ps -q cp)" >/dev/null
i=0
until $DC logs --no-log-prefix cp 2>&1 | tail -n +$((n0 + 1)) | grep -q 'dataplane node joined" dp_type=l4lb'; do
	i=$((i+1)); [ $i -gt 60 ] && { echo "l4lb never rejoined" >&2; docker unpause "$($DC ps -q popcache)"; exit 1; }
	sleep 2
done
echo "l4lb rejoined; holding popcache 10s more"
sleep 10
docker unpause "$($DC ps -q popcache)" >/dev/null
# popcache reconnect + reconcile + stats.
sleep 45
after=$(code)
echo "after CP restart:  HTTP $after"

echo "--- CP peering pushes to l4lb since the restart ---"
pushes=$($DC logs --no-log-prefix cp 2>&1 | tail -n +$((n0 + 1)) | grep -oE 'peering: pushed" node=node1\.l4lb[^ ]* count=[0-9]+' || true)
echo "${pushes:-<none>}"

b2=$(built)
echo "l4lb driver builds after the restart:  $b2"

fail=0
[ "$before" = "502" ] || { echo "FAIL: popcache not serving before the restart (HTTP $before)"; fail=1; }
[ "$after" = "502" ] && echo "PASS: popcache still serving after the CP restart" || { echo "FAIL: popcache not serving after the CP restart (HTTP $after)"; fail=1; }
if echo "$pushes" | grep -q 'count=1$'; then
	echo "FAIL: restarted CP pushed l4lb a self-only dest table"; fail=1
else
	echo "PASS: no self-only dest push to l4lb after the restart"
fi
if [ "$b2" = "$b1" ]; then
	echo "PASS: l4lb kept its driver across the reconnect (no XDP re-attach)"
else
	echo "FAIL: l4lb rebuilt its driver on reconnect ($b1 -> $b2)"; fail=1
fi
exit $fail
