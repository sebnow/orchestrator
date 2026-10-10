package pki_test

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/pki"
	"github.com/sebnow/orchestrator/internal/protocol"
)

func newCA(t *testing.T) *pki.CA {
	t.Helper()
	ca, err := pki.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// certificate returns the TLS certificate of what an issuing call returned.
func certificate(t *testing.T) func(pki.Issued, error) tls.Certificate {
	return func(issued pki.Issued, err error) tls.Certificate {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		cert, err := issued.Certificate()
		if err != nil {
			t.Fatal(err)
		}
		return cert
	}
}

// serveTLS serves, over TLS with the server's certificate from ca, the
// common name of the verified client certificate, or "none".
func serveTLS(t *testing.T, ca *pki.CA) string {
	t.Helper()
	serverCert := certificate(t)(ca.IssueServer([]string{"127.0.0.1", "localhost"}))
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.VerifiedChains) == 0 {
			io.WriteString(w, "none")
			return
		}
		daemon, err := pki.DaemonID(r.TLS.VerifiedChains[0][0])
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		io.WriteString(w, string(daemon))
	}))
	srv.TLS = pki.ServerConfig(serverCert, ca.Pool())
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.URL
}

func get(t *testing.T, config *tls.Config, url string) (string, error) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: config, ForceAttemptHTTP2: true}}
	defer client.CloseIdleConnections()
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

func TestGivenCAWrittenAndLoadedWhenADaemonConnectsWithItsCertificateThenBothEndsVerifyAndTheServerSeesTheDaemonID(t *testing.T) {
	dir := t.TempDir()
	if err := newCA(t).Write(dir); err != nil {
		t.Fatal(err)
	}
	ca, err := pki.LoadCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	url := serveTLS(t, ca)
	daemonCert := certificate(t)(ca.IssueDaemon("laptop"))

	roots, err := pki.LoadPool(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := get(t, pki.ClientConfig(daemonCert, roots), url)
	if err != nil {
		t.Fatal(err)
	}
	if got != "laptop" {
		t.Errorf("server saw %q, want laptop", got)
	}
}

func TestGivenDaemonCertificateWhenIssuedThenItIsForClientsOnlyAndLastsAYear(t *testing.T) {
	cert := certificate(t)(newCA(t).IssueDaemon("vps-1")).Leaf
	if cert.Subject.CommonName != "vps-1" {
		t.Errorf("common name = %q", cert.Subject.CommonName)
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("extended key usage = %v, want client auth only", cert.ExtKeyUsage)
	}
	if validity := cert.NotAfter.Sub(time.Now()); validity < 364*24*time.Hour || validity > 366*24*time.Hour {
		t.Errorf("valid for %s more, want about a year", validity)
	}
}

func TestGivenInvalidDaemonIDWhenIssuingThenItIsRefused(t *testing.T) {
	_, err := newCA(t).IssueDaemon("../laptop")
	if !errors.Is(err, protocol.ErrInvalidDaemonID) {
		t.Errorf("err = %v, want ErrInvalidDaemonID", err)
	}
}

func TestGivenHostWithAPortWhenIssuingTheServerCertificateThenItIsRefused(t *testing.T) {
	if _, err := newCA(t).IssueServer([]string{"example.com:8443"}); err == nil {
		t.Error("issued a certificate for example.com:8443")
	}
}

func TestGivenClientCertificateFromAnotherCAWhenConnectingThenTheHandshakeFails(t *testing.T) {
	ca := newCA(t)
	url := serveTLS(t, ca)
	stranger := certificate(t)(newCA(t).IssueDaemon("laptop"))

	if _, err := get(t, pki.ClientConfig(stranger, ca.Pool()), url); err == nil {
		t.Error("connected with a certificate from another CA")
	}
}

func TestGivenServerCertificateWhenPresentedAsAClientCertificateThenTheHandshakeFails(t *testing.T) {
	ca := newCA(t)
	url := serveTLS(t, ca)
	server := certificate(t)(ca.IssueServer([]string{"laptop"}))

	if _, err := get(t, pki.ClientConfig(server, ca.Pool()), url); err == nil {
		t.Error("connected with a server certificate as the client's")
	}
}

func TestGivenServerFromAnotherCAWhenTheDaemonConnectsThenItRefusesTheServer(t *testing.T) {
	url := serveTLS(t, newCA(t))
	ca := newCA(t)
	daemonCert := certificate(t)(ca.IssueDaemon("laptop"))

	if _, err := get(t, pki.ClientConfig(daemonCert, ca.Pool()), url); err == nil {
		t.Error("trusted a server certificate from another CA")
	}
}

func TestGivenNoClientCertificateWhenConnectingThenTheServerSeesNone(t *testing.T) {
	ca := newCA(t)
	url := serveTLS(t, ca)

	got, err := get(t, &tls.Config{RootCAs: ca.Pool()}, url)
	if err != nil {
		t.Fatal(err)
	}
	if got != "none" {
		t.Errorf("server saw %q, want none", got)
	}
}

func TestGivenIssuedPairWhenWrittenThenOnlyTheOwnerCanReadTheKey(t *testing.T) {
	dir := t.TempDir()
	issued, err := newCA(t).IssueDaemon("laptop")
	if err != nil {
		t.Fatal(err)
	}
	if err := issued.Write(dir, "daemon"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]fs.FileMode{"daemon.key": 0o600, "daemon.crt": 0o644} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode = %v, want %v", name, info.Mode().Perm(), want)
		}
	}
}

func TestGivenExistingFilesWhenWritingThenTheyAreKept(t *testing.T) {
	dir := t.TempDir()
	if err := newCA(t).Write(dir); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}

	if err := newCA(t).Write(dir); !errors.Is(err, fs.ErrExist) {
		t.Errorf("second write: err = %v, want fs.ErrExist", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the CA key was replaced")
	}
}

func TestGivenDaemonRequestWhenSignedThenTheCertificateIsTheDaemonsForItsOwnKeyAndConnects(t *testing.T) {
	ca := newCA(t)
	keyPEM, csrPEM, err := pki.NewDaemonRequest("vps-ab12")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := ca.SignDaemon("vps-ab12", csrPEM)
	if err != nil {
		t.Fatal(err)
	}
	cert := certificate(t)(pki.Issued{CertPEM: certPEM, KeyPEM: keyPEM}, nil)
	if len(cert.Leaf.ExtKeyUsage) != 1 || cert.Leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("extended key usage = %v, want client auth only", cert.Leaf.ExtKeyUsage)
	}
	if validity := cert.Leaf.NotAfter.Sub(time.Now()); validity < 364*24*time.Hour || validity > 366*24*time.Hour {
		t.Errorf("valid for %s more, want about a year", validity)
	}
	got, err := get(t, pki.ClientConfig(cert, ca.Pool()), serveTLS(t, ca))
	if err != nil {
		t.Fatal(err)
	}
	if got != "vps-ab12" {
		t.Errorf("server saw %q, want vps-ab12", got)
	}
}

func TestGivenRequestForAnotherDaemonWhenSigningThenItIsRefused(t *testing.T) {
	_, csrPEM, err := pki.NewDaemonRequest("laptop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newCA(t).SignDaemon("vps-ab12", csrPEM); !errors.Is(err, pki.ErrInvalidRequest) {
		t.Errorf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestGivenMalformedRequestWhenSigningThenItIsRefused(t *testing.T) {
	ca := newCA(t)
	for name, csrPEM := range map[string][]byte{
		"empty":       nil,
		"certificate": ca.CertPEM(),
		"garbage":     []byte("-----BEGIN CERTIFICATE REQUEST-----\nAAAA\n-----END CERTIFICATE REQUEST-----\n"),
	} {
		if _, err := ca.SignDaemon("vps-ab12", csrPEM); !errors.Is(err, pki.ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
}
