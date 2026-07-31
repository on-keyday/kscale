package storage

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/on-keyday/kscale/ca/storage/index"
)

type ManagerID = index.ManagerID

func NewManagerID(id [16]byte) ManagerID {
	return ManagerID{Id: id}
}

type indexEntry[T any] struct {
	FileOffset int64
	Manager    index.ManagerID
	Value      T
}

const certHashContext = "kcdn-certificate-file"
const recordEncryptionContext = "kcdn-certificate-record"
const fileEncryptionContext = "kcdn-certificate-file-data"
const fileHeaderEncryptionContext = "kcdn-certificate-file-header"

var ErrExist = fmt.Errorf("key already exists")
var ErrNotExist = fmt.Errorf("no such key")
var ErrMgrID = fmt.Errorf("manager ID mismatch")

type CertMap = map[string]*indexEntry[index.CertIndex]
type KeyPairMap = map[string]*indexEntry[index.KeyPairIndex]
type PrivateKey = map[string]*indexEntry[index.PrivateKeyIndex]

type MapPerManager[T any] map[index.ManagerID]map[string]*indexEntry[T]

func (c *MapPerManager[T]) Add(mgrID ManagerID, name string, entry *indexEntry[T]) {
	if *c == nil {
		*c = make(MapPerManager[T])
	}
	if (*c)[mgrID] == nil {
		(*c)[mgrID] = make(map[string]*indexEntry[T])
	}
	(*c)[mgrID][name] = entry
}

func (c *MapPerManager[T]) Get(mgrID ManagerID, name string) (*indexEntry[T], bool) {
	if items, ok := (*c)[mgrID]; ok {
		entry, ok := items[name]
		return entry, ok
	}
	return nil, false
}

func (c *MapPerManager[T]) Delete(mgrID ManagerID, name string) {
	if items, ok := (*c)[mgrID]; ok {
		delete(items, name)
	}
}

func (c *MapPerManager[T]) GetCount(mgrID ManagerID) uint64 {
	if items, ok := (*c)[mgrID]; ok {
		return uint64(len(items))
	}
	return 0
}

func (c *MapPerManager[T]) List(mgrID ManagerID) []T {
	if items, ok := (*c)[mgrID]; ok {
		var result []T
		for _, entry := range items {
			result = append(result, entry.Value)
		}
		return result
	}
	return nil
}

type CertMapPerManager = MapPerManager[index.CertIndex]
type KeyPairMapPerManager = MapPerManager[index.KeyPairIndex]
type PrivateKeyMapPerManager = MapPerManager[index.PrivateKeyIndex]
type BlobMapPerManager = MapPerManager[index.BlobIndex]

/*
type CertMapPerManager map[index.ManagerID]CertMap

func (c *CertMapPerManager) AddCertificate(mgrID ManagerID, name string, entry *indexEntry[index.CertIndex]) {
	if *c == nil {
		*c = make(CertMapPerManager)
	}
	if (*c)[mgrID] == nil {
		(*c)[mgrID] = make(CertMap)
	}
	(*c)[mgrID][name] = entry
}

func (c *CertMapPerManager) GetCertificateIndex(mgrID ManagerID, name string) (*indexEntry[index.CertIndex], bool) {
	if certs, ok := (*c)[mgrID]; ok {
		entry, ok := certs[name]
		return entry, ok
	}
	return nil, false
}

func (c *CertMapPerManager) DeleteCertificate(mgrID ManagerID, name string) {
	if certs, ok := (*c)[mgrID]; ok {
		delete(certs, name)
	}
}

func (c *CertMapPerManager) GetCertificateCount(mgrID ManagerID) uint64 {
	if certs, ok := (*c)[mgrID]; ok {
		return uint64(len(certs))
	}
	return 0
}

func (c *CertMapPerManager) ListCertificates(mgrID ManagerID) []index.CertIndex {
	if certs, ok := (*c)[mgrID]; ok {
		var result []index.CertIndex
		for _, entry := range certs {
			result = append(result, entry.Value)
		}
		return result
	}
	return nil
}

type KeyPairMapPerManager map[index.ManagerID]KeyPairMap

func (k *KeyPairMapPerManager) AddKeyPair(mgrID ManagerID, name string, entry *indexEntry[index.KeyPairIndex]) {
	if *k == nil {
		*k = make(KeyPairMapPerManager)
	}
	if (*k)[mgrID] == nil {
		(*k)[mgrID] = make(KeyPairMap)
	}
	(*k)[mgrID][name] = entry
}

func (k *KeyPairMapPerManager) DeleteKeyPair(mgrID ManagerID, name string) {
	if keypairs, ok := (*k)[mgrID]; ok {
		delete(keypairs, name)
	}
}

func (k *KeyPairMapPerManager) GetKeyPairIndex(mgrID ManagerID, name string) (*indexEntry[index.KeyPairIndex], bool) {
	if keypairs, ok := (*k)[mgrID]; ok {
		entry, ok := keypairs[name]
		return entry, ok
	}
	return nil, false
}

func (k *KeyPairMapPerManager) ListKeyPairs(mgrID ManagerID) []index.KeyPairIndex {
	if keypairs, ok := (*k)[mgrID]; ok {
		var result []index.KeyPairIndex
		for _, entry := range keypairs {
			result = append(result, entry.Value)
		}
		return result
	}
	return nil
}

func (k *KeyPairMapPerManager) GetKeyPairCount(mgrID ManagerID) uint64 {
	if keypairs, ok := (*k)[mgrID]; ok {
		return uint64(len(keypairs))
	}
	return 0
}
*/

type dirStorage struct {
	dir          *os.Root
	certFileLock sync.Locker
	certIndex    *os.File
	fileCounter  uint64
	now          func() time.Time

	certMapLock      sync.RWMutex
	certs            CertMapPerManager
	keypairs         KeyPairMapPerManager
	privKeys         PrivateKeyMapPerManager
	blobs            BlobMapPerManager
	storageMasterKey []byte

	recordCounter uint64
}

func (s *dirStorage) Close() error {
	s.certIndex.Close()
	s.dir.Close()
	clear(s.certs)
	clear(s.keypairs)
	clear(s.storageMasterKey)
	s.storageMasterKey = nil
	return nil
}

func (s *dirStorage) GetRecordCounter() uint64 {
	s.certMapLock.RLock()
	defer s.certMapLock.RUnlock()
	return s.recordCounter
}

func NewDirStorage(path string, now func() time.Time, storageMasterSecret []byte) (CertificateStorage, error) {
	dir, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	certIndexFile, err := dir.OpenFile("index.dat", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		dir.Close()
		return nil, err
	}
	info, err := certIndexFile.Stat()
	if err != nil {
		certIndexFile.Close()
		dir.Close()
		return nil, err
	}
	if info.Size() < index.IndexFileHeaderSize {
		if info.Size() != 0 {
			certIndexFile.Close()
			dir.Close()
			return nil, fmt.Errorf("corrupted index file")
		}
		// initialize header
		_, err = certIndexFile.Write(make([]byte, index.IndexFileHeaderSize))
		if err != nil {
			certIndexFile.Close()
			dir.Close()
			return nil, err
		}
		err = rewriteHeaderUnlocked(certIndexFile, 0, storageMasterSecret)
		if err != nil {
			certIndexFile.Close()
			dir.Close()
			return nil, fmt.Errorf("failed to write index header: %w", err)
		}
	}
	recordCounter, err := decodeHeaderUnlocked(certIndexFile, storageMasterSecret)
	if err != nil {
		certIndexFile.Close()
		dir.Close()
		return nil, fmt.Errorf("failed to read index header: %w", err)
	}
	storage := &dirStorage{
		dir:              dir,
		certIndex:        certIndexFile,
		certFileLock:     &sync.Mutex{},
		now:              now,
		storageMasterKey: storageMasterSecret,
	}
	certs, keypairs, privKeys, blobs, maxIndex, maxRecordCounter, entryCount, err := loadIndex(certIndexFile, storage.storageMasterKey, now())
	if err != nil {
		certIndexFile.Close()
		dir.Close()
		return nil, err
	}
	if entryCount != 0 && recordCounter < maxRecordCounter {
		certIndexFile.Close()
		dir.Close()
		return nil, fmt.Errorf("record counter mismatch")
	}
	storage.certs = certs
	storage.keypairs = keypairs
	storage.privKeys = privKeys
	storage.blobs = blobs
	storage.fileCounter = maxIndex
	storage.recordCounter = recordCounter
	return storage, nil
}

func readHeaderAndDecrypt(file *os.File, offset int64, baseKey []byte) (_ *index.Index, _ uint64, _ index.ManagerID, originalData []byte, _ error) {
	var recordHeader [index.RecordHeaderSize]byte
	n, err := file.ReadAt(recordHeader[:], offset)
	if err != nil {
		if err == io.EOF && n != 0 {
			return nil, 0, index.ManagerID{}, nil, fmt.Errorf("incomplete record header")
		}
		return nil, 0, index.ManagerID{}, nil, err
	}
	if n != len(recordHeader) {
		return nil, 0, index.ManagerID{}, nil, fmt.Errorf("failed to read full record header")
	}
	hdr := &index.RecordHeaderWithNonce{}
	err = hdr.DecodeExact(recordHeader[:n])
	if err != nil {
		return nil, 0, index.ManagerID{}, nil, err
	}
	recBuf := make([]byte, hdr.Header.IndexLen)
	n, err = file.ReadAt(recBuf, offset+int64(n))
	if err != nil {
		return nil, 0, index.ManagerID{}, nil, err
	}
	if int64(n) != int64(hdr.Header.IndexLen) {
		return nil, 0, index.ManagerID{}, nil, fmt.Errorf("incomplete record")
	}
	// decrypt index
	key := DeriveKey(hdr.Header.KeyMaterial, recordEncryptionForManagerContext(hdr.Header.ManagerID), baseKey)
	block, err := aes.NewCipher(key[:32])
	if err != nil {
		return nil, 0, index.ManagerID{}, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, 0, index.ManagerID{}, nil, err
	}
	headerBuf := hdr.MustEncode()
	headerBuf = append(headerBuf, binary.BigEndian.AppendUint64([]byte{}, uint64(offset))...)
	decrypted, err := gcm.Open(nil, hdr.Nonce.Nonce[:], recBuf, headerBuf)
	if err != nil {
		return nil, 0, index.ManagerID{}, nil, err
	}
	rec := &index.Index{}
	err = rec.DecodeExact(decrypted)
	if err != nil {
		return nil, 0, index.ManagerID{}, nil, err
	}
	if rec.Kind != index.IndexKind_Cert && rec.Kind != index.IndexKind_Keypair && rec.Kind != index.IndexKind_Priv && rec.Kind != index.IndexKind_Blob {
		return nil, 0, index.ManagerID{}, nil, fmt.Errorf("unsupported index kind: %d", rec.Kind)
	}
	return rec, rec.RecordCounter, hdr.Header.ManagerID, append(recordHeader[:], recBuf...), nil
}

type encoder interface {
	Write(io.Writer) error
}

func recordEncryptionForManagerContext(mgrID index.ManagerID) string {
	return string(mgrID.Id[:]) + recordEncryptionContext
}

func packRecord(idx encoder, kind index.IndexKind, baseKey []byte, mgrID index.ManagerID, file_offset uint64, record_counter uint64) ([]byte, error) {
	buf := &bytes.Buffer{}
	buf.Write(binary.BigEndian.AppendUint64([]byte{}, record_counter))
	buf.Write([]byte{byte(kind)})
	err := idx.Write(buf)
	if err != nil {
		return nil, err
	}
	rec := &index.Record{}
	rec.Header.Header.ManagerID = mgrID
	var keyMaterial [8]byte
	_, err = io.ReadFull(rand.Reader, keyMaterial[:])
	if err != nil {
		return nil, err
	}
	rec.Header.Header.KeyMaterial = binary.BigEndian.Uint64(keyMaterial[:])
	key := DeriveKey(rec.Header.Header.KeyMaterial, recordEncryptionForManagerContext(mgrID), baseKey)
	block, err := aes.NewCipher(key[:32])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	var nonce [12]byte
	_, err = io.ReadFull(rand.Reader, nonce[:])
	if err != nil {
		return nil, err
	}
	certIndex := buf.Bytes()
	recLen := uint16(len(certIndex) + gcm.Overhead())
	rec.Header.Header.IndexLen = recLen
	rec.Header.Nonce.Nonce = nonce
	headerBuf := rec.Header.MustEncode()
	headerBuf = append(headerBuf, binary.BigEndian.AppendUint64([]byte{}, file_offset)...)
	encIndex := gcm.Seal(nil, nonce[:], certIndex, headerBuf)
	rec.Index = encIndex
	encodedRec, err := rec.Encode()
	if err != nil {
		return nil, err
	}
	return encodedRec, nil
}

// if this fail, its fatal
func RenameDir(storage CertificateStorage, newPath string) error {
	dirStorage, ok := storage.(*dirStorage)
	if !ok {
		return errors.New("expected dirStorage type for swap")
	}
	dirStorage.certFileLock.Lock()
	defer dirStorage.certFileLock.Unlock()
	err := dirStorage.certIndex.Close()
	if err != nil {
		return err
	}
	err = dirStorage.dir.Close()
	if err != nil {
		return err
	}
	err = os.Rename(dirStorage.dir.Name(), newPath)
	if err != nil {
		return err
	}
	dir, err := os.OpenRoot(newPath)
	if err != nil {
		return err
	}
	dirStorage.dir = dir
	certIndexFile, err := dir.OpenFile("index.dat", os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	dirStorage.certIndex = certIndexFile
	return nil
}

func MigrateDirStorage(oldStorage CertificateStorage, newPath string, newKey []byte, now func() time.Time) (_ CertificateStorage, err error) {
	old, ok := oldStorage.(*dirStorage)
	if !ok {
		return nil, errors.New("expected dirStorage type for migration")
	}

	// Migrate the index
	certs := old.certs
	keypairs := old.keypairs
	if newKey == nil {
		copyOfKey := make([]byte, len(old.storageMasterKey))
		copy(copyOfKey, old.storageMasterKey)
		newKey = copyOfKey
	}

	newStorage, err := NewDirStorage(newPath, now, newKey)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			newStorage.Close()
		}
	}()
	newS := newStorage.(*dirStorage)

	var wg sync.WaitGroup

	errs := make(chan error, 1)
	deadline := now()

	for _, certsMap := range certs {
		for name, entry := range certsMap {
			if isGarbageRecord(&entry.Value.Common, deadline) {
				continue // skip garbage records
			}
			wg.Add(1)
			go func(name string, entry *indexEntry[index.CertIndex]) {
				defer wg.Done()
				certData, err := old.LoadCertificate(entry.Manager, name)
				if err != nil {
					errs <- err
					return
				}
				notBefore := time.Unix(int64(entry.Value.Common.NotBefore.Seconds), 0)
				notAfter := time.Unix(int64(entry.Value.Common.NotAfter.Seconds), 0)
				err = newS.SaveCertificate(entry.Manager, name, entry.Value.SerialNumber, ValidityInfo{NotBefore: notBefore, NotAfter: notAfter}, certData)
				if err != nil {
					errs <- err
				}
			}(name, entry)
		}
	}

	for _, kpMap := range keypairs {
		for name, entry := range kpMap {
			if isGarbageRecord(&entry.Value.Cert.Common, deadline) {
				continue // skip garbage records
			}
			wg.Add(1)
			go func(name string, entry *indexEntry[index.KeyPairIndex]) {
				defer wg.Done()
				priv, certData, err := old.LoadKeyPair(entry.Manager, name)
				if err != nil {
					errs <- err
					return
				}
				notBefore := time.Unix(int64(entry.Value.Cert.Common.NotBefore.Seconds), 0)
				notAfter := time.Unix(int64(entry.Value.Cert.Common.NotAfter.Seconds), 0)
				err = newS.SaveKeyPair(entry.Manager, name, entry.Value.Cert.SerialNumber, ValidityInfo{NotBefore: notBefore, NotAfter: notAfter}, priv, certData)
				if err != nil {
					errs <- err
				}
			}(name, entry)
		}
	}

	for _, privMap := range old.privKeys {
		for name, entry := range privMap {
			wg.Add(1)
			go func(name string, entry *indexEntry[index.PrivateKeyIndex]) {
				defer wg.Done()
				priv, err := old.LoadPrivateKey(entry.Manager, name)
				if err != nil {
					errs <- err
					return
				}
				notBefore := time.Unix(int64(entry.Value.Common.NotBefore.Seconds), 0)
				notAfter := time.Unix(int64(entry.Value.Common.NotAfter.Seconds), 0)
				err = newS.SavePrivateKey(entry.Manager, name, ValidityInfo{NotBefore: notBefore, NotAfter: notAfter}, priv)
				if err != nil {
					errs <- err
				}
			}(name, entry)
		}
	}

	for _, blobMap := range old.blobs {
		for name, entry := range blobMap {
			wg.Add(1)
			go func(name string, entry *indexEntry[index.BlobIndex]) {
				defer wg.Done()
				blobData, err := old.LoadBlob(entry.Manager, name)
				if err != nil {
					errs <- err
					return
				}
				notBefore := time.Unix(int64(entry.Value.Common.NotBefore.Seconds), 0)
				notAfter := time.Unix(int64(entry.Value.Common.NotAfter.Seconds), 0)
				err = newS.SaveBlob(entry.Manager, name, ValidityInfo{NotBefore: notBefore, NotAfter: notAfter}, blobData)
				if err != nil {
					errs <- err
				}
			}(name, entry)
		}
	}

	go func() {
		wg.Wait()
		close(errs)
	}()

	var errList []error
	for err := range errs {
		errList = append(errList, err)
	}
	if len(errList) > 0 {
		return nil, fmt.Errorf("errors occurred during migration: %w", errors.Join(errList...))
	}
	return newStorage, nil
}

func isGarbageRecord(c *index.IndexCommonInfo, now time.Time) bool {
	if c.Revoked() {
		return true
	}
	return (&ValidityInfo{
		NotBefore: time.Unix(int64(c.NotBefore.Seconds), 0),
		NotAfter:  time.Unix(int64(c.NotAfter.Seconds), 0),
	}).Expired(now)
}

func loadIndex(certIndexFile *os.File, baseKey []byte, now time.Time) (CertMapPerManager, KeyPairMapPerManager, PrivateKeyMapPerManager, BlobMapPerManager, uint64, uint64, uint64, error) {
	var offset int64 = index.IndexFileHeaderSize
	certs := make(CertMapPerManager)
	keypairs := make(KeyPairMapPerManager)
	privKeys := make(PrivateKeyMapPerManager)
	blobs := make(BlobMapPerManager)
	maxID := uint64(0)
	recordCounter := uint64(0)
	commitCount := uint64(0)
	for {
		entryOffset := offset
		loaded, recordC, mgrID, recBuf, err := readHeaderAndDecrypt(certIndexFile, offset, baseKey)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, nil, nil, 0, 0, 0, err
		}
		commitCount++
		offset += int64(len(recBuf))
		if recordC > recordCounter {
			recordCounter = recordC
		}
		var ptr uint64
		if rec := loaded.Cert(); rec != nil {
			if isGarbageRecord(&rec.Common, now) {
				continue
			}
			ptr = rec.Pointer.Pointer
			name := string(rec.Name.Name)
			certs.Add(mgrID, name, &indexEntry[index.CertIndex]{
				FileOffset: entryOffset,
				Manager:    mgrID,
				Value:      *rec,
			})
		} else if kp := loaded.KeyPair(); kp != nil {
			if isGarbageRecord(&kp.Cert.Common, now) {
				continue
			}
			ptr = kp.Cert.Pointer.Pointer
			name := string(kp.Cert.Name.Name)
			keypairs.Add(mgrID, name, &indexEntry[index.KeyPairIndex]{
				FileOffset: entryOffset,
				Manager:    mgrID,
				Value:      *kp,
			})
		} else if pk := loaded.Priv(); pk != nil {
			if isGarbageRecord(&pk.Common, now) {
				continue
			}
			ptr = pk.Pointer.Pointer
			name := string(pk.Name.Name)
			privKeys.Add(mgrID, name, &indexEntry[index.PrivateKeyIndex]{
				FileOffset: entryOffset,
				Manager:    mgrID,
				Value:      *pk,
			})
		} else if blob := loaded.Blob(); blob != nil {
			if isGarbageRecord(&blob.Common, now) {
				continue
			}
			ptr = blob.Pointer.Pointer
			name := string(blob.Name.Name)
			blobs.Add(mgrID, name, &indexEntry[index.BlobIndex]{
				FileOffset: entryOffset,
				Manager:    mgrID,
				Value:      *blob,
			})
		} else {
			return nil, nil, nil, nil, 0, 0, 0, fmt.Errorf("unknown index kind")
		}
		if ptr >= maxID {
			maxID = ptr + 1
		}
	}
	return certs, keypairs, privKeys, blobs, maxID, recordCounter, commitCount, nil
}

func fileOffset(certIndex *os.File) (int64, error) {
	return certIndex.Seek(0, io.SeekEnd)
}

func decodeHeaderUnlocked(certIndex *os.File, storageSecret []byte) (uint64, error) {
	var headerBuf [index.IndexFileHeaderSize]byte
	_, err := certIndex.ReadAt(headerBuf[:], 0)
	if err != nil {
		return 0, err
	}
	fileOffset, err := certIndex.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	key := DeriveKey(uint64(fileOffset), fileHeaderEncryptionContext, storageSecret)
	var nonce [12]byte
	copy(nonce[:], headerBuf[:12])
	aesBlock, err := aes.NewCipher(key[:32])
	if err != nil {
		return 0, err
	}
	gcm, err := cipher.NewGCM(aesBlock)
	if err != nil {
		return 0, err
	}
	if gcm.Overhead() != 16 {
		return 0, fmt.Errorf("unexpected GCM overhead: %d", gcm.Overhead())
	}
	decrypted, err := gcm.Open(nil, nonce[:], headerBuf[12:], nil)
	if err != nil {
		return 0, err
	}
	recordCounter := binary.BigEndian.Uint64(decrypted)
	return recordCounter, nil
}

func rewriteHeaderUnlocked(certIndex *os.File, new_record_counter uint64, storageSecret []byte) error {
	currentFileSize, err := certIndex.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if currentFileSize < index.IndexFileHeaderSize {
		return fmt.Errorf("file too small to rewrite header")
	}
	key := DeriveKey(uint64(currentFileSize), fileHeaderEncryptionContext, storageSecret)
	var nonce [12]byte
	_, _ = io.ReadFull(rand.Reader, nonce[:])
	var headerBuf [index.IndexFileHeaderSize]byte
	aesBlock, err := aes.NewCipher(key[:32])
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(aesBlock)
	if err != nil {
		return err
	}
	if gcm.Overhead() != 16 {
		return fmt.Errorf("unexpected GCM overhead: %d", gcm.Overhead())
	}
	copy(headerBuf[:12], nonce[:12])
	binary.BigEndian.PutUint64(headerBuf[12:], new_record_counter)
	gcm.Seal(headerBuf[12:12], nonce[:], headerBuf[12:12+8], nil)
	_, err = certIndex.WriteAt(headerBuf[:], 0)
	if err != nil {
		return err
	}
	return certIndex.Sync()
}

func commitDataUnlocked(certIndex *os.File, data []byte, newCounter uint64, storageSecret []byte) (int64, error) {
	offset, err := certIndex.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	_, err = certIndex.Write(data)
	if err != nil {
		return 0, err
	}
	return offset, rewriteHeaderUnlocked(certIndex, newCounter, storageSecret)
}

func rewriteData(lock sync.Locker, certIndex *os.File, offset int64, data []byte, newCounter uint64, storageSecret []byte) error {
	lock.Lock()
	defer lock.Unlock()
	// check data size
	var dataLenBuf [2]byte
	r, err := certIndex.ReadAt(dataLenBuf[:], offset)
	if err != nil {
		return err
	}
	if r != 2 {
		return fmt.Errorf("failed to read data length")
	}
	dataLen := int64(binary.BigEndian.Uint16(dataLenBuf[:]))
	if dataLen != int64(len(data))-index.RecordHeaderSize {
		return fmt.Errorf("data size mismatch: %d vs %d", dataLen, len(data)-index.RecordHeaderSize)
	}
	_, err = certIndex.WriteAt(data, offset)
	if err != nil {
		return err
	}
	return rewriteHeaderUnlocked(certIndex, newCounter, storageSecret)
}

func commitRecord(locker sync.Locker, file *os.File, idx encoder, kind index.IndexKind, mgrID index.ManagerID, record_counter uint64, baseKey []byte) (_ int64, err error) {
	locker.Lock()
	defer locker.Unlock()
	offset, err := fileOffset(file)
	if err != nil {
		return 0, err
	}
	encoded, err := packRecord(idx, kind, baseKey, mgrID, uint64(offset), record_counter)
	if err != nil {
		return 0, err
	}
	return commitDataUnlocked(file, encoded, record_counter, baseKey)
}

func rewriteRecord(locker sync.Locker, file *os.File, offset int64, idx encoder, kind index.IndexKind, mgrID index.ManagerID, record_counter uint64, baseKey []byte) error {
	encoded, err := packRecord(idx, kind, baseKey, mgrID, uint64(offset), record_counter)
	if err != nil {
		return err
	}
	return rewriteData(locker, file, offset, encoded, record_counter, baseKey)
}

func (c *dirStorage) commitIndex(idx encoder, kind index.IndexKind, mgrID index.ManagerID) (int64, error) {
	c.recordCounter++ // first state is 0, so increment before use
	counter := c.recordCounter
	return commitRecord(c.certFileLock, c.certIndex, idx, kind, mgrID, counter, c.storageMasterKey)
}

func (c *dirStorage) rewriteIndex(offset int64, idx encoder, kind index.IndexKind, mgrID index.ManagerID) error {
	c.recordCounter++
	counter := c.recordCounter
	return rewriteRecord(c.certFileLock, c.certIndex, offset, idx, kind, mgrID, counter, c.storageMasterKey)
}

func fileName(fileNumber uint64) string {
	return fmt.Sprintf("file_%d", fileNumber)
}

func fileEncryptionForManagerContext(mgrID index.ManagerID, kind index.IndexKind) string {
	return string(mgrID.Id[:]) + fileEncryptionContext + kind.String()
}

func (c *dirStorage) saveContent(fileNumber uint64, index encoder, kind index.IndexKind, mgrID index.ManagerID, data []byte) (int64, error) {
	context := fileEncryptionForManagerContext(mgrID, kind)
	fileEncKey := DeriveKey(fileNumber, context, c.storageMasterKey)
	data, err := EncryptAESGCM(data, fileEncKey[:32])
	if err != nil {
		return 0, err
	}
	certFileName := fileName(fileNumber)
	file, err := c.dir.OpenFile(certFileName, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	entryOffset, err := c.commitIndex(index, kind, mgrID)
	if err != nil {
		return 0, err
	}
	_, err = file.Write(data)
	if err != nil {
		return 0, err
	}
	syncErr := file.Sync()
	if syncErr != nil {
		return 0, syncErr
	}
	return entryOffset, nil
}

func (c *dirStorage) updateContent(fileNumber uint64, offset int64, index encoder, kind index.IndexKind, mgrID index.ManagerID, data []byte, update func()) error {
	context := fileEncryptionForManagerContext(mgrID, kind)
	fileEncKey := DeriveKey(fileNumber, context, c.storageMasterKey)
	data, err := EncryptAESGCM(data, fileEncKey[:32])
	if err != nil {
		return err
	}
	certFileName := fileName(fileNumber)
	file, err := c.dir.OpenFile(certFileName, os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	err = c.rewriteIndex(offset, index, kind, mgrID)
	if err != nil {
		return err
	}
	update()
	_, err = file.Write(data)
	if err != nil {
		return err
	}
	return file.Sync()
}

func (c *dirStorage) loadContent(fileNumber uint64, mgrID index.ManagerID, kind index.IndexKind) ([]byte, error) {
	certFileName := fileName(fileNumber)
	data, err := c.dir.ReadFile(certFileName)
	if err != nil {
		return nil, err
	}
	fileEncKey := DeriveKey(fileNumber, fileEncryptionForManagerContext(mgrID, kind), c.storageMasterKey)
	data, err = DecryptAESGCM(data, fileEncKey[:32])
	if err != nil {
		return nil, err
	}
	return data, nil
}

type dummyLocker struct{}

func (d *dummyLocker) Lock()   {}
func (d *dummyLocker) Unlock() {}

func save[T any](c *dirStorage, locker sync.Locker, mgrMaps MapPerManager[T], kind index.IndexKind, id ManagerID, name string, valid ValidityInfo, data []byte, generate func(now time.Time, fileNumber uint64, hashKey []byte) (*T, error)) error {
	if !valid.Consistent() {
		return fmt.Errorf("invalid validity period")
	}
	locker.Lock()
	defer locker.Unlock()
	if _, exists := mgrMaps.Get(id, name); exists {
		return fmt.Errorf("%w: certificate with name %s already exists", ErrExist, name)
	}
	now := c.now()
	fileNumber := c.fileCounter
	c.fileCounter++
	hashkey := DeriveKey(fileNumber, certHashContext, c.storageMasterKey)
	indexInfo, err := generate(now, fileNumber, hashkey)
	if err != nil {
		return err
	}
	var x any = indexInfo
	entryOffset, err := c.saveContent(fileNumber, x.(encoder), kind, id, data)
	if err != nil {
		return err
	}
	mgrMaps.Add(id, name, &indexEntry[T]{
		FileOffset: entryOffset,
		Manager:    id,
		Value:      *indexInfo,
	})
	return nil
}

func update[T any](c *dirStorage, locker sync.Locker, mgrMaps MapPerManager[T], kind index.IndexKind, id ManagerID, name string, valid ValidityInfo, data []byte, getPointer func(entry *indexEntry[T]) uint64, generate func(now time.Time, fileNumber uint64, hashKey []byte) (*T, error)) error {
	if !valid.Consistent() {
		return fmt.Errorf("invalid validity period")
	}
	locker.Lock()
	defer locker.Unlock()
	entry, exists := mgrMaps.Get(id, name)
	if !exists {
		return fmt.Errorf("%w: certificate with name %s does not exist", ErrNotExist, name)
	}
	ptr := getPointer(entry)
	now := c.now()
	hashkey := DeriveKey(ptr, certHashContext, c.storageMasterKey)
	certIndex, err := generate(now, ptr, hashkey)
	if err != nil {
		return err
	}
	var x any = certIndex
	return c.updateContent(ptr, entry.FileOffset, x.(encoder), kind, id, data, func() {
		entry.Value = *certIndex
	})
}

func deleteInternal[T any](c *dirStorage, mgrMaps MapPerManager[T], kind index.IndexKind, id ManagerID, name string, entry *indexEntry[T], getPointerAndRevoke func(entry *indexEntry[T]) uint64) error {
	ptr := getPointerAndRevoke(entry)
	certFileName := fmt.Sprintf("file_%d", ptr)
	var x any = &entry.Value
	err := c.rewriteIndex(entry.FileOffset, x.(encoder), kind, id)
	if err != nil {
		return err
	}
	c.dir.Remove(certFileName)
	mgrMaps.Delete(id, name)
	return nil
}

func deleteLock[T any](c *dirStorage, locker sync.Locker, mgrMaps MapPerManager[T], kind index.IndexKind, id ManagerID, name string, getPointerAndRevoke func(entry *indexEntry[T]) uint64) error {
	locker.Lock()
	defer locker.Unlock()
	entry, exists := mgrMaps.Get(id, name)
	if !exists {
		return fmt.Errorf("%w: certificate with name %s does not exist", ErrNotExist, name)
	}
	return deleteInternal(c, mgrMaps, kind, id, name, entry, getPointerAndRevoke)
}

func load[T any](c *dirStorage, locker *sync.RWMutex, mgrMaps MapPerManager[T], id ManagerID, name string, kind index.IndexKind, getPointer func(entry *indexEntry[T]) uint64, validateHash func(entry *indexEntry[T], expectedHash []byte, data []byte) error) ([]byte, error) {
	locker.RLock()
	defer locker.RUnlock()
	entry, exists := mgrMaps.Get(id, name)
	if !exists {
		return nil, fmt.Errorf("%w: certificate with name %s does not exist", ErrNotExist, name)
	}
	ptr := getPointer(entry)
	data, err := c.loadContent(ptr, id, kind)
	if err != nil {
		return nil, err
	}
	derivedKey := DeriveKey(ptr, certHashContext, c.storageMasterKey)
	expectedHash := KeyHash(derivedKey, data)
	if err := validateHash(entry, expectedHash[:], data); err != nil {
		return nil, err
	}
	return data, nil
}

func (c *dirStorage) SaveCertificate(id ManagerID, name string, serial SerialNumber, valid ValidityInfo, data []byte) error {
	return save(c, &c.certMapLock, c.certs, index.IndexKind_Cert, id, name, valid, data, func(now time.Time, fileNumber uint64, hashKey []byte) (*index.CertIndex, error) {
		return CertInformation(name, data, serial, now, valid, fileNumber, hashKey)
	})
}

func (c *dirStorage) UpdateCertificate(id ManagerID, name string, serial SerialNumber, validity ValidityInfo, data []byte) error {
	return update(c, &c.certMapLock, c.certs, index.IndexKind_Cert, id, name, validity, data, func(entry *indexEntry[index.CertIndex]) uint64 {
		return entry.Value.Pointer.Pointer
	}, func(now time.Time, fileNumber uint64, hashKey []byte) (*index.CertIndex, error) {
		return CertInformation(name, data, serial, now, validity, fileNumber, hashKey)
	})
}

func (c *dirStorage) DeleteCertificate(id ManagerID, name string) error {
	return deleteLock(c, &c.certMapLock, c.certs, index.IndexKind_Cert, id, name, func(entry *indexEntry[index.CertIndex]) uint64 {
		entry.Value.Common.SetRevoked(true)
		return entry.Value.Pointer.Pointer
	})
}

func (c *dirStorage) deleteCertificate(id ManagerID, name string, entry *indexEntry[index.CertIndex]) error {
	return deleteInternal(c, c.certs, index.IndexKind_Cert, id, name, entry, func(entry *indexEntry[index.CertIndex]) uint64 {
		entry.Value.Common.SetRevoked(true)
		return entry.Value.Pointer.Pointer
	})
}

func (c *dirStorage) LoadCertificate(id ManagerID, name string) ([]byte, error) {
	return load(c, &c.certMapLock, c.certs, id, name, index.IndexKind_Cert, func(entry *indexEntry[index.CertIndex]) uint64 {
		return entry.Value.Pointer.Pointer
	}, func(entry *indexEntry[index.CertIndex], expectedHash []byte, _ []byte) error {
		if subtle.ConstantTimeCompare(expectedHash[:], entry.Value.CertificateHash.Hash[:]) != 1 {
			return fmt.Errorf("certificate hash does not match")
		}
		return nil
	})
}

func (c *dirStorage) SaveKeyPair(id ManagerID, name string, serial SerialNumber, valid ValidityInfo, privData, certData []byte) error {
	return save(c, &c.certMapLock, c.keypairs, index.IndexKind_Keypair, id, name, valid, append(privData, certData...), func(now time.Time, fileNumber uint64, hashKey []byte) (*index.KeyPairIndex, error) {
		return KeyPairInformation(name, now, valid, serial, privData, certData, fileNumber, fileNumber, hashKey)
	})
}

func (c *dirStorage) UpdateKeyPair(id ManagerID, name string, serial SerialNumber, validity ValidityInfo, privData, certData []byte) error {
	/*
		if !validity.Consistent() {
			return fmt.Errorf("invalid validity period")
		}
		c.certMapLock.Lock()
		defer c.certMapLock.Unlock()
		entry, exists := c.keypairs.Get(id, name)
		if !exists {
			return fmt.Errorf("%w: key pair with name %s does not exist", ErrNotExist, name)
		}
		ptr := entry.Value.Cert.Pointer.Pointer
		now := c.now()
		hashkey := DeriveKey(ptr, certHashContext, c.storageMasterKey)
		keyPairIndex, err := KeyPairInformation(name, now, validity, serial, privData, certData, ptr, ptr, hashkey)
		if err != nil {
			return err
		}
		return c.updateContent(ptr, entry.FileOffset, keyPairIndex, index.IndexKind_Keypair, id, append(privData, certData...), func() {
			entry.Value = *keyPairIndex
		})
	*/
	return update(c, &c.certMapLock, c.keypairs, index.IndexKind_Keypair, id, name, validity, append(privData, certData...), func(entry *indexEntry[index.KeyPairIndex]) uint64 {
		return entry.Value.Cert.Pointer.Pointer
	}, func(now time.Time, fileNumber uint64, hashKey []byte) (*index.KeyPairIndex, error) {
		return KeyPairInformation(name, now, validity, serial, privData, certData, fileNumber, fileNumber, hashKey)
	})
}

func (c *dirStorage) DeleteKeyPair(id ManagerID, name string) error {
	/*
		c.certMapLock.Lock()
		defer c.certMapLock.Unlock()
		entry, exists := c.keypairs.Get(id, name)
		if !exists {
			return fmt.Errorf("%w: key pair with name %s does not exist", ErrNotExist, name)
		}
		return c.deleteKeyPair(id, name, entry)
	*/
	return deleteLock(c, &c.certMapLock, c.keypairs, index.IndexKind_Keypair, id, name, func(entry *indexEntry[index.KeyPairIndex]) uint64 {
		entry.Value.Cert.Common.SetRevoked(true)
		return entry.Value.Cert.Pointer.Pointer
	})
}

func (c *dirStorage) deleteKeyPair(mgrID ManagerID, name string, entry *indexEntry[index.KeyPairIndex]) error {
	/*
		ptr := entry.Value.Cert.Pointer.Pointer
		certFileName := fmt.Sprintf("file_%d", ptr)
		entry.Value.Cert.Common.SetRevoked(true)
		err := c.rewriteIndex(entry.FileOffset, &entry.Value, index.IndexKind_Keypair, mgrID)
		if err != nil {
			return err
		}
		c.dir.Remove(certFileName)
		c.keypairs.Delete(mgrID, name)
		return nil
	*/
	return deleteInternal(c, c.keypairs, index.IndexKind_Keypair, mgrID, name, entry, func(entry *indexEntry[index.KeyPairIndex]) uint64 {
		entry.Value.Cert.Common.SetRevoked(true)
		return entry.Value.Cert.Pointer.Pointer
	})
}

func (c *dirStorage) LoadKeyPair(id ManagerID, name string) ([]byte, []byte, error) {
	/*
		c.certMapLock.RLock()
		defer c.certMapLock.RUnlock()
		entry, exists := c.keypairs.Get(id, name)
		if !exists {
			return nil, nil, fmt.Errorf("%w: key pair with name %s does not exist", ErrNotExist, name)
		}
		ptr := entry.Value.Cert.Pointer.Pointer
		data, err := c.loadContent(ptr, id)
		if err != nil {
			return nil, nil, err
		}
		derivedKey := DeriveKey(ptr, certHashContext, c.storageMasterKey)
		expectedHash := KeyHash(derivedKey, data)
		if subtle.ConstantTimeCompare(expectedHash[:], entry.Value.Cert.CertificateHash.Hash[:]) != 1 {
			return nil, nil, fmt.Errorf("key pair hash does not match")
		}
		privLen := entry.Value.PrivLen
		pubLen := entry.Value.Cert.CertLen
		if uint64(privLen+pubLen) != uint64(len(data)) {
			return nil, nil, fmt.Errorf("key pair data length mismatch")
		}
		if int(privLen) > len(data) {
			return nil, nil, fmt.Errorf("invalid private key length")
		}
		return data[:privLen], data[privLen:], nil
	*/
	peivLen := 0
	data, err := load(c, &c.certMapLock, c.keypairs, id, name, index.IndexKind_Keypair, func(entry *indexEntry[index.KeyPairIndex]) uint64 {
		return entry.Value.Cert.Pointer.Pointer
	}, func(entry *indexEntry[index.KeyPairIndex], expectedHash []byte, data []byte) error {
		if subtle.ConstantTimeCompare(expectedHash[:], entry.Value.Cert.CertificateHash.Hash[:]) != 1 {
			return fmt.Errorf("key pair hash does not match")
		}
		if uint64(entry.Value.PrivLen+entry.Value.Cert.CertLen) != uint64(len(data)) {
			return fmt.Errorf("key pair data length mismatch")
		}
		if int(entry.Value.PrivLen) > len(data) {
			return fmt.Errorf("invalid private key length")
		}
		peivLen = int(entry.Value.PrivLen)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return data[:peivLen], data[peivLen:], nil
}

func (c *dirStorage) SavePrivateKey(id ManagerID, name string, validity ValidityInfo, data []byte) error {
	return save(c, &c.certMapLock, c.privKeys, index.IndexKind_Priv, id, name, validity, data, func(now time.Time, fileNumber uint64, hashKey []byte) (*index.PrivateKeyIndex, error) {
		return PrivateKeyInformation(name, data, fileNumber, now, validity, hashKey)
	})
}

func (c *dirStorage) UpdatePrivateKey(id ManagerID, name string, validity ValidityInfo, data []byte) error {
	return update(c, &c.certMapLock, c.privKeys, index.IndexKind_Priv, id, name, validity, data, func(entry *indexEntry[index.PrivateKeyIndex]) uint64 {
		return entry.Value.Pointer.Pointer
	}, func(now time.Time, fileNumber uint64, hashKey []byte) (*index.PrivateKeyIndex, error) {
		return PrivateKeyInformation(name, data, fileNumber, now, validity, hashKey)
	})
}

func (c *dirStorage) LoadPrivateKey(id ManagerID, name string) ([]byte, error) {
	return load(c, &c.certMapLock, c.privKeys, id, name, index.IndexKind_Priv, func(entry *indexEntry[index.PrivateKeyIndex]) uint64 {
		return entry.Value.Pointer.Pointer
	}, func(entry *indexEntry[index.PrivateKeyIndex], expectedHash []byte, data []byte) error {
		if subtle.ConstantTimeCompare(expectedHash[:], entry.Value.PrivateKeyHash.Hash[:]) != 1 {
			return fmt.Errorf("private key hash does not match")
		}
		return nil
	})
}

func (c *dirStorage) DeletePrivateKey(id ManagerID, name string) error {
	return deleteLock(c, &c.certMapLock, c.privKeys, index.IndexKind_Priv, id, name, func(entry *indexEntry[index.PrivateKeyIndex]) uint64 {
		entry.Value.Common.SetRevoked(true)
		return entry.Value.Pointer.Pointer
	})
}

func (c *dirStorage) deletePrivateKey(mgrID ManagerID, name string, entry *indexEntry[index.PrivateKeyIndex]) error {
	return deleteInternal(c, c.privKeys, index.IndexKind_Priv, mgrID, name, entry, func(entry *indexEntry[index.PrivateKeyIndex]) uint64 {
		entry.Value.Common.SetRevoked(true)
		return entry.Value.Pointer.Pointer
	})
}

func (c *dirStorage) SaveBlob(id ManagerID, name string, valid ValidityInfo, data []byte) error {
	return save(c, &c.certMapLock, c.blobs, index.IndexKind_Blob, id, name, valid, data, func(now time.Time, fileNumber uint64, hashKey []byte) (*index.BlobIndex, error) {
		return BlobInformation(name, data, fileNumber, now, valid, hashKey)
	})
}

func (c *dirStorage) UpdateBlob(id ManagerID, name string, validity ValidityInfo, data []byte) error {
	return update(c, &c.certMapLock, c.blobs, index.IndexKind_Blob, id, name, validity, data, func(entry *indexEntry[index.BlobIndex]) uint64 {
		return entry.Value.Pointer.Pointer
	}, func(now time.Time, fileNumber uint64, hashKey []byte) (*index.BlobIndex, error) {
		return BlobInformation(name, data, fileNumber, now, validity, hashKey)
	})
}

func (c *dirStorage) LoadBlob(id ManagerID, name string) ([]byte, error) {
	return load(c, &c.certMapLock, c.blobs, id, name, index.IndexKind_Blob, func(entry *indexEntry[index.BlobIndex]) uint64 {
		return entry.Value.Pointer.Pointer
	}, func(entry *indexEntry[index.BlobIndex], expectedHash []byte, data []byte) error {
		if subtle.ConstantTimeCompare(expectedHash[:], entry.Value.BlobHash.Hash[:]) != 1 {
			return fmt.Errorf("blob hash does not match")
		}
		return nil
	})
}

func (c *dirStorage) DeleteBlob(id ManagerID, name string) error {
	return deleteLock(c, &c.certMapLock, c.blobs, index.IndexKind_Blob, id, name, func(entry *indexEntry[index.BlobIndex]) uint64 {
		entry.Value.Common.SetRevoked(true)
		return entry.Value.Pointer.Pointer
	})
}

func (c *dirStorage) GetBlobRawIndex(id ManagerID, name string) (*index.BlobIndex, error) {
	c.certMapLock.RLock()
	defer c.certMapLock.RUnlock()
	entry, exists := c.blobs.Get(id, name)
	if !exists {
		return nil, fmt.Errorf("%w: blob with name %s does not exist", ErrNotExist, name)
	}
	copy := entry.Value
	return &copy, nil
}

func (c *dirStorage) GetPrivateKeyRawIndex(id ManagerID, name string) (*index.PrivateKeyIndex, error) {
	c.certMapLock.RLock()
	defer c.certMapLock.RUnlock()
	entry, exists := c.privKeys.Get(id, name)
	if !exists {
		return nil, fmt.Errorf("%w: private key with name %s does not exist", ErrNotExist, name)
	}
	copy := entry.Value
	return &copy, nil
}

func (c *dirStorage) GetCertificateRawIndex(id ManagerID, name string) (*index.CertIndex, error) {
	c.certMapLock.RLock()
	defer c.certMapLock.RUnlock()
	entry, exists := c.certs.Get(id, name)
	if !exists {
		return nil, fmt.Errorf("%w: certificate with name %s does not exist", ErrNotExist, name)
	}
	copy := entry.Value
	return &copy, nil
}

func (c *dirStorage) GetKeyPairRawIndex(id ManagerID, name string) (*index.KeyPairIndex, error) {
	c.certMapLock.RLock()
	defer c.certMapLock.RUnlock()
	entry, exists := c.keypairs.Get(id, name)
	if !exists {
		return nil, fmt.Errorf("%w: key pair with name %s does not exist", ErrNotExist, name)
	}
	copy := entry.Value
	return &copy, nil
}

func (c *dirStorage) GetCertificateCount(mgrID ManagerID) int {
	c.certMapLock.RLock()
	defer c.certMapLock.RUnlock()
	return int(c.certs.GetCount(mgrID))
}

func (c *dirStorage) GetKeyPairCount(mgrID ManagerID) int {
	c.certMapLock.RLock()
	defer c.certMapLock.RUnlock()
	return int(c.keypairs.GetCount(mgrID))
}

func (c *dirStorage) GetPrivateKeyCount(mgrID ManagerID) int {
	c.certMapLock.RLock()
	defer c.certMapLock.RUnlock()
	return int(c.privKeys.GetCount(mgrID))
}

func (c *dirStorage) GetBlobCount(mgrID ManagerID) int {
	c.certMapLock.RLock()
	defer c.certMapLock.RUnlock()
	return int(c.blobs.GetCount(mgrID))
}

func (c *dirStorage) ListCertificates(mgrID ManagerID) []index.CertIndex {
	c.certMapLock.RLock()
	defer c.certMapLock.RUnlock()
	return c.certs.List(mgrID)
}

func (c *dirStorage) ListKeyPairs(mgrID ManagerID) []index.KeyPairIndex {
	c.certMapLock.RLock()
	defer c.certMapLock.RUnlock()
	return c.keypairs.List(mgrID)
}

func (c *dirStorage) ListPrivateKeys(mgrID ManagerID) []index.PrivateKeyIndex {
	c.certMapLock.RLock()
	defer c.certMapLock.RUnlock()
	return c.privKeys.List(mgrID)
}

func (c *dirStorage) ListBlobs(mgrID ManagerID) []index.BlobIndex {
	c.certMapLock.RLock()
	defer c.certMapLock.RUnlock()
	return c.blobs.List(mgrID)
}

func (c *dirStorage) DeleteExpired(at time.Time) error {
	c.certMapLock.Lock()
	defer c.certMapLock.Unlock()
	var errs []error
	for mgrID, certs := range c.certs {
		for name, entry := range certs {
			validity := &ValidityInfo{
				NotBefore: time.Unix(int64(entry.Value.Common.NotBefore.Seconds), 0),
				NotAfter:  time.Unix(int64(entry.Value.Common.NotAfter.Seconds), 0),
			}
			if validity.Expired(at) {
				err := c.deleteCertificate(mgrID, name, entry)
				if err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	for mgrID, keypairs := range c.keypairs {
		for name, entry := range keypairs {
			validity := &ValidityInfo{
				NotBefore: time.Unix(int64(entry.Value.Cert.Common.NotBefore.Seconds), 0),
				NotAfter:  time.Unix(int64(entry.Value.Cert.Common.NotAfter.Seconds), 0),
			}
			if validity.Expired(at) {
				err := c.deleteKeyPair(mgrID, name, entry)
				if err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	for mgrID, privKeys := range c.privKeys {
		for name, entry := range privKeys {
			validity := &ValidityInfo{
				NotBefore: time.Unix(int64(entry.Value.Common.NotBefore.Seconds), 0),
				NotAfter:  time.Unix(int64(entry.Value.Common.NotAfter.Seconds), 0),
			}
			if validity.Expired(at) {
				err := c.deletePrivateKey(mgrID, name, entry)
				if err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	for mgrID, blobs := range c.blobs {
		for name, entry := range blobs {
			validity := &ValidityInfo{
				NotBefore: time.Unix(int64(entry.Value.Common.NotBefore.Seconds), 0),
				NotAfter:  time.Unix(int64(entry.Value.Common.NotAfter.Seconds), 0),
			}
			if validity.Expired(at) {
				// blobs do not have a dedicated delete function
				c.blobs.Delete(mgrID, name)
			}
		}
	}
	return errors.Join(errs...)
}
