// kscale workload datapath: steer VIP:<port> TCP into pod-network containers
// and send their replies straight back (DSR). Loaded and fed by workloadagent
// (workload/netdp); see notes/ai/2026_09_25_workload_pod_netns_ebpf_design.md.
//
//   netdp_ingress  (tcx ingress on the node's bound NIC)
//     IPIP from a known l4lb front, inner TCP to VIP:<port in ports_in>
//       -> strip the outer IPv4, DNAT VIP -> pod IP, set pod/host-veth MACs,
//          bpf_redirect_peer into the pod.
//     anything else -> TC_ACT_OK (popcache's kernel IPIP decap still handles it).
//
//   netdp_egress   (tcx ingress on each pod's host-side veth = what the pod sends)
//     TCP from <pod IP, sport in ports_out>
//       -> SNAT pod IP -> VIP, bpf_redirect_neigh out of the bound NIC.
//     anything else -> TC_ACT_OK (host stack; forwarding is off, so it goes nowhere).
//
// The NAT is stateless: (proto, port) maps 1:1 to one pod, so no conntrack.
#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/in.h>
#include <linux/ip.h>
#include <linux/pkt_cls.h>
#include <linux/tcp.h>
#include <linux/types.h>
#include <bpf/bpf_endian.h>
#include <bpf/bpf_helpers.h>

#ifndef BPF_F_ADJ_ROOM_DECAP_L3_IPV4
#define BPF_F_ADJ_ROOM_DECAP_L3_IPV4 (1ULL << 7)
#endif

#define IP_MF 0x2000
#define IP_OFFSET 0x1FFF

struct config {
  __u32 vip;          // network order; 0 = unset (steer nothing)
  __u32 nic_ifindex;  // bound NIC for replies; 0 = unset
};

struct port_key {
  __u8 proto;
  __u8 pad;
  __u16 port;  // network order
};

struct pod_dest {
  __u32 ifindex;  // host-side veth
  __u32 pod_ip;   // network order
  __u8 pod_mac[ETH_ALEN];
  __u8 host_mac[ETH_ALEN];
};

struct out_key {
  __u32 pod_ip;  // network order
  __u8 proto;
  __u8 pad;
  __u16 port;  // network order (the pod's source port)
};

enum counter {
  C_IN_STEERED,     // decapped + redirected into a pod
  C_IN_NOT_LB_SRC,  // IPIP whose outer source is not a known l4lb (left alone)
  C_IN_NO_PORT,     // IPIP to VIP from an l4lb but no pod owns the port (left alone)
  C_IN_ERR,         // a helper failed mid-rewrite (dropped)
  C_OUT_SNAT,       // reply SNATed + redirected out
  C_OUT_ERR,
  C_MAX,
};

struct {
  __uint(type, BPF_MAP_TYPE_ARRAY);
  __uint(max_entries, 1);
  __type(key, __u32);
  __type(value, struct config);
} netdp_config SEC(".maps");

struct {
  __uint(type, BPF_MAP_TYPE_HASH);
  __uint(max_entries, 256);
  __type(key, __u32);  // l4lb front IPv4, network order
  __type(value, __u8);
} netdp_lb_srcs SEC(".maps");

struct {
  __uint(type, BPF_MAP_TYPE_HASH);
  __uint(max_entries, 1024);
  __type(key, struct port_key);
  __type(value, struct pod_dest);
} netdp_ports_in SEC(".maps");

struct {
  __uint(type, BPF_MAP_TYPE_HASH);
  __uint(max_entries, 1024);
  __type(key, struct out_key);
  __type(value, __u8);
} netdp_ports_out SEC(".maps");

struct {
  __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
  __uint(max_entries, C_MAX);
  __type(key, __u32);
  __type(value, __u64);
} netdp_counters SEC(".maps");

static __always_inline void count(__u32 c) {
  __u64 *v = bpf_map_lookup_elem(&netdp_counters, &c);
  if (v) *v += 1;
}

static __always_inline struct config *get_config(void) {
  __u32 zero = 0;
  return bpf_map_lookup_elem(&netdp_config, &zero);
}

// Offsets within an Ethernet + option-less IPv4 + TCP frame.
#define ETH_OFF_DST 0
#define ETH_OFF_SRC ETH_ALEN
#define IP_OFF (sizeof(struct ethhdr))
#define IP_OFF_CSUM (IP_OFF + __builtin_offsetof(struct iphdr, check))
#define IP_OFF_SADDR (IP_OFF + __builtin_offsetof(struct iphdr, saddr))
#define IP_OFF_DADDR (IP_OFF + __builtin_offsetof(struct iphdr, daddr))
#define TCP_OFF (IP_OFF + sizeof(struct iphdr))
#define TCP_OFF_CSUM (TCP_OFF + __builtin_offsetof(struct tcphdr, check))

static __always_inline int plain_ipv4(struct iphdr *ip) {
  return ip->version == 4 && ip->ihl == 5 && (ip->frag_off & bpf_htons(IP_MF | IP_OFFSET)) == 0;
}

SEC("tc")
int netdp_ingress(struct __sk_buff *skb) {
  void *data = (void *)(long)skb->data;
  void *data_end = (void *)(long)skb->data_end;

  struct ethhdr *eth = data;
  struct iphdr *outer = (void *)(eth + 1);
  struct iphdr *inner = (void *)(outer + 1);
  struct tcphdr *tcp = (void *)(inner + 1);
  if ((void *)(tcp + 1) > data_end) return TC_ACT_OK;
  if (eth->h_proto != bpf_htons(ETH_P_IP)) return TC_ACT_OK;
  if (!plain_ipv4(outer) || outer->protocol != IPPROTO_IPIP) return TC_ACT_OK;

  if (!bpf_map_lookup_elem(&netdp_lb_srcs, &outer->saddr)) {
    count(C_IN_NOT_LB_SRC);
    return TC_ACT_OK;
  }
  struct config *cfg = get_config();
  if (!cfg || cfg->vip == 0) return TC_ACT_OK;
  if (!plain_ipv4(inner) || inner->protocol != IPPROTO_TCP || inner->daddr != cfg->vip) return TC_ACT_OK;

  struct port_key key = {.proto = IPPROTO_TCP, .port = tcp->dest};
  struct pod_dest *dst = bpf_map_lookup_elem(&netdp_ports_in, &key);
  if (!dst) {
    count(C_IN_NO_PORT);
    return TC_ACT_OK;
  }
  __u32 vip = cfg->vip;
  __u32 pod_ip = dst->pod_ip;
  __u32 ifindex = dst->ifindex;
  __u8 macs[2 * ETH_ALEN];
  __builtin_memcpy(macs, dst->pod_mac, ETH_ALEN);
  __builtin_memcpy(macs + ETH_ALEN, dst->host_mac, ETH_ALEN);

  // Strip the outer IPv4 header (the Ethernet header stays in front).
  if (bpf_skb_adjust_room(skb, -(int)sizeof(struct iphdr), BPF_ADJ_ROOM_MAC, BPF_F_ADJ_ROOM_DECAP_L3_IPV4)) goto err;
  // DNAT VIP -> pod IP, fixing the IPv4 and the TCP (pseudo-header) checksums.
  if (bpf_l3_csum_replace(skb, IP_OFF_CSUM, vip, pod_ip, sizeof(pod_ip))) goto err;
  if (bpf_l4_csum_replace(skb, TCP_OFF_CSUM, vip, pod_ip, BPF_F_PSEUDO_HDR | sizeof(pod_ip))) goto err;
  if (bpf_skb_store_bytes(skb, IP_OFF_DADDR, &pod_ip, sizeof(pod_ip), 0)) goto err;
  if (bpf_skb_store_bytes(skb, ETH_OFF_DST, macs, sizeof(macs), 0)) goto err;

  count(C_IN_STEERED);
  return bpf_redirect_peer(ifindex, 0);
err:
  count(C_IN_ERR);
  return TC_ACT_SHOT;
}

SEC("tc")
int netdp_egress(struct __sk_buff *skb) {
  void *data = (void *)(long)skb->data;
  void *data_end = (void *)(long)skb->data_end;

  struct ethhdr *eth = data;
  struct iphdr *ip = (void *)(eth + 1);
  struct tcphdr *tcp = (void *)(ip + 1);
  if ((void *)(tcp + 1) > data_end) return TC_ACT_OK;
  if (eth->h_proto != bpf_htons(ETH_P_IP)) return TC_ACT_OK;
  if (!plain_ipv4(ip) || ip->protocol != IPPROTO_TCP) return TC_ACT_OK;

  struct out_key key = {.pod_ip = ip->saddr, .proto = IPPROTO_TCP, .port = tcp->source};
  if (!bpf_map_lookup_elem(&netdp_ports_out, &key)) return TC_ACT_OK;
  struct config *cfg = get_config();
  if (!cfg || cfg->vip == 0 || cfg->nic_ifindex == 0) return TC_ACT_OK;
  __u32 vip = cfg->vip;
  __u32 pod_ip = ip->saddr;
  __u32 nic = cfg->nic_ifindex;

  // SNAT pod IP -> VIP so the client sees the reply come from the address it
  // dialled (DSR: the reply does not go back through l4lb).
  if (bpf_l3_csum_replace(skb, IP_OFF_CSUM, pod_ip, vip, sizeof(vip))) goto err;
  if (bpf_l4_csum_replace(skb, TCP_OFF_CSUM, pod_ip, vip, BPF_F_PSEUDO_HDR | sizeof(vip))) goto err;
  if (bpf_skb_store_bytes(skb, IP_OFF_SADDR, &vip, sizeof(vip), 0)) goto err;

  count(C_OUT_SNAT);
  // FIB lookup + neighbor resolution fill in the L2 header for the NIC.
  return bpf_redirect_neigh(nic, NULL, 0, 0);
err:
  count(C_OUT_ERR);
  return TC_ACT_SHOT;
}

char _license[] SEC("license") = "GPL";
