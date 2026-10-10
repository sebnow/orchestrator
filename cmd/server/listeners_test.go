package main

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// tlsServer is the PKI, database and daemon binaries of a server that
// serves TLS, with an owner token issued.
type tlsServer struct {
	dir, dbPath, token string
	client             *http.Client
}

func newTLSServer(t *testing.T) tlsServer {
	t.Helper()
	dir := t.TempDir()
	mustRun(t, "init-ca", "-pki-dir", dir)
	mustRun(t, "issue-server-cert", "-pki-dir", dir, "-host", "127.0.0.1")
	dbPath := filepath.Join(dir, "server.db")
	status, stdout, stderr := runCommand("issue-owner-token", "-db", dbPath)
	if status != 0 {
		t.Fatalf("issue-owner-token: %d %s", status, stderr)
	}
	token := strings.TrimSpace(stdout)
	if fields := strings.Fields(token); len(fields) > 0 {
		token = fields[len(fields)-1]
	}
	binaries := filepath.Join(dir, "binaries")
	if err := os.MkdirAll(binaries, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if err := os.WriteFile(filepath.Join(binaries, "daemon-linux-"+arch), []byte("binary "+arch), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	return tlsServer{dir: dir, dbPath: dbPath, token: token,
		client: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}}
}

// args are the server's flags, with daemonListen as -daemon-listen when
// it is not empty.
func (s tlsServer) args(daemonListen string) []string {
	args := []string{"-listen", "127.0.0.1:0", "-db", s.dbPath,
		"-tls-cert", filepath.Join(s.dir, "server.crt"), "-tls-key", filepath.Join(s.dir, "server.key"),
		"-client-ca", filepath.Join(s.dir, "ca.crt"), "-ca-key", filepath.Join(s.dir, "ca.key"),
		"-daemon-binaries-dir", filepath.Join(s.dir, "binaries"), "-backup-every", "0", "-github-meta-url", ""}
	if daemonListen != "" {
		args = append(args, "-daemon-listen", daemonListen)
	}
	return args
}

// status sends method to url, with the owner's token when withToken,
// and returns the status.
func (s tlsServer) status(t *testing.T, method, url string, withToken bool) int {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if withToken {
		request.Header.Set("Authorization", "Bearer "+s.token)
	}
	response, err := s.client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return response.StatusCode
}

var servingAddresses = regexp.MustCompile(`msg=serving address=(\S+) tls=(\S+)(?: daemon_address=(\S+))?`)

// startServing runs the server with args until it serves, and returns its
// -listen address, its -daemon-listen address, empty without one, and a
// function that stops it with SIGINT and returns its status and log.
func startServing(t *testing.T, args ...string) (owner, daemons string, stop func() (int, string)) {
	t.Helper()
	stderr := &syncWriter{}
	served := make(chan int, 1)
	go func() { served <- run(args, io.Discard, stderr) }()
	deadline := time.Now().Add(10 * time.Second)
	var match []string
	for match == nil {
		select {
		case status := <-served:
			t.Fatalf("the server exited with %d before serving:\n%s", status, stderr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server did not serve within 10 s:\n%s", stderr)
		}
		time.Sleep(10 * time.Millisecond)
		match = servingAddresses.FindStringSubmatch(stderr.String())
	}
	stopped := false
	stop = func() (int, string) {
		stopped = true
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		select {
		case status := <-served:
			return status, stderr.String()
		case <-time.After(30 * time.Second):
			t.Fatalf("the server did not exit within 30 s of SIGINT:\n%s", stderr)
			return 0, ""
		}
	}
	t.Cleanup(func() {
		if !stopped {
			stop()
		}
	})
	return match[1], match[3], stop
}

func TestGivenADaemonListenerWhenEitherListenerIsAskedForTheOtherSidesRoutesThenTheyAreNotFound(t *testing.T) {
	s := newTLSServer(t)
	owner, daemons, stop := startServing(t, s.args("127.0.0.1:0")...)
	if daemons == "" {
		t.Fatal("the server logged no daemon address")
	}
	ownerURL, daemonURL := "http://"+owner, "https://"+daemons

	for _, tc := range []struct {
		method, url string
		withToken   bool
		want        int
	}{
		// The owner's listener serves plain HTTP, and the owner's routes.
		{http.MethodGet, ownerURL + "/login", false, http.StatusOK},
		{http.MethodGet, ownerURL + "/v1/tasks", true, http.StatusOK},
		{http.MethodGet, ownerURL + "/static/style.css", false, http.StatusOK},
		// Not the daemons'.
		{http.MethodGet, ownerURL + "/v1/daemons/vps/acks", true, http.StatusNotFound},
		{http.MethodPost, ownerURL + "/v1/enrol", true, http.StatusNotFound},
		{http.MethodGet, ownerURL + "/daemon/linux-amd64", true, http.StatusNotFound},
		// The daemons' listener serves TLS, enrolment and the binaries.
		{http.MethodPost, daemonURL + "/v1/enrol", false, http.StatusBadRequest},
		{http.MethodGet, daemonURL + "/daemon/linux-amd64", false, http.StatusOK},
		{http.MethodGet, daemonURL + "/v1/daemons/vps/acks", false, http.StatusUnauthorized},
		// Not the owner's, even with the owner's token.
		{http.MethodGet, daemonURL + "/", true, http.StatusNotFound},
		{http.MethodGet, daemonURL + "/login", false, http.StatusNotFound},
		{http.MethodGet, daemonURL + "/v1/tasks", true, http.StatusNotFound},
		{http.MethodGet, daemonURL + "/static/style.css", false, http.StatusNotFound},
	} {
		if got := s.status(t, tc.method, tc.url, tc.withToken); got != tc.want {
			t.Errorf("%s %s: %d, want %d", tc.method, tc.url, got, tc.want)
		}
	}

	status, logs := stop()
	if status != 0 {
		t.Errorf("status %d, want 0:\n%s", status, logs)
	}
	shutdown := strings.Index(logs, "shutting down")
	if backedUp := strings.LastIndex(logs, "backed up database"); shutdown < 0 || backedUp < shutdown {
		t.Errorf("no backup logged after both listeners shut down:\n%s", logs)
	}
}

func TestGivenNoDaemonListenerWhenTheServerServesTLSThenOneListenerServesEveryRoute(t *testing.T) {
	s := newTLSServer(t)
	owner, daemons, _ := startServing(t, s.args("")...)
	if daemons != "" {
		t.Fatalf("the server logged a daemon address %s without -daemon-listen", daemons)
	}
	url := "https://" + owner

	for _, tc := range []struct {
		method, path string
		withToken    bool
		want         int
	}{
		{http.MethodGet, "/login", false, http.StatusOK},
		{http.MethodGet, "/v1/tasks", true, http.StatusOK},
		{http.MethodPost, "/v1/enrol", false, http.StatusBadRequest},
		{http.MethodGet, "/daemon/linux-arm64", false, http.StatusOK},
		{http.MethodGet, "/v1/daemons/vps/acks", false, http.StatusUnauthorized},
	} {
		if got := s.status(t, tc.method, url+tc.path, tc.withToken); got != tc.want {
			t.Errorf("%s %s: %d, want %d", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestGivenADaemonListenerWhenTheFlagsCannotServeItThenTheServerRefusesToStart(t *testing.T) {
	s := newTLSServer(t)
	for name, args := range map[string][]string{
		"insecure":           {"-insecure-loopback", "-db", filepath.Join(t.TempDir(), "server.db"), "-daemon-listen", "127.0.0.1:0"},
		"non-loopback owner": append(s.args("127.0.0.1:0"), "-listen", "0.0.0.0:0"),
	} {
		status, _, stderr := runCommand(args...)

		if status != 2 || !strings.Contains(stderr, "-daemon-listen") {
			t.Errorf("%s: status %d, stderr %q; want 2 naming -daemon-listen", name, status, stderr)
		}
	}
}
