package server

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/pki"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// enrolAs posts an enrolment with token and a request for daemon's
// certificate, without a client certificate, and returns the status, the
// enrolment when it succeeded, and the key the request was made with.
func enrolAs(t *testing.T, srv tlsTestServer, token string, daemon protocol.DaemonID) (int, protocol.Enrolled, []byte) {
	t.Helper()
	keyPEM, csrPEM, err := pki.NewDaemonRequest(daemon)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(protocol.Enrolment{Token: token, CSR: string(csrPEM)})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.url+protocol.EnrolPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.client(t, "").Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var enrolled protocol.Enrolled
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(data, &enrolled); err != nil {
			t.Fatalf("decode %s: %v", data, err)
		}
	}
	return resp.StatusCode, enrolled, keyPEM
}

func issueToken(t *testing.T, srv tlsTestServer, daemon protocol.DaemonID) protocol.EnrolmentToken {
	t.Helper()
	token, err := srv.store.IssueEnrolmentToken(t.Context(), daemon, DefaultEnrolmentTokenLifetime)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestGivenEnrolmentTokenWhenTheDaemonEnrolsThenItsCertificateOpensItsRoutes(t *testing.T) {
	srv := startTLSTestServer(t)
	token := issueToken(t, srv, "vps-ab12")

	code, enrolled, keyPEM := enrolAs(t, srv, token.String(), "vps-ab12")
	if code != http.StatusOK {
		t.Fatalf("enrol: status %d, want 200", code)
	}
	if enrolled.CA != string(srv.ca.CertPEM()) {
		t.Errorf("CA = %q, want the server's CA", enrolled.CA)
	}
	cert, err := tls.X509KeyPair([]byte(enrolled.Certificate), keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := pki.DaemonID(cert.Leaf); err != nil || id != "vps-ab12" {
		t.Errorf("certificate's daemon = %q, %v; want vps-ab12", id, err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: pki.ClientConfig(cert, srv.ca.Pool())}}
	t.Cleanup(client.CloseIdleConnections)
	if got := status(t, client, newRequest(t, http.MethodGet, srv.url+"/v1/daemons/vps-ab12/acks")); got != http.StatusOK {
		t.Errorf("acks: status %d, want 200", got)
	}
}

func TestGivenUsedEnrolmentTokenWhenItIsSentAgainThenEnrolmentIsRefused(t *testing.T) {
	srv := startTLSTestServer(t)
	token := issueToken(t, srv, "vps-ab12")
	if status, _, _ := enrolAs(t, srv, token.String(), "vps-ab12"); status != http.StatusOK {
		t.Fatalf("first enrolment: status %d, want 200", status)
	}

	if status, _, _ := enrolAs(t, srv, token.String(), "vps-ab12"); status != http.StatusUnauthorized {
		t.Errorf("second enrolment: status %d, want 401", status)
	}
}

func TestGivenExpiredEnrolmentTokenWhenTheDaemonEnrolsThenItIsRefused(t *testing.T) {
	clock := newTestClock()
	srv := startTLSTestServerWith(t, Options{Now: clock.now})
	token := issueToken(t, srv, "vps-ab12")
	clock.advance(DefaultEnrolmentTokenLifetime)

	if status, _, _ := enrolAs(t, srv, token.String(), "vps-ab12"); status != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", status)
	}
}

func TestGivenTokenForAnotherDaemonWhenItEnrolsUnderAnotherIDThenItIsRefusedAndTheTokenKept(t *testing.T) {
	srv := startTLSTestServer(t)
	token := issueToken(t, srv, "vps-ab12")
	wrong := protocol.EnrolmentToken{Daemon: "laptop", Secret: token.Secret}

	if status, _, _ := enrolAs(t, srv, wrong.String(), "laptop"); status != http.StatusUnauthorized {
		t.Errorf("wrong id: status %d, want 401", status)
	}
	if status, _, _ := enrolAs(t, srv, token.String(), "vps-ab12"); status != http.StatusOK {
		t.Errorf("right id afterwards: status %d, want 200", status)
	}
}

func TestGivenRequestNamingAnotherDaemonWhenEnrollingThenItIsRefusedAndTheTokenKept(t *testing.T) {
	srv := startTLSTestServer(t)
	token := issueToken(t, srv, "vps-ab12")

	if status, _, _ := enrolAs(t, srv, token.String(), "laptop"); status != http.StatusBadRequest {
		t.Errorf("other common name: status %d, want 400", status)
	}
	if status, _, _ := enrolAs(t, srv, token.String(), "vps-ab12"); status != http.StatusOK {
		t.Errorf("own common name afterwards: status %d, want 200", status)
	}
}

func TestGivenUnknownOrMalformedTokenWhenEnrollingThenItIsRefused(t *testing.T) {
	srv := startTLSTestServer(t)
	issueToken(t, srv, "vps-ab12")

	if status, _, _ := enrolAs(t, srv, "vps-ab12:"+newSecret(), "vps-ab12"); status != http.StatusUnauthorized {
		t.Errorf("unknown secret: status %d, want 401", status)
	}
	if status, _, _ := enrolAs(t, srv, "vps-ab12", "vps-ab12"); status != http.StatusBadRequest {
		t.Errorf("no secret: status %d, want 400", status)
	}
}

func TestGivenServerWithoutTheCAKeyWhenADaemonEnrolsThenTheRouteIsOff(t *testing.T) {
	srv := startTestServer(t)
	status, body := doRequest(t, http.MethodPost, srv.url+protocol.EnrolPath, `{"token":"vps-ab12:x","csr":""}`)
	if status != http.StatusNotFound || !strings.Contains(body, "enrolment is off") {
		t.Errorf("status %d %q, want 404 saying enrolment is off", status, body)
	}
}

func TestGivenEnrolmentTokensWhenOneIsIssuedThenTheExpiredAreDeleted(t *testing.T) {
	clock := newTestClock()
	srv := startTLSTestServerWith(t, Options{Now: clock.now})
	issueToken(t, srv, "vps-old")
	clock.advance(2 * time.Hour)
	issueToken(t, srv, "vps-new")

	var count int
	if err := srv.store.db.QueryRowContext(t.Context(), `SELECT count(*) FROM enrolment_tokens`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("%d tokens kept, want 1", count)
	}
}
