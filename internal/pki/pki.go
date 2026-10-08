// Package pki is the server's private certificate authority: it creates
// the CA, issues the server's certificate and each daemon's, and makes
// the TLS configurations of both ends
// (docs/adr/2026-10-08-daemon-authentication.md).
//
// Keys are ECDSA P-256. Files are PEM: certificates as CERTIFICATE,
// private keys as PKCS #8 PRIVATE KEY.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

const (
	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 365 * 24 * time.Hour
	// backdate lets a certificate be used at once by a machine whose
	// clock is behind the issuer's.
	backdate = time.Hour
)

// CA is the certificate authority that issues every certificate the
// server and its daemons present.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// Issued is a certificate and its private key, PEM-encoded.
type Issued struct {
	CertPEM []byte
	KeyPEM  []byte
}

// NewCA creates a CA with a new key.
func NewCA() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		Subject:               pkix.Name{CommonName: "orchestrator CA"},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(caValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}
	return &CA{cert: cert, key: key}, nil
}

// LoadCA reads the CA that Write wrote to dir.
func LoadCA(dir string) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("load CA: %w", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		return nil, fmt.Errorf("load CA: %w", err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load CA: %w", err)
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok || !pair.Leaf.IsCA {
		return nil, fmt.Errorf("load CA: %s does not hold a CA certificate with an ECDSA key", dir)
	}
	return &CA{cert: pair.Leaf, key: key}, nil
}

// Write writes the CA to dir as ca.crt and ca.key, the key readable by
// its owner only. It refuses to replace either file.
func (ca *CA) Write(dir string) error {
	keyPEM, err := encodeKey(ca.key)
	if err != nil {
		return err
	}
	return Issued{CertPEM: ca.CertPEM(), KeyPEM: keyPEM}.Write(dir, "ca")
}

// CertPEM is the CA's certificate, which both ends verify the other
// against.
func (ca *CA) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
}

// Pool is a pool holding only the CA's certificate.
func (ca *CA) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return pool
}

// IssueDaemon issues daemon its client certificate, whose Common Name is
// the daemon's id.
func (ca *CA) IssueDaemon(daemon protocol.DaemonID) (Issued, error) {
	if _, err := protocol.ParseDaemonID(string(daemon)); err != nil {
		return Issued{}, err
	}
	return ca.issue(&x509.Certificate{
		Subject:     pkix.Name{CommonName: string(daemon)},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
}

// IssueServer issues the server's certificate, valid for each of hosts,
// a DNS name or an IP address that daemons and the owner reach it by.
func (ca *CA) IssueServer(hosts []string) (Issued, error) {
	if len(hosts) == 0 {
		return Issued{}, errors.New("issue server certificate: no host")
	}
	template := &x509.Certificate{
		Subject:     pkix.Name{CommonName: hosts[0]},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
			continue
		}
		if host == "" || strings.ContainsAny(host, ":/ ") {
			return Issued{}, fmt.Errorf("issue server certificate: %q is neither a DNS name nor an IP address", host)
		}
		template.DNSNames = append(template.DNSNames, host)
	}
	return ca.issue(template)
}

// issue signs template, given its subject and use, with a new key.
func (ca *CA) issue(template *x509.Certificate) (Issued, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Issued{}, fmt.Errorf("generate key: %w", err)
	}
	now := time.Now()
	template.NotBefore = now.Add(-backdate)
	template.NotAfter = now.Add(leafValidity)
	if template.NotAfter.After(ca.cert.NotAfter) {
		template.NotAfter = ca.cert.NotAfter
	}
	template.KeyUsage = x509.KeyUsageDigitalSignature
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return Issued{}, fmt.Errorf("create certificate: %w", err)
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return Issued{}, err
	}
	return Issued{CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), KeyPEM: keyPEM}, nil
}

func encodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// Write writes the certificate to dir as name.crt and the key as
// name.key, readable by its owner only. It refuses to replace either
// file, and leaves neither behind when it fails.
func (i Issued) Write(dir, name string) error {
	keyPath := filepath.Join(dir, name+".key")
	if err := writeNew(keyPath, i.KeyPEM, 0o600); err != nil {
		return err
	}
	if err := writeNew(filepath.Join(dir, name+".crt"), i.CertPEM, 0o644); err != nil {
		os.Remove(keyPath)
		return err
	}
	return nil
}

// Certificate is the issued pair ready for a TLS configuration.
func (i Issued) Certificate() (tls.Certificate, error) {
	return tls.X509KeyPair(i.CertPEM, i.KeyPEM)
}

func writeNew(path string, data []byte, perm os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(path)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

// LoadPool reads the PEM certificates in path into a pool, such as the
// CA certificate to verify the other end against.
func LoadPool(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("%s holds no PEM certificate", path)
	}
	return pool, nil
}

// ServerConfig serves cert and verifies the certificate a client offers
// against clientCAs. A client need not offer one, so that browsers can
// connect; the routes that need one check for it.
func ServerConfig(cert tls.Certificate, clientCAs *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.VerifyClientCertIfGiven,
		MinVersion:   tls.VersionTLS13,
	}
}

// ClientConfig presents cert and trusts only the servers whose
// certificates roots issued.
func ClientConfig(cert tls.Certificate, roots *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      roots,
		MinVersion:   tls.VersionTLS13,
	}
}

// DaemonID is the id of the daemon cert was issued to.
func DaemonID(cert *x509.Certificate) (protocol.DaemonID, error) {
	daemon, err := protocol.ParseDaemonID(cert.Subject.CommonName)
	if err != nil {
		return "", fmt.Errorf("certificate's common name: %w", err)
	}
	return daemon, nil
}
