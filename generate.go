// Package kscale exists only to host the repository's go:generate directive.
// `go generate ./...` runs ./generate.sh, which regenerates everything derived
// from access/defs/resource.yaml + policy/ — the core access layer (predefined,
// access.proto + *.pbg.go, registry) plus the gate adapters and client dispatch
// — making resource.yaml the single source of truth (kscale charter).
package kscale

//go:generate ./generate.sh
