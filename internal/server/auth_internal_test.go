package server

import (
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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
	return startTLSTestServerWith(t, Options{})
}

// startTLSTestServerWith is startTLSTestServer with options, and with
// its CA as the one daemons enrol with.
func startTLSTestServerWith(t *testing.T, options Options) tlsTestServer {
	t.Helper()
	store, _ := openTestStore(t)
	logs := &syncBuffer{}
	ca, err := pki.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	options.DefaultModel, options.CA = testDefaultModel, ca
	srv := New(store, slog.New(slog.NewTextHandler(logs, nil)), options)
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
		newRequest(t, http.MethodPost, srv.url+"/v1/daemons/laptop/tasks/task-1/requests"),
		newRequest(t, http.MethodPut, srv.url+"/v1/daemons/laptop/facts"),
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
		newRequest(t, http.MethodPost, srv.url+"/v1/daemons/laptop/tasks/task-1/requests"),
		newRequest(t, http.MethodPut, srv.url+"/v1/daemons/laptop/facts"),
	} {
		if got := status(t, client, req); got != http.StatusUnauthorized {
			t.Errorf("%s %s: status %d, want 401", req.Method, req.URL.Path, got)
		}
	}
}

func issueOwnerToken(t *testing.T, srv tlsTestServer) string {
	t.Helper()
	token, err := srv.store.IssueOwnerToken(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func formRequest(t *testing.T, url string, form url.Values) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func do(t *testing.T, client *http.Client, req *http.Request) (*http.Response, string) {
	t.Helper()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

// logIn logs in with token through the form and returns the session
// cookie it sets.
func logIn(t *testing.T, srv tlsTestServer, client *http.Client, token string) *http.Cookie {
	t.Helper()
	resp, body := do(t, client, formRequest(t, srv.url+"/login", url.Values{"token": {token}}))
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("login: status %d, location %q: %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == sessionCookie {
			return cookie
		}
	}
	t.Fatalf("login set no %s cookie", sessionCookie)
	return nil
}

func withCookie(req *http.Request, cookie *http.Cookie) *http.Request {
	req.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	return req
}

func requireSentToLogin(t *testing.T, client *http.Client, req *http.Request) {
	t.Helper()
	resp, _ := do(t, client, req)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("%s %s: status %d, location %q; want 303 to /login", req.Method, req.URL.Path, resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestGivenNoSessionWhenAGUIRouteIsRequestedThenTheBrowserIsSentToTheLoginForm(t *testing.T) {
	srv := startTLSTestServer(t)
	issueOwnerToken(t, srv)
	client := srv.client(t, "")

	requireSentToLogin(t, client, newRequest(t, http.MethodGet, srv.url+"/"))
	requireSentToLogin(t, client, newRequest(t, http.MethodGet, srv.url+"/tasks/task-1"))
	requireSentToLogin(t, client, formRequest(t, srv.url+"/tasks", startForm("laptop", "Do it.")))
}

func TestGivenNoSessionWhenHTMXRequestsAFragmentThenItIsToldToGoToTheLoginForm(t *testing.T) {
	srv := startTLSTestServer(t)
	issueOwnerToken(t, srv)
	req := newRequest(t, http.MethodGet, srv.url+"/tasks/task-1/updates")
	req.Header.Set("HX-Request", "true")

	resp, _ := do(t, srv.client(t, ""), req)
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("HX-Redirect") != "/login" {
		t.Errorf("status %d, HX-Redirect %q; want 401 and /login", resp.StatusCode, resp.Header.Get("HX-Redirect"))
	}
}

func TestGivenNoOwnerTokenWhenTheOwnerAPIIsRequestedThenUnauthorized(t *testing.T) {
	srv := startTLSTestServer(t)
	issueOwnerToken(t, srv)
	wrongToken := newRequest(t, http.MethodGet, srv.url+"/v1/tasks")
	wrongToken.Header.Set("Authorization", "Bearer "+newSecret())
	notBearer := newRequest(t, http.MethodGet, srv.url+"/v1/tasks")
	notBearer.Header.Set("Authorization", "Basic b3duZXI6b3duZXI=")

	for name, request := range map[string]struct {
		client *http.Client
		req    *http.Request
	}{
		"no header":          {srv.client(t, ""), newRequest(t, http.MethodGet, srv.url+"/v1/tasks")},
		"wrong token":        {srv.client(t, ""), wrongToken},
		"another scheme":     {srv.client(t, ""), notBearer},
		"daemon certificate": {srv.client(t, "laptop"), newRequest(t, http.MethodGet, srv.url+"/v1/tasks")},
		"create task":        {srv.client(t, ""), newRequest(t, http.MethodPost, srv.url+"/v1/tasks")},
	} {
		resp, _ := do(t, request.client, request.req)
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s: status %d, WWW-Authenticate %q; want 401 and Bearer", name, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
	}
}

func TestGivenOwnerTokenAsABearerTokenWhenTheOwnerAPIIsRequestedThenItIsServed(t *testing.T) {
	srv := startTLSTestServer(t)
	token := issueOwnerToken(t, srv)
	req := newRequest(t, http.MethodGet, srv.url+"/v1/tasks")
	req.Header.Set("Authorization", "Bearer "+token)

	if resp, body := do(t, srv.client(t, ""), req); resp.StatusCode != http.StatusOK {
		t.Errorf("status %d: %s", resp.StatusCode, body)
	}
}

func TestGivenWrongTokenWhenLoggingInThenTheFormSaysSoAndNoSessionStarts(t *testing.T) {
	srv := startTLSTestServer(t)
	issueOwnerToken(t, srv)

	resp, body := do(t, srv.client(t, ""), formRequest(t, srv.url+"/login", url.Values{"token": {newSecret()}}))
	if resp.StatusCode != http.StatusUnauthorized || len(resp.Cookies()) != 0 {
		t.Errorf("status %d, cookies %v; want 401 and none", resp.StatusCode, resp.Cookies())
	}
	requireContains(t, body, "That is not the owner&#39;s token.", `name="token"`, `type="password"`)
}

func TestGivenOwnerTokenWhenLoggingInThenASessionCookieIsSetThatOnlyTheServerReadsAndThePagesAreServed(t *testing.T) {
	srv := startTLSTestServer(t)
	client := srv.client(t, "")

	cookie := logIn(t, srv, client, issueOwnerToken(t, srv))
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
		t.Errorf("cookie %s: want Secure, HttpOnly, SameSite=Strict and Path=/", cookie)
	}
	if cookie.MaxAge != 30*24*60*60 {
		t.Errorf("cookie Max-Age = %d, want 30 days", cookie.MaxAge)
	}
	resp, body := do(t, client, withCookie(newRequest(t, http.MethodGet, srv.url+"/"), cookie))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard: status %d", resp.StatusCode)
	}
	requireContains(t, body, "New task", `action="/logout"`)
}

func TestGivenSessionWhenLoggingOutThenTheCookieIsClearedAndTheSessionEnds(t *testing.T) {
	srv := startTLSTestServer(t)
	client := srv.client(t, "")
	cookie := logIn(t, srv, client, issueOwnerToken(t, srv))

	resp, _ := do(t, client, withCookie(formRequest(t, srv.url+"/logout", nil), cookie))
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("logout: status %d, location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if cleared := resp.Cookies(); len(cleared) != 1 || cleared[0].Name != sessionCookie || cleared[0].MaxAge >= 0 {
		t.Errorf("logout cookies = %v, want %s cleared", cleared, sessionCookie)
	}
	requireSentToLogin(t, client, withCookie(newRequest(t, http.MethodGet, srv.url+"/"), cookie))
}

func TestGivenSessionWhenANewTokenIsIssuedThenTheSessionAndTheOldTokenStopWorking(t *testing.T) {
	srv := startTLSTestServer(t)
	client := srv.client(t, "")
	old := issueOwnerToken(t, srv)
	cookie := logIn(t, srv, client, old)

	issueOwnerToken(t, srv)

	requireSentToLogin(t, client, withCookie(newRequest(t, http.MethodGet, srv.url+"/"), cookie))
	resp, _ := do(t, client, formRequest(t, srv.url+"/login", url.Values{"token": {old}}))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("login with the old token: status %d, want 401", resp.StatusCode)
	}
}

func TestGivenExpiredSessionWhenAPageIsRequestedThenTheBrowserIsSentToTheLoginForm(t *testing.T) {
	srv := startTLSTestServer(t)
	token := issueOwnerToken(t, srv)
	session, err := srv.store.login(t.Context(), token, time.Now().Add(-sessionLifetime-time.Second))
	if err != nil {
		t.Fatal(err)
	}

	cookie := &http.Cookie{Name: sessionCookie, Value: session}
	requireSentToLogin(t, srv.client(t, ""), withCookie(newRequest(t, http.MethodGet, srv.url+"/"), cookie))
}

func TestGivenNoSessionWhenTheLoginFormAndStaticFilesAreRequestedThenTheyAreServed(t *testing.T) {
	srv := startTLSTestServer(t)
	issueOwnerToken(t, srv)
	client := srv.client(t, "")

	for _, path := range []string{"/login", "/static/style.css", "/static/htmx.min.js"} {
		if resp, _ := do(t, client, newRequest(t, http.MethodGet, srv.url+path)); resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d, want 200", path, resp.StatusCode)
		}
	}
}

func TestGivenNoTokenIssuedWhenLoggingInWithAnEmptyTokenThenItIsRefused(t *testing.T) {
	srv := startTLSTestServer(t)

	resp, _ := do(t, srv.client(t, ""), formRequest(t, srv.url+"/login", url.Values{"token": {""}}))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", resp.StatusCode)
	}
	if has, err := srv.store.HasOwnerToken(t.Context()); err != nil || has {
		t.Errorf("HasOwnerToken = %v, %v; want false", has, err)
	}
}
