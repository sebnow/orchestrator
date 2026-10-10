package daemon

import (
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sebnow/orchestrator/internal/pki"
	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/server"
)

// enrolmentServer is the real server over TLS with enrolment on, its
// store, and a client that trusts it and presents no certificate.
type enrolmentServer struct {
	url    string
	store  *server.Store
	ca     *pki.CA
	client *http.Client
}

func startEnrolmentServer(t *testing.T) enrolmentServer {
	t.Helper()
	store, err := server.OpenStore(t.Context(), filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ca, err := pki.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewUnstartedServer(server.New(store, slog.New(slog.DiscardHandler), server.Options{DefaultModel: "haiku", CA: ca}))
	httpServer.TLS = pki.ServerConfig(tlsCertificate(t)(ca.IssueServer([]string{"127.0.0.1"})), ca.Pool())
	httpServer.StartTLS()
	t.Cleanup(httpServer.Close)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca.Pool()}}}
	t.Cleanup(client.CloseIdleConnections)
	return enrolmentServer{url: httpServer.URL, store: store, ca: ca, client: client}
}

func TestGivenEnrolmentTokenWhenTheDaemonEnrolsThenItWritesACertificateThatOpensItsRoutes(t *testing.T) {
	srv := startEnrolmentServer(t)
	token, err := srv.store.IssueEnrolmentToken(t.Context(), "vps-ab12", server.DefaultEnrolmentTokenLifetime)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	if err := Enrol(t.Context(), srv.client, mustParseURL(t, srv.url), token, dir); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, "daemon.key"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("daemon.key mode %v, want 0600", info.Mode().Perm())
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "daemon.crt"), filepath.Join(dir, "daemon.key"))
	if err != nil {
		t.Fatal(err)
	}
	if id, err := pki.DaemonID(cert.Leaf); err != nil || id != "vps-ab12" {
		t.Errorf("daemon id = %q, %v; want vps-ab12", id, err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: pki.ClientConfig(cert, srv.ca.Pool())}}
	t.Cleanup(client.CloseIdleConnections)
	resp, err := client.Get(srv.url + "/v1/daemons/vps-ab12/acks")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("acks: status %d, want 200", resp.StatusCode)
	}
}

func TestGivenRefusedTokenWhenTheDaemonEnrolsThenItFailsAndWritesNothing(t *testing.T) {
	srv := startEnrolmentServer(t)
	dir := t.TempDir()

	err := Enrol(t.Context(), srv.client, mustParseURL(t, srv.url), protocol.EnrolmentToken{Daemon: "vps-ab12", Secret: "unknown"}, dir)
	if err == nil {
		t.Fatal("enrolled with an unknown token")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %v", entries)
	}
}
