package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/pki"
	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/server"
)

// ownerAPIStatus is the status of GET /v1/tasks with token as the bearer
// token, from a server over the database at dbPath.
func ownerAPIStatus(t *testing.T, dbPath, token string) int {
	t.Helper()
	store, err := server.OpenStore(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	srv := httptest.NewServer(server.New(store, slog.New(slog.DiscardHandler), server.Options{DefaultModel: "haiku"}))
	defer srv.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/tasks", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestGivenNewDatabaseWhenAnOwnerTokenIsIssuedThenItIsPrintedOnceAndTheServerAcceptsIt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state", "server.db")

	status, stdout, stderr := runCommand("issue-owner-token", "-db", dbPath)
	if status != 0 {
		t.Fatalf("status %d: %s", status, stderr)
	}
	token := strings.TrimSpace(stdout)
	if len(token) != 43 || strings.Contains(stderr, token) {
		t.Fatalf("stdout %q, stderr %q; want a 43-character token on stdout only", stdout, stderr)
	}
	if got := ownerAPIStatus(t, dbPath, token); got != http.StatusOK {
		t.Errorf("owner API with the token: status %d, want 200", got)
	}
}

func TestGivenIssuedTokenWhenAnotherIsIssuedThenOnlyTheNewOneIsAccepted(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "server.db")
	_, first, _ := runCommand("issue-owner-token", "-db", dbPath)
	_, second, _ := runCommand("issue-owner-token", "-db", dbPath)

	if got := ownerAPIStatus(t, dbPath, strings.TrimSpace(first)); got != http.StatusUnauthorized {
		t.Errorf("first token: status %d, want 401", got)
	}
	if got := ownerAPIStatus(t, dbPath, strings.TrimSpace(second)); got != http.StatusOK {
		t.Errorf("second token: status %d, want 200", got)
	}
}

func TestGivenDaemonIDWhenAnEnrolmentTokenIsIssuedThenItIsPrintedOnceAndEnrolsThatDaemon(t *testing.T) {
	dir := t.TempDir()
	mustRun(t, "init-ca", "-pki-dir", dir)
	dbPath := filepath.Join(dir, "server.db")

	status, stdout, stderr := runCommand("enrol-token", "-db", dbPath, "-id", "build-box")
	if status != 0 {
		t.Fatalf("status %d: %s", status, stderr)
	}
	token, err := protocol.ParseEnrolmentToken(stdout)
	if err != nil || token.Daemon != "build-box" || strings.Contains(stderr, token.Secret) {
		t.Fatalf("stdout %q, stderr %q, %v; want a token for build-box on stdout only", stdout, stderr, err)
	}

	ca, err := pki.LoadCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := server.OpenStore(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	srv := httptest.NewServer(server.New(store, slog.New(slog.DiscardHandler), server.Options{DefaultModel: "haiku", CA: ca}))
	defer srv.Close()
	_, csrPEM, err := pki.NewDaemonRequest("build-box")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(protocol.Enrolment{Token: token.String(), CSR: string(csrPEM)})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Post(srv.URL+protocol.EnrolPath, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("enrol: status %d, want 200", resp.StatusCode)
	}
}

func TestGivenInvalidDaemonIDWhenAnEnrolmentTokenIsAskedForThenItIsRefused(t *testing.T) {
	status, stdout, _ := runCommand("enrol-token", "-db", filepath.Join(t.TempDir(), "server.db"), "-id", "a:b")
	if status != 2 || stdout != "" {
		t.Errorf("status %d, stdout %q; want 2 and no token", status, stdout)
	}
}
