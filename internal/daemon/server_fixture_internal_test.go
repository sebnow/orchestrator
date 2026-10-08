package daemon

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/pki"
	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/server"
)

// serverFixture is the real server under httptest, with a front that
// records the daemon-facing requests and can answer them with 503.
type serverFixture struct {
	url *url.URL
	// owner makes the owner's requests, with token as the bearer token
	// when it is set.
	owner *http.Client
	token string
	// daemon is the client a daemon reaches the server with; nil means a
	// default client.
	daemon *http.Client

	down atomic.Bool

	mu       sync.Mutex
	requests []recordedRequest
}

type recordedRequest struct {
	method   string
	endpoint string // the last path element: events, acks or commands
	body     []byte
	// failed is true when the fixture answered 503 instead of the server.
	failed bool
}

// startServer serves the server over plain HTTP with authentication off,
// as with -insecure-loopback.
func startServer(t *testing.T) *serverFixture {
	t.Helper()
	f, httpServer := newServerFixture(t, true)
	httpServer.Start()
	f.owner = http.DefaultClient
	f.url = mustParseURL(t, httpServer.URL)
	return f
}

// startTLSServer serves the server over TLS with authentication on: the
// server, the owner and testDaemon each get a certificate or token from
// the test's own CA and store.
func startTLSServer(t *testing.T) *serverFixture {
	t.Helper()
	f, httpServer := newServerFixture(t, false)
	ca, err := pki.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	serverCert := tlsCertificate(t)(ca.IssueServer([]string{"127.0.0.1"}))
	daemonCert := tlsCertificate(t)(ca.IssueDaemon(testDaemon))
	httpServer.TLS = pki.ServerConfig(serverCert, ca.Pool())
	httpServer.EnableHTTP2 = true
	httpServer.StartTLS()
	f.owner = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca.Pool()}, ForceAttemptHTTP2: true}}
	f.daemon = &http.Client{Transport: &http.Transport{TLSClientConfig: pki.ClientConfig(daemonCert, ca.Pool()), ForceAttemptHTTP2: true}}
	t.Cleanup(f.owner.CloseIdleConnections)
	t.Cleanup(f.daemon.CloseIdleConnections)
	f.url = mustParseURL(t, httpServer.URL)
	return f
}

// tlsCertificate returns the TLS certificate of what an issuing call
// returned.
func tlsCertificate(t *testing.T) func(pki.Issued, error) tls.Certificate {
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

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// newServerFixture makes the server, and the unstarted HTTP server that
// fronts it. Unless insecure, the owner's token is issued into f.token.
func newServerFixture(t *testing.T, insecure bool) (*serverFixture, *httptest.Server) {
	t.Helper()
	store, err := server.OpenStore(t.Context(), filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(store, slog.New(slog.DiscardHandler), server.Options{DefaultModel: "haiku", Insecure: insecure})
	f := &serverFixture{}
	if !insecure {
		if f.token, err = store.IssueOwnerToken(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	httpServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/daemons/") {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			parts := strings.Split(r.URL.Path, "/")
			down := f.down.Load()
			f.mu.Lock()
			f.requests = append(f.requests, recordedRequest{method: r.Method, endpoint: parts[len(parts)-1], body: body, failed: down})
			f.mu.Unlock()
			if down {
				http.Error(w, "down for the test", http.StatusServiceUnavailable)
				return
			}
		}
		srv.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		srv.EndStreams()
		httpServer.Close()
		store.Close()
	})
	return f, httpServer
}

func (f *serverFixture) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

// call makes an owner request and decodes a 2xx response into out.
func (f *serverFixture) call(t *testing.T, method, path string, body any, out any) {
	t.Helper()
	status, data := f.try(t, method, path, body)
	if status/100 != 2 {
		t.Fatalf("%s %s: %d %s", method, path, status, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: decode %s: %v", method, path, data, err)
		}
	}
}

func (f *serverFixture) try(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, f.url.JoinPath(path).String(), reader)
	if err != nil {
		t.Fatal(err)
	}
	if f.token != "" {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}
	resp, err := f.owner.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

var testPauseLimits = protocol.PauseLimits{Acknowledge: 2 * time.Minute, Cleanup: 5 * time.Minute}

// createTask creates a task on daemon through the owner API, once the
// server has seen the daemon, and returns its start_task command.
func (f *serverFixture) createTask(t *testing.T, daemon protocol.DaemonID, start protocol.StartTask) protocol.Command {
	t.Helper()
	request := struct {
		DaemonID protocol.DaemonID `json:"daemon_id"`
		protocol.StartTask
	}{daemon, start}
	var command protocol.Command
	eventually(t, "the server to know the daemon", func() bool {
		status, data := f.try(t, http.MethodPost, "/v1/tasks", request)
		if status == http.StatusUnprocessableEntity {
			return false
		}
		if status != http.StatusCreated {
			t.Fatalf("create task: %d %s", status, data)
		}
		if err := json.Unmarshal(data, &command); err != nil {
			t.Fatal(err)
		}
		return true
	})
	return command
}

// registerDaemon makes the server know daemon, as its first request would.
func (f *serverFixture) registerDaemon(t *testing.T, daemon protocol.DaemonID) {
	t.Helper()
	f.call(t, http.MethodGet, "/v1/daemons/"+string(daemon)+"/acks", nil, nil)
}

func (f *serverFixture) command(t *testing.T, task protocol.TaskID, kind protocol.CommandKind, payload any) protocol.Command {
	t.Helper()
	var raw json.RawMessage
	if payload != nil {
		var err error
		if raw, err = json.Marshal(payload); err != nil {
			t.Fatal(err)
		}
	}
	var command protocol.Command
	f.call(t, http.MethodPost, "/v1/tasks/"+string(task)+"/commands", map[string]any{"kind": kind, "payload": raw}, &command)
	return command
}

func (f *serverFixture) events(t *testing.T, task protocol.TaskID) []protocol.Event {
	t.Helper()
	var events []protocol.Event
	f.call(t, http.MethodGet, "/v1/tasks/"+string(task)+"/events", nil, &events)
	return events
}

// waitForEvent waits until the server holds an event of task that match
// accepts, and returns the task's events.
func (f *serverFixture) waitForEvent(t *testing.T, task protocol.TaskID, what string, match func(protocol.Event) bool) []protocol.Event {
	t.Helper()
	var events []protocol.Event
	eventually(t, what, func() bool {
		events = f.events(t, task)
		for _, event := range events {
			if match(event) {
				return true
			}
		}
		return false
	})
	return events
}

func assertContiguous(t *testing.T, events []protocol.Event) {
	t.Helper()
	for idx, event := range events {
		if event.Seq != uint64(idx+1) {
			t.Fatalf("event %d has seq %d; seqs are not contiguous from 1", idx, event.Seq)
		}
	}
}

// eventually polls done until it reports true, failing the test after
// five seconds.
func eventually(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func describe(events []protocol.Event) string {
	var b strings.Builder
	for _, event := range events {
		fmt.Fprintf(&b, "\n  %d %s %s", event.Seq, event.Kind, event.Payload)
	}
	return b.String()
}
