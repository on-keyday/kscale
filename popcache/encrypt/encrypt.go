package encrypt

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/registration"
	"github.com/google/uuid"
	"github.com/on-keyday/kscale/ca"
	"github.com/on-keyday/kscale/ca/storage"
	"github.com/on-keyday/kscale/ca/storage/index"
)

type MyUserPub struct {
	Email        string                 `json:"email"`
	Registration *registration.Resource `json:"registration"`
}

type MyUser struct {
	MyUserPub
	key crypto.PrivateKey
}

func (u *MyUser) GetEmail() string {
	return u.Email
}
func (u MyUser) GetRegistration() *registration.Resource {
	return u.Registration
}
func (u *MyUser) GetPrivateKey() crypto.PrivateKey {
	return u.key
}

type AcmeAgent struct {
	User     *MyUser
	Client   *lego.Client
	Server   challenge.Provider
	ca       *ca.CA // for storage
	caDirURL string
}

func hashUUID(name string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(name)).String()
}

type AcmeResponse struct {
	Certificates []*x509.Certificate
	PrivateKey   crypto.PrivateKey
	CertPEM      []byte
	KeyPEM       []byte
	Cached       bool
}

func (a *AcmeAgent) DeleteAccount(ctx context.Context, logger *slog.Logger) error {
	accountKeyID := hashUUID("acme-account-key-" + a.caDirURL)
	err := a.ca.DeletePrivateKey(letsEncryptStorageKey, accountKeyID)
	if err != nil {
		return fmt.Errorf("acme agent: failed to delete account private key: %w", err)
	}
	accountInfoKey := hashUUID("acme-account-info-" + a.caDirURL)
	err = a.ca.DeleteBlob(letsEncryptStorageKey, accountInfoKey)
	if err != nil {
		return fmt.Errorf("acme agent: failed to delete account info blob: %w", err)
	}
	logger.Info("acme agent: deleted account information")
	return nil
}

func (a *AcmeAgent) ListCertificates(ctx context.Context, logger *slog.Logger) ([]index.KeyPairIndex, error) {
	index := a.ca.ListKeyPairs(letsEncryptStorageKey)
	return index, nil
}

func (a *AcmeAgent) GetCertificateInfo(ctx context.Context, logger *slog.Logger, domain []string) (*index.KeyPairIndex, error) {
	domainCertKey := domainNameKey(domain, a.caDirURL)
	index := a.ca.ListKeyPairs(letsEncryptStorageKey)
	for _, idx := range index {
		if string(idx.Cert.Name.Name) == domainCertKey {
			return &idx, nil
		}
	}
	return nil, fmt.Errorf("acme agent: certificate info for domain %s not found", domain)
}

func (a *AcmeAgent) DeleteCertificateByIndex(ctx context.Context, logger *slog.Logger, index string) error {
	err := a.ca.DeleteKeyPair(letsEncryptStorageKey, index)
	if err != nil {
		return fmt.Errorf("acme agent: failed to delete certificate for index %v: %w", index, err)
	}
	logger.Info("acme agent: deleted certificate for index", "index", index)
	return nil
}

func domainNameKey(domain []string, caDirURL string) string {
	uuidKey := hashUUID("acme-domain-cert-" + strings.Join(domain, ",") + caDirURL)
	// adding domains and caDirURL until 0xff length
	uuidKey += "-" + strings.Join(domain, ",")
	uuidKey += "-" + caDirURL
	// strip to 255 bytes
	const maxLen = 0xff
	if len(uuidKey) > maxLen {
		uuidKey = uuidKey[:maxLen]
	}
	return uuidKey
}

func (a *AcmeAgent) DeleteCertificate(ctx context.Context, logger *slog.Logger, domain []string) error {
	domainCertKey := domainNameKey(domain, a.caDirURL)
	err := a.ca.DeleteKeyPair(letsEncryptStorageKey, domainCertKey)
	if err != nil {
		return fmt.Errorf("acme agent: failed to delete certificate for domain %s: %w", domain, err)
	}
	logger.Info("acme agent: deleted certificate for domain", "domain", domain)
	return nil
}

func (a *AcmeAgent) GetCertificate(ctx context.Context, logger *slog.Logger, domain []string) (*AcmeResponse, error) {
	domainCertKey := domainNameKey(domain, a.caDirURL)
	priv, pubKey, err := a.ca.LoadKeyPair(letsEncryptStorageKey, domainCertKey)
	if err == nil {
		// check public key expiry
		x509Pub, err := x509.ParseCertificates(pubKey)
		if err != nil {
			return nil, fmt.Errorf("acme agent: failed to parse existing certificate for domain %s: %w", domain, err)
		}
		if len(x509Pub) == 0 {
			return nil, fmt.Errorf("acme agent: no certificates found in stored cert for domain %s", domain)
		}
		// assume first cert is domain cert
		cert := x509Pub[0]
		if cert.NotAfter.After(time.Now().AddDate(0, 1, 0)) {
			// convert to pem
			pem := bytes.NewBuffer(nil)
			for _, c := range x509Pub {
				pem.Write(certcrypto.PEMEncode(certcrypto.DERCertificateBytes(c.Raw)))
			}
			priv := certcrypto.PEMEncode(priv)
			return &AcmeResponse{
				Certificates: x509Pub,
				PrivateKey:   priv,
				CertPEM:      pem.Bytes(),
				KeyPEM:       priv,
				Cached:       true,
			}, nil
		}
		// else expired or expiring soon, obtain new cert
		logger.Info("acme agent: certificate for domain is expiring soon or expired, obtaining new certificate", "domain", domain)
	} else if !errors.Is(err, storage.ErrNotExist) {
		return nil, fmt.Errorf("acme agent: failed to load existing certificate for domain %s: %w", domain, err)
	} else {
		logger.Info("acme agent: no existing certificate for domain, obtaining new certificate", "domain", domain)
	}
	response, err := a.Client.Certificate.Obtain(certificate.ObtainRequest{
		Domains:        domain,
		Bundle:         true,
		PreferredChain: "ISRG Root X1", // Let's Encrypt default
	})
	if err != nil {
		return nil, fmt.Errorf("acme agent: failed to obtain certificate for domain %s: %w", domain, err)
	}
	// parse pem certs
	certs, err := certcrypto.ParsePEMBundle(response.Certificate)
	if err != nil {
		return nil, fmt.Errorf("acme agent: failed to parse obtained certificate for domain %s: %w", domain, err)
	}
	var certBytes []byte
	for _, cert := range certs {
		certBytes = append(certBytes, cert.Raw...)
	}
	serial, err := storage.SerialNumberFromBigInt(certs[0].SerialNumber)
	if err != nil {
		return nil, fmt.Errorf("acme agent: failed to get serial number from obtained certificate for domain %s: %w", domain, err)
	}
	p, err := certcrypto.ParsePEMPrivateKey(response.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("acme agent: failed to parse obtained private key for domain %s: %w", domain, err)
	}
	signer, ok := p.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("acme agent: obtained private key for domain %s is not a crypto.Signer", domain)
	}
	// store cert and key
	err = a.ca.SaveOrUpdateKeyPair(letsEncryptStorageKey, serial, domainCertKey, storage.ValidityInfo{
		NotBefore: certs[0].NotBefore,
		NotAfter:  certs[0].NotAfter,
	}, signer, certBytes)
	if err != nil {
		return nil, fmt.Errorf("acme agent: failed to store obtained certificate for domain %s: %w", domain, err)
	}
	return &AcmeResponse{
		Certificates: certs,
		PrivateKey:   signer,
		CertPEM:      response.Certificate,
		KeyPEM:       response.PrivateKey,
		Cached:       false,
	}, nil
}

const (
	DefaultCADirURL = "https://acme-v02.api.letsencrypt.org/directory"
	StagingCADirURL = "https://acme-staging-v02.api.letsencrypt.org/directory"
)

// for storage
var letsEncryptStorageKey = storage.NewManagerID(uuid.MustParse("76526a05-cf39-4f2b-bddf-ac41f97fb552"))

func SetupAcme(caStorage *ca.CA, dnsServer challenge.Provider, email string, caDirURL string) (*AcmeAgent, error) {
	accountKeyID := hashUUID("acme-account-key-" + caDirURL)
	privateKey, err := caStorage.LoadPrivateKey(letsEncryptStorageKey, accountKeyID)
	var pendingSavePrivateKey func() error
	if err != nil {
		if errors.Is(err, storage.ErrNotExist) {
			privateKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				return nil, err
			}
			pendingSavePrivateKey = func() error {
				return caStorage.SavePrivateKey(letsEncryptStorageKey, accountKeyID, storage.ValidityInfo{
					NotBefore: time.Now(),
					NotAfter:  time.Now().AddDate(10, 0, 0),
				}, privateKey)
			}
		} else {
			return nil, err
		}
	}
	var pendingSaveBlob func() error
	accountInfoKey := hashUUID("acme-account-info-" + caDirURL)
	infoBlob, err := caStorage.LoadBlob(letsEncryptStorageKey, accountInfoKey)
	if err != nil && !errors.Is(err, storage.ErrNotExist) {
		return nil, err
	}
	var info MyUserPub
	if errors.Is(err, storage.ErrNotExist) {
		info = MyUserPub{
			Email: email,
		}
		pendingSaveBlob = func() error {
			infoData, err := json.Marshal(info)
			if err != nil {
				return err
			}
			return caStorage.SaveBlob(letsEncryptStorageKey, accountInfoKey, storage.ValidityInfo{
				NotBefore: time.Now(),
				NotAfter:  time.Now().AddDate(10, 0, 0),
			}, infoData)
		}
	} else {
		err = json.Unmarshal(infoBlob, &info)
		if err != nil {
			return nil, err
		}
	}

	myUser := &MyUser{
		MyUserPub: info,
		key:       privateKey,
	}

	config := lego.NewConfig(myUser)
	config.CADirURL = caDirURL
	config.Certificate.KeyType = certcrypto.EC256
	config.UserAgent = "my-acme-client/1.0"

	// A client facilitates communication with the CA server.
	client, err := lego.NewClient(config)
	if err != nil {
		return nil, err
	}

	agent := &AcmeAgent{
		User:     myUser,
		Client:   client,
		Server:   dnsServer,
		ca:       caStorage,
		caDirURL: caDirURL,
	}

	err = client.Challenge.SetDNS01Provider(agent.Server, dns01.AddRecursiveNameservers([]string{"8.8.8.8:53", "1.1.1.1:53"}))
	if err != nil {
		return agent, err
	}
	resource, err := client.Registration.Register(registration.RegisterOptions{
		TermsOfServiceAgreed: true,
	})
	if err != nil {
		return agent, err
	}
	myUser.Registration = resource

	if pendingSavePrivateKey != nil {
		err = pendingSavePrivateKey()
		if err != nil {
			return nil, fmt.Errorf("acme agent: failed to save account private key: %w", err)
		}
	}
	if pendingSaveBlob != nil {
		err = pendingSaveBlob()
		if err != nil {
			return nil, fmt.Errorf("acme agent: failed to save account info blob: %w", err)
		}
	}

	return agent, nil
}
