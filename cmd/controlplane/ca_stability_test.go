package main

import (
	"bytes"
	"testing"
)

// TestSetupCA_StableAcrossRestarts guards the root-CA persistence: setupCA on the same
// data dir must reuse the same CA root key/cert across "restarts". Regenerating it
// every startup re-signs a new "kscale Root CA", invalidating every previously issued
// cert (the dataplane agents then fail the handshake with "x509: Ed25519 verification
// failure ... kscale Root CA" forever).
func TestSetupCA_StableAcrossRestarts(t *testing.T) {
	dir := t.TempDir()

	ca1, err := setupCA(dir)
	if err != nil {
		t.Fatalf("first setupCA: %v", err)
	}
	cert1 := ca1.CACertificate()

	// Simulate a process restart against the same persisted data dir.
	ca2, err := setupCA(dir)
	if err != nil {
		t.Fatalf("second setupCA (restart): %v", err)
	}
	cert2 := ca2.CACertificate()

	if !bytes.Equal(cert1, cert2) {
		t.Fatalf("CA root cert changed across restart — the root key was regenerated; certs issued under the old key are now invalid (cert1=%dB cert2=%dB)", len(cert1), len(cert2))
	}
}
