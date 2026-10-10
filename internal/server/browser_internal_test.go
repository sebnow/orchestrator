//go:build browser

package server

import (
	"context"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/browsertest"
	"github.com/sebnow/orchestrator/internal/pki"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// seenRequest is a request the server under a browser test received.
type seenRequest struct {
	method, path string
	htmx         bool
	session      bool
	status       int
	at           time.Time
}

// requestLog records every request the server receives, so that a test
// can tell an htmx request from a navigation.
type requestLog struct {
	mu   sync.Mutex
	seen []seenRequest
	// failStreams makes every task stream answer 503, as a proxy that
	// does not pass server-sent events might.
	failStreams atomic.Bool
}

// newRequestLog returns an empty log that is printed if the test fails.
func newRequestLog(t *testing.T) *requestLog {
	l := &requestLog{}
	t.Cleanup(func() {
		if t.Failed() {
			for _, req := range l.requests() {
				t.Logf("request: %+v", req)
			}
		}
	})
	return l
}

func (l *requestLog) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := r.Cookie(sessionCookie)
		l.mu.Lock()
		idx := len(l.seen)
		l.seen = append(l.seen, seenRequest{method: r.Method, path: r.URL.Path, htmx: fromHTMX(r), session: err == nil, at: time.Now()})
		l.mu.Unlock()
		w = &statusWriter{ResponseWriter: w, log: l, idx: idx}
		if l.failStreams.Load() && strings.HasSuffix(r.URL.Path, "/stream") {
			http.Error(w, "streams are off", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (l *requestLog) requests() []seenRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]seenRequest(nil), l.seen...)
}

// matching returns the requests with method to path.
func (l *requestLog) matching(method, path string) []seenRequest {
	var found []seenRequest
	for _, req := range l.requests() {
		if req.method == method && req.path == path {
			found = append(found, req)
		}
	}
	return found
}

// statusWriter notes the status of a response. Unwrap lets the task
// stream flush through it.
type statusWriter struct {
	http.ResponseWriter
	log *requestLog
	idx int
}

func (w *statusWriter) WriteHeader(status int) {
	w.log.mu.Lock()
	w.log.seen[w.idx].status = status
	w.log.mu.Unlock()
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// startBrowserServer is startTestServer, insecure on loopback with the
// scheduler running, with every request logged.
func startBrowserServer(t *testing.T) (testServer, *requestLog) {
	t.Helper()
	store, _ := openTestStore(t)
	logs := &syncBuffer{}
	srv := New(store, slog.New(slog.NewTextHandler(logs, nil)), Options{DefaultModel: testDefaultModel, Insecure: true})
	requests := newRequestLog(t)
	httpServer := httptest.NewServer(requests.wrap(srv))
	ctx, stopScheduling := context.WithCancel(context.Background())
	scheduled := make(chan struct{})
	go func() {
		defer close(scheduled)
		srv.Schedule(ctx)
	}()
	t.Cleanup(func() {
		srv.EndStreams()
		httpServer.Close()
		stopScheduling()
		<-scheduled
	})
	return testServer{Server: srv, url: httpServer.URL, logs: logs}, requests
}

// startBrowserTLSServer is startTLSTestServer, with every request logged.
func startBrowserTLSServer(t *testing.T) (tlsTestServer, *requestLog) {
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
	requests := newRequestLog(t)
	httpServer := httptest.NewUnstartedServer(requests.wrap(srv))
	httpServer.TLS = pki.ServerConfig(cert, ca.Pool())
	httpServer.EnableHTTP2 = true
	httpServer.StartTLS()
	t.Cleanup(func() {
		srv.EndStreams()
		httpServer.Close()
	})
	return tlsTestServer{testServer: testServer{Server: srv, url: httpServer.URL, logs: logs}, ca: ca}, requests
}

// openPage opens a tab in a new headless browser with flags.
func openPage(t *testing.T, flags ...string) *browsertest.Page {
	t.Helper()
	return browsertest.Launch(t, flags...).NewPage()
}

// markLoaded marks the page's document, so that requireNotReloaded can
// tell whether the browser has since loaded another.
func markLoaded(page *browsertest.Page) {
	page.MustEval(`window.loadedOnce = true`, nil)
}

func requireNotReloaded(t *testing.T, page *browsertest.Page) {
	t.Helper()
	var marked bool
	page.MustEval(`window.loadedOnce === true`, &marked)
	if !marked {
		page.Screenshot("reloaded")
		t.Fatal("the page was reloaded")
	}
}

// requireHTMXPosts checks that every POST to path came from htmx.
func requireHTMXPosts(t *testing.T, requests *requestLog, path string, want int) {
	t.Helper()
	posts := requests.matching(http.MethodPost, path)
	if len(posts) != want {
		t.Fatalf("%d POSTs to %s, want %d", len(posts), path, want)
	}
	for _, post := range posts {
		if !post.htmx || post.status != http.StatusOK {
			t.Errorf("POST %s: htmx %t, status %d; want htmx and 200", path, post.htmx, post.status)
		}
	}
}

// js quotes s as a JavaScript string.
func js(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\n", `\n`).Replace(s) + "'"
}

// hasElement is a JavaScript condition that selector matches an element.
func hasElement(selector string) string {
	return "document.querySelector(" + js(selector) + ") !== null"
}

// hasText is a JavaScript condition that the element selector matches
// contains text.
func hasText(selector, text string) string {
	return "(document.querySelector(" + js(selector) + ")?.textContent ?? '').includes(" + js(text) + ")"
}

// clickSettled clicks selector once htmx has settled every swap. htmx
// processes swapped-in content, and so takes over its forms, only when
// the swap settles, after htmx.config.defaultSettleDelay; a click before
// then submits the form without htmx.
func clickSettled(page *browsertest.Page, selector string) {
	page.WaitTrue(`document.querySelector('.htmx-added, .htmx-settling') === null`)
	page.Click(selector)
}

func TestGivenDashboardWhenABrowserLoadsItThenHTMXAndItsSSEExtensionHaveRun(t *testing.T) {
	srv, _ := startBrowserServer(t)
	page := openPage(t)

	page.Navigate(srv.url + "/")

	var version string
	page.MustEval(`window.htmx ? htmx.version : ""`, &version)
	if version != "2.0.11" {
		t.Errorf("htmx.version = %q, want 2.0.11", version)
	}
	// The extension's init, which htmx calls when the extension is
	// defined, adds createEventSource.
	var sse bool
	page.MustEval(`typeof htmx.createEventSource === "function"`, &sse)
	if !sse {
		t.Error("the sse extension has not been defined")
	}
}

func TestGivenTaskPageWhenANewEventArrivesThenTheTranscriptShowsItOverSSEWithoutAReload(t *testing.T) {
	srv, requests := startBrowserServer(t)
	task, events := runningTask(t, srv)
	page := openPage(t)
	page.Navigate(srv.url + "/tasks/" + string(task))
	markLoaded(page)
	// htmx keeps the extension's EventSource in the element's internal
	// data; readyState 1 is OPEN.
	page.WaitTrue(`document.querySelector('[sse-connect]')?.['htmx-internal-data']?.sseEventSource?.readyState === 1`)

	events.add(protocol.KindHarnessOutput, `{"type":"assistant","message":{"content":[{"type":"text","text":"BROWSER-SSE-1"}]}}`)
	events.ingest(t, srv, "laptop")

	page.WaitTrue(hasText("#transcript li.entry.from-agent:last-child", "BROWSER-SSE-1"))
	requireNotReloaded(t, page)
	if got := len(requests.matching(http.MethodGet, "/tasks/"+string(task))); got != 1 {
		t.Errorf("task page loaded %d times, want once", got)
	}
	if got := len(requests.matching(http.MethodGet, "/tasks/"+string(task)+"/stream")); got != 1 {
		t.Errorf("stream opened %d times, want once", got)
	}
	if got := requests.matching(http.MethodGet, "/tasks/"+string(task)+"/updates"); len(got) != 0 {
		t.Errorf("the page polled %d times, want none", len(got))
	}
}

func TestGivenTaskPageWhenASubagentsEntryArrivesThenItIsAppendedUnderTheToolCallThatStartedIt(t *testing.T) {
	srv, _ := startBrowserServer(t)
	task, events := runningTask(t, srv)
	events.add(protocol.KindHarnessOutput, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_sub","name":"Agent","input":{"description":"Look around"}}]},"parent_tool_use_id":null}`)
	events.ingest(t, srv, "laptop")
	page := openPage(t)
	page.Navigate(srv.url + "/tasks/" + string(task))
	markLoaded(page)
	page.WaitTrue(`document.querySelector('[sse-connect]')?.['htmx-internal-data']?.sseEventSource?.readyState === 1`)
	subagentHidden := `getComputedStyle(document.getElementById(` + js("subagent-"+hex.EncodeToString([]byte("toolu_sub"))) + `).closest('details')).display === 'none'`
	var hidden bool
	page.MustEval(subagentHidden, &hidden)
	if !hidden {
		t.Error("the empty subagent list is shown")
	}

	events.add(protocol.KindHarnessOutput, `{"type":"assistant","message":{"content":[{"type":"text","text":"SUBAGENT-1"}]},"parent_tool_use_id":"toolu_sub"}`)
	events.add(protocol.KindHarnessOutput, `{"type":"assistant","message":{"content":[{"type":"text","text":"MAIN-1"}]},"parent_tool_use_id":null}`)
	events.ingest(t, srv, "laptop")

	page.WaitTrue(hasText("#transcript > li.entry:last-child", "MAIN-1"))
	page.WaitTrue(hasText("#transcript li.entry details.subagent > ol > li.entry", "SUBAGENT-1"))
	page.MustEval(subagentHidden, &hidden)
	if hidden {
		t.Error("the subagent list is hidden once it has an entry")
	}
	requireNotReloaded(t, page)
}

// waitRequests waits until the server has received at least want
// requests with method to path, and returns them.
func waitRequests(t *testing.T, requests *requestLog, method, path string, want int) []seenRequest {
	t.Helper()
	deadline := time.Now().Add(browsertest.Timeout)
	for {
		got := requests.matching(method, path)
		if len(got) >= want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d requests %s %s within %s, want %d", len(got), method, path, browsertest.Timeout, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestGivenTaskPageWhoseStreamFailsWhenAFormIsPostedThenThePollerShowsTheChangeAtOnce(t *testing.T) {
	srv, requests := startBrowserServer(t)
	requests.failStreams.Store(true)
	task, _ := runningTask(t, srv)
	updatesPath := "/tasks/" + string(task) + "/updates"
	page := openPage(t)
	page.Navigate(srv.url + "/tasks/" + string(task))
	markLoaded(page)
	// The stream's failure fetches the updates once and starts the poller,
	// whose first poll comes five seconds later. The next is five seconds
	// after that, so a poll soon after the form's response is the form's.
	page.WaitTrue(hasElement(`#task-live [hx-trigger^="every 5s"]`))
	waitRequests(t, requests, http.MethodGet, updatesPath, 2)

	clickSettled(page, `#task-header button[value="pause"]`)

	page.WaitTrue(hasElement("#task-header .badge.state-pausing"))
	post := waitRequests(t, requests, http.MethodPost, "/tasks/"+string(task)+"/commands", 1)[0]
	polls := requests.matching(http.MethodGet, updatesPath)
	if len(polls) < 3 || polls[2].at.Before(post.at) || polls[2].at.Sub(post.at) > time.Second {
		t.Errorf("polls = %+v after the POST at %s, want one within a second of it", polls, post.at)
	}
	requireNotReloaded(t, page)
}

// receiveCommandOf skips commands until one of kind arrives.
func receiveCommandOf(t *testing.T, commands <-chan sseEvent, kind protocol.CommandKind) protocol.Command {
	t.Helper()
	for {
		if command := receiveCommand(t, commands); command.Kind == kind {
			return command
		}
	}
}

func TestGivenTaskPageWhenItsFormsArePostedThenHTMXUpdatesThePageInPlace(t *testing.T) {
	srv, requests := startBrowserServer(t)
	task := startTaskViaForm(t, srv, "laptop", "Touch two files")
	// The fake daemon connects, so the scheduler admits queued turns.
	commands := openCommandStream(t, srv, "laptop", "")
	receiveCommandOf(t, commands, protocol.CommandStartTask)
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
	events.add(protocol.KindPermissionRequested, bashRequest)
	events.ingest(t, srv, "laptop")
	commandsPath := "/tasks/" + string(task) + "/commands"
	page := openPage(t)
	page.Navigate(srv.url + "/tasks/" + string(task))
	markLoaded(page)
	page.WaitTrue(hasElement("#task-header .badge.state-awaiting"))

	clickSettled(page, `#permission button[value="allow"]`)
	page.WaitTrue(hasElement("#task-header .badge.state-running") + " && " + `document.querySelector('#permission').children.length === 0`)
	if answer := receiveCommandOf(t, commands, protocol.CommandAnswerPermission); !strings.Contains(string(answer.Payload), `"allow":true`) {
		t.Errorf("answer = %s, want an allowance", answer.Payload)
	}

	clickSettled(page, `#task-header button[value="pause"]`)
	page.WaitTrue(hasElement("#task-header .badge.state-pausing") + " && " + hasElement("#prompt-submit button[disabled]"))
	receiveCommandOf(t, commands, protocol.CommandPause)
	events.add(protocol.KindPauseAcknowledged, `{"note":"after step 1"}`)
	events.add(protocol.KindPauseSettled, `{"interrupted":false}`)
	events.add(protocol.KindHarnessExited, `{"exit_code":0}`)
	events.ingest(t, srv, "laptop")
	page.WaitTrue(hasElement("#task-header .badge.state-paused") + " && " + hasElement(`#task-header button[value="resume"]`))

	clickSettled(page, `#task-header button[value="resume"]`)
	page.WaitTrue(hasElement("#task-header .badge.state-running"))
	receiveCommandOf(t, commands, protocol.CommandResume)
	events.add(protocol.KindHarnessStarted, `{"pid":8,"model":"haiku","workdir":"/w"}`)
	events.add(protocol.KindHarnessExited, `{"exit_code":0}`)
	events.ingest(t, srv, "laptop")
	page.WaitTrue(hasElement("#task-header .badge.state-finished"))

	page.Type(`#prompt textarea[name="text"]`, "Touch a third file")
	clickSettled(page, "#prompt-submit button")
	page.WaitTrue(hasElement("#task-header .badge.state-running") + " && " + `document.querySelector('#prompt textarea[name="text"]').value === ''`)
	if prompt := receiveCommandOf(t, commands, protocol.CommandPrompt); string(prompt.Payload) != `{"text":"Touch a third file"}` {
		t.Errorf("prompt = %s", prompt.Payload)
	}
	events.add(protocol.KindHarnessExited, `{"exit_code":0}`)
	events.ingest(t, srv, "laptop")
	page.WaitTrue(hasElement("#task-header .badge.state-finished"))

	clickSettled(page, `#task-header button[value="stop"]`)
	page.WaitTrue(hasElement("#task-header .badge.state-stopped") + " && " + hasText("#task-header .dismiss button", "Dismiss"))

	requireNotReloaded(t, page)
	requireHTMXPosts(t, requests, commandsPath, 5)
	if got := len(requests.matching(http.MethodGet, "/tasks/"+string(task))); got != 1 {
		t.Errorf("task page loaded %d times, want once", got)
	}
}

func TestGivenRunningTaskPageWhenAPromptIsSentAfterTheTurnOrNowThenItIsListedAsQueuedWithdrawableAndNowSteers(t *testing.T) {
	srv, requests := startBrowserServer(t)
	task := startTaskViaForm(t, srv, "laptop", "Count to twenty slowly")
	commands := openCommandStream(t, srv, "laptop", "")
	receiveCommandOf(t, commands, protocol.CommandStartTask)
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
	events.ingest(t, srv, "laptop")
	page := openPage(t)
	page.Navigate(srv.url + "/tasks/" + string(task))
	markLoaded(page)
	page.WaitTrue(hasElement("#task-header .badge.state-running") + " && " + hasElement(`#prompt-submit button[value="now"]`))

	page.Type(`#prompt textarea[name="text"]`, "Then count back down")
	clickSettled(page, `#prompt-submit button[name="steer"][value=""]`)
	prompt := receiveCommandOf(t, commands, protocol.CommandPrompt)
	if string(prompt.Payload) != `{"text":"Then count back down"}` {
		t.Errorf("prompt = %s, want it sent after the turn", prompt.Payload)
	}
	ref := strconv.FormatUint(prompt.ID, 10)
	events.add(protocol.KindPromptHeld, `{"prompt":`+ref+`}`)
	events.ingest(t, srv, "laptop")
	page.WaitTrue(hasText("#task-queued", "held by the daemon until the turn ends") + " && " + hasText("#task-queued", "Then count back down"))

	clickSettled(page, `#task-queued button`)
	if withdraw := receiveCommandOf(t, commands, protocol.CommandWithdraw); string(withdraw.Payload) != `{"prompt":`+ref+`}` {
		t.Errorf("withdraw = %s", withdraw.Payload)
	}
	events.add(protocol.KindPromptReleased, `{"prompt":`+ref+`,"outcome":"withdrawn"}`)
	events.ingest(t, srv, "laptop")
	page.WaitTrue(hasText("#task-queued", "No prompt waits.") + " && " + hasText("#transcript", "Owner withdrew a held prompt"))

	page.Type(`#prompt textarea[name="text"]`, "Stop at ten")
	clickSettled(page, `#prompt-submit button[value="now"]`)
	if steer := receiveCommandOf(t, commands, protocol.CommandPrompt); string(steer.Payload) != `{"text":"Stop at ten","steer":true}` {
		t.Errorf("steering prompt = %s", steer.Payload)
	}
	page.WaitTrue(hasText("#transcript", "Owner steered the task"))

	requireNotReloaded(t, page)
	requireHTMXPosts(t, requests, "/tasks/"+string(task)+"/commands", 3)
}

func TestGivenFailedTaskOnTheDashboardWhenDismissedThenItLeavesTheAttentionListUntilDismissedTasksAreShown(t *testing.T) {
	srv, requests := startBrowserServer(t)
	task := failedTask(t, srv, "Doomed work")
	page := openPage(t)
	page.Navigate(srv.url + "/")
	markLoaded(page)
	page.WaitTrue(hasText("#dashboard .attention", "Doomed work"))

	clickSettled(page, "#dashboard .attention .dismiss button")

	page.WaitTrue(hasText("#dashboard", "Nothing needs attention.") + " && !" + hasText("#dashboard", "Doomed work"))
	requireNotReloaded(t, page)
	requireHTMXPosts(t, requests, "/tasks/"+string(task)+"/dismiss", 1)

	page.Navigate(srv.url + "/?dismissed=show")

	page.WaitTrue(hasText("#dashboard table", "Doomed work") + " && " + hasText("#dashboard table", "dismissed") +
		" && " + hasText("#dashboard", "Nothing needs attention."))
}

func TestGivenOwnerSignedInWhenTheDashboardURLIsTypedIntoTheAddressBarThenTheSessionCookieIsSentAndTheDashboardShown(t *testing.T) {
	srv, requests := startBrowserTLSServer(t)
	token := issueOwnerToken(t, srv)
	// The server's certificate is from a CA the browser does not trust.
	page := openPage(t, "--ignore-certificate-errors")
	page.Navigate(srv.url + "/")
	page.WaitTrue(`location.pathname === '/login'`)
	page.Type(`input[name="token"]`, token)
	page.LoadsAfter(func() { page.Click(`form[action="/login"] button`) })
	page.WaitTrue(`location.pathname === '/' && document.title === 'Tasks · orchestrator'`)
	var session *browsertest.Cookie
	for _, cookie := range page.Cookies(srv.url + "/") {
		if cookie.Name == sessionCookie {
			session = &cookie
		}
	}
	if session == nil || !session.Secure || !session.HTTPOnly || session.SameSite != "Strict" || session.Path != "/" {
		t.Fatalf("session cookie = %+v, want Secure, HttpOnly, SameSite=Strict, Path=/", session)
	}
	before := len(requests.matching(http.MethodGet, "/"))

	page.Navigate(srv.url + "/")

	page.WaitTrue(`location.pathname === '/' && document.title === 'Tasks · orchestrator'`)
	typed := requests.matching(http.MethodGet, "/")[before:]
	if len(typed) == 0 || !typed[0].session || typed[0].status != http.StatusOK {
		t.Errorf("typed navigation = %+v, want a 200 with the session cookie", typed)
	}
}

func TestGivenJavaScriptOffWhenTheTaskPageIsUsedThenItShowsTheTranscriptAndItsFormsPostAndRedirect(t *testing.T) {
	srv, requests := startBrowserServer(t)
	task, _ := runningTask(t, srv)
	taskPath := "/tasks/" + string(task)
	page := openPage(t)
	page.SetScriptsDisabled(true)

	page.Navigate(srv.url + taskPath)

	if transcript := page.HTML("#transcript"); !strings.Contains(transcript, "DONE-1") || !strings.Contains(transcript, "DONE-3") {
		t.Errorf("transcript lacks the run's output: %s", transcript)
	}
	page.Type(`#prompt textarea[name="text"]`, "And once more")
	page.LoadsAfter(func() { page.Click("#prompt-submit button") })
	// A running task's turn is admitted at once, so the prompt is issued.
	if transcript := page.HTML("#transcript"); !strings.Contains(transcript, "And once more") {
		t.Errorf("transcript after the prompt lacks it: %s", transcript)
	}
	page.LoadsAfter(func() { page.Click(`#task-header button[value="pause"]`) })
	if header := page.HTML("#task-header"); !strings.Contains(header, `class="badge state-pausing"`) {
		t.Errorf("header after pausing lacks the pausing badge: %s", header)
	}

	posts := requests.matching(http.MethodPost, taskPath+"/commands")
	if len(posts) != 2 {
		t.Fatalf("%d POSTs to the commands, want 2", len(posts))
	}
	for _, post := range posts {
		if post.htmx || post.status != http.StatusSeeOther {
			t.Errorf("POST: htmx %t, status %d; want a plain POST answered 303", post.htmx, post.status)
		}
	}
	if got := len(requests.matching(http.MethodGet, taskPath)); got != 3 {
		t.Errorf("task page loaded %d times, want 3: at first and after each POST", got)
	}
	if got := requests.matching(http.MethodGet, taskPath+"/stream"); len(got) != 0 {
		t.Errorf("the page opened the stream %d times without JavaScript", len(got))
	}
}

func TestGivenTaskPageWhenTheTaskSpawnsAChildThenItsTreeShowsTheChildsPurposeWithoutAReload(t *testing.T) {
	srv, _ := startBrowserServer(t)
	task, _ := runningTask(t, srv)
	page := openPage(t)
	page.Navigate(srv.url + "/tasks/" + string(task))
	markLoaded(page)
	page.WaitTrue(`document.querySelector('[sse-connect]')?.['htmx-internal-data']?.sseEventSource?.readyState === 1`)

	status, body := postAgentRequest(t, srv, "laptop", task, protocol.AgentSpawn, protocol.Spawn{Purpose: "BROWSER-TREE-1", Prompt: "Say PEAR."})
	if status != http.StatusOK {
		t.Fatalf("spawn: %d %s", status, body)
	}

	page.WaitTrue(hasText("#task-tree ul.tree > li > ul > li > a", "BROWSER-TREE-1"))
	requireNotReloaded(t, page)
}
