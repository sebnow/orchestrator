package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

const testLoginURL = "https://claude.com/cai/oauth/authorize?code=true&state=STATE"

// daemonLoginView reads the daemon's login from the owner API.
func daemonLoginView(t *testing.T, srv testServer, daemon string) *loginView {
	t.Helper()
	status, body := doRequest(t, http.MethodGet, srv.url+"/v1/daemons/"+daemon, "")
	if status != http.StatusOK {
		t.Fatalf("GET daemon: %d %s", status, body)
	}
	var view daemonView
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatal(err)
	}
	return view.Login
}

func postLoginEventTo(t *testing.T, srv testServer, daemon string, event protocol.LoginEvent) {
	t.Helper()
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if status, response := doRequest(t, http.MethodPost, srv.url+"/v1/daemons/"+daemon+"/login-events", string(body)); status != http.StatusNoContent {
		t.Fatalf("POST login event: %d %s", status, response)
	}
}

func TestGivenConnectedDaemonWhenTheOwnerLogsItInThroughTheAPIThenTheCommandsAndEventsCarryTheLoginThrough(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodPut, srv.url+"/v1/daemons/vps/facts", `{"login":"no","login_method":"none"}`)
	d := connectDaemon(t, srv, "vps")

	status, body := doRequest(t, http.MethodPost, srv.url+"/v1/daemons/vps/login", "")
	if status != http.StatusAccepted {
		t.Fatalf("POST login: %d %s", status, body)
	}
	login := receiveCommand(t, d.commands)
	if login.Kind != protocol.CommandLogin || login.TaskID != "" || login.Payload != nil {
		t.Fatalf("command = %+v, want a login to the daemon itself", login)
	}
	if got := daemonLoginView(t, srv, "vps"); got == nil || got.Phase != loginRequested || got.Command != login.ID {
		t.Errorf("login = %+v, want requested by command %d", got, login.ID)
	}
	if status, _ := doRequest(t, http.MethodPost, srv.url+"/v1/daemons/vps/login/code", `{"code":"too-early"}`); status != http.StatusConflict {
		t.Errorf("code before the URL: status %d, want 409", status)
	}

	postLoginEventTo(t, srv, "vps", protocol.LoginEvent{Kind: protocol.KindLoginStarted, Login: login.ID, URL: testLoginURL})
	if got := daemonLoginView(t, srv, "vps"); got.Phase != loginStarted || got.URL != testLoginURL {
		t.Errorf("login = %+v, want started at the URL", got)
	}
	if status, body := doRequest(t, http.MethodPost, srv.url+"/v1/daemons/vps/login/code", `{"code":" code#STATE \n"}`); status != http.StatusAccepted {
		t.Fatalf("POST code: %d %s", status, body)
	}
	code := receiveCommand(t, d.commands)
	var payload protocol.LoginCode
	if err := json.Unmarshal(code.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if code.Kind != protocol.CommandLoginCode || code.TaskID != "" || payload != (protocol.LoginCode{Login: login.ID, Code: "code#STATE"}) {
		t.Errorf("command = %+v carrying %+v, want the trimmed code for login %d", code, payload, login.ID)
	}
	if status, _ := doRequest(t, http.MethodPost, srv.url+"/v1/daemons/vps/login/code", `{"code":"again"}`); status != http.StatusConflict {
		t.Errorf("a second code: status %d, want 409", status)
	}

	doRequest(t, http.MethodPut, srv.url+"/v1/daemons/vps/facts", `{"login":"yes","login_method":"claude.ai","account":"owner@example.com/org-1"}`)
	postLoginEventTo(t, srv, "vps", protocol.LoginEvent{Kind: protocol.KindLoginFinished, Login: login.ID, OK: true})
	if got := daemonLoginView(t, srv, "vps"); got.Phase != loginFinished || !got.OK || got.Error != "" {
		t.Errorf("login = %+v, want finished ok", got)
	}
	stored, err := srv.store.commandsAfter(t.Context(), "vps", login.Position())
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || strings.Contains(string(stored[0].Payload), "code#STATE") {
		t.Errorf("stored commands = %s, want the code forgotten once the login ended", stored[0].Payload)
	}
}

func TestGivenEventOfALoginOlderThanTheLatestWhenPostedThenItIsDropped(t *testing.T) {
	srv := startTestServer(t)
	connectDaemon(t, srv, "vps")
	doRequest(t, http.MethodPost, srv.url+"/v1/daemons/vps/login", "")
	doRequest(t, http.MethodPost, srv.url+"/v1/daemons/vps/login", "")
	latest := daemonLoginView(t, srv, "vps").Command

	postLoginEventTo(t, srv, "vps", protocol.LoginEvent{Kind: protocol.KindLoginFinished, Login: latest - 1, Error: "a newer login replaced this one"})

	if got := daemonLoginView(t, srv, "vps"); got.Command != latest || got.Phase != loginRequested {
		t.Errorf("login = %+v, want the latest still requested", got)
	}
}

func TestGivenDaemonThatIsNotConnectedWhenTheOwnerLogsItInThenItIsRefused(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodPut, srv.url+"/v1/daemons/vps/facts", `{"login":"no"}`)

	status, _ := doRequest(t, http.MethodPost, srv.url+"/v1/daemons/vps/login", "")
	unknown, _ := doRequest(t, http.MethodPost, srv.url+"/v1/daemons/nowhere/login", "")
	refused := send(t, http.MethodPost, srv.url+"/daemons/vps/login", url.Values{}, false)

	if status != http.StatusConflict || unknown != http.StatusNotFound {
		t.Errorf("status = %d, unknown daemon %d; want 409 and 404", status, unknown)
	}
	if refused.status != http.StatusConflict {
		t.Errorf("form: status = %d, want 409", refused.status)
	}
	requireContains(t, refused.body, "Not done: the daemon is not connected.", "log it in once it is")
}

func TestGivenMalformedLoginEventWhenPostedThenBadRequest(t *testing.T) {
	srv := startTestServer(t)
	for name, body := range map[string]string{
		"no login":     `{"kind":"login_started","url":"https://x"}`,
		"unknown kind": `{"kind":"harness_started","login":1}`,
		"extra field":  `{"kind":"login_started","login":1,"extra":true}`,
	} {
		if status, _ := doRequest(t, http.MethodPost, srv.url+"/v1/daemons/vps/login-events", body); status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, status)
		}
	}
}

func TestGivenDaemonPageWhenTheOwnerLogsInWithFormsThenTheLoginSectionFollowsAndItsStreamSendsChanges(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodPut, srv.url+"/v1/daemons/vps/facts", `{"login":"no","login_method":"none"}`)
	d := connectDaemon(t, srv, "vps")
	stream := openEventStream(t, srv.url+"/daemons/vps/stream", "")
	first := receiveEvent(t, stream)
	if !strings.Contains(first.data, "login needed") {
		t.Errorf("first event = %q, want the login section", first.data)
	}

	page := getPage(t, srv.url+"/daemons/vps")
	requireContains(t, page, `<section id="login">`, `sse-connect="/daemons/vps/stream"`, `hx-target="#daemon-login"`, "login needed",
		`hx-post="/daemons/vps/login"`)

	started := send(t, http.MethodPost, srv.url+"/daemons/vps/login", url.Values{}, true)
	if started.status != http.StatusOK {
		t.Fatalf("htmx login: %d %s", started.status, started.body)
	}
	requireContains(t, started.body, `hx-swap-oob="innerHTML:#daemon-login"`, "Waiting for the daemon to start the login.")
	login := receiveCommand(t, d.commands)
	requireContains(t, receiveEvent(t, stream).data, "Waiting for the daemon")

	postLoginEventTo(t, srv, "vps", protocol.LoginEvent{Kind: protocol.KindLoginStarted, Login: login.ID, URL: testLoginURL})
	requireContains(t, receiveEvent(t, stream).data, "If a browser opened on the daemon&#39;s machine, authorise there. Otherwise ",
		`href="`+strings.ReplaceAll(testLoginURL, "&", "&amp;")+`"`, ">open this link</a>, authorise, and paste the code shown.", `hx-post="/daemons/vps/login/code"`)

	plain := send(t, http.MethodPost, srv.url+"/daemons/vps/login/code", url.Values{"code": {"code#STATE"}}, false)
	if plain.status != http.StatusSeeOther || plain.header.Get("Location") != "/daemons/vps#login" {
		t.Errorf("code without JavaScript: %d to %q", plain.status, plain.header.Get("Location"))
	}
	if code := receiveCommand(t, d.commands); code.Kind != protocol.CommandLoginCode {
		t.Errorf("command = %+v, want the code", code)
	}
	requireContains(t, receiveEvent(t, stream).data, "Code sent")

	postLoginEventTo(t, srv, "vps", protocol.LoginEvent{Kind: protocol.KindLoginFinished, Login: login.ID, Error: "Login failed: Request failed with status code 400"})
	requireContains(t, receiveEvent(t, stream).data, "The login failed: Login failed: Request failed with status code 400")
}

func TestGivenLoginThatSucceededWhenTheDaemonPageIsShownThenItNamesTheAccountTheHarnessReports(t *testing.T) {
	for name, tc := range map[string]struct {
		facts, want string
	}{
		"account":    {facts: `{"login":"yes","login_method":"claude.ai","account":"owner@example.com/org-1"}`, want: "The login succeeded: logged in as <code>owner@example.com/org-1</code>."},
		"no account": {facts: `{"login":"yes","login_method":"claude.ai"}`, want: "The login succeeded; the harness reports no account yet."},
	} {
		t.Run(name, func(t *testing.T) {
			srv := startTestServer(t)
			d := connectDaemon(t, srv, "vps")
			doRequest(t, http.MethodPost, srv.url+"/v1/daemons/vps/login", "")
			login := receiveCommand(t, d.commands)

			doRequest(t, http.MethodPut, srv.url+"/v1/daemons/vps/facts", tc.facts)
			postLoginEventTo(t, srv, "vps", protocol.LoginEvent{Kind: protocol.KindLoginFinished, Login: login.ID, OK: true})

			requireContains(t, getPage(t, srv.url+"/daemons/vps"), tc.want)
		})
	}
}

// receiveEvent returns the next event of a stream within five seconds.
func receiveEvent(t *testing.T, events <-chan sseEvent) sseEvent {
	t.Helper()
	select {
	case event, ok := <-events:
		if !ok {
			t.Fatal("stream ended")
		}
		if _, err := strconv.Atoi(event.id); err != nil {
			t.Errorf("event id %q", event.id)
		}
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5s")
	}
	return sseEvent{}
}
