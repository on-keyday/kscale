# kscale

kscale is an experimental, self-hosted edge platform: a control plane and a set
of data-plane agents that together run a small Point-of-Presence — L4 load
balancing, edge HTTP caching with WebAssembly request handlers, authoritative
DNS, and router management — on your own machines.

It is a research project. Interfaces, wire formats, and on-disk formats change
without notice.

## Components

| Binary | Path | Role |
|---|---|---|
| `controlplane` | `cmd/controlplane` | CA, resource store, policy-based ABAC, reconciliation, RPC hub |
| `cli` | `cmd/cli` | Operator CLI (`cli --resource R --op A ...`) |
| `katui` | `cmd/katui` | Terminal UI for monitoring and operations |
| `popcacheagent` | `cmd/popcacheagent` | Edge HTTP cache; runs user WASM modules from `edge_app/` |
| `dpagent` | `cmd/dpagent` | L4 load balancer (XDP/eBPF data plane) |
| `dnsagent` | `cmd/dnsagent` | Authoritative DNS with upstream (e.g. Cloudflare) sync |
| `routeragent` | `cmd/routeragent` | Network router configuration |
| `workloadagent` | `cmd/workloadagent` | Container workloads via CRI (containerd) |
| `nodewatch` | `cmd/nodewatch` | Local-LLM (Ollama) node monitoring agent |
| `metricsgw` | `cmd/metricsgw` | Prometheus metrics gateway |

## Design

Two things make this tree look unusual at first glance; both are deliberate.

**Most of the stack is hand-rolled.** Building the infrastructure itself is the
point of the project, so layers that would normally be off-the-shelf are
reimplemented here: agents talk to the control plane over an encrypted,
reliable-UDP multiplexed transport ([objtrsf](https://github.com/on-keyday/objtrsf))
instead of QUIC, carrying an RPC layer (`rpc/`) with stubs emitted by an
in-repo protobuf runtime and protoc plugin (`protobuf/`) instead of grpc-go —
the same stack also speaks native gRPC framing where needed (e.g. CRI to
containerd, `cri/`).

**The backbone is declarative code generation.** Resources, their operations,
and access policies are declared once in `access/defs/resource.yaml` +
`access/defs/policy/`, and `./generate.sh` derives the rest: proto definitions,
the CRUD/watch/apply RPC surface, per-action ABAC authorization gates, the CLI
dispatch, and reconcile controllers that push desired state to the data planes.
Hand-written code is mostly business logic and desired-state stores; if you are
reading generated files (`*_gen.go`, `*.pbg.go`), the interesting part is
usually the generator and the yaml, not the output.

## Build

```bash
./generate.sh   # code generation (resource.yaml → dispatch/proto/policy code)
go build ./...
```

Edge WASM modules are built separately:

```bash
cd edge_app && ./build.sh   # requires Rust with the wasm32-wasip1 target
```

The L4 data plane requires Linux with eBPF/XDP support; the eBPF objects are
built via `go generate` inside `generate.sh`.

## Tests

```bash
go test ./...
```

End-to-end datapath tests using docker compose live in `e2e/compose/`.

## Repository conventions

- `docs/` — documentation.
- Generated files (`*_gen.go`, `*.pbg.go`, `consts/`, parts of `access/` and
  `client/`) are produced by `./generate.sh` — do not edit them by hand.

## License

Apache-2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
