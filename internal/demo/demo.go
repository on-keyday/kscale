// Package demo holds the small bits of demo identity + shared config that both
// the control plane (cmd/controlplane) and the admin client (cmd/cli) need — they
// are separate main packages, so this is their common, importable home.
package demo

import "time"

// Domain is the kscale CA domain; the authority tree is rooted at it (so
// $env.authority.* and the certificate CommonNames carry it). Default "kscale.local";
// each binary overrides it from its --domain flag at startup. It MUST be identical
// across the control plane, clients, and dataplane agents (it is the shared CA root).
var Domain = "kscale.local"

const (
	// PingInterval is the keepalive interval for peer connections (server + client).
	PingInterval = 15 * time.Second
	// WebSocketPath is the mount/dial path for the objtrsf WebSocket transport leg,
	// shared by the control plane (server) and clients (e.g. the CLI over an SSH
	// tunnel when the UDP port is not reachable).
	WebSocketPath = "/objtrsf"
)

// ManagerRoles are the admin-tier roles the control plane seeds an
// admin.ca.manager.<role> leaf for, so they enroll out-of-box with the default
// (role-derived) CommonName. (Dataplane roles get system.dp.* leaves instead.)
//
// Authz keys on the authority prefix (admin.ca.manager.) plus the role — NOT the leaf
// label — so any CommonName under manager works; the role is what the CA-manager policy
// distinguishes (admin allowed, viewer denied).
var ManagerRoles = []string{"admin", "viewer", "monitor"}

// ClientCN is the DEFAULT cert CommonName for a role: <role>.manager.ca.admin.<domain>
// (its FullName, reverse-parsed + domain-stripped, is admin.ca.manager.<role>).
// Operators override it wholesale with --common-name (ksdk's flag) for a custom
// identity; that custom leaf must exist first — the CP seeds only the role-named
// defaults, so a custom CN needs an `authority apply` of its leaf (the dataplane flow).
func ClientCN(role string) string {
	return role + ".manager.ca.admin." + Domain
}
