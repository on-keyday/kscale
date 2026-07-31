package probe

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ping/traceroute need ICMP (raw sockets = privilege) that pure Go can't get
// portably unprivileged, so these shell out to the system binaries. Safety: args are
// passed as an explicit slice (never a shell string, so no metacharacter injection),
// the target is validated to a hostname/IP charset, counts/hops are bounded, and the
// whole command is context-timeout-bound.

const shellProbeTimeout = 30 * time.Second

// hostRe restricts a probe target to DNS-name / IP characters — no spaces, no shell
// metacharacters, no leading dash (which would be read as a flag).
var hostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.:-]{0,253}[A-Za-z0-9])?$`)

func validHost(h string) (string, error) {
	h = strings.TrimSpace(h)
	if !hostRe.MatchString(h) {
		return "", fmt.Errorf("invalid host %q (letters/digits/dot/colon/hyphen only, no leading hyphen)", h)
	}
	return h, nil
}

func init() {
	register(kind{
		spec: Spec{
			Name:        "probe_ping",
			Description: "ICMP ping a host from the external vantage. Reports round-trip min/avg/max and packet loss. count defaults to 4 (max 10).",
			Params: obj(map[string]any{
				"host":  str("hostname or IP"),
				"count": inti("echo requests, 1–10 (default 4)"),
			}, "host"),
		},
		run: probePing,
	})
	register(kind{
		spec: Spec{
			Name:        "probe_traceroute",
			Description: "Trace the network path to a host from the external vantage. max_hops defaults to 20 (max 30). Use to see where connectivity to the public endpoint breaks.",
			Params: obj(map[string]any{
				"host":     str("hostname or IP"),
				"max_hops": inti("max hops, 1–30 (default 20)"),
			}, "host"),
		},
		run: probeTraceroute,
	})
}

// runShell executes bin with args (no shell), bounded by shellProbeTimeout, and
// returns combined output. A non-zero exit is not itself an error — the output (e.g.
// "100% packet loss") is the finding the LLM should read.
func runShell(ctx context.Context, bin string, args ...string) (string, error) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("%s not available on this host: %w", bin, err)
	}
	ctx, cancel := context.WithTimeout(ctx, shellProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	text := strings.TrimRight(string(out), "\n")
	if ctx.Err() == context.DeadlineExceeded {
		return text + "\n  (timed out)", nil
	}
	if err != nil && text == "" {
		return "", fmt.Errorf("%s: %w", bin, err)
	}
	return text, nil
}

func probePing(ctx context.Context, args map[string]string) (string, error) {
	host, err := validHost(args["host"])
	if err != nil {
		return "", err
	}
	count := argInt(args, "count", 4)
	if count < 1 || count > 10 {
		count = 4
	}
	// -c count, -w overall deadline (seconds) as a backstop alongside the ctx timeout.
	out, err := runShell(ctx, "ping", "-c", strconv.Itoa(count), "-w", "20", host)
	if err != nil {
		return "", err
	}
	return "ping " + host + "\n" + out, nil
}

func probeTraceroute(ctx context.Context, args map[string]string) (string, error) {
	host, err := validHost(args["host"])
	if err != nil {
		return "", err
	}
	hops := argInt(args, "max_hops", 20)
	if hops < 1 || hops > 30 {
		hops = 20
	}
	// Prefer traceroute; fall back to tracepath (often present without setuid).
	if _, err := exec.LookPath("traceroute"); err == nil {
		out, err := runShell(ctx, "traceroute", "-m", strconv.Itoa(hops), "-w", "2", host)
		if err != nil {
			return "", err
		}
		return "traceroute " + host + "\n" + out, nil
	}
	out, err := runShell(ctx, "tracepath", "-m", strconv.Itoa(hops), host)
	if err != nil {
		return "", err
	}
	return "tracepath " + host + "\n" + out, nil
}
