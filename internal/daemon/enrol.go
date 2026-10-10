package daemon

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/sebnow/orchestrator/internal/pki"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// EnrolledName is the name, before .crt and .key, of the certificate and
// key that Enrol writes to the state directory.
const EnrolledName = "daemon"

// maxEnrolledBytes bounds the server's answer to an enrolment, two
// certificates, and the error text it sends instead.
const maxEnrolledBytes = 64 << 10

// Enrol gets the daemon its certificate with token
// (docs/adr/2026-10-10-vps-provisioning.md): it makes a key, sends the
// server a certificate signing request for token's daemon, and writes
// the certificate the server issues and the key to dir as daemon.crt and
// daemon.key, the key readable by its owner only. It writes nothing when
// it fails, and refuses to replace either file. client must verify the
// server against the CA and need present no certificate.
func Enrol(ctx context.Context, client *http.Client, server *url.URL, token protocol.EnrolmentToken, dir string) error {
	keyPEM, csrPEM, err := pki.NewDaemonRequest(token.Daemon)
	if err != nil {
		return fmt.Errorf("enrol: %w", err)
	}
	body, err := json.Marshal(protocol.Enrolment{Token: token.String(), CSR: string(csrPEM)})
	if err != nil {
		return fmt.Errorf("enrol: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.JoinPath(protocol.EnrolPath).String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("enrol: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("enrol: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxEnrolledBytes))
	if err != nil {
		return fmt.Errorf("enrol: read the answer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("enrol: the server answered %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var enrolled protocol.Enrolled
	if err := json.Unmarshal(data, &enrolled); err != nil {
		return fmt.Errorf("enrol: decode the answer: %w", err)
	}
	issued := pki.Issued{CertPEM: []byte(enrolled.Certificate), KeyPEM: keyPEM}
	cert, err := tls.X509KeyPair(issued.CertPEM, issued.KeyPEM)
	if err != nil {
		return fmt.Errorf("enrol: the server's certificate: %w", err)
	}
	if daemon, err := pki.DaemonID(cert.Leaf); err != nil || daemon != token.Daemon {
		return fmt.Errorf("enrol: the server's certificate is not daemon %q's", token.Daemon)
	}
	if err := issued.Write(dir, EnrolledName); err != nil {
		return fmt.Errorf("enrol: %w", err)
	}
	return nil
}
