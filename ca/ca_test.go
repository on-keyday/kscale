package ca_test

import (
	"crypto/ed25519"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/kscale/ca"
	"github.com/on-keyday/kscale/ca/storage"
)

func TestCA(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	tmpDir := t.TempDir()
	caStatePath := filepath.Join(tmpDir, "ca_state.dat")
	var secret = make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	certStorage, err := storage.NewDirStorage(tmpDir, time.Now, secret)
	if err != nil {
		t.Fatalf("failed to create storage: %v", err)
	}
	ssCA, err := ca.NewSelfSignedCA(priv, "Test CA", certStorage, caStatePath, secret)
	if err != nil {
		t.Fatalf("failed to create CA: %v", err)
	}
	pubKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	issued, err := ssCA.IssueCertificate("hoge", pubKey, "test", 24*time.Hour)
	if err != nil {
		t.Fatalf("failed to issue certificate: %v", err)
	}
	_, err = ssCA.VerifyCertificate(issued, "test", ca.EqualAppName)
	if err != nil {
		t.Fatalf("failed to verify certificate: %v", err)
	}
	caState, err := ssCA.DumpCAState()
	if err != nil {
		t.Fatalf("failed to dump CA state: %v", err)
	}
	var copySecret = make([]byte, 32)
	copy(copySecret, secret)
	certStorage.Close()

	newCertStorage, err := storage.NewDirStorage(tmpDir, time.Now, copySecret)
	if err != nil {
		t.Fatalf("failed to create storage: %v", err)
	}
	certStorage = newCertStorage

	newCA, err := ca.NewFromCAState(caState, certStorage, caStatePath, copySecret)
	if err != nil {
		t.Fatalf("failed to load CA from state: %v", err)
	}
	_, err = newCA.VerifyCertificate(issued, "test", ca.EqualAppName)
	if err != nil {
		t.Fatalf("failed to verify certificate with new CA: %v", err)
	}
	// test update
	newKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("failed to generate new key: %v", err)
	}
	newlyIssued, _, err := newCA.UpdateCertificate("hoge", newKey, "hoge", 24*time.Hour)
	if err != nil {
		t.Fatalf("failed to update certificate: %v", err)
	}
	_, err = newCA.VerifyCertificate(newlyIssued, "hoge", ca.EqualAppName)
	if err != nil {
		t.Fatalf("failed to verify updated certificate: %v", err)
	}
	_, err = newCA.VerifyCertificate(issued, "test", ca.EqualAppName)
	if err == nil {
		t.Fatalf("expected error when verifying old certificate, got none")
	}

	// test revoke
	err = newCA.RevokeCertificate("hoge")
	if err != nil {
		t.Fatalf("failed to revoke certificate: %v", err)
	}
	_, err = newCA.VerifyCertificate(newlyIssued, "test", ca.EqualAppName)
	if err == nil {
		t.Fatalf("expected error when verifying old certificate, got none")
	}

	fugaPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	fugaIssued, err := newCA.IssueCertificate("fuga", fugaPub, "fuga", 24*time.Hour)
	if err != nil {
		t.Fatalf("failed to issue certificate: %v", err)
	}
	migratedDir := filepath.Join(tmpDir, "migrated")
	err = os.MkdirAll(migratedDir, 0700)
	if err != nil {
		t.Fatalf("failed to create migrated directory: %v", err)
	}
	err = newCA.DoMigration(func(old storage.CertificateStorage) (storage.CertificateStorage, error) {
		return storage.MigrateDirStorage(old, migratedDir, nil, time.Now)
	})
	if err != nil {
		t.Fatalf("failed to migrate storage: %v", err)
	}
	certStorage.Close()
	_, err = newCA.VerifyCertificate(fugaIssued, "fuga", ca.EqualAppName)
	if err != nil {
		t.Fatalf("failed to verify certificate after migration: %v", err)
	}
	_, err = newCA.VerifyCertificate(newlyIssued, "test", ca.EqualAppName)
	if err == nil {
		t.Fatalf("expected error when verifying revoked certificate after migration, got none")
	}
	issuedAfterMigration, err := newCA.IssueCertificate("after_migration", pubKey, "after", 24*time.Hour)
	if err != nil {
		t.Fatalf("failed to issue certificate after migration: %v", err)
	}
	_, err = newCA.VerifyCertificate(issuedAfterMigration, "after", ca.EqualAppName)
	if err != nil {
		t.Fatalf("failed to verify certificate issued after migration: %v", err)
	}
}

func TestRealMigration(t *testing.T) {
	tmpDir := t.TempDir()
	currentPath := filepath.Join(tmpDir, "current_storage")
	oldPath := filepath.Join(tmpDir, "old_storage")
	migrateDir := filepath.Join(tmpDir, "migrated_storage")

	var secret = make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	os.MkdirAll(currentPath, 0700)
	dirStorage, err := storage.NewDirStorage(currentPath, time.Now, secret)
	if err != nil {
		t.Fatalf("failed to create initial storage: %v", err)
	}

	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	selfCA, err := ca.NewSelfSignedCA(priv, "Migration CA", dirStorage, filepath.Join(tmpDir, "ca_state.dat"), secret)
	if err != nil {
		t.Fatalf("failed to create self-signed CA: %v", err)
	}

	issued, err := selfCA.IssueCertificate("migrate_test", priv.Public(), "migrate", 24*time.Hour)
	if err != nil {
		t.Fatalf("failed to issue certificate before migration: %v", err)
	}
	secondIssued, err := selfCA.IssueCertificate("migrate_test_2", priv.Public(), "migrate", 24*time.Hour)
	if err != nil {
		t.Fatalf("failed to issue second certificate before migration: %v", err)
	}
	// revoke first certificate
	err = selfCA.RevokeCertificate("migrate_test")
	if err != nil {
		t.Fatalf("failed to revoke certificate before migration: %v", err)
	}
	thirdIssued, err := selfCA.IssueCertificate("migrate_test_3", priv.Public(), "migrate", 24*time.Hour)
	if err != nil {
		t.Fatalf("failed to issue third certificate before migration: %v", err)
	}

	err = selfCA.DoMigration(ca.DefaultMigrator(slog.Default(), oldPath, currentPath, migrateDir))
	if err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	fourthIssued, err := selfCA.IssueCertificate("migrate_test_4", priv.Public(), "migrate", 24*time.Hour)
	if err != nil {
		t.Fatalf("failed to issue fourth certificate after migration: %v", err)
	}

	_, err = selfCA.VerifyCertificate(issued, "migrate", ca.EqualAppName)
	if err == nil {
		t.Fatalf("expected error when verifying revoked certificate after migration, got none")
	}
	_, err = selfCA.VerifyCertificate(secondIssued, "migrate", ca.EqualAppName)
	if err != nil {
		t.Fatalf("failed to verify second certificate after migration: %v", err)
	}
	_, err = selfCA.VerifyCertificate(thirdIssued, "migrate", ca.EqualAppName)
	if err != nil {
		t.Fatalf("failed to verify third certificate after migration: %v", err)
	}
	_, err = selfCA.VerifyCertificate(fourthIssued, "migrate", ca.EqualAppName)
	if err != nil {
		t.Fatalf("failed to verify fourth certificate after migration: %v", err)
	}
}
