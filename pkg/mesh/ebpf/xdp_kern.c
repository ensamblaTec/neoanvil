#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/if_vlan.h>
#include <linux/ip.h>
#include <linux/ipv6.h>
#include <linux/udp.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

#ifndef ETH_P_8021Q
#define ETH_P_8021Q 0x8100
#endif
#ifndef ETH_P_8021AD
#define ETH_P_8021AD 0x88A8
#endif

struct {
    __uint(type, BPF_MAP_TYPE_XSKMAP);
    __uint(max_entries, 64);
    __type(key, int);
    __type(value, int);
} xsks_map SEC(".maps");

struct config_struct {
    __u32 swarm_port;
};

struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct config_struct);
} config_map SEC(".maps");

#define MAX_VLAN_LAYERS 2

SEC("xdp")
int xdp_swarm_router(struct xdp_md *ctx) {
    void *data_end = (void *)(long)ctx->data_end;
    void *data = (void *)(long)ctx->data;

    struct ethhdr *eth = data;
    if ((void *)(eth + 1) > data_end) return XDP_PASS;

    __u16 h_proto = eth->h_proto;
    int payload_offset = sizeof(*eth);

    #pragma unroll
    for (int i = 0; i < MAX_VLAN_LAYERS; i++) {
        if (h_proto == bpf_htons(ETH_P_8021Q) || h_proto == bpf_htons(ETH_P_8021AD)) {
            // Se utiliza una estructura equivalente en lugar de la inestable vlan_hdr de C pura
            struct vlan_hdr_raw {
                __be16 h_vlan_TCI;
                __be16 h_vlan_encapsulated_proto;
            } *vhdr = (struct vlan_hdr_raw *)(data + payload_offset);
            
            if ((void *)(vhdr + 1) > data_end) return XDP_PASS;
            h_proto = vhdr->h_vlan_encapsulated_proto;
            payload_offset += sizeof(struct vlan_hdr_raw);
        } else {
            break;
        }
    }

    __u8 protocol = 0;
    
    if (h_proto == bpf_htons(ETH_P_IP)) {
        struct iphdr *ip = data + payload_offset;
        if ((void *)(ip + 1) > data_end) return XDP_PASS;
        protocol = ip->protocol;
        payload_offset += (ip->ihl * 4);
    } else if (h_proto == bpf_htons(ETH_P_IPV6)) {
        struct ipv6hdr *ipv6 = data + payload_offset;
        if ((void *)(ipv6 + 1) > data_end) return XDP_PASS;
        protocol = ipv6->nexthdr;
        payload_offset += sizeof(*ipv6);
    } else {
        return XDP_PASS;
    }

    if (protocol != IPPROTO_UDP) return XDP_PASS;

    struct udphdr *udp = data + payload_offset;
    if ((void *)(udp + 1) > data_end) return XDP_PASS;

    __u32 zero_key = 0;
    struct config_struct *cfg = bpf_map_lookup_elem(&config_map, &zero_key);
    if (!cfg) return XDP_PASS; // Fail-open: si el mapa falla, dejar al OS pasar

    if (udp->dest == bpf_htons(cfg->swarm_port)) {
        return bpf_redirect_map(&xsks_map, ctx->rx_queue_index, XDP_PASS);
    }
    
    return XDP_PASS;
}
char _license[] SEC("license") = "GPL";

// BPF Map para Reflejo Inmunológico Wasm -> Kernel
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 100000); // Max Tolerancia HNSW
    __type(key, __u32);          // IPv4 Atacante
    __type(value, __u8);         // Flag Booleano
} attacker_blacklist SEC(".maps");

// Uso en pipeline RX (SRE-Veto Automático)
// __u8 *banned = bpf_map_lookup_elem(&attacker_blacklist, &ip_header->saddr);
// if (banned) return XDP_DROP;
