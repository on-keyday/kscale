package ca

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/on-keyday/kscale/ca/storage"
	"github.com/on-keyday/kscale/ca/storage/index"
	pbca "github.com/on-keyday/kscale/protobuf/proto/ca"
	"github.com/on-keyday/objtrsf/objproto"
)

type bootstrapInfo struct {
	token            []byte
	exp              time.Time
	certExp          time.Duration
	usage            string
	commonNameSuffix string
}

type CA struct {
	privateKey crypto.Signer
	caCert     *x509.Certificate

	storage   storage.CertificateStorage
	caLock    sync.Mutex
	serialNum *big.Int

	caStatePath   string
	caStateSecret []byte
	mgrID         storage.ManagerID

	bootstrapLock sync.Mutex
	bootstrapInfo map[string]bootstrapInfo

	// RenewCertExp resolves the validity period a RENEWED certificate gets, from the app
	// (usage) and CommonName of the one being renewed. Returning 0 — or leaving this nil —
	// carries the existing certificate's period forward, which is what renewal always did.
	// Set it after construction; the CA itself holds no policy about who gets how long, so
	// the control plane injects the decision (cmd/controlplane wires it to its cert-exp
	// flags). See updateHandshake.
	RenewCertExp func(app, commonName string) time.Duration
}

func (ca *CA) GetSuffixCommonNames(suffix string) ([]string, error) {
	ca.caLock.Lock()
	defer ca.caLock.Unlock()
	if ca.storage == nil { // on migration, storage can be nil
		return nil, ErrOnMigration
	}
	certIndex := ca.storage.ListCertificates(ca.mgrID)
	certNames := make([]string, 0)
	now := time.Now()
	for _, idx := range certIndex {
		name := string(idx.Name.Name)
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		// Skip expired certs. The sole caller is the root-issuer's admin-existence gate
		// (cmd/controlplane runRootIssuer): an expired admin cert can no longer complete an
		// mTLS handshake, so counting it as "an admin exists" wedges the root-issuer — it
		// never re-issues a first-admin token, and there is no admin to mint one manually
		// (the short-lived admin-cert-exp deadlock). Treating an expired cert as absent lets
		// the root-issuer self-heal by re-issuing a fresh first-admin token. Revoked certs
		// are already excluded (RevokeCertificate deletes them from storage).
		if time.Unix(int64(idx.Common.NotAfter.Seconds), 0).Before(now) {
			continue
		}
		certNames = append(certNames, name)
	}
	return certNames, nil
}

func (ca *CA) GetCertificateCount() int {
	return ca.storage.GetCertificateCount(ca.mgrID)
}

func (ca *CA) GetBootstrapTokenCount() int {
	ca.bootstrapLock.Lock()
	defer ca.bootstrapLock.Unlock()
	return len(ca.bootstrapInfo)
}

type CertificateInfo struct {
	CommonName   string
	SerialNumber string
	IssuedAt     time.Time
	ExpiresAt    time.Time
}

func (ca *CA) ListCertificates() []CertificateInfo {
	ca.caLock.Lock()
	defer ca.caLock.Unlock()
	certInfos := make([]CertificateInfo, 0)
	for _, cert := range ca.storage.ListCertificates(ca.mgrID) {
		certInfos = append(certInfos, CertificateInfo{
			CommonName:   string(cert.Name.Name),
			SerialNumber: big.NewInt(0).SetBytes(cert.SerialNumber.Serial[:]).String(),
			IssuedAt:     time.Unix(int64(cert.Common.NotBefore.Seconds), 0),
			ExpiresAt:    time.Unix(int64(cert.Common.NotAfter.Seconds), 0),
		})
	}
	return certInfos
}

func (ca *CA) ListCommonNames() ([]string, error) {
	ca.caLock.Lock()
	defer ca.caLock.Unlock()
	if ca.storage == nil { // on migration, storage can be nil
		return nil, ErrOnMigration
	}
	certIndex := ca.storage.ListCertificates(ca.mgrID)
	certNames := make([]string, 0, len(certIndex))
	for _, idx := range certIndex {
		certNames = append(certNames, string(idx.Name.Name))
	}
	return certNames, nil
}

func (ca *CA) DumpCAState() ([]byte, error) {
	ca.caLock.Lock()
	defer ca.caLock.Unlock()
	return ca.dumpCAState()
}

func (ca *CA) dumpCAState() ([]byte, error) {
	s := &index.CAState{}
	s.SavedTime = uint64(time.Now().Unix())
	ca.serialNum.FillBytes(s.CurrentSerial[:])
	rawIndex, err := ca.storage.GetKeyPairRawIndex(ca.mgrID, ca.caCert.Subject.CommonName)
	if err != nil {
		return nil, fmt.Errorf("failed to get CA key pair index: %w", err)
	}
	s.Ca = *rawIndex
	s.ManagerID = ca.mgrID
	s.RecordCounter = ca.storage.GetRecordCounter()
	return s.Encode()
}

func (ca *CA) saveCAState() error {
	data, err := ca.dumpCAState()
	if err != nil {
		return fmt.Errorf("failed to dump CA state: %w", err)
	}
	err = storage.SaveAESEncryptedDataToFile(ca.caStatePath, data, ca.caStateSecret)
	if err != nil {
		return fmt.Errorf("failed to save CA state to file: %w", err)
	}
	return nil
}

func NewFromCAState(data []byte, c storage.CertificateStorage, caStatePath string, caStateSecret []byte) (*CA, error) {
	ca := &CA{
		bootstrapInfo: make(map[string]bootstrapInfo),
		storage:       c,
		caStatePath:   caStatePath,
		caStateSecret: caStateSecret,
	}
	s := &index.CAState{}
	err := s.DecodeExact(data)
	if err != nil {
		return nil, fmt.Errorf("failed to decode CA state: %w", err)
	}
	serial := new(big.Int).SetBytes(s.CurrentSerial[:])
	if ca.storage.GetRecordCounter() != s.RecordCounter {
		return nil, fmt.Errorf("storage record counter is not equal to CA state record counter, %d != %d", ca.storage.GetRecordCounter(), s.RecordCounter)
	}
	rawIndex, err := ca.storage.GetKeyPairRawIndex(s.ManagerID, string(s.Ca.Cert.Name.Name))
	if err != nil {
		return nil, fmt.Errorf("failed to get CA key pair index: %w", err)
	}
	if rawIndex.Cert.Pointer.Pointer != s.Ca.Cert.Pointer.Pointer ||
		subtle.ConstantTimeCompare(
			rawIndex.Cert.CertificateHash.Hash[:],
			s.Ca.Cert.CertificateHash.Hash[:]) != 1 {
		return nil, fmt.Errorf("CA key pair index pointer mismatch")
	}
	priv, cert, err := ca.storage.LoadKeyPair(s.ManagerID, string(s.Ca.Cert.Name.Name))
	if err != nil {
		return nil, fmt.Errorf("failed to load CA key pair: %w", err)
	}
	parsedCert, err := x509.ParseCertificate(cert)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CA certificate: %w", err)
	}
	privKey, err := x509.ParsePKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CA private key: %w", err)
	}
	signer, ok := privKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("CA private key is not a crypto.Signer")
	}
	ca.privateKey = signer
	ca.caCert = parsedCert
	ca.serialNum = serial
	ca.mgrID = s.ManagerID
	return ca, nil
}

func NewSelfSignedCA(privateKey crypto.Signer, commonName string, certStorage storage.CertificateStorage, caStatePath string, caStateSecret []byte) (*CA, error) {
	if privateKey == nil {
		return nil, fmt.Errorf("private key must not be nil")
	}
	if commonName == "" {
		return nil, fmt.Errorf("common name must not be empty")
	}
	if certStorage == nil {
		return nil, fmt.Errorf("certificate storage must not be nil")
	}
	if caStatePath == "" {
		return nil, fmt.Errorf("CA state path must not be empty")
	}
	if len(caStateSecret) == 0 {
		return nil, fmt.Errorf("CA state secret must not be nil")
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		MaxPathLenZero:        true,
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(nil, template, template, privateKey.Public(), privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create self-signed CA certificate: %w", err)
	}
	parsed, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("failed to parse self-signed CA certificate: %w", err)
	}
	mgrID, err := storage.MakeRandomManagerID()
	if err != nil {
		return nil, fmt.Errorf("failed to generate manager ID: %w", err)
	}
	ca := &CA{
		privateKey:    privateKey,
		caCert:        parsed,
		serialNum:     big.NewInt(2),
		bootstrapInfo: make(map[string]bootstrapInfo),
		storage:       certStorage,
		caStatePath:   caStatePath,
		caStateSecret: caStateSecret,
		mgrID:         mgrID,
	}
	// save CA key pair
	privKey, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal CA private key: %w", err)
	}
	err = certStorage.SaveKeyPair(ca.mgrID, commonName, storage.MustNewSerialNumber(1), storage.ValidityInfo{
		NotBefore: template.NotBefore,
		NotAfter:  template.NotAfter,
	}, privKey, certDER)
	if err != nil {
		return nil, fmt.Errorf("failed to save CA key pair: %w", err)
	}
	err = ca.saveCAState()
	if err != nil {
		return nil, fmt.Errorf("failed to save CA state: %w", err)
	}
	return ca, nil
}

func (ca *CA) CommonName() string {
	return ca.caCert.Subject.CommonName
}

func (ca *CA) CACertificate() []byte {
	return ca.caCert.Raw
}

var ErrOnMigration = fmt.Errorf("certificate storage is on migration")

func (ca *CA) RevokeBootstrapToken(token []byte) error {
	ca.bootstrapLock.Lock()
	delete(ca.bootstrapInfo, string(token))
	ca.bootstrapLock.Unlock()
	return nil
}

func (ca *CA) RevokeCertificate(commonName string) error {
	ca.caLock.Lock()
	defer ca.caLock.Unlock()
	if ca.storage == nil { // on migration, storage can be nil
		return ErrOnMigration
	}
	err := ca.storage.DeleteCertificate(ca.mgrID, commonName)
	if err != nil {
		return fmt.Errorf("failed to revoke certificate: %w", err)
	}
	return ca.saveCAState()
}

// for saving let's encrypt server certificates
func (ca *CA) SaveOrUpdateKeyPair(mgrID storage.ManagerID, serialNumber index.SerialNumber, commonName string, validityInfo storage.ValidityInfo, privKey crypto.Signer, certDER []byte) error {
	privBytes, err := x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		return fmt.Errorf("failed to marshal private key: %w", err)
	}
	ca.caLock.Lock()
	defer ca.caLock.Unlock()
	if ca.storage == nil { // on migration, storage can be nil
		return ErrOnMigration
	}
	err = ca.storage.SaveKeyPair(mgrID, commonName, serialNumber, validityInfo, privBytes, certDER)
	if err != nil {
		if errors.Is(err, storage.ErrExist) {
			err = ca.storage.UpdateKeyPair(mgrID, commonName, serialNumber, validityInfo, privBytes, certDER)
			if err != nil {
				return fmt.Errorf("failed to update key pair: %w", err)
			}
		}
		return fmt.Errorf("failed to save key pair: %w", err)
	}
	return ca.saveCAState()
}

// for let's encrypt account keys
func (ca *CA) SavePrivateKey(mgrID storage.ManagerID, commonName string, validity storage.ValidityInfo, privKey crypto.PrivateKey) error {
	privBytes, err := x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		return fmt.Errorf("failed to marshal private key: %w", err)
	}
	ca.caLock.Lock()
	defer ca.caLock.Unlock()
	if ca.storage == nil { // on migration, storage can be nil
		return ErrOnMigration
	}
	err = ca.storage.SavePrivateKey(mgrID, commonName, validity, privBytes)
	if err != nil {
		return fmt.Errorf("failed to save private key: %w", err)
	}
	return ca.saveCAState()
}

func (ca *CA) LoadPrivateKey(mgrID storage.ManagerID, commonName string) (crypto.Signer, error) {
	ca.caLock.Lock()
	defer ca.caLock.Unlock()
	if ca.storage == nil { // on migration, storage can be nil
		return nil, ErrOnMigration
	}
	privKey, err := ca.storage.LoadPrivateKey(mgrID, commonName)
	if err != nil {
		return nil, fmt.Errorf("failed to load private key: %w", err)
	}
	// parse private key
	signer, err := x509.ParsePKCS8PrivateKey(privKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %w", err)
	}
	s, ok := signer.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("private key is not a crypto.Signer")
	}
	return s, nil
}

func (ca *CA) ListKeyPairs(mgrID storage.ManagerID) []index.KeyPairIndex {
	ca.caLock.Lock()
	defer ca.caLock.Unlock()
	return ca.storage.ListKeyPairs(mgrID)
}

func (ca *CA) DeleteKeyPair(mgrID storage.ManagerID, commonName string) error {
	ca.caLock.Lock()
	defer ca.caLock.Unlock()
	if ca.storage == nil { // on migration, storage can be nil
		return ErrOnMigration
	}
	err := ca.storage.DeleteKeyPair(mgrID, commonName)
	if err != nil {
		return fmt.Errorf("failed to delete key pair: %w", err)
	}
	return ca.saveCAState()
}

func (ca *CA) DeletePrivateKey(mgrID storage.ManagerID, commonName string) error {
	ca.caLock.Lock()
	defer ca.caLock.Unlock()
	if ca.storage == nil { // on migration, storage can be nil
		return ErrOnMigration
	}
	err := ca.storage.DeletePrivateKey(mgrID, commonName)
	if err != nil {
		return fmt.Errorf("failed to delete private key: %w", err)
	}
	return ca.saveCAState()
}

func (ca *CA) DeleteBlob(mgrID storage.ManagerID, name string) error {
	ca.caLock.Lock()
	defer ca.caLock.Unlock()
	if ca.storage == nil { // on migration, storage can be nil
		return ErrOnMigration
	}
	err := ca.storage.DeleteBlob(mgrID, name)
	if err != nil {
		return fmt.Errorf("failed to delete blob: %w", err)
	}
	return ca.saveCAState()
}

func (ca *CA) DeleteCertificate(mgrID storage.ManagerID, commonName string) error {
	ca.caLock.Lock()
	defer ca.caLock.Unlock()
	if ca.storage == nil { // on migration, storage can be nil
		return ErrOnMigration
	}
	err := ca.storage.DeleteCertificate(mgrID, commonName)
	if err != nil {
		return fmt.Errorf("failed to delete certificate: %w", err)
	}
	return ca.saveCAState()
}

func (ca *CA) LoadKeyPair(mgrID storage.ManagerID, commonName string) (crypto.Signer, []byte, error) {
	ca.caLock.Lock()
	defer ca.caLock.Unlock()
	if ca.storage == nil { // on migration, storage can be nil
		return nil, nil, ErrOnMigration
	}
	privKey, certDER, err := ca.storage.LoadKeyPair(mgrID, commonName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load key pair: %w", err)
	}
	// parse private key
	signer, err := x509.ParsePKCS8PrivateKey(privKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse private key: %w", err)
	}
	s, ok := signer.(crypto.Signer)
	if !ok {
		return nil, nil, fmt.Errorf("private key is not a crypto.Signer")
	}
	return s, certDER, nil
}

func (ca *CA) SaveBlob(mgrID storage.ManagerID, name string, validity storage.ValidityInfo, data []byte) error {
	ca.caLock.Lock()
	defer ca.caLock.Unlock()
	if ca.storage == nil { // on migration, storage can be nil
		return ErrOnMigration
	}
	err := ca.storage.SaveBlob(mgrID, name, validity, data)
	if err != nil {
		return fmt.Errorf("failed to save blob: %w", err)
	}
	return ca.saveCAState()
}

func (ca *CA) UpdateBlob(mgrID storage.ManagerID, name string, validity storage.ValidityInfo, data []byte) error {
	ca.caLock.Lock()
	defer ca.caLock.Unlock()
	if ca.storage == nil { // on migration, storage can be nil
		return ErrOnMigration
	}
	err := ca.storage.UpdateBlob(mgrID, name, validity, data)
	if err != nil {
		return fmt.Errorf("failed to save blob: %w", err)
	}
	return ca.saveCAState()
}

func (ca *CA) LoadBlob(mgrID storage.ManagerID, name string) ([]byte, error) {
	ca.caLock.Lock()
	defer ca.caLock.Unlock()
	if ca.storage == nil { // on migration, storage can be nil
		return nil, ErrOnMigration
	}
	return ca.storage.LoadBlob(mgrID, name)
}

func (ca *CA) makeCertificateTemplate(commonName string, pubKey any, appName string, expires time.Duration) (*x509.Certificate, error) {
	if commonName == "" {
		return nil, fmt.Errorf("common name must not be empty")
	}
	if pubKey == nil {
		return nil, fmt.Errorf("public key must not be nil")
	}
	if appName == "" {
		return nil, fmt.Errorf("app name must not be empty")
	}
	return &x509.Certificate{
		SerialNumber: ca.serialNum,
		Subject: pkix.Name{
			CommonName:         commonName,
			OrganizationalUnit: []string{appName},
		},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().Add(expires),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, nil
}

func (ca *CA) UpdateCertificate(commonName string, pubKey any, appName string, expires time.Duration) ([]byte, *big.Int, error) {
	ca.caLock.Lock()
	defer ca.caLock.Unlock()
	if ca.storage == nil { // on migration, storage can be nil
		return nil, nil, ErrOnMigration
	}
	template, err := ca.makeCertificateTemplate(commonName, pubKey, appName, expires)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to make certificate template: %w", err)
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, ca.caCert, pubKey, ca.privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create updated certificate for %s: %w", commonName, err)
	}
	serial, err := storage.SerialNumberFromBigInt(template.SerialNumber)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get serial number: %w", err)
	}
	err = ca.storage.UpdateCertificate(ca.mgrID, commonName, serial, storage.ValidityInfo{
		NotBefore: template.NotBefore,
		NotAfter:  template.NotAfter,
	}, certDER)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to update certificate: %w", err)
	}
	ca.serialNum.Add(ca.serialNum, big.NewInt(1))
	return certDER, template.SerialNumber, ca.saveCAState()
}

func (ca *CA) IssueCertificate(commonName string, pubKey any, appName string, expires time.Duration) ([]byte, error) {
	ca.caLock.Lock()
	defer ca.caLock.Unlock()
	if ca.storage == nil { // on migration, storage can be nil
		return nil, ErrOnMigration
	}
	template, err := ca.makeCertificateTemplate(commonName, pubKey, appName, expires)
	if err != nil {
		return nil, fmt.Errorf("failed to make certificate template: %w", err)
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, ca.caCert, pubKey, ca.privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create certificate for %s: %w", commonName, err)
	}
	serial, err := storage.SerialNumberFromBigInt(template.SerialNumber)
	if err != nil {
		return nil, fmt.Errorf("failed to get serial number: %w", err)
	}
	err = ca.storage.SaveCertificate(ca.mgrID, commonName, serial, storage.ValidityInfo{
		NotBefore: template.NotBefore,
		NotAfter:  template.NotAfter,
	}, certDER)
	if err != nil {
		return nil, fmt.Errorf("failed to save issued certificate: %w", err)
	}
	ca.serialNum.Add(ca.serialNum, big.NewInt(1))
	if err := ca.saveCAState(); err != nil {
		return nil, fmt.Errorf("failed to save CA state: %w", err)
	}
	return certDER, nil
}

func (ca *CA) VerifyCertificate(certDER []byte, app string, validateApp func(certApp, candidateApp string) error) (*x509.Certificate, error) {
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("failed to parse certificate: %w", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.caCert)
	opts := x509.VerifyOptions{
		Roots: roots,
		KeyUsages: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth,
		},
	}
	if _, err := cert.Verify(opts); err != nil {
		return nil, fmt.Errorf("failed to verify certificate: %w", err)
	}
	if len(cert.Subject.OrganizationalUnit) != 1 {
		return nil, fmt.Errorf("certificate organization is invalid")
	}
	if err := validateApp(cert.Subject.OrganizationalUnit[0], app); err != nil {
		return nil, fmt.Errorf("failed to validate certificate organization: %w", err)
	}
	idx, err := ca.storage.GetCertificateRawIndex(ca.mgrID, cert.Subject.CommonName)
	if err != nil { // if fail, maybe revoked
		return nil, fmt.Errorf("failed to get certificate index: %w", err)
	}
	idxSerial := big.NewInt(0).SetBytes(idx.SerialNumber.Serial[:])
	if idxSerial.Cmp(cert.SerialNumber) != 0 {
		return nil, fmt.Errorf("certificate serial number mismatch: expected %d, got %d", idxSerial, cert.SerialNumber)
	}
	return cert, nil
}

func (ca *CA) GenerateBootstrapToken(duration time.Duration, app string, commonNameSuffix string, certExp time.Duration) ([]byte, error) {
	token := make([]byte, 16)
	_, err := rand.Read(token)
	if err != nil {
		return nil, fmt.Errorf("failed to generate bootstrap token: %w", err)
	}
	ca.bootstrapLock.Lock()
	ca.bootstrapInfo[string(token)] = bootstrapInfo{
		token:            token,
		exp:              time.Now().Add(duration),
		usage:            app,
		commonNameSuffix: commonNameSuffix,
		certExp:          certExp,
	}
	ca.bootstrapLock.Unlock()
	return token, nil
}

func (ca *CA) DeleteExpiredBootstrapTokens(logger *slog.Logger) {
	ca.bootstrapLock.Lock()
	now := time.Now()
	for token, info := range ca.bootstrapInfo {
		if now.After(info.exp) {
			logger.Info("deleting expired bootstrap token", "token", base64.StdEncoding.EncodeToString(info.token), "usage", info.usage)
			delete(ca.bootstrapInfo, token)
		}
	}
	ca.bootstrapLock.Unlock()
}

func (ca *CA) DeleteExpiredCertificates(logger *slog.Logger) error {
	ca.caLock.Lock()
	defer ca.caLock.Unlock()
	err := ca.storage.DeleteExpired(time.Now())
	if err != nil {
		logger.Error("failed to delete expired certificates", "error", err)
		return err
	}
	return ca.saveCAState()
}

func (ca *CA) sendSelfCertificate(logger *slog.Logger, role string, transcript []byte, conn objproto.Connection, app string) error {
	signature, err := crypto.SignMessage(ca.privateKey, rand.Reader, transcript, crypto.Hash(0))
	if err != nil {
		logger.Error("failed to sign handshake transcript", "error", err)
		return err
	}
	resp := &pbca.HandshakeResponse{
		Role:       role,
		X509:       ca.CACertificate(),
		Sig:        signature,
		SelfSigned: true,
	}
	if _, _, err := conn.SendMessage(mustEncodeProto(resp)); err != nil {
		logger.Error("failed to send CA certificate", "error", err)
		return err
	}
	return nil
}

func (ca *CA) bootStrapHandshake(
	logger *slog.Logger, active objproto.Connection,
	req *pbca.HandshakeRequest, validateCommonName func(cn string) error) (commonName string, err error) {
	defer func() {
		active.Close()
		logger.Info("closed bootstrap connection", "remote", active.ConnectionID())
	}()
	token := req.Token
	app := req.App
	ca.bootstrapLock.Lock()
	info, exists := ca.bootstrapInfo[string(token)]
	delete(ca.bootstrapInfo, string(token))
	ca.bootstrapLock.Unlock()
	if !exists || time.Now().After(info.exp) {
		logger.Error("invalid or expired bootstrap token")
		return "", fmt.Errorf("invalid or expired bootstrap token")
	}
	logger.Info("valid bootstrap token received", "remote", active.ConnectionID(), "token", base64.StdEncoding.EncodeToString(token))
	pkeyKind := req.PubkeyKind
	if pkeyKind == "" {
		logger.Error("invalid pubkey_kind in bootstrap object")
		return "", fmt.Errorf("invalid pubkey_kind in bootstrap object")
	}
	pubKeyBytes := req.Pubkey
	if len(pubKeyBytes) == 0 {
		logger.Error("invalid pubkey in bootstrap object")
		return "", fmt.Errorf("invalid pubkey in bootstrap object")
	}
	commonName = req.CommonName
	if commonName == "" {
		logger.Error("invalid common_name in bootstrap object")
		return "", fmt.Errorf("invalid common_name in bootstrap object")
	}
	if !strings.HasSuffix(commonName, info.commonNameSuffix) || commonName == info.commonNameSuffix {
		logger.Error("common name suffix mismatch", "expected_suffix", info.commonNameSuffix, "got", commonName)
		return "", fmt.Errorf("common name suffix mismatch: expected %s, got %s", info.commonNameSuffix, commonName)
	}
	sign := req.Sig
	if len(sign) == 0 {
		logger.Error("invalid signature in bootstrap object")
		return "", fmt.Errorf("invalid signature in bootstrap object")
	}
	if info.usage != app {
		logger.Error("bootstrap token usage mismatch", "expected", info.usage, "got", app)
		return "", fmt.Errorf("bootstrap token usage mismatch: expected %s, got %s", info.usage, app)
	}
	transcript := active.GetTranscript()
	signed := concatBootStrap(pubKeyBytes, commonName, info.token, info.usage, transcript)
	var pubKey any
	switch pkeyKind {
	case "ed25519":
		pubKey = ed25519.PublicKey(pubKeyBytes)
		if !ed25519.Verify(pubKey.(ed25519.PublicKey), signed, sign) {
			slog.Error("failed to verify bootstrap signature")
			return "", fmt.Errorf("failed to verify bootstrap signature")
		}
	default:
		logger.Error("unsupported pubkey_kind in bootstrap object", "kind", pkeyKind)
		return "", fmt.Errorf("unsupported pubkey_kind in bootstrap object: %s", pkeyKind)
	}
	if err := validateCommonName(commonName); err != nil {
		logger.Error("common name validation failed", "error", err)
		return "", fmt.Errorf("common name validation failed: %w", err)
	}
	certDER, err := ca.IssueCertificate(commonName, pubKey, info.usage, info.certExp)
	if err != nil {
		logger.Error("failed to issue certificate", "error", err)
		return "", err
	}
	bootSignature := concatBootStrap(certDER, commonName, info.token, app, transcript)
	err = ca.sendSelfCertificate(logger, "bootstrap", bootSignature, active, app)
	if err != nil {
		return "", err
	}
	cert := &pbca.Certificate{X509: certDER}
	if _, _, err := active.SendMessage(mustEncodeProto(cert)); err != nil {
		logger.Error("failed to send issued certificate", "error", err)
		return "", err
	}
	logger.Info("bootstrap handshake completed", "remote", active.ConnectionID(), "common_name", commonName)
	return commonName, nil
}

func (ca *CA) updateHandshake(ctx context.Context, logger *slog.Logger, active objproto.Connection, commonName, app string, oldCertBytes []byte, oldCert *x509.Certificate) error {
	newCertMsg, err := active.ReceiveMessageContext(ctx)
	if err != nil {
		logger.Error("failed to receive updated certificate message", "error", err)
		return err
	}
	req := &pbca.UpdateCertRequest{}
	if err := req.Decode(newCertMsg.Data); err != nil {
		logger.Error("failed to decode updated certificate message", "error", err)
		return err
	}
	pubKeyKind := req.PubkeyKind
	if pubKeyKind == "" {
		logger.Error("invalid pubkey_kind in updated certificate message")
		return fmt.Errorf("invalid pubkey_kind in updated certificate message")
	}
	pubKeyBytes := req.Pubkey
	if len(pubKeyBytes) == 0 {
		logger.Error("invalid pubkey in updated certificate message")
		return fmt.Errorf("invalid pubkey in updated certificate message")
	}
	sig := req.Sig
	if len(sig) == 0 {
		logger.Error("invalid signature in updated certificate message")
		return fmt.Errorf("invalid signature in updated certificate message")
	}
	transcript := active.GetTranscript()
	signed := concatUpdateCert(oldCertBytes, pubKeyBytes, commonName, app, transcript)
	var pubKey any
	switch pubKeyKind {
	case "ed25519":
		pky := ed25519.PublicKey(pubKeyBytes)
		if !ed25519.Verify(pky, signed, sig) {
			logger.Error("failed to verify updated certificate signature")
			return fmt.Errorf("failed to verify updated certificate signature")
		}
		pubKey = pky
	default:
		logger.Error("unsupported pubkey_kind in updated certificate message", "kind", pubKeyKind)
		return fmt.Errorf("unsupported pubkey_kind in updated certificate message: %s", pubKeyKind)
	}
	// Renewal used to ALWAYS carry the old certificate's period forward, which froze the
	// issued lifetime at enrollment time: raising it meant re-enrolling every node (discard
	// its saved cert, mint a fresh token, hope nothing goes wrong while the fleet is out).
	// RenewCertExp lets the control plane state what a renewed cert should get; 0 (or an
	// unset hook) keeps the old period, so this is a no-op unless the CP configures it.
	expiresPeriod := oldCert.NotAfter.Sub(oldCert.NotBefore)
	if ca.RenewCertExp != nil {
		if d := ca.RenewCertExp(app, commonName); d > 0 && d != expiresPeriod {
			logger.Info("renewal period changed by policy", "common_name", commonName,
				"old", expiresPeriod.String(), "new", d.String())
			expiresPeriod = d
		}
	}
	updatedCertDER, serial, err := ca.UpdateCertificate(commonName, pubKey, app, expiresPeriod)
	if err != nil {
		logger.Error("failed to update certificate", "error", err)
		return err
	}
	cert := &pbca.Certificate{X509: updatedCertDER}
	if _, _, err := active.SendMessage(mustEncodeProto(cert)); err != nil {
		logger.Error("failed to send updated certificate", "error", err)
		return err
	}
	logger.Info("update certificate handshake completed", "remote", active.ConnectionID(), "common_name", commonName, "serial_number", serial.String())
	return nil
}

func EqualAppName(certApp, candidateApp string) error {
	if certApp != candidateApp {
		return fmt.Errorf("certificate app name mismatch: expected %s, got %s", candidateApp, certApp)
	}
	return nil
}

type HandshakeResult struct {
	CommonName string
	App        string
	// AdditionalData carries the original HandshakeRequest from the peer.
	// Set for bootstrap role; otherwise the cert-bearing fields will be
	// populated and pubkey/token fields empty.
	AdditionalData *pbca.HandshakeRequest
}

func (ca *CA) CAHandshake(ctx context.Context, logger *slog.Logger, active objproto.Connection, role string, validateApp func(certApp, candidateApp string) error, validateCommonName func(cn string) error) (_ *HandshakeResult, err error) {
	timeout, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ctx = timeout
	firstMsg, err := active.ReceiveMessageContext(ctx)
	if err != nil {
		logger.Error("failed to receive message", "error", err)
		return nil, err
	}
	req := &pbca.HandshakeRequest{}
	if err := req.Decode(firstMsg.Data); err != nil {
		logger.Error("failed to decode peer message", "error", err)
		return nil, err
	}
	peerRole := req.Role
	if peerRole == "" {
		logger.Error("invalid role in peer message")
		return nil, fmt.Errorf("invalid role in peer message")
	}
	app := req.App
	if app == "" {
		logger.Error("invalid app in peer message")
		return nil, fmt.Errorf("invalid app in peer message")
	}
	if peerRole == "bootstrap" {
		if len(req.Token) == 0 {
			logger.Error("invalid bootstrap token in peer message")
			return nil, fmt.Errorf("invalid bootstrap token in peer message")
		}
		commonName, err := ca.bootStrapHandshake(logger, active, req, validateCommonName)
		return &HandshakeResult{CommonName: commonName, App: "bootstrap", AdditionalData: req}, err
	}
	peerCert := req.X509
	if len(peerCert) == 0 {
		logger.Error("failed to get peer certificate from object")
		return nil, fmt.Errorf("invalid peer certificate object")
	}
	cert, err := ca.VerifyCertificate(peerCert, app, validateApp)
	if err != nil {
		logger.Error("failed to verify peer certificate", "error", err)
		return nil, err
	}
	sig := req.Sig
	if len(sig) == 0 {
		logger.Error("invalid signature in peer message")
		return nil, fmt.Errorf("invalid signature in peer message")
	}
	transcript := active.GetTranscript()
	slog.Debug("handshake transcript", "data", fmt.Sprintf("%x", transcript))
	err = cert.CheckSignature(cert.SignatureAlgorithm, append(transcript, app...), sig)
	if err != nil {
		logger.Error("failed to verify peer signature", "error", err)
		return nil, fmt.Errorf("failed to verify peer signature: %w", err)
	}
	logger.Info("peer certificate verified", "remote", active.ConnectionID(), "common_name", cert.Subject.CommonName, "app", app)
	err = ca.sendSelfCertificate(logger, role, append(transcript, app...), active, app)
	if err != nil {
		return nil, err
	}
	if peerRole == "update_cert" {
		return &HandshakeResult{CommonName: cert.Subject.CommonName, App: "update_cert"}, ca.updateHandshake(ctx, logger, active, cert.Subject.CommonName, app, peerCert, cert)
	}
	return &HandshakeResult{CommonName: cert.Subject.CommonName, App: app}, nil
}

func (ca *CA) DoMigration(migrator func(old storage.CertificateStorage) (storage.CertificateStorage, error)) error {
	ca.caLock.Lock()
	oldStorage := ca.storage
	ca.storage = nil
	ca.caLock.Unlock()
	if oldStorage == nil {
		return ErrOnMigration
	}
	newStorage, err := migrator(oldStorage)
	if err != nil {
		ca.caLock.Lock()
		ca.storage = oldStorage
		ca.caLock.Unlock()
		return err
	}
	ca.caLock.Lock()
	ca.storage = newStorage
	err = ca.saveCAState()
	ca.caLock.Unlock()
	return err
}
