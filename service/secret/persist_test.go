package secret

import (
	"encoding/json"
	"testing"
)

// TestPersistRoundTripRawBytes reproduces the 2026-07-07 reconcile failure: a
// generated secret is raw HKDF output (not UTF-8); the persisted form must bring it
// back byte-identical across a control-plane restart. The old string-typed form let
// json.Marshal substitute U+FFFD for invalid sequences, changing the key length.
func TestPersistRoundTripRawBytes(t *testing.T) {
	// Deliberately invalid UTF-8: lone continuation / overlong / 0xff bytes.
	raw := string([]byte{0x80, 0xff, 0xc0, 0x00, 0x01, 0xfe, 0x9f, 0xa2, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88})
	s := NewStore()
	s.set("quic-lb-key", entry{value: raw, length: 16})

	snap, err := s.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	restored := NewStore()
	if err := restored.Restore(snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got := restored.byName["quic-lb-key"]
	if got.value != raw {
		t.Fatalf("value corrupted across roundtrip: %d bytes, want %d", len(got.value), len(raw))
	}
	if got.length != 16 {
		t.Fatalf("length = %d, want 16", got.length)
	}
}

// TestRestoreLegacyStringForm: a pre-fix snapshot stored the value as a JSON string;
// Restore must load it (verbatim) rather than failing the whole desired-state file.
func TestRestoreLegacyStringForm(t *testing.T) {
	legacy := json.RawMessage(`{"quic-lb-key":{"value":"legacy-value","length":16}}`)
	s := NewStore()
	if err := s.Restore(legacy); err != nil {
		t.Fatalf("Restore legacy form: %v", err)
	}
	if got := s.byName["quic-lb-key"].value; got != "legacy-value" {
		t.Fatalf("legacy value = %q", got)
	}
}

// TestGenerateSecretLength: 0 defaults to the AES-128 length, anything except 16 is
// rejected (the dataplane consumers hard-require 16 bytes; the old 32-byte default
// was refused by every node).
func TestGenerateSecretLength(t *testing.T) {
	v, err := generateSecret(0)
	if err != nil || len(v) != quicLBSecretLen {
		t.Fatalf("generateSecret(0) = %d bytes, err %v; want %d", len(v), err, quicLBSecretLen)
	}
	v, err = generateSecret(16)
	if err != nil || len(v) != 16 {
		t.Fatalf("generateSecret(16) = %d bytes, err %v", len(v), err)
	}
	if _, err := generateSecret(32); err == nil {
		t.Fatal("generateSecret(32) should be rejected (AES-128 only)")
	}
}
