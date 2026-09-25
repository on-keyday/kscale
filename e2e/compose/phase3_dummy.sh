#!/bin/sh
# Phase 3 + dummy workaround. veth's ndo_xdp_xmit (used by XDP_TX) is disabled
# unless the PEER veth has an XDP program, so l4lb's XDP_TX silently drops in a
# docker bridge (the peer is a host-side veth with no XDP). ksdk ships l4lb/c/dummy.o
# (XDP_PASS) for exactly this: attach it on the host-side veth peers so ndo_xdp_xmit
# works. Host-side veths live in the host netns -> reach them via a --network host
# privileged container.
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
# e.g. a stale flag — otherwise surfaces much later as an opaque "curl timeout").
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
echo "=== wait peering ==="; sleep 9
ex popcache "ip addr add $VIP/32 dev lo 2>/dev/null; sysctl -wq net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.lo.rp_filter=0 net.ipv4.conf.eth0.rp_filter=0; true"

# Attach dummy.o (XDP_PASS) on the HOST-SIDE veth peer of each container's eth0 so
# veth ndo_xdp_xmit is enabled (l4lb's XDP_TX can then traverse). The host-side
# ifindex is /sys/class/net/eth0/iflink inside the container; a --network host
# container finds that link and attaches dummy.o to it.
echo "=== attach dummy.o on host-side veth peers ==="
for c in l4lb popcache client; do
	IDX=$(ex $c "cat /sys/class/net/eth0/iflink" | tr -d '\r')
	docker run --rm --privileged --network host kscale-e2e sh -c "
		veth=\$(ip -o link | awk -F': ' '/^${IDX}: /{print \$2}' | cut -d'@' -f1)
		[ -n \"\$veth\" ] && ip link set dev \$veth xdpdrv obj /objs/dummy.o sec xdp 2>&1 && echo \"$c: dummy.o (native) on host veth \$veth (idx ${IDX})\" || echo \"$c: attach failed (idx ${IDX} veth \$veth)\"
	"
done

# Disable TX checksum offload on the client: veth offloads leave an unfinalized
# checksum on the wire, which l4lb's XDP encapsulates raw -> after popcache decap
# the inner TCP checksum is invalid (TcpInCsumErrors) and the SYN is dropped.
ex client "ethtool -K eth0 tx off rx off 2>/dev/null; true"
L4LB_IP=$(ex l4lb "ip -4 -o addr show eth0 | awk '{print \$4}' | cut -d/ -f1" | tr -d '\r')
ex client "ip route replace $VIP/32 via $L4LB_IP dev eth0"
echo "=== capture all 3 points + curl ==="
$DC exec -d popcache sh -c 'timeout 9 tcpdump -nni any "ip proto 4 or tcp port 80" -w /tmp/pop.pcap 2>/dev/null'
$DC exec -d client  sh -c 'timeout 9 tcpdump -nni eth0 host '"$VIP"' -w /tmp/cli.pcap 2>/dev/null'
sleep 1
ex client "for i in 1 2 3; do curl -s -m 3 -o /dev/null -w 'HTTP %{http_code}\n' http://$VIP/ || echo 'curl timeout'; done"
sleep 3
echo "--- popcache: outer IPIP (proto 4) in, decapped inner (tcp 80)? ---"
ex popcache 'echo "ipip(proto4): $(tcpdump -nr /tmp/pop.pcap ip proto 4 2>/dev/null | wc -l)  inner(tcp80): $(tcpdump -nr /tmp/pop.pcap tcp port 80 2>/dev/null | wc -l)"; tcpdump -nr /tmp/pop.pcap 2>/dev/null | head -6'
echo "--- client: any reply from VIP (SYN-ACK back)? ---"
ex client 'tcpdump -nr /tmp/cli.pcap 2>/dev/null | head -6'
