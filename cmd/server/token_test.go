package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
