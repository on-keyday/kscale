// Command kscale-cni is the kscale CNI plugin: containerd's CRI plugin execs it
// (as root, once per sandbox) to wire a pod netns — a veth pair, a node-local
// pod IP, and a gateway route/neighbor so every pod packet leaves through the
// host-side veth. It does nothing else: which VIP ports reach the pod is the
// workload eBPF datapath's job (in workloadagent), and the two agree on names
// through workload/podnet's rules instead of talking to each other.
//
// Implements CNI spec 1.0.0 (ADD / DEL / CHECK / VERSION) by hand, without
// libcni. Its only inputs are the network config on stdin (from the root-owned
// conf_dir) and the CNI_* environment containerd sets.
//
// It is also the "loopback" plugin when installed (copied or linked) under that
// name: containerd's CRI plugin runs a loopback network for every pod sandbox in
// addition to the configured one, and that just brings lo up. Shipping it here
// keeps the node free of the reference plugins.
// See notes/ai/2026_09_25_workload_pod_netns_ebpf_design.md.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/on-keyday/kscale/workload/podnet"
)

// netConf is this plugin's entry in the conflist (libcni injects name and
// cniVersion into it).
type netConf struct {
	CNIVersion string `json:"cniVersion"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Subnet     string `json:"subnet"`  // pod IPs, e.g. 10.200.0.0/24
	DataDir    string `json:"dataDir"` // IPAM state; default /var/lib/kscale-cni
	MTU        int    `json:"mtu"`     // default 1500
}

var supportedVersions = []string{"1.0.0"}

// cniError is the spec's error result, printed on stdout with a non-zero exit.
type cniError struct {
	CNIVersion string `json:"cniVersion"`
	Code       int    `json:"code"`
	Msg        string `json:"msg"`
	Details    string `json:"details,omitempty"`
}

// Well-known error codes (CNI spec "Error").
const (
	errIncompatibleVersion = 1
	errUnsupportedField    = 2
	errUnknownContainer    = 3
	errInvalidEnv          = 4
	errIO                  = 5
	errDecode              = 6
	errInvalidConfig       = 7
	errTryLater            = 11
	errGeneric             = 100 // plugin-specific codes start at 100
)

type failure struct {
	code int
	err  error
}

func (f *failure) Error() string { return f.err.Error() }

func fail(code int, format string, args ...any) error {
	return &failure{code: code, err: fmt.Errorf(format, args...)}
}

func main() {
	// netlink handles and namespace fds must stay on one OS thread.
	runtime.LockOSThread()
	version := "1.0.0"
	runFn := run
	if filepath.Base(os.Args[0]) == "loopback" {
		runFn = runLoopback
	}
	if err := runFn(os.Stdin, os.Stdout, &version); err != nil {
		code := errGeneric
		if f, ok := err.(*failure); ok {
			code = f.code
		}
		_ = json.NewEncoder(os.Stdout).Encode(cniError{CNIVersion: version, Code: code, Msg: err.Error()})
		os.Exit(1)
	}
}

func run(stdin io.Reader, stdout io.Writer, version *string) error {
	cmd := os.Getenv("CNI_COMMAND")
	if cmd == "VERSION" {
		return json.NewEncoder(stdout).Encode(map[string]any{
			"cniVersion": supportedVersions[len(supportedVersions)-1], "supportedVersions": supportedVersions,
		})
	}

	raw, err := io.ReadAll(stdin)
	if err != nil {
		return fail(errIO, "read config: %v", err)
	}
	var conf netConf
	if err := json.Unmarshal(raw, &conf); err != nil {
		return fail(errDecode, "decode config: %v", err)
	}
	if conf.CNIVersion != "" {
		*version = conf.CNIVersion
	}
	if !supported(conf.CNIVersion) {
		return fail(errIncompatibleVersion, "cniVersion %q not supported (have %v)", conf.CNIVersion, supportedVersions)
	}
	subnet, err := netip.ParsePrefix(conf.Subnet)
	if err != nil {
		return fail(errInvalidConfig, "subnet %q: %v", conf.Subnet, err)
	}
	if conf.DataDir == "" {
		conf.DataDir = "/var/lib/kscale-cni"
	}
	if conf.MTU == 0 {
		conf.MTU = 1500
	}
	if conf.Name == "" || filepath.Base(conf.Name) != conf.Name {
		return fail(errInvalidConfig, "network name %q must be a plain name", conf.Name)
	}
	ipam, err := podnet.NewIPAM(filepath.Join(conf.DataDir, conf.Name), subnet)
	if err != nil {
		return fail(errInvalidConfig, "%v", err)
	}

	id := os.Getenv("CNI_CONTAINERID")
	if id == "" {
		return fail(errInvalidEnv, "CNI_CONTAINERID is empty")
	}
	ifName := os.Getenv("CNI_IFNAME")
	netnsPath := os.Getenv("CNI_NETNS")

	switch cmd {
	case "ADD":
		if ifName == "" || netnsPath == "" {
			return fail(errInvalidEnv, "ADD needs CNI_IFNAME and CNI_NETNS")
		}
		return add(stdout, conf, ipam, id, ifName, netnsPath)
	case "DEL":
		// Idempotent, and must succeed even when the netns is already gone.
		if err := podnet.Teardown(id); err != nil {
			return fail(errGeneric, "teardown: %v", err)
		}
		if err := ipam.Release(id); err != nil {
			return fail(errIO, "release ip: %v", err)
		}
		return nil
	case "CHECK":
		if _, ok, err := ipam.Lookup(id); err != nil {
			return fail(errIO, "lookup ip: %v", err)
		} else if !ok {
			return fail(errUnknownContainer, "no IP allocated for %s", id)
		}
		if err := podnet.Check(id); err != nil {
			return fail(errGeneric, "%v", err)
		}
		return nil
	default:
		return fail(errInvalidEnv, "unknown CNI_COMMAND %q", cmd)
	}
}

func add(stdout io.Writer, conf netConf, ipam *podnet.IPAM, id, ifName, netnsPath string) error {
	ip, err := ipam.Allocate(id)
	if err != nil {
		return fail(errTryLater, "allocate ip: %v", err)
	}
	w, err := podnet.Setup(id, netnsPath, ifName, ip, conf.MTU)
	if err != nil {
		_ = ipam.Release(id)
		return fail(errGeneric, "wire pod: %v", err)
	}
	podIdx := 1
	return json.NewEncoder(stdout).Encode(map[string]any{
		"cniVersion": conf.CNIVersion,
		"interfaces": []map[string]any{
			{"name": w.HostVeth, "mac": w.HostMAC.String()},
			{"name": w.PodIfName, "mac": w.PodMAC.String(), "sandbox": w.NetnsPath},
		},
		"ips": []map[string]any{
			{"address": netip.PrefixFrom(w.PodIP, 32).String(), "gateway": w.GatewayAddr.String(), "interface": podIdx},
		},
		"routes": []map[string]any{
			{"dst": "0.0.0.0/0", "gw": w.GatewayAddr.String()},
		},
		"dns": map[string]any{},
	})
}

// loopbackVersions: containerd's built-in loopback network config says 0.3.1.
var loopbackVersions = []string{"0.3.0", "0.3.1", "0.4.0", "1.0.0"}

func runLoopback(stdin io.Reader, stdout io.Writer, version *string) error {
	cmd := os.Getenv("CNI_COMMAND")
	if cmd == "VERSION" {
		return json.NewEncoder(stdout).Encode(map[string]any{
			"cniVersion": loopbackVersions[len(loopbackVersions)-1], "supportedVersions": loopbackVersions,
		})
	}
	var conf netConf
	if err := json.NewDecoder(stdin).Decode(&conf); err != nil {
		return fail(errDecode, "decode config: %v", err)
	}
	if conf.CNIVersion != "" {
		*version = conf.CNIVersion
	}
	if !slices.Contains(loopbackVersions, conf.CNIVersion) {
		return fail(errIncompatibleVersion, "cniVersion %q not supported (have %v)", conf.CNIVersion, loopbackVersions)
	}
	netnsPath := os.Getenv("CNI_NETNS")
	switch cmd {
	case "ADD":
		if netnsPath == "" {
			return fail(errInvalidEnv, "ADD needs CNI_NETNS")
		}
		if err := podnet.LoopbackUp(netnsPath); err != nil {
			return fail(errGeneric, "lo up: %v", err)
		}
		ipv4 := map[string]any{"address": "127.0.0.1/8", "interface": 0}
		if strings.HasPrefix(conf.CNIVersion, "0.") {
			ipv4["version"] = "4" // pre-1.0 results carry the IP family
		}
		return json.NewEncoder(stdout).Encode(map[string]any{
			"cniVersion": conf.CNIVersion,
			"interfaces": []map[string]any{{"name": "lo", "sandbox": netnsPath}},
			"ips":        []map[string]any{ipv4},
			"dns":        map[string]any{},
		})
	case "DEL", "CHECK":
		return nil // lo goes away with the netns
	default:
		return fail(errInvalidEnv, "unknown CNI_COMMAND %q", cmd)
	}
}

func supported(v string) bool {
	for _, s := range supportedVersions {
		if s == v {
			return true
		}
	}
	return false
}
