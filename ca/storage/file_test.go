package storage_test

import (
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/kscale/ca/storage"
)

func TestFileStorage(t *testing.T) {
	tmpDir := t.TempDir()
	storageTestDir := filepath.Join(tmpDir, "filestorage_test")
	os.MkdirAll(storageTestDir, 0755)
	randomSecret := make([]byte, 32)
	for i := range randomSecret {
		randomSecret[i] = byte(i + 1)
	}
	st, err := storage.NewDirStorage(storageTestDir, time.Now, randomSecret)
	if err != nil {
		t.Fatalf("failed to create file storage: %v", err)
	}
	mgrID, err := storage.MakeRandomManagerID()
	if err != nil {
		t.Fatalf("failed to make random manager ID: %v", err)
	}
	otherMgrID, err := storage.MakeRandomManagerID()
	if err != nil {
		t.Fatalf("failed to make random manager ID: %v", err)
	}
	err = st.SaveCertificate(mgrID, "common_name", storage.MustNewSerialNumber(1), storage.ValidityInfo{
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(3 * time.Minute),
	}, []byte("certificate_data"))
	if err != nil {
		t.Fatalf("failed to save certificate: %v", err)
	}
	certData, err := st.LoadCertificate(mgrID, "common_name")
	if err != nil {
		t.Fatalf("failed to load certificate: %v", err)
	}
	if string(certData) != "certificate_data" {
		t.Fatalf("loaded certificate data does not match saved data")
	}
	err = st.UpdateCertificate(mgrID, "common_name", storage.MustNewSerialNumber(2), storage.ValidityInfo{
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(5 * time.Minute),
	}, []byte("updated_certificate_data"))
	if err != nil {
		t.Fatalf("failed to update certificate: %v", err)
	}
	certData, err = st.LoadCertificate(mgrID, "common_name")
	if err != nil {
		t.Fatalf("failed to load certificate: %v", err)
	}
	if string(certData) != "updated_certificate_data" {
		t.Fatalf("loaded certificate data does not match updated data")
	}
	err = st.DeleteCertificate(mgrID, "common_name")
	if err != nil {
		t.Fatalf("failed to delete certificate: %v", err)
	}
	_, err = st.LoadCertificate(mgrID, "common_name")
	if err == nil {
		t.Fatalf("expected error when loading deleted certificate, got none")
	}
	err = st.SaveCertificate(mgrID, "another_key", storage.MustNewSerialNumber(3), storage.ValidityInfo{
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(3 * time.Minute),
	}, []byte("data"))
	if err != nil {
		t.Fatalf("failed to save another certificate: %v", err)
	}
	_, err = st.LoadCertificate(mgrID, "another_key")
	if err != nil {
		t.Fatalf("failed to load another certificate: %v", err)
	}
	st.SaveKeyPair(mgrID, "keypair1", storage.MustNewSerialNumber(3), storage.ValidityInfo{
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(10 * time.Minute),
	}, []byte("private_key_data"), []byte("certificate_data"))
	privData, certData, err := st.LoadKeyPair(mgrID, "keypair1")
	if err != nil {
		t.Fatalf("failed to load key pair: %v", err)
	}
	if string(privData) != "private_key_data" || string(certData) != "certificate_data" {
		t.Fatalf("loaded key pair data does not match saved data")
	}
	err = st.UpdateKeyPair(mgrID, "keypair1", storage.MustNewSerialNumber(4), storage.ValidityInfo{
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(20 * time.Minute),
	}, []byte("updated_private_key_data"), []byte("updated_certificate_data"))
	if err != nil {
		t.Fatalf("failed to update key pair: %v", err)
	}
	privData, certData, err = st.LoadKeyPair(mgrID, "keypair1")
	if err != nil {
		t.Fatalf("failed to load key pair: %v", err)
	}
	if string(privData) != "updated_private_key_data" || string(certData) != "updated_certificate_data" {
		t.Fatalf("loaded key pair data does not match updated data")
	}
	err = st.DeleteKeyPair(mgrID, "keypair1")
	if err != nil {
		t.Fatalf("failed to delete key pair: %v", err)
	}
	_, _, err = st.LoadKeyPair(mgrID, "keypair1")
	if err == nil {
		t.Fatalf("expected error when loading deleted key pair, got none")
	}
	// non existent key pair
	_, _, err = st.LoadKeyPair(mgrID, "non_existent")
	if err == nil {
		t.Fatalf("expected error when loading non-existent key pair, got none")
	}
	if errors.Is(err, storage.ErrNotExist) == false {
		t.Fatalf("expected ErrNotExist when loading non-existent key pair, got: %v", err)
	}
	_, err = st.LoadCertificate(mgrID, "non_existent_cert")
	if err == nil {
		t.Fatalf("expected error when loading non-existent certificate, got none")
	}
	if errors.Is(err, storage.ErrNotExist) == false {
		t.Fatalf("expected ErrNotExist when loading non-existent certificate, got: %v", err)
	}
	// exist check
	err = st.SaveCertificate(mgrID, "exist_cert", storage.MustNewSerialNumber(1), storage.ValidityInfo{
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(3 * time.Minute),
	}, []byte("data"))
	if err != nil {
		t.Fatalf("failed to save exist_cert certificate: %v", err)
	}
	err = st.SaveCertificate(mgrID, "exist_cert", storage.MustNewSerialNumber(1), storage.ValidityInfo{
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(3 * time.Minute),
	}, []byte("data"))
	if err == nil {
		t.Fatalf("expected error when saving existing certificate, got none")
	}
	if errors.Is(err, storage.ErrExist) == false {
		t.Fatalf("expected ErrExist when saving existing certificate, got: %v", err)
	}
	// exist check for key pair
	err = st.SaveKeyPair(mgrID, "exist_keypair", storage.MustNewSerialNumber(2), storage.ValidityInfo{
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(10 * time.Minute),
	}, []byte("private_key_data"), []byte("certificate_data"))
	if err != nil {
		t.Fatalf("failed to save exist_keypair key pair: %v", err)
	}
	err = st.SaveKeyPair(mgrID, "exist_keypair", storage.MustNewSerialNumber(2), storage.ValidityInfo{
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(10 * time.Minute),
	}, []byte("private_key_data"), []byte("certificate_data"))
	if err == nil {
		t.Fatalf("expected error when saving existing key pair, got none")
	}
	if errors.Is(err, storage.ErrExist) == false {
		t.Fatalf("expected ErrExist when saving existing key pair, got: %v", err)
	}
	idx, err := st.GetCertificateRawIndex(mgrID, "exist_cert")
	if err != nil {
		t.Fatalf("failed to get certificate raw index: %v", err)
	}
	if string(idx.Name.Name) != "exist_cert" {
		t.Fatalf("certificate index name mismatch")
	}
	idxSerial := big.NewInt(0).SetBytes(idx.SerialNumber.Serial[:])
	if idxSerial.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("certificate index serial number mismatch")
	}
	idx2, err := st.GetKeyPairRawIndex(mgrID, "exist_keypair")
	if err != nil {
		t.Fatalf("failed to get key pair raw index: %v", err)
	}
	if string(idx2.Cert.Name.Name) != "exist_keypair" {
		t.Fatalf("key pair index name mismatch")
	}
	idx2Serial := big.NewInt(0).SetBytes(idx2.Cert.SerialNumber.Serial[:])
	if idx2Serial.Cmp(big.NewInt(2)) != 0 {
		t.Fatalf("key pair index serial number mismatch")
	}
	// cannnot access with different manager ID
	_, err = st.LoadCertificate(otherMgrID, "exist_cert")
	if err == nil {
		t.Fatalf("expected error when loading certificate with different manager ID, got none")
	}
	if errors.Is(err, storage.ErrNotExist) == false {
		t.Fatalf("expected ErrNotExist when loading certificate with different manager ID, got: %v", err)
	}
}

func TestSaveAndLoad(t *testing.T) {
	tmpDir := t.TempDir()
	storageDir := filepath.Join(tmpDir, "saveandload_test")
	os.MkdirAll(storageDir, 0755)
	randomSecret := make([]byte, 32)
	for i := range randomSecret {
		randomSecret[i] = byte(i + 1)
	}
	st, err := storage.NewDirStorage(storageDir, time.Now, randomSecret)
	if err != nil {
		t.Fatalf("failed to create storage: %v", err)
	}
	mgrID, err := storage.MakeRandomManagerID()
	if err != nil {
		t.Fatalf("failed to make random manager ID: %v", err)
	}
	err = st.SaveBlob(mgrID, "blob1", storage.ValidityInfo{
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(10 * time.Minute),
	}, []byte("blobdata1"))
	if err != nil {
		t.Fatalf("failed to save blob: %v", err)
	}
	randomSecretCopy := make([]byte, 32)
	copy(randomSecretCopy, randomSecret)
	st.Close()
	st, err = storage.NewDirStorage(storageDir, time.Now, randomSecretCopy)
	if err != nil {
		t.Fatalf("failed to reopen storage: %v", err)
	}
	blobData, err := st.LoadBlob(mgrID, "blob1")
	if err != nil {
		t.Fatalf("failed to load blob: %v", err)
	}
	if string(blobData) != "blobdata1" {
		t.Fatalf("loaded blob data does not match original data")
	}
}

func TestMigration(t *testing.T) {
	tmpDir := t.TempDir()
	oldDir := filepath.Join(tmpDir, "old_storage")
	newDir := filepath.Join(tmpDir, "new_storage")
	os.MkdirAll(oldDir, 0755)
	os.MkdirAll(newDir, 0755)
	randomSecret := make([]byte, 32)
	for i := range randomSecret {
		randomSecret[i] = byte(i + 1)
	}
	old, err := storage.NewDirStorage(oldDir, time.Now, randomSecret)
	if err != nil {
		t.Fatalf("failed to create old storage: %v", err)
	}
	mgrID, err := storage.MakeRandomManagerID()
	if err != nil {
		t.Fatalf("failed to make random manager ID: %v", err)
	}
	old.SaveCertificate(mgrID, "cert1", storage.MustNewSerialNumber(1), storage.ValidityInfo{
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(5 * time.Minute),
	}, []byte("certificate1"))
	old.SavePrivateKey(mgrID, "priv1", storage.ValidityInfo{
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(10 * time.Minute),
	}, []byte("privatekey1"))
	old.SaveBlob(mgrID, "blob1", storage.ValidityInfo{
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(10 * time.Minute),
	}, []byte("blobdata1"))
	old.SaveKeyPair(mgrID, "keypair1", storage.MustNewSerialNumber(2), storage.ValidityInfo{
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(15 * time.Minute),
	}, []byte("private_key1"), []byte("certificate1"))
	old.SaveKeyPair(mgrID, "keypair2", storage.MustNewSerialNumber(3), storage.ValidityInfo{
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(1 * time.Second),
	}, []byte("private_key2"), []byte("certificate2")) // garbage record
	time.Sleep(2 * time.Second) // ensure keypair2 is expired
	var newSecret [32]byte
	copy(newSecret[:], randomSecret)
	newSecret[0] ^= 0xFF
	new, err := storage.MigrateDirStorage(old, newDir, newSecret[:], time.Now)
	if err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	certData, err := new.LoadCertificate(mgrID, "cert1")
	if err != nil {
		t.Fatalf("failed to load migrated certificate: %v", err)
	}
	if string(certData) != "certificate1" {
		t.Fatalf("migrated certificate data does not match original data")
	}
	privData, err := new.LoadPrivateKey(mgrID, "priv1")
	if err != nil {
		t.Fatalf("failed to load migrated private key: %v", err)
	}
	if string(privData) != "privatekey1" {
		t.Fatalf("migrated private key data does not match original data")
	}
	privData, certData, err = new.LoadKeyPair(mgrID, "keypair1")
	if err != nil {
		t.Fatalf("failed to load migrated key pair: %v", err)
	}
	if string(privData) != "private_key1" || string(certData) != "certificate1" {
		t.Fatalf("migrated key pair data does not match original data")
	}
	_, _, err = new.LoadKeyPair(mgrID, "keypair2")
	if err == nil {
		t.Fatalf("expected error when loading garbage record key pair, got none")
	}
	if errors.Is(err, storage.ErrNotExist) == false {
		t.Fatalf("expected ErrNotExist when loading garbage record key pair, got: %v", err)
	}
	blobData, err := new.LoadBlob(mgrID, "blob1")
	if err != nil {
		t.Fatalf("failed to load migrated blob: %v", err)
	}
	if string(blobData) != "blobdata1" {
		t.Fatalf("migrated blob data does not match original data")
	}
	// try delete blob
	err = new.DeleteBlob(mgrID, "blob1")
	if err != nil {
		t.Fatalf("failed to delete migrated blob: %v", err)
	}
	_, err = new.LoadBlob(mgrID, "blob1")
	if err == nil {
		t.Fatalf("expected error when loading deleted blob, got none")
	}
	if errors.Is(err, storage.ErrNotExist) == false {
		t.Fatalf("expected ErrNotExist when loading deleted blob, got: %v", err)
	}
}

func TestMultiTenancy(t *testing.T) {
	tmpDir := t.TempDir()
	storageDir := filepath.Join(tmpDir, "multitenancy_test")
	os.MkdirAll(storageDir, 0755)
	randomSecret := make([]byte, 32)
	for i := range randomSecret {
		randomSecret[i] = byte(i + 1)
	}
	st, err := storage.NewDirStorage(storageDir, time.Now, randomSecret)
	if err != nil {
		t.Fatalf("failed to create storage: %v", err)
	}
	mgrID1, err := storage.MakeRandomManagerID()
	if err != nil {
		t.Fatalf("failed to make random manager ID 1: %v", err)
	}
	mgrID2, err := storage.MakeRandomManagerID()
	if err != nil {
		t.Fatalf("failed to make random manager ID 2: %v", err)
	}
	err = st.SaveCertificate(mgrID1, "shared_name", storage.MustNewSerialNumber(1), storage.ValidityInfo{
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(3 * time.Minute),
	}, []byte("certificate_data_mgr1"))
	if err != nil {
		t.Fatalf("failed to save certificate for mgrID1: %v", err)
	}
	err = st.SaveCertificate(mgrID2, "shared_name", storage.MustNewSerialNumber(2), storage.ValidityInfo{
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(3 * time.Minute),
	}, []byte("certificate_data_mgr2"))
	if err != nil {
		t.Fatalf("failed to save certificate for mgrID2: %v", err)
	}
	certData1, err := st.LoadCertificate(mgrID1, "shared_name")
	if err != nil {
		t.Fatalf("failed to load certificate for mgrID1: %v", err)
	}
	if string(certData1) != "certificate_data_mgr1" {
		t.Fatalf("loaded certificate data for mgrID1 does not match saved data")
	}
	certData2, err := st.LoadCertificate(mgrID2, "shared_name")
	if err != nil {
		t.Fatalf("failed to load certificate for mgrID2: %v", err)
	}
	if string(certData2) != "certificate_data_mgr2" {
		t.Fatalf("loaded certificate data for mgrID2 does not match saved data")
	}
	mgrID3, err := storage.MakeRandomManagerID()
	if err != nil {
		t.Fatalf("failed to make random manager ID 3: %v", err)
	}
	_, err = st.LoadCertificate(mgrID3, "shared_name")
	if err == nil {
		t.Fatalf("expected error when loading certificate with non-existent manager ID, got none")
	}
	if errors.Is(err, storage.ErrNotExist) == false {
		t.Fatalf("expected ErrNotExist when loading certificate with non-existent manager ID, got: %v", err)
	}
	// blobs
	err = st.SaveBlob(mgrID1, "shared_blob", storage.ValidityInfo{
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(3 * time.Minute),
	}, []byte("blob_data_mgr1"))
	if err != nil {
		t.Fatalf("failed to save blob for mgrID1: %v", err)
	}
	err = st.SaveBlob(mgrID2, "shared_blob", storage.ValidityInfo{
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(3 * time.Minute),
	}, []byte("blob_data_mgr2"))
	if err != nil {
		t.Fatalf("failed to save blob for mgrID2: %v", err)
	}
	blobData1, err := st.LoadBlob(mgrID1, "shared_blob")
	if err != nil {
		t.Fatalf("failed to load blob for mgrID1: %v", err)
	}
	if string(blobData1) != "blob_data_mgr1" {
		t.Fatalf("loaded blob data for mgrID1 does not match saved data")
	}
	blobData2, err := st.LoadBlob(mgrID2, "shared_blob")
	if err != nil {
		t.Fatalf("failed to load blob for mgrID2: %v", err)
	}
	if string(blobData2) != "blob_data_mgr2" {
		t.Fatalf("loaded blob data for mgrID2 does not match saved data")
	}
	_, err = st.LoadBlob(mgrID3, "shared_blob")
	if err == nil {
		t.Fatalf("expected error when loading blob with non-existent manager ID, got none")
	}
	if errors.Is(err, storage.ErrNotExist) == false {
		t.Fatalf("expected ErrNotExist when loading blob with non-existent manager ID, got: %v", err)
	}
}

func TestInvalidValidity(t *testing.T) {
	tmpDir := t.TempDir()
	storageDir := filepath.Join(tmpDir, "invalid_validity_test")
	os.MkdirAll(storageDir, 0755)
	randomSecret := make([]byte, 32)
	for i := range randomSecret {
		randomSecret[i] = byte(i + 1)
	}
	st, err := storage.NewDirStorage(storageDir, time.Now, randomSecret)
	if err != nil {
		t.Fatalf("failed to create storage: %v", err)
	}
	mgrID, err := storage.MakeRandomManagerID()
	if err != nil {
		t.Fatalf("failed to make random manager ID: %v", err)
	}
	err = st.SaveCertificate(mgrID, "invalid_cert", storage.MustNewSerialNumber(1), storage.ValidityInfo{
		NotBefore: time.Now().Add(5 * time.Minute),
		NotAfter:  time.Now(),
	}, []byte("data"))
	if err == nil {
		t.Fatalf("expected error when saving certificate with invalid validity, got none")
	}
	// blobs
	err = st.SaveBlob(mgrID, "invalid_blob", storage.ValidityInfo{
		NotBefore: time.Now().Add(5 * time.Minute),
		NotAfter:  time.Now(),
	}, []byte("data"))
	if err == nil {
		t.Fatalf("expected error when saving blob with invalid validity, got none")
	}
}
