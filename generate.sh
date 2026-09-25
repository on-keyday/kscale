#!/usr/bin/env bash
# Regenerate everything that derives from access/defs/resource.yaml + policy/,
# making resource.yaml the single source of truth (kscale charter). Pipeline:
#
#   1. gen_access  : resource.yaml + policy/ -> predefined/root.go, access.proto,
#                    admin_rpc_services.proto, registry.go (the core access layer)
#   2. protoc      : *.proto -> *.pbg.go via the bespoke protoc plugin
#   3. gen         : resource.yaml -> service/*_gated_gen.go + client/*_dispatch_gen.go
#   4. gofmt       : normalize the generated Go
#
# ksdk_options.pb.go is intentionally NOT regenerated (it derives from
# ksdk_options.proto, not the resource declarations); we build the plugin against
# the committed one to avoid protoc-gen-go version churn.
set -euo pipefail

cd "$(dirname "$0")"

RES=./access/defs/resource.yaml
POL=./access/defs/policy/

echo "[1/4] gen_access: core access layer from resource.yaml"
go run ./cmd/gen_access resource       "$RES" "$POL" > ./access/predefined/root.go
go run ./cmd/gen_access proto          "$RES" "$POL" > ./protobuf/proto/access/access.proto
go run ./cmd/gen_access proto_services "$RES" "$POL" > ./protobuf/proto/admin_rpc_services.proto
go run ./cmd/gen_access proto_registry "$RES" "$POL" > ./protobuf/proto/access/registry.go

echo "[2/4] protoc: *.proto -> *.pbg.go"
(
  cd protobuf
  go build -o ./plugin/protoc-gen-ksdk ./plugin/main.go
  protoc --plugin=./plugin/protoc-gen-ksdk \
         --ksdk_out=. \
         --ksdk_opt=paths=source_relative \
         proto/*.proto proto/**/*.proto
  rm ./plugin/protoc-gen-ksdk
)

echo "[metrics] internal_value.py + metrics.py: consts + dataplane stat structs + proto converters"
# Salvaged metrics/stat codegen (from ksdk). internal_value.py emits consts/* and
# the dataplane metric leaves (popcache/popmetrics, dns/dnsmetrics) from the JSON
# under access/defs/internal/. metrics.py reads access/defs/internal/stats/ and
# emits the stat package (stat/stat_metrics.go, stat/stat_proto.go,
# stat/prometheus_metrics.go, stat/stat_metrics_test.go) plus
# protobuf/proto/stat/typed_stats.proto. metrics.py self-runs go fmt + goimports.
PY=python3
[ -f ./.venv/bin/activate ] && source ./.venv/bin/activate
INTERNAL_PATH="access/defs/internal"

$PY script/internal_value.py \
   "$INTERNAL_PATH/config_key.json" \
   "$INTERNAL_PATH/app_name.json" \
   "$INTERNAL_PATH/stream_magic.json" \
   "$INTERNAL_PATH/magic_numbers.json" \
   "$INTERNAL_PATH/app_status.json" \
   "$INTERNAL_PATH/generic_control.json" \
   "popcache/popmetrics/metrics.json" \
   "dns/dnsmetrics/metrics.json" \
   "workload/netdp/netdpmetrics/metrics.json"

$PY script/metrics.py \
   "$INTERNAL_PATH/stats"

echo "[3/4] gen: gate adapters + client dispatch"
# Scope is declarative: every `kind: resource` rpc_service resource is generated.
go run ./cmd/gen "$RES"

echo "[4/4] gofmt generated Go"
gofmt -w ./access/predefined/root.go ./protobuf/proto/access/registry.go

echo "done."
