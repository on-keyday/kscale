// Command cli is the kscale admin client. It enrolls (kubelet-style) as a demo
// role, mTLS-handshakes with the control plane (cmd/controlplane), and dispatches
// a resource action — admin is allowed, viewer is denied by policy:
//
//	cli [--addr host:port] [--data DIR] [--role admin|viewer]
//	    --resource R --op ACTION [--<arg> value ...]
//
// The selected resource/op determine the available typed --<arg> flags (e.g.
// `cli --resource popcache-config --op apply --name default --origin http://x`);
// `cli --resource R --op A -h` lists them. The resource router + per-action
// bind/call/render is generated from resource.yaml (the client package).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/on-keyday/kscale/ca"
	"github.com/on-keyday/kscale/client"
	"github.com/on-keyday/kscale/internal/demo"
	"github.com/on-keyday/kscale/logbuf"
	"github.com/on-keyday/objtrsf/objproto"
)

func main() {
	logger := logbuf.NewStderrLogger(os.Stderr, slog.LevelInfo)
	if err := run(context.Background(), logger, os.Args[1:]); err != nil {
		logger.Error("cli failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger, args []string) error {
	// Pre-scan resource/op so we can expose the selected action's args as typed
	// --<arg> flags (discoverable in -h) rather than a generic --arg name=value.
	resourceName := scanFlag(args, "resource", "authority")
	op := scanFlag(args, "op", "list")

	fs := flag.NewFlagSet("cli", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:9443", "control plane address: host:port (udp), or a connection id <transport>:<host>:<port>-<id> e.g. ws:127.0.0.1:9444-* to reach the CP over an SSH-forwarded WebSocket port")
	dataDir := fs.String("data", "/tmp/kscale-ca", "dir holding bootstrap tokens / saved certs")
	role := fs.String("role", "admin", "identity role: admin | viewer")
	applyFile := fs.String("apply", "", "apply a yaml config of declarative resources (kubectl-apply style); ignores --resource/--op")
	applySchema := fs.Bool("apply-schema", false, "print a yaml-apply config skeleton for the declarative resources, then exit (no connection)")
	dump := fs.Bool("dump", false, "dump the live desired state of every declarative resource as a yaml-apply config (re-appliable with --apply); sensitive fields are blanked")
	validateFile := fs.String("validate", "", "validate a yaml-apply config offline (no connection) and report problems, then exit")
	fs.String("resource", "authority", "resource command_name (see the list below)")
	fs.String("op", "list", "action command_name (see the list below)")
	// Identity flags come BEFORE the per-action arg flags so a collision is
	// detectable below. The enroll CN override is --enroll-cn (NOT --common-name:
	// many resources have a common_name ARG — node get, stats get, dp-file send,
	// expected-node apply — and registering both paniced with "flag redefined";
	// the plain kebab name always belongs to the resource arg).
	domainFlag := fs.String("ca-domain", demo.Domain, "CA domain — must match the control plane's --ca-domain")
	commonNameFlag := fs.String("enroll-cn", "", "cert CommonName to enroll as (default <role>.manager.ca.admin.<ca-domain>)")
	// One typed flag per arg the selected resource/op accepts. Flags are
	// registered under the kebab-case spelling (--module-id); the snake_case
	// resource.yaml spelling (--module_id) keeps working via normalizeFlagNames
	// below. The map recovers the snake_case args-map key the dispatch expects.
	argFlagSet := map[string]string{} // kebab flag name -> resource arg name
	if specs, ok := client.ActionArgs(resourceName, op); ok {
		for _, a := range specs {
			usage := client.ArgTypeHint(a.Type)
			if a.Default != "" {
				usage += "; default " + a.Default
			}
			flagName := strings.ReplaceAll(a.Name, "_", "-")
			if fs.Lookup(flagName) != nil {
				// A resource arg shadowing a global cli flag would panic the flag
				// package; keep the cli usable and route the arg through --arg.
				fmt.Fprintf(os.Stderr, "warning: arg --%s collides with a cli flag; pass it as --arg %s=<value>\n", flagName, a.Name)
				continue
			}
			fs.String(flagName, "", usage)
			argFlagSet[flagName] = a.Name
		}
	}
	var rawArgs multiFlag
	fs.Var(&rawArgs, "arg", "extra request arg as name=value (repeatable; the typed --<arg> flags are preferred)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: cli --resource <name> --op <action> [--<arg> value ...]\n\n")
		fs.PrintDefaults()
		fmt.Fprint(os.Stderr, "\n"+client.Usage())
	}
	_ = fs.Parse(normalizeFlagNames(args))
	demo.Domain = *domainFlag

	reqArgs, err := parseArgs(rawArgs)
	if err != nil {
		return err
	}
	// Collect the typed arg flags that were explicitly set (an unset flag must not
	// override its resource.yaml default, so only Visit'd flags are sent).
	fs.Visit(func(f *flag.Flag) {
		if argName, ok := argFlagSet[f.Name]; ok {
			reqArgs[argName] = f.Value.String()
		}
	})

	// --apply-schema is offline: emit the yaml-apply skeleton and exit.
	if *applySchema {
		fmt.Print(client.ApplyExample())
		return nil
	}
	// --validate is offline: check a yaml-apply config and report problems, exit.
	if *validateFile != "" {
		data, err := os.ReadFile(*validateFile)
		if err != nil {
			return err
		}
		problems := client.ValidateConfig(data)
		if len(problems) == 0 {
			fmt.Println("OK: config is valid")
			return nil
		}
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "INVALID:", p)
		}
		return fmt.Errorf("%d problem(s) found", len(problems))
	}

	// --addr is a connection ID ("<transport>:<host>:<port>-<id>", e.g.
	// "ws:127.0.0.1:9444-*") or a bare "host:port" (defaults to udp). ws lets the CLI
	// reach a CP over an SSH-forwarded TCP port when the UDP port isn't routable.
	cid, ep, err := client.Endpoint(logger, *addr)
	if err != nil {
		return err
	}

	boot, err := enroll(ctx, ep, cid, *dataDir, *role, *commonNameFlag)
	if err != nil {
		return err
	}
	p, src, err := client.Connect(ctx, ep, cid, *role, boot, demo.PingInterval, logger)
	if err != nil {
		return err
	}
	defer p.Connection().Close()

	// --dump: render the live desired state of every declarative resource as a
	// yaml-apply config (the inverse of --apply). Sensitive fields are blanked.
	if *dump {
		out, err := client.DumpDesired(ctx, src)
		if err != nil {
			return err
		}
		fmt.Print(out)
		return nil
	}

	// kubectl-apply style: apply a yaml config of declarative resources via the
	// generated ResourceSpecs (driven entirely by metadata, no per-resource code).
	if *applyFile != "" {
		data, err := os.ReadFile(*applyFile)
		if err != nil {
			return err
		}
		results, applyErr := client.ApplyConfig(ctx, src, data)
		for _, r := range results {
			if r.Err != nil {
				fmt.Printf("apply %s: FAILED: %v\n", r.Resource, r.Err)
			} else {
				fmt.Printf("apply %s: OK\n", r.Resource)
			}
		}
		return applyErr
	}

	// The resource router (generated from resource.yaml) builds the right typed
	// client and runs the per-action bind + call + render.
	out, err := client.DispatchResource(ctx, src, resourceName, op, reqArgs)
	if err != nil {
		// A policy denial is an expected outcome (e.g. viewer, or a disallowed
		// authority_name prefix).
		fmt.Printf("DENIED  (resource=%s op=%s role=%s): %v\n", resourceName, op, *role, err)
		return nil
	}
	if out == "" {
		fmt.Printf("ALLOWED (resource=%s op=%s role=%s)\n", resourceName, op, *role)
	} else {
		fmt.Printf("ALLOWED (resource=%s op=%s role=%s):\n%s\n", resourceName, op, *role, out)
	}
	return nil
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// scanFlag finds a flag's value in args before the real flagset is built — the
// selected --resource/--op decide which typed --<arg> flags to register, so they
// must be read first. Handles "--name v", "-name v", "--name=v", "-name=v".
func scanFlag(args []string, name, def string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if v, ok := strings.CutPrefix(a, "--"+name+"="); ok {
			return v
		}
		if v, ok := strings.CutPrefix(a, "-"+name+"="); ok {
			return v
		}
		if (a == "--"+name || a == "-"+name) && i+1 < len(args) {
			return args[i+1]
		}
	}
	return def
}

// parseArgs turns repeated name=value items into the args map the generated
// dispatch binds the request DTO from.
func parseArgs(items []string) (map[string]string, error) {
	args := map[string]string{}
	for _, it := range items {
		k, v, ok := strings.Cut(it, "=")
		if !ok {
			return nil, fmt.Errorf("bad --arg %q, want name=value", it)
		}
		// The dispatch layer keys the args map by the resource.yaml snake_case
		// name; accept the kebab spelling here too (values are never touched).
		args[strings.ReplaceAll(k, "-", "_")] = v
	}
	return args, nil
}

// normalizeFlagNames rewrites snake_case flag names to the kebab-case form the
// flagset registers (--module_id -> --module-id) so both spellings work. Only
// the name part of a flag token is rewritten — values ("--path=/a_b" or the
// following token) are never touched, and rewriting stops at the "--"
// terminator. No built-in flag contains an underscore, so this is unambiguous.
func normalizeFlagNames(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = a
		if a == "--" {
			copy(out[i+1:], args[i+1:])
			break
		}
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name, val, hasVal := strings.Cut(a, "=")
		if !strings.Contains(name, "_") {
			continue
		}
		name = strings.ReplaceAll(name, "_", "-")
		if hasVal {
			out[i] = name + "=" + val
		} else {
			out[i] = name
		}
	}
	return out
}

// enroll resolves the demo identity for role (its CommonName, bootstrap-token
// file, and cached-cert path) and delegates the transport plumbing to
// client.Enroll. The token is only read when no cached cert exists.
func enroll(ctx context.Context, ep objproto.Endpoint, cid objproto.ConnectionID, dataDir, role, commonName string) (*ca.BootstrapInfo, error) {
	savePath := filepath.Join(dataDir, "client."+role+".boot")
	var token []byte
	if _, err := os.Stat(savePath); os.IsNotExist(err) {
		token, err = os.ReadFile(filepath.Join(dataDir, "bootstrap."+role+".token"))
		if err != nil {
			return nil, fmt.Errorf("read %s bootstrap token: %w", role, err)
		}
	}
	cn := commonName
	if cn == "" {
		cn = demo.ClientCN(role)
	}
	return client.Enroll(ctx, ep, cid, cn, token, role, savePath)
}
