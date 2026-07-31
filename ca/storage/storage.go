package storage

import (
	"crypto/rand"
	"fmt"
	"io"
	"math/big"
	"time"

	"github.com/on-keyday/kscale/ca/storage/index"
)

type ValidityInfo struct {
	NotBefore time.Time
	NotAfter  time.Time
}

func (v *ValidityInfo) ToIndexFormat() (uint64, uint64) {
	return uint64(v.NotBefore.Unix()), uint64(v.NotAfter.Unix())
}

func (v *ValidityInfo) FromIndexFormat(notBefore uint64, notAfter uint64) {
	v.NotBefore = time.Unix(int64(notBefore), 0)
	v.NotAfter = time.Unix(int64(notAfter), 0)
}

func (v *ValidityInfo) IsValid(at time.Time) bool {
	return !at.Before(v.NotBefore) && !at.After(v.NotAfter)
}

func (v *ValidityInfo) Consistent() bool {
	return !v.NotBefore.IsZero() && !v.NotAfter.IsZero() && v.NotBefore.Before(v.NotAfter)
}

func (v *ValidityInfo) Expired(at time.Time) bool {
	return at.After(v.NotAfter)
}

type SerialNumber = index.SerialNumber

func NewSerialNumber(n uint64) (SerialNumber, error) {
	var sn SerialNumber
	big.NewInt(0).SetUint64(n).FillBytes(sn.Serial[:])
	return sn, nil
}

func MustNewSerialNumber(n uint64) SerialNumber {
	sn, err := NewSerialNumber(n)
	if err != nil {
		panic(err)
	}
	return sn
}

func SerialNumberFromBigInt(b *big.Int) (SerialNumber, error) {
	var sn SerialNumber
	bytes := b.Bytes()
	if len(bytes) > len(sn.Serial) {
		return sn, fmt.Errorf("big.Int too large to fit in SerialNumber")
	}
	copy(sn.Serial[len(sn.Serial)-len(bytes):], bytes)
	return sn, nil
}

type CertificateStorage interface {
	SaveCertificate(id ManagerID, name string, serial SerialNumber, validity ValidityInfo, certData []byte) error
	UpdateCertificate(id ManagerID, name string, serial SerialNumber, validity ValidityInfo, certData []byte) error
	LoadCertificate(id ManagerID, name string) ([]byte, error)
	GetCertificateRawIndex(id ManagerID, name string) (*index.CertIndex, error)
	DeleteCertificate(id ManagerID, name string) error

	SaveKeyPair(id ManagerID, name string, serial SerialNumber, validity ValidityInfo, privData, certData []byte) error
	UpdateKeyPair(id ManagerID, name string, serial SerialNumber, validity ValidityInfo, privData, certData []byte) error
	LoadKeyPair(id ManagerID, name string) (privData, certData []byte, err error)
	GetKeyPairRawIndex(id ManagerID, name string) (*index.KeyPairIndex, error)
	DeleteKeyPair(id ManagerID, name string) error

	SavePrivateKey(id ManagerID, name string, validity ValidityInfo, privData []byte) error
	UpdatePrivateKey(id ManagerID, name string, validity ValidityInfo, privData []byte) error
	LoadPrivateKey(id ManagerID, name string) ([]byte, error)
	GetPrivateKeyRawIndex(id ManagerID, name string) (*index.PrivateKeyIndex, error)
	DeletePrivateKey(id ManagerID, name string) error

	SaveBlob(id ManagerID, name string, validity ValidityInfo, data []byte) error
	LoadBlob(id ManagerID, name string) ([]byte, error)
	UpdateBlob(id ManagerID, name string, validity ValidityInfo, data []byte) error
	GetBlobRawIndex(id ManagerID, name string) (*index.BlobIndex, error)
	DeleteBlob(id ManagerID, name string) error

	DeleteExpired(at time.Time) error

	GetRecordCounter() uint64
	GetCertificateCount(mgrID ManagerID) int
	GetKeyPairCount(mgrID ManagerID) int
	GetPrivateKeyCount(mgrID ManagerID) int
	GetBlobCount(mgrID ManagerID) int

	ListCertificates(mgrID ManagerID) []index.CertIndex
	ListKeyPairs(mgrID ManagerID) []index.KeyPairIndex
	ListPrivateKeys(mgrID ManagerID) []index.PrivateKeyIndex
	ListBlobs(mgrID ManagerID) []index.BlobIndex
	io.Closer
}

func MakeRandomManagerID() (ManagerID, error) {
	var id ManagerID
	_, err := io.ReadFull(rand.Reader, id.Id[:])
	if err != nil {
		return ManagerID{}, err
	}
	return id, nil
}
