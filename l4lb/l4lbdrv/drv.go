package l4lbdrv

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/netip"
	"path/filepath"
	"syscall"

	"github.com/cilium/ebpf"
	"github.com/vishvananda/netlink"
	"go.uber.org/multierr"
)

type FixedConfig struct {
	BinPath        string
	XdpCapHookPath string
	CryptoBin      string
	EBPFPinDir     string // Path to pinning directory for crypto context map
}

func (f *FixedConfig) ToAbsolutePaths() error {
	var err error
	absWithCheck := func(path *string) {
		if err != nil || *path == "" {
			return
		}
		*path, err = filepath.Abs(*path)
		if err != nil {
			err = fmt.Errorf("Failed to get absolute path for %s: %w", *path, err)
		}
	}
	absWithCheck(&f.BinPath)
	absWithCheck(&f.XdpCapHookPath)
	absWithCheck(&f.CryptoBin)
	absWithCheck(&f.EBPFPinDir)
	return err
}

// LbConfigFlagIcmpEchoReply mirrors LB_CONFIG_FLAG_ICMP_ECHO_REPLY in lb.c: when
// set in LbConfig.Flags, the XDP program answers VIP-destined ICMPv4 echo
// requests (else it drops them). The bit value is hand-kept in lockstep with the
// C #define (the generated bindings assert struct layout, not flag values).
const LbConfigFlagIcmpEchoReply uint32 = 1 << 2

// configFlags derives the LbConfig.Flags bitfield from the dynamic config, so
// the mapping is unit-testable without a BPF host (Sync writes to a kernel map).
func configFlags(cfg *DynamicConfig) uint32 {
	var flags uint32
	if cfg.IcmpEcho {
		flags |= LbConfigFlagIcmpEchoReply
	}
	return flags
}

type DynamicConfig struct {
	InterfaceName string
	VIP           netip.Addr
	VIPv6         netip.Addr // IPv6 VIP, if applicable
	Dests         DestinationEntries
	SharedKey     []byte  // Shared key for QUIC connection ID generation
	MTU           uint16  // Maximum Transmission Unit
	RoutingRandom [4]byte // Random bytes for load balancing
	IcmpEcho      bool    // answer VIP-destined ICMPv4 echo requests in XDP
}

type L4LB struct {
	cfg       *DynamicConfig
	fixConfig *FixedConfig

	bindings     *Bindings
	linkAttacher *LinkAttacher
}

func (l4lb *L4LB) SyncSecret(newSecret []byte, logger *slog.Logger) error {
	l4lb.cfg.SharedKey = newSecret
	err := InitCrypto(logger, l4lb.fixConfig.CryptoBin, filepath.Join(l4lb.fixConfig.EBPFPinDir, "__crypto_ctx_map"), l4lb.fixConfig.EBPFPinDir, l4lb.cfg.SharedKey)
	if err != nil {
		return fmt.Errorf("Failed to sync crypto: %w", err)
	}
	return nil
}

func New(logger *slog.Logger, fixCfg *FixedConfig, cfg *DynamicConfig) (*L4LB, error) {
	if err := PrepSystemForXDP(logger); err != nil {
		return nil, fmt.Errorf("Failed to prep system for XDP: %w", err)
	}
	err := fixCfg.ToAbsolutePaths()
	if err != nil {
		return nil, fmt.Errorf("Failed to get absolute paths: %w", err)
	}
	err = InitCrypto(logger, fixCfg.CryptoBin, filepath.Join(fixCfg.EBPFPinDir, "__crypto_ctx_map"), fixCfg.EBPFPinDir, cfg.SharedKey)
	if err != nil {
		return nil, fmt.Errorf("Failed to init crypto: %w", err)
	}

	bindings, err := BindBalancer(logger, fixCfg.BinPath, fixCfg.XdpCapHookPath, fixCfg.EBPFPinDir)
	if err != nil {
		return nil, fmt.Errorf("Failed to bind balancer: %w", err)
	}

	lb := &L4LB{
		cfg:       cfg,
		fixConfig: fixCfg,
		bindings:  bindings,
	}

	var link netlink.Link
	if cfg.InterfaceName == "" {
		logger.Info("No interface name provided, skipping link attachment.")
	} else {
		l, err := netlink.LinkByName(cfg.InterfaceName)
		if err != nil {
			return nil, fmt.Errorf("Failed to find interface %q: %w", cfg.InterfaceName, err)
		}
		link = l
	}
	if link != nil {
		logger.Info("Attaching to link", slog.String("interface", cfg.InterfaceName))
		a, err := AttachToLink(logger, link, bindings.LBMain.FD())
		if err != nil {
			return nil, multierr.Combine(err, bindings.Close())
		}
		lb.linkAttacher = a
	}
	if err := lb.Sync(); err != nil {
		return nil, fmt.Errorf("Initial map sync failed: %w", err)
	}

	return lb, nil
}

var hostOrder = binary.LittleEndian

func IPToUint32(ip netip.Addr) (uint32, error) {
	if !ip.Is4() {
		return 0, errors.New("Given IP is not an IPv4 address.")
	}

	ip4 := ip.As4()
	return hostOrder.Uint32(ip4[:]), nil
}

func (lb *L4LB) UpdateVIP(newVIP netip.Addr, icmpEcho bool) error {
	if newVIP.Is4() {
		lb.cfg.VIP = newVIP
	} else if newVIP.Is6() {
		lb.cfg.VIPv6 = newVIP
	} else {
		return fmt.Errorf("invalid VIP address: %s", newVIP)
	}
	lb.cfg.IcmpEcho = icmpEcho

	if err := lb.Sync(); err != nil {
		slog.Error("Failed to sync load balancer configuration", slog.Any("error", err))
		return err
	}
	return nil
}

func (lb *L4LB) UpdateDestinations(newDestIPs DestinationEntries) error {
	if len(newDestIPs) == 0 {
		slog.Warn("No destinations provided, skipping update.")
		return nil
	}

	lb.cfg.Dests = newDestIPs

	if err := lb.Sync(); err != nil {
		slog.Error("Failed to sync load balancer configuration", slog.Any("error", err))
		return err
	}
	return nil
}

func (lb *L4LB) Sync() error {
	vip4, err := IPToUint32(lb.cfg.VIP)
	if err != nil {
		return fmt.Errorf("vip: %w", err)
	}

	err = lb.bindings.ConfigMap.Update(uint32(0), &LbConfig{
		VipAddress:      vip4,
		Vipv6Address:    lb.cfg.VIPv6.As16(),
		NumDests:        uint32(len(lb.cfg.Dests) - 1),
		Mtu:             lb.cfg.MTU,
		Flags:           configFlags(lb.cfg),
		ServerIdHashKey: hostOrder.Uint32(lb.cfg.RoutingRandom[:]),
	}, 0)
	if err != nil {
		return fmt.Errorf("Failed to update ConfigMap: %w", err)
	}

	keys := make([]uint32, len(lb.cfg.Dests))
	for i := range keys {
		keys[i] = uint32(i)
	}

	_, err = lb.bindings.DestinationArray.BatchUpdate(keys, lb.cfg.Dests, &ebpf.BatchOptions{})
	if err != nil {
		return fmt.Errorf("Failed to update DestinationArray: %w", err)
	}

	return nil
}

func (lb *L4LB) Close() error {
	return multierr.Combine(
		lb.bindings.Close(),
		func() error {
			if lb.linkAttacher != nil {
				return lb.linkAttacher.Close()
			}
			return nil
		}(),
	)
}

func (lb *L4LB) GetCounters() (*StatCounters, error) {
	return lb.bindings.ReadStatCountersAggregate()
}

func (lb *L4LB) GetSrcIPCounters() (*SrcIPCounts, error) {
	m, totalSize, err := lb.bindings.ReadSrcIPCounters()
	if err != nil {
		return nil, err
	}
	return &SrcIPCounts{
		Map: m,
		Sum: totalSize,
	}, nil
}

func (lb *L4LB) GetPacketSizeCounters(thresholds []uint32) (*PacketSizeDist, error) {
	histogram := make(map[float64]uint64)
	for _, t := range thresholds {
		histogram[float64(t)] = 0
	}
	m, totalCount, totalSize, err := lb.bindings.ReadPacketSizeCounters(histogram)
	if err != nil {
		return nil, err
	}
	return &PacketSizeDist{
		Map:       m,
		Count:     totalCount,
		Sum:       totalSize,
		Histogram: histogram,
	}, nil
}

func (lb *L4LB) GetISNLsbDistribution(thresholds []uint8) (*ISNLeastSignificantByteMap, error) {
	histogram := make(map[float64]uint64)
	for _, t := range thresholds {
		histogram[float64(t)] = 0
	}
	m, totalCount, totalSize, err := lb.bindings.ReadISNLsbDistribution(histogram)
	if err != nil {
		return nil, err
	}
	return &ISNLeastSignificantByteMap{
		Map:       m,
		Count:     totalCount,
		Sum:       totalSize,
		Histogram: histogram,
	}, nil
}

// `PrepSystemForXDP` configures RLIMIT_MEMLOCK to ensure enough room to
// allocate eBPF programs and maps on older Linux systems.
func PrepSystemForXDP(logger *slog.Logger) error {
	const RLIMIT_MEMLOCK = 8
	var rlim syscall.Rlimit
	if err := syscall.Getrlimit(RLIMIT_MEMLOCK, &rlim); err != nil {
		return fmt.Errorf("Failed to Getrlimit(RLIMIT_MEMLOCK): %v", err)
	}
	logger.Info("Getrlimit(RLIMIT_MEMLOCK)", "Cur", rlim.Cur, "Max", rlim.Max)

	rlim.Cur = math.MaxUint64
	rlim.Max = math.MaxUint64
	if err := syscall.Setrlimit(RLIMIT_MEMLOCK, &rlim); err != nil {
		return fmt.Errorf("Failed to Setrlimit(RLIMIT_MEMLOCK): %v", err)
	}
	logger.Info("Setrlimit(RLIMIT_MEMLOCK)", "Cur", rlim.Cur, "Max", rlim.Max)

	return nil
}
