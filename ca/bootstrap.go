package ca

import (
	"context"
	"crypto"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"crypto/x509"
	"fmt"
	"log/slog"
	"time"

	"github.com/on-keyday/kscale/ca/storage/index"
	pbca "github.com/on-keyday/kscale/protobuf/proto/ca"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/objproto/packet"
)

// decodeHandshakeResponse reads a HandshakeResponse-shaped message from the
// connection and returns the decoded proto value. Used in client-side
// handshake / bootstrap flows where the CA / peer sends its certificate.
func decodeHandshakeResponse(msg *objproto.Message) (*pbca.HandshakeResponse, error) {
	if msg == nil || len(msg.Data) == 0 {
		return nil, fmt.Errorf("empty handshake response")
	}
	resp := &pbca.HandshakeResponse{}
	if err := resp.Decode(msg.Data); err != nil {
		return nil, fmt.Errorf("failed to decode handshake response: %w", err)
	}
	return resp, nil
}

func decodeCertificate(msg *objproto.Message) (*pbca.Certificate, error) {
	if msg == nil || len(msg.Data) == 0 {
		return nil, fmt.Errorf("empty certificate message")
	}
	cert := &pbca.Certificate{}
	if err := cert.Decode(msg.Data); err != nil {
		return nil, fmt.Errorf("failed to decode certificate: %w", err)
	}
	return cert, nil
}

func mustEncodeProto(msg interface {
	Append([]byte) ([]byte, error)
}) []byte {
	body, err := msg.Append(nil)
	if err != nil {
		panic(fmt.Errorf("encode proto: %w", err))
	}
	return body
}

type BootstrapInfo struct {
	CACert     []byte
	PeerCert   []byte
	SelfCert   []byte
	PrivateKey crypto.Signer
}

func (info *BootstrapInfo) DumpResult() ([]byte, error) {
	boot := index.BootInfo{}
	if !boot.PeerCert.SetCertificate(info.PeerCert) {
		return nil, fmt.Errorf("failed to set peer certificate")
	}
	if info.CACert != nil && subtle.ConstantTimeCompare(info.CACert, info.PeerCert) != 1 {
		boot.SetPeerIsSelfSigned(false)
		iss := index.IssuedCert{}
		if !iss.SetCertificate(info.CACert) {
			return nil, fmt.Errorf("failed to set CA certificate")
		}
		if !boot.SetCaCert(iss) {
			return nil, fmt.Errorf("failed to set CA certificate")
		}
	} else {
		boot.SetPeerIsSelfSigned(true)
	}
	if !boot.SelfCert.Cert.SetCertificate(info.SelfCert) {
		return nil, fmt.Errorf("failed to set self certificate")
	}
	privBytes, err := x509.MarshalPKCS8PrivateKey(info.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal private key: %w", err)
	}
	if !boot.SelfCert.SetPrivateKey(privBytes) {
		return nil, fmt.Errorf("failed to set private key")
	}
	return boot.Encode()
}

func LoadCertInfo(data []byte) (*BootstrapInfo, error) {
	boot := index.BootInfo{}
	if err := boot.DecodeExact(data); err != nil {
		return nil, fmt.Errorf("failed to decode bootstrap info: %w", err)
	}
	privKey, err := x509.ParsePKCS8PrivateKey(boot.SelfCert.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %w", err)
	}
	signer, ok := privKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("private key is not a crypto.Signer")
	}
	result := &BootstrapInfo{
		PeerCert:   boot.PeerCert.Certificate,
		SelfCert:   boot.SelfCert.Cert.Certificate,
		PrivateKey: signer,
	}
	if ca := boot.CaCert(); ca != nil {
		result.CACert = ca.Certificate
	} else {
		result.CACert = result.PeerCert
	}
	return result, nil
}

func BootstrapProtocol(ctx context.Context, peer objproto.ConnectionID, sess objproto.Endpoint, pubKeyKind string, commonName string, bootToken []byte, usage string) (*BootstrapInfo, error) {
	conn, err := objproto.DoECDHHandshake(ctx, sess, peer, ecdh.P521(), packet.CommonKeyKind_Aes128Gcm)
	if err != nil {
		return nil, fmt.Errorf("failed to perform ECDH handshake: %w", err)
	}
	defer conn.Close()
	slog.Debug("handshake transcript", "data", fmt.Sprintf("%x", conn.GetTranscript()))
	switch pubKeyKind {
	case "ed25519":
		return bootstrapEd25519(ctx, conn, commonName, bootToken, usage)
	default:
		return nil, fmt.Errorf("unsupported public key kind: %s", pubKeyKind)
	}
}

func concatBootStrap(pubKey []byte, commonName string, bootToken []byte, app string, transcript []byte) []byte {
	signed := make([]byte, 0, len(pubKey)+len(commonName)+len(bootToken)+len(transcript)+len(app))
	signed = append(signed, pubKey...)
	signed = append(signed, []byte(commonName)...)
	signed = append(signed, bootToken...)
	signed = append(signed, []byte(app)...)
	signed = append(signed, transcript...)
	return signed
}

func bootstrapEd25519(ctx context.Context, conn objproto.Connection, commonName string, bootToken []byte, app string) (*BootstrapInfo, error) {
	ed25519Pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to generate ed25519 key: %w", err)
	}
	signed := concatBootStrap([]byte(ed25519Pub), commonName, bootToken, app, conn.GetTranscript())
	signature, err := crypto.SignMessage(priv, rand.Reader, signed, crypto.Hash(0))
	if err != nil {
		return nil, fmt.Errorf("failed to sign bootstrap transcript: %w", err)
	}
	req := &pbca.HandshakeRequest{
		Role:       "bootstrap",
		App:        app,
		Sig:        signature,
		PubkeyKind: "ed25519",
		Pubkey:     []byte(ed25519Pub),
		CommonName: commonName,
		Token:      bootToken,
	}
	if _, _, err := conn.SendMessage(mustEncodeProto(req)); err != nil {
		return nil, fmt.Errorf("failed to send bootstrap message: %w", err)
	}
	// Generous timeout: the CP answers a bootstrap by minting+signing a cert, which under
	// load can take several seconds. A too-tight deadline here is especially costly because
	// a timed-out attempt still CONSUMES the single-use token on the CP, so every retry then
	// fails "invalid bootstrap token" — an unrecoverable wedge. Bootstrap happens once per
	// identity, so waiting is cheap; 30s tolerates a slow/busy CP.
	msg, err := conn.ReceiveMessageTimeout(ctx, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to receive bootstrap response: %w", err)
	}
	resp, err := decodeHandshakeResponse(msg)
	if err != nil {
		return nil, err
	}
	if !resp.SelfSigned {
		return nil, fmt.Errorf("invalid bootstrap response: self_signed not set")
	}
	if len(resp.X509) == 0 {
		return nil, fmt.Errorf("invalid bootstrap response: missing x509")
	}
	parsedPeerCert, err := x509.ParseCertificate(resp.X509)
	if err != nil {
		return nil, fmt.Errorf("failed to parse peer certificate: %w", err)
	}
	parsedCaCert := parsedPeerCert // self-signed
	result := &BootstrapInfo{
		CACert:     parsedPeerCert.Raw, // currently same as PeerCert
		PeerCert:   parsedPeerCert.Raw,
		PrivateKey: priv,
	}
	msg, err = conn.ReceiveMessageTimeout(ctx, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to receive self certificate: %w", err)
	}
	cert, err := decodeCertificate(msg)
	if err != nil {
		return nil, err
	}
	if len(cert.X509) == 0 {
		return nil, fmt.Errorf("invalid self certificate message: empty x509")
	}
	signed = concatBootStrap(cert.X509, commonName, bootToken, app, conn.GetTranscript())
	if err := parsedCaCert.CheckSignature(parsedCaCert.SignatureAlgorithm, signed, resp.Sig); err != nil {
		return nil, fmt.Errorf("failed to verify bootstrap response signature: %w", err)
	}
	result.SelfCert = cert.X509
	if err := conn.Close(); err != nil {
		return nil, fmt.Errorf("failed to close connection: %w", err)
	}
	return result, nil
}

func HandshakeProtocol(ctx context.Context, sess objproto.Endpoint, cid objproto.ConnectionID, role string, app string, bootStrap *BootstrapInfo) (objproto.Connection, error) {
	conn, err := objproto.DoECDHHandshake(ctx, sess, cid, ecdh.P521(), packet.CommonKeyKind_Aes128Gcm)
	if err != nil {
		return nil, fmt.Errorf("failed to perform ECDH handshake: %w", err)
	}
	return conn, HandshakeProtocolEx(ctx, conn, role, app, bootStrap)
}

func HandshakeProtocolEx(ctx context.Context, sess objproto.Connection, role string, app string, bootStrap *BootstrapInfo) error {
	caCert, err := x509.ParseCertificate(bootStrap.CACert)
	if err != nil {
		return fmt.Errorf("failed to parse CA certificate: %w", err)
	}
	transcript := sess.GetTranscript()
	sign, err := crypto.SignMessage(bootStrap.PrivateKey, rand.Reader, append(transcript, app...), crypto.Hash(0))
	if err != nil {
		return fmt.Errorf("failed to sign handshake transcript: %w", err)
	}
	req := &pbca.HandshakeRequest{
		Role: role,
		App:  app,
		Sig:  sign,
		X509: bootStrap.SelfCert,
	}
	if _, _, err := sess.SendMessage(mustEncodeProto(req)); err != nil {
		return fmt.Errorf("failed to send handshake message: %w", err)
	}
	msg, err := sess.ReceiveMessageTimeout(ctx, 5*time.Second)
	if err != nil {
		return fmt.Errorf("failed to receive handshake response: %w", err)
	}
	resp, err := decodeHandshakeResponse(msg)
	if err != nil {
		return err
	}
	if len(resp.X509) == 0 {
		return fmt.Errorf("invalid x509 in peer message")
	}
	peerCert, err := x509.ParseCertificate(resp.X509)
	if err != nil {
		return fmt.Errorf("failed to parse peer certificate: %w", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	if _, err := peerCert.Verify(x509.VerifyOptions{Roots: roots}); err != nil {
		return fmt.Errorf("failed to verify peer certificate: %w", err)
	}
	if len(resp.Sig) == 0 {
		return fmt.Errorf("invalid signature in peer message")
	}
	if err := peerCert.CheckSignature(peerCert.SignatureAlgorithm, append(transcript, app...), resp.Sig); err != nil {
		return fmt.Errorf("failed to verify peer signature: %w", err)
	}
	return nil
}

func concatUpdateCert(oldCert, pubKey []byte, commonName string, app string, transcript []byte) []byte {
	signed := make([]byte, 0, len(oldCert)+len(pubKey)+len(commonName)+len(app)+len(transcript))
	signed = append(signed, oldCert...)
	signed = append(signed, pubKey...)
	signed = append(signed, []byte(commonName)...)
	signed = append(signed, []byte(app)...)
	signed = append(signed, transcript...)
	return signed
}

func UpdateCertProtocol(ctx context.Context, sess objproto.Endpoint, cid objproto.ConnectionID, commonName string, app string, bootStrap *BootstrapInfo) (*BootstrapInfo, error) {
	conn, err := objproto.DoECDHHandshake(ctx, sess, cid, ecdh.P521(), packet.CommonKeyKind_Aes128Gcm)
	if err != nil {
		return nil, fmt.Errorf("failed to perform ECDH handshake: %w", err)
	}
	defer conn.Close()
	slog.Debug("handshake transcript", "data", fmt.Sprintf("%x", conn.GetTranscript()))
	return UpdateCertProtocolEx(ctx, conn, commonName, app, bootStrap)
}

func UpdateCertProtocolEx(ctx context.Context, conn objproto.Connection, commonName string, app string, bootStrap *BootstrapInfo) (*BootstrapInfo, error) {
	err := HandshakeProtocolEx(ctx, conn, "update_cert", app, bootStrap)
	if err != nil {
		return nil, fmt.Errorf("failed to perform update certificate handshake: %w", err)
	}
	// update certificate
	newPub, newPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to generate new ed25519 key: %w", err)
	}
	signed := concatUpdateCert(bootStrap.SelfCert, newPub, commonName, app, conn.GetTranscript())
	signature, err := crypto.SignMessage(newPriv, rand.Reader, signed, crypto.Hash(0))
	if err != nil {
		return nil, fmt.Errorf("failed to sign update certificate transcript: %w", err)
	}
	req := &pbca.UpdateCertRequest{
		PubkeyKind: "ed25519",
		Pubkey:     []byte(newPub),
		Sig:        signature,
	}
	if _, _, err := conn.SendMessage(mustEncodeProto(req)); err != nil {
		return nil, fmt.Errorf("failed to send update certificate message: %w", err)
	}
	newCertMsg, err := conn.ReceiveMessageTimeout(ctx, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to receive new certificate: %w", err)
	}
	cert, err := decodeCertificate(newCertMsg)
	if err != nil {
		return nil, err
	}
	newSelfCertRaw := cert.X509
	if len(newSelfCertRaw) == 0 {
		return nil, fmt.Errorf("failed to parse new certificate: empty x509")
	}
	// check cert is signed by same CA
	newSelfCert, err := x509.ParseCertificate(newSelfCertRaw)
	if err != nil {
		return nil, fmt.Errorf("failed to parse new certificate: %w", err)
	}
	caCert, err := x509.ParseCertificate(bootStrap.CACert)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CA certificate: %w", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	if _, err := newSelfCert.Verify(x509.VerifyOptions{
		Roots: roots,
		KeyUsages: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth,
		}}); err != nil {
		return nil, fmt.Errorf("failed to verify new certificate: %w", err)
	}
	result := &BootstrapInfo{
		CACert:     bootStrap.CACert,
		PeerCert:   bootStrap.PeerCert,
		SelfCert:   newSelfCertRaw,
		PrivateKey: newPriv,
	}
	return result, nil
}
