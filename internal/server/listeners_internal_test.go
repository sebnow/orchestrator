package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/pki"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// splitServer is an insecure server, so that every route answers without
// credentials, with enrolment, backups and daemon binaries on, so that
// no route of either side is answered 404 for being off. It serves its
// owner handler, its daemon handler and both together.
func splitServer(t *testing.T) (owners, daemons, all string) {
	t.Helper()
	dir := t.TempDir()
	for _, arch := range daemonArchitectures {
		if err := os.WriteFile(filepath.Join(dir, "daemon-linux-"+arch), []byte("binary "+arch), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store, _ := openTestStore(t)
	ca, err := pki.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(store, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
		Insecure: true, DaemonBinariesDir: dir, CA: ca,
		Backups: &BackupPolicy{Dir: filepath.Join(t.TempDir(), "backups"), Keep: 1},
	})
	t.Cleanup(srv.EndStreams)
	serve := func(h http.Handler) string {
		server := httptest.NewServer(h)
		t.Cleanup(server.Close)
		return server.URL
	}
	return serve(srv.OwnerHandler()), serve(srv.DaemonHandler()), serve(srv)
}

// statusOf sends method to url with body and returns the status.
func statusOf(t *testing.T, method, url, body string) int {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	return response.StatusCode
}

var (
	// ownerRoutes are routes of the owner's side: the GUI, the owner
	// API, the login form and the static files.
	ownerRoutes = [][2]string{
		{http.MethodGet, "/"},
		{http.MethodGet, "/v1/tasks"},
		{http.MethodGet, "/v1/daemons"},
		{http.MethodPost, "/v1/backup"},
		{http.MethodGet, "/login"},
		{http.MethodGet, "/static/style.css"},
	}
	// daemonRoutes are routes of the daemons' side: the daemon API,
	// enrolment and the daemon binaries.
	daemonRoutes = [][2]string{
		{http.MethodGet, "/v1/daemons/laptop/acks"},
		{http.MethodPost, "/v1/daemons/laptop/events"},
		{http.MethodGet, "/v1/daemons/laptop/commands"},
		{http.MethodPut, "/v1/daemons/laptop/facts"},
		{http.MethodPost, protocol.EnrolPath},
		{http.MethodGet, DaemonBinaryPath + "amd64"},
		{http.MethodGet, DaemonBinaryPath + "arm64"},
	}
)

func TestGivenTheDaemonHandlerWhenAnOwnersRouteIsRequestedThenItIsNotFound(t *testing.T) {
	_, daemons, _ := splitServer(t)
	for _, route := range ownerRoutes {
		if status := statusOf(t, route[0], daemons+route[1], ""); status != http.StatusNotFound {
			t.Errorf("%s %s on the daemon handler: %d, want 404", route[0], route[1], status)
		}
	}
}

func TestGivenTheOwnerHandlerWhenADaemonsRouteIsRequestedThenItIsNotFound(t *testing.T) {
	owners, _, _ := splitServer(t)
	for _, route := range daemonRoutes {
		if status := statusOf(t, route[0], owners+route[1], ""); status != http.StatusNotFound {
			t.Errorf("%s %s on the owner handler: %d, want 404", route[0], route[1], status)
		}
	}
}

// Each handler serves its own side's routes: none of them is answered
// 404 or 405 there, nor on the handler that serves both.
func TestGivenEachHandlerWhenItsOwnSidesRoutesAreRequestedThenTheyAreServed(t *testing.T) {
	owners, daemons, all := splitServer(t)
	for _, tc := range []struct {
		name   string
		url    string
		routes [][2]string
	}{
		{"owner handler", owners, ownerRoutes},
		{"daemon handler", daemons, daemonRoutes},
		{"both", all, ownerRoutes},
		{"both", all, daemonRoutes},
	} {
		for _, route := range tc.routes {
			status := statusOf(t, route[0], tc.url+route[1], "")
			if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
				t.Errorf("%s %s on %s: %d, want it served", route[0], route[1], tc.name, status)
			}
		}
	}
}

func TestGivenTheDaemonHandlerWhenADaemonBinaryIsRequestedThenItIsServed(t *testing.T) {
	_, daemons, _ := splitServer(t)

	response, err := http.Get(daemons + DaemonBinaryPath + "arm64")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)

	if response.StatusCode != http.StatusOK || string(body) != "binary arm64" {
		t.Errorf("status %d, body %q; want the arm64 binary", response.StatusCode, body)
	}
}
