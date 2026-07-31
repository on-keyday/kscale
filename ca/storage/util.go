package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/on-keyday/kscale/ca/storage/index"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/hkdf"
)

func KeyHash(key []byte, data []byte) [64]byte {
	h := hmac.New(sha512.New, key)
	h.Write(data)
	var hash [64]byte
	copy(hash[:], h.Sum(nil))
	return hash
}

func DeriveKey(index uint64, context string, baseKey []byte) []byte {
	k := binary.BigEndian.AppendUint64([]byte{}, index)
	hkdf := hkdf.New(sha512.New, baseKey, nil, append(k, context...))
	var key [64]byte
	_, err := hkdf.Read(key[:])
	if err != nil {
		return nil
	}
	return key[:]
}

func CertInformation(name string, certData []byte, serial SerialNumber, now time.Time, validity ValidityInfo, idx uint64, hashKey []byte) (*index.CertIndex, error) {
	if len(name) > 255 {
		return nil, fmt.Errorf("name too long")
	}
	if len(certData) > 65535 {
		return nil, fmt.Errorf("certificate data too long")
	}
	hash := &index.CertIndex{}
	hash.CertLen = uint16(len(certData))
	hash.Pointer.Pointer = idx
	if !hash.Name.SetName([]uint8(name)) {
		return nil, fmt.Errorf("failed to set name")
	}
	hash.Common.NotBefore.Seconds = uint64(validity.NotBefore.Unix())
	hash.Common.NotAfter.Seconds = uint64(validity.NotAfter.Unix())
	hash.Common.UpdatedAt.Seconds = uint64(now.Unix())
	hash.SerialNumber = serial
	sum := KeyHash(hashKey, certData)
	copy(hash.CertificateHash.Hash[:], sum[:])
	return hash, nil
}

func PrivateKeyInformation(name string, privData []byte, idx uint64, now time.Time, validity ValidityInfo, hashedKey []byte) (*index.PrivateKeyIndex, error) {
	if len(privData) > 65535 {
		return nil, fmt.Errorf("private key data too long")
	}
	hash := &index.PrivateKeyIndex{}
	hash.PrivLen = uint16(len(privData))
	hash.Pointer.Pointer = idx
	hash.Common.NotBefore.Seconds = uint64(validity.NotBefore.Unix())
	hash.Common.NotAfter.Seconds = uint64(validity.NotAfter.Unix())
	hash.Common.UpdatedAt.Seconds = uint64(now.Unix())
	if !hash.Name.SetName([]uint8(name)) {
		return nil, fmt.Errorf("failed to set name")
	}
	sum := KeyHash(hashedKey, privData)
	copy(hash.PrivateKeyHash.Hash[:], sum[:])
	return hash, nil
}

func KeyPairInformation(name string, now time.Time, validity ValidityInfo, serial SerialNumber, privData, certData []byte, privIdx, certIdx uint64, hashKey []byte) (*index.KeyPairIndex, error) {
	certInfo, err := CertInformation(name, append(privData, certData...), serial, now, validity, certIdx, hashKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create certificate information: %w", err)
	}
	certInfo.CertLen = uint16(len(certData)) // adjust cert length only
	keyPair := &index.KeyPairIndex{
		PrivLen: uint16(len(privData)),
		Cert:    *certInfo,
	}
	return keyPair, nil
}

func BlobInformation(name string, data []byte, idx uint64, now time.Time, validity ValidityInfo, hashKey []byte) (*index.BlobIndex, error) {
	// no length limit for blob data
	hash := &index.BlobIndex{}
	hash.BlobLen = uint64(len(data))
	hash.Pointer.Pointer = idx
	hash.Common.NotBefore.Seconds = uint64(validity.NotBefore.Unix())
	hash.Common.NotAfter.Seconds = uint64(validity.NotAfter.Unix())
	hash.Common.UpdatedAt.Seconds = uint64(now.Unix())
	if !hash.Name.SetName([]uint8(name)) {
		return nil, fmt.Errorf("failed to set name")
	}
	sum := KeyHash(hashKey, data)
	copy(hash.BlobHash.Hash[:], sum[:])
	return hash, nil
}

func SavePasswordEncryptedDataToFile(path string, data []byte, password string) error {
	encData, err := EncryptDataWithPassword(data, password)
	if err != nil {
		return err
	}
	return saveToFile(path, encData)
}

func SaveAESEncryptedDataToFile(path string, data []byte, key []byte) error {
	encData, err := EncryptAESGCM(data, key)
	if err != nil {
		return err
	}
	return saveToFile(path, encData)
}

func saveToFile(path string, data []byte) error {
	// force sync write
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	if err != nil {
		return err
	}
	return f.Sync()
}

func LoadPasswordEncryptedDataFromFile(path string, password string) ([]byte, error) {
	encData, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return DecryptDataWithPassword(encData, password)
}

func LoadAESEncryptedDataFromFile(path string, key []byte) ([]byte, error) {
	encData, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return DecryptAESGCM(encData, key)
}

func EncryptDataWithPassword(data []byte, password string) ([]byte, error) {
	salt := make([]byte, 16)
	_, err := rand.Read(salt)
	if err != nil {
		return nil, err
	}
	key := argon2.IDKey([]byte(password), salt, 3, 64*1024, 4, 32)
	encData, err := EncryptAESGCM(data, key)
	if err != nil {
		return nil, err
	}
	// format EncryptedDataPassword:
	//    salt :[16]u8
	//    encrypted_data :EncryptedDataAESGCM
	encWithSalt := make([]byte, 16+len(encData))
	copy(encWithSalt[:16], salt)
	copy(encWithSalt[16:], encData)
	return encWithSalt, nil
}

func EncryptAESGCM(data, key []byte) ([]byte, error) {
	// format EncryptedDataAESGCM:
	//	 nonce :[nonce_size]u8
	//	 encrypted_data :[..]u8
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aesgcm.NonceSize())
	_, err = rand.Read(nonce)
	if err != nil {
		return nil, err
	}
	ciphertext := aesgcm.Seal(nil, nonce, data, nil)
	return append(nonce, ciphertext...), nil
}

func DecryptDataWithPassword(encData []byte, password string) ([]byte, error) {
	if len(encData) < 16 {
		return nil, errors.New("ciphertext too short")
	}
	salt := encData[:16]
	ciphertext := encData[16:]
	key := argon2.IDKey([]byte(password), salt, 3, 64*1024, 4, 32)
	return DecryptAESGCM(ciphertext, key)
}

func DecryptAESGCM(encData, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonceSize := aesgcm.NonceSize()
	if len(encData) < nonceSize {
		return nil, errors.New("ciphertext too short")
	}
	nonce, ciphertext := encData[:nonceSize], encData[nonceSize:]
	plaintext, err := aesgcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}
	return plaintext, nil
}
