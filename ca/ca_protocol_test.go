package ca_test

import (
	"context"
	"crypto/ed25519"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/kscale/ca"
	"github.com/on-keyday/kscale/ca/storage"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/transport"
)

func commonSetup(t *testing.T) (*ca.CA, objproto.Endpoint) {
	tmpDir := t.TempDir()
	var storageSecret = make([]byte, 32)
	for i := range storageSecret {
		storageSecret[i] = byte(i + 1)
	}
	storageDir := filepath.Join(tmpDir, "storage")
	os.MkdirAll(storageDir, 0755)
	newCertStorage, err := storage.NewDirStorage(storageDir, time.Now, storageSecret)
	if err != nil {
		t.Fatalf("failed to create storage: %v", err)
	}
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	var caSecret = make([]byte, 32)
	for i := range caSecret {
		caSecret[i] = byte(i + 1)
	}
	caStatePath := filepath.Join(tmpDir, "ca_state.dat")
	// Create a new self-signed CA
	c, err := ca.NewSelfSignedCA(priv, "Test CA", newCertStorage, caStatePath, caSecret)
	if err != nil {
		t.Fatalf("failed to create CA: %v", err)
	}
	peer, self := transport.InMemoryPipeSession(slog.Default())
	go func() {
		for conn := range peer.GetNewActiveConnectionChannel() {
			_, err := c.CAHandshake(context.Background(), slog.Default(), conn, "server", ca.EqualAppName, func(cn string) error { return nil })
			if err != nil {
				slog.Default().Error("CA handshake failed", "error", err)
			}
			conn.Close()
		}
	}()
	return c, self
}

func TestBootstrap(t *testing.T) {
	caObj, sess := commonSetup(t)
	_, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	random, err := objproto.NewRandomConnectionID("mock", netip.MustParseAddrPort("127.0.0.1:8080"))
	if err != nil {
		t.Fatalf("failed to create random connection ID: %v", err)
	}
	data, err := caObj.GenerateBootstrapToken(1*time.Minute, "app", "Test Bootstrap", 10*time.Minute)
	if err != nil {
		t.Fatalf("failed to generate bootstrap token: %v", err)
	}
	ctx := context.Background()
	boot, err := ca.BootstrapProtocol(ctx, random, sess, "ed25519", "Success Test Bootstrap", data, "app")
	if err != nil {
		t.Fatalf("bootstrap protocol failed: %v", err)
	}
	conn, err := ca.HandshakeProtocol(ctx, sess, random, "client", "app", boot)
	if err != nil {
		t.Fatalf("handshake protocol failed: %v", err)
	}
	conn.Close()
	updated, err := ca.UpdateCertProtocol(ctx, sess, random, "Success Test Bootstrap", "app", boot)
	if err != nil {
		t.Fatalf("update cert protocol failed: %v", err)
	}
	conn2, err := ca.HandshakeProtocol(ctx, sess, random, "client", "app", updated)
	if err != nil {
		t.Fatalf("handshake protocol with updated cert failed: %v", err)
	}
	conn2.Close()
	dumped, err := updated.DumpResult()
	if err != nil {
		t.Fatalf("failed to dump updated cert info: %v", err)
	}
	loaded, err := ca.LoadCertInfo(dumped)
	if err != nil {
		t.Fatalf("failed to load dumped cert info: %v", err)
	}
	if string(loaded.SelfCert) != string(updated.SelfCert) {
		t.Fatalf("loaded self cert does not match updated self cert")
	}
}
