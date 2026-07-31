#!/bin/sh
# Stage the static kscale binaries + eBPF objects for the compose image.
# Built on the host (the build needs the sibling ../quic-go replace, awkward inside
# a docker build context), then COPYed into a thin alpine image by the Dockerfile.
set -e
cd "$(dirname "$0")/../.."
out=e2e/compose/stage
rm -rf "$out"
mkdir -p "$out/bin" "$out/objs"
for b in controlplane cli dpagent popcacheagent metricsgw katui; do
	CGO_ENABLED=0 go build -o "$out/bin/$b" "./cmd/$b"
done
cp l4lb/c/lb.o l4lb/c/init_crypto.o l4lb/c/dummy.o "$out/objs/"
echo "staged: $(ls "$out/bin") + objs"
