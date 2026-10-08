package server

import (
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sebnow/orchestrator/internal/pki"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// tlsTestServer is the server with authentication on, over TLS with a
// server certificate from its own CA.
type tlsTestServer struct {
	testServer
	ca *pki.CA
}

func startTLSTestServer(t *testing.T) tlsTestServer {
	t.Helper()
	store, _ := openTestStore(t)
	logs := &syncBuffer{}
	srv := New(store, slog.New(slog.NewTextHandler(logs, nil)), Options{DefaultModel: testDefaultModel})
	ca, err := pki.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	issued, err := ca.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := issued.Certificate()
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewUnstartedServer(srv)
	httpServer.TLS = pki.ServerConfig(cert, ca.Pool())
	httpServer.EnableHTTP2 = true
	httpServer.StartTLS()
	t.Cleanup(func() {
		srv.EndStreams()
		httpServer.Close()
	})
	return tlsTestServer{testServer: testServer{Server: srv, url: httpServer.URL, logs: logs}, ca: ca}
}

// client trusts the server's CA and presents daemon's certificate, or no
// certificate when daemon is empty. It follows no redirects.
func (s tlsTestServer) client(t *testing.T, daemon protocol.DaemonID) *http.Client {
	t.Helper()
	config := &tls.Config{RootCAs: s.ca.Pool()}
	if daemon != "" {
		issued, err := s.ca.IssueDaemon(daemon)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := issued.Certificate()
		if err != nil {
			t.Fatal(err)
		}
		config = pki.ClientConfig(cert, s.ca.Pool())
	}
	client := &http.Client{
		Transport:     &http.Transport{TLSClientConfig: config, ForceAttemptHTTP2: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func status(t *testing.T, client *http.Client, req *http.Request) int {
	t.Helper()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func newRequest(t *testing.T, method, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestGivenDaemonCertificateWhenTheDaemonRequestsItsOwnRoutesThenTheyAreServed(t *testing.T) {
	srv := startTLSTestServer(t)
	client := srv.client(t, "laptop")

	if got := status(t, client, newRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks")); got != http.StatusOK {
		t.Errorf("acks: status %d, want 200", got)
	}
}

func TestGivenAnotherDaemonsCertificateWhenItRequestsTheDaemonsRoutesThenForbidden(t *testing.T) {
	srv := startTLSTestServer(t)
	client := srv.client(t, "vps-1")

	for _, req := range []*http.Request{
		newRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks"),
		newRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/commands"),
		newRequest(t, http.MethodPost, srv.url+"/v1/daemons/laptop/events"),
	} {
		if got := status(t, client, req); got != http.StatusForbidden {
			t.Errorf("%s %s: status %d, want 403", req.Method, req.URL.Path, got)
		}
	}
}

func TestGivenNoClientCertificateWhenTheDaemonRoutesAreRequestedThenUnauthorized(t *testing.T) {
	srv := startTLSTestServer(t)
	client := srv.client(t, "")

	for _, req := range []*http.Request{
		newRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks"),
		newRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/commands"),
		newRequest(t, http.MethodPost, srv.url+"/v1/daemons/laptop/events"),
	} {
		if got := status(t, client, req); got != http.StatusUnauthorized {
			t.Errorf("%s %s: status %d, want 401", req.Method, req.URL.Path, got)
		}
	}
}
