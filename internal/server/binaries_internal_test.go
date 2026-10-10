package server

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestGivenDaemonBinariesDirWhenARequestWithoutCredentialsAsksForABinaryThenTheFileIsServedOr404WhenMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "daemon-linux-amd64"), []byte("ELF amd64"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := startTLSTestServerWith(t, Options{DaemonBinariesDir: dir})
	client := srv.client(t, "")

	resp, err := client.Do(newRequest(t, http.MethodGet, srv.url+"/daemon/linux-amd64"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ELF amd64" {
		t.Errorf("amd64: %d %q, want 200 with the file", resp.StatusCode, body)
	}
	if got := status(t, client, newRequest(t, http.MethodGet, srv.url+"/daemon/linux-arm64")); got != http.StatusNotFound {
		t.Errorf("arm64, missing: status %d, want 404", got)
	}
	if got := status(t, client, newRequest(t, http.MethodGet, srv.url+"/daemon/linux-..%2fdaemon-linux-amd64")); got == http.StatusOK {
		t.Errorf("another path: status %d, want no file", got)
	}
}

func TestGivenNoDaemonBinariesDirWhenABinaryIsAskedForThenNothingIsServed(t *testing.T) {
	srv := startTLSTestServer(t)

	if got := status(t, srv.client(t, ""), newRequest(t, http.MethodGet, srv.url+"/daemon/linux-amd64")); got == http.StatusOK {
		t.Errorf("status %d, want no file", got)
	}
}
