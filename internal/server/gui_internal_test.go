package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/component"
	"github.com/sebnow/orchestrator/internal/harness/claude"
	"github.com/sebnow/orchestrator/internal/html"
	"github.com/sebnow/orchestrator/internal/protocol"
)

const fixtureRuns = "../../spikes/mod-vs-stdout/runs"

// noRedirects is a client that reports a redirect instead of following
// it, as the GUI's tests check where a form sends the browser.
var noRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

type response struct {
	status int
	header http.Header
	body   string
}

func send(t *testing.T, method, target string, form url.Values, htmx bool) response {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request, err := http.NewRequestWithContext(t.Context(), method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if htmx {
		request.Header.Set("HX-Request", "true")
	}
	got, err := noRedirects.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	data, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{status: got.StatusCode, header: got.Header, body: string(data)}
}

func getPage(t *testing.T, target string) string {
	t.Helper()
	got := send(t, http.MethodGet, target, nil, false)
	if got.status != http.StatusOK {
		t.Fatalf("GET %s: status = %d (%s), want 200", target, got.status, got.body)
	}
	if contentType := got.header.Get("Content-Type"); contentType != "text/html; charset=utf-8" {
		t.Fatalf("GET %s: Content-Type = %q", target, contentType)
	}
	return got.body
}

func requireContains(t *testing.T, page string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func requireLacks(t *testing.T, page string, unwanted ...string) {
	t.Helper()
	for _, text := range unwanted {
		if strings.Contains(page, text) {
			t.Errorf("page has %q", text)
		}
	}
}

func startForm(daemon, prompt string) url.Values {
	return url.Values{"prompt": {prompt}, "daemon": {daemon}, "repo": {""}, "ref": {""}, "model": {""},
		"acknowledge": {"1m"}, "cleanup": {"5m"}}
}

// startTaskViaForm has daemon seen, starts a task on it through the
// new-task form, admits its start, and returns the task's id.
func startTaskViaForm(t *testing.T, srv testServer, daemon protocol.DaemonID, prompt string) protocol.TaskID {
	t.Helper()
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/"+string(daemon)+"/acks", "")
	got := send(t, http.MethodPost, srv.url+"/tasks", startForm(string(daemon), prompt), false)
	if got.status != http.StatusSeeOther {
		t.Fatalf("status = %d (%s), want 303", got.status, got.body)
	}
	task, found := strings.CutPrefix(got.header.Get("Location"), "/tasks/")
	if !found {
		t.Fatalf("redirected to %q, want a task page", got.header.Get("Location"))
	}
	admitTurns(t, srv.store)
	return protocol.TaskID(task)
}

// taskEvents builds a task's events from seq on, a second apart.
type taskEvents struct {
	task   protocol.TaskID
	seq    uint64
	events []protocol.Event
}

func (e *taskEvents) add(kind protocol.Kind, payload string) {
	e.seq++
	e.events = append(e.events, protocol.Event{
		TaskID: e.task, Seq: e.seq, Kind: kind,
		Harness: protocol.Harness{Name: claude.Name, Version: "2.1.289"},
		Time:    time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC).Add(time.Duration(e.seq) * time.Second),
		Payload: json.RawMessage(payload),
	})
}

// addFixture adds every line a spike run's harness wrote, as the daemon
// would send them.
func (e *taskEvents) addFixture(t *testing.T, run string) {
	t.Helper()
	file, err := os.Open(filepath.Join(fixtureRuns, run, "stdout.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	lines := bufio.NewScanner(file)
	lines.Buffer(nil, 4<<20)
	for lines.Scan() {
		e.add(protocol.KindHarnessOutput, lines.Text())
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
}

// ingest posts the events added since the last ingest through the
// daemon API.
func (e *taskEvents) ingest(t *testing.T, srv testServer, daemon protocol.DaemonID) {
	t.Helper()
	held := e.events[len(e.events)-1].Seq
	status, body := postEvents(t, srv, daemon, e.events...)
	requireAcks(t, status, body, map[protocol.TaskID]uint64{e.task: held})
	e.events = nil
}

const bashRequest = `{"request_id":"toolu_01KYWDtRqQK6PRLSQzHm7bag","tool":"Bash",` +
	`"input":{"command":"touch spike-allowed.txt","description":"Create spike-allowed.txt file"}}`

func TestGivenTasksAndAQuotaReadingWhenDashboardRequestedThenAttentionTasksDaemonsAndTheFormAreShown(t *testing.T) {
	srv := startTestServer(t)
	paused := startTaskViaForm(t, srv, "laptop", "Run ping five times\nthen say DONE")
	waiting := startTaskViaForm(t, srv, "laptop", "Touch two files")
	pausedEvents := &taskEvents{task: paused}
	pausedEvents.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
	pausedEvents.addFixture(t, "pause-stdin")
	pausedEvents.add(protocol.KindQuotaObserved, `{"status":"allowed","windows":[{"name":"five_hour","utilization":0.42,"resets_at":"2026-10-07T17:00:00Z"}]}`)
	pausedEvents.ingest(t, srv, "laptop")
	postForm(t, srv, paused, url.Values{"kind": {"pause"}})
	pausedEvents.add(protocol.KindPauseAcknowledged, `{"note":"stopped after DONE-1"}`)
	pausedEvents.add(protocol.KindPauseSettled, `{"interrupted":false}`)
	pausedEvents.ingest(t, srv, "laptop")
	waitingEvents := &taskEvents{task: waiting}
	waitingEvents.add(protocol.KindHarnessStarted, `{"pid":8,"model":"haiku","workdir":"/w"}`)
	waitingEvents.add(protocol.KindPermissionRequested, bashRequest)
	waitingEvents.ingest(t, srv, "laptop")

	page := getPage(t, srv.url+"/")

	requireContains(t, page,
		`<h2>Needs attention</h2><ul class="attention">`,
		`<span class="badge state-paused">paused</span> <a href="/tasks/`+string(paused)+`">Run ping five times…</a><span class="reason">paused: stopped after DONE-1</span>`,
		`<a href="/tasks/`+string(waiting)+`">Touch two files</a><span class="reason">asks to run Bash</span>`,
		`<td>laptop</td><td>`+testDefaultModel+`</td><td>$0.0313</td>`,
		`<td><a href="/daemons/laptop">laptop</a></td><td>claude-code 2.1.289</td>`,
		`<span class="badge quota-allowed">allowed</span>`,
		"five hour: ", "42% used",
		`hx-trigger="every 5s"`,
		`<form method="post" action="/tasks"`,
		`<option value="laptop">laptop</option>`,
		`placeholder="the agent&#39;s, or `+testDefaultModel+`"`,
	)
	if waitingAt, pausedAt := strings.Index(page, "Touch two files</a></td>"), strings.Index(page, "Run ping five times…</a></td>"); waitingAt < 0 || pausedAt < waitingAt {
		t.Errorf("task table is not newest first")
	}
}

func TestGivenNothingStoredWhenDashboardRequestedThenItSaysSo(t *testing.T) {
	srv := startTestServer(t)

	page := getPage(t, srv.url+"/")

	requireContains(t, page, "Nothing needs attention.", "No tasks yet.", "No daemon has connected yet.")
}

func postForm(t *testing.T, srv testServer, task protocol.TaskID, form url.Values) {
	t.Helper()
	got := send(t, http.MethodPost, srv.url+"/tasks/"+string(task)+"/commands", form, false)
	if got.status != http.StatusSeeOther || got.header.Get("Location") != "/tasks/"+string(task) {
		t.Fatalf("status = %d to %q (%s), want 303 to the task page", got.status, got.header.Get("Location"), got.body)
	}
}

func TestGivenUnansweredPermissionRequestWhenTaskPageRequestedThenThePromptShowsUntilItIsAnswered(t *testing.T) {
	srv := startTestServer(t)
	task := startTaskViaForm(t, srv, "laptop", "Touch two files")
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
	events.addFixture(t, "mcp")
	events.add(protocol.KindPermissionRequested, bashRequest)
	events.ingest(t, srv, "laptop")
	// The state reads pausing, yet the request still waits.
	postForm(t, srv, task, url.Values{"kind": {"pause"}})

	page := getPage(t, srv.url+"/tasks/"+string(task))

	requireContains(t, page,
		`<span class="badge state-pausing">pausing</span>`,
		`<h2>Permission requested</h2>`,
		`<code>Bash</code>`,
		"&#34;command&#34;: &#34;touch spike-allowed.txt&#34;",
		`<input name="request_id" type="hidden" value="toolu_01KYWDtRqQK6PRLSQzHm7bag">`,
		`<button type="submit" class="primary" name="decision" value="allow">Allow</button>`,
		`<button type="submit" class="danger" name="decision" value="deny">Deny</button>`,
		"Agent called Bash", "Owner asked the agent to pause",
		`<button type="submit" class="primary" disabled="">Send</button>`,
	)

	postForm(t, srv, task, url.Values{"kind": {"answer_permission"}, "request_id": {"toolu_01KYWDtRqQK6PRLSQzHm7bag"}, "decision": {"deny"}, "message": {"not that file"}})
	page = getPage(t, srv.url+"/tasks/"+string(task))

	requireLacks(t, page, `<h2>Permission requested</h2>`, `name="request_id"`)
	requireContains(t, page, "Owner denied request toolu_01KYWDtRqQK6PRLSQzHm7bag", "not that file")
	commands, err := srv.store.commandsAfter(t.Context(), "laptop", protocol.CommandPosition{})
	if err != nil {
		t.Fatal(err)
	}
	last := commands[len(commands)-1]
	if last.Kind != protocol.CommandAnswerPermission || string(last.Payload) != `{"request_id":"toolu_01KYWDtRqQK6PRLSQzHm7bag","allow":false,"message":"not that file"}` {
		t.Errorf("last command = %s %s, want the denial", last.Kind, last.Payload)
	}
}

func TestGivenRunningTaskWhenPauseFormPostedThenAPauseIsIssuedAndTheBrowserSentBackToTheTask(t *testing.T) {
	srv := startTestServer(t)
	task := startTaskViaForm(t, srv, "laptop", "Run ping five times")
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
	events.ingest(t, srv, "laptop")

	postForm(t, srv, task, url.Values{"kind": {"pause"}})

	commands, err := srv.store.commandsAfter(t.Context(), "laptop", protocol.CommandPosition{})
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 || commands[1].Kind != protocol.CommandPause || commands[1].TaskID != task || commands[1].Payload != nil {
		t.Fatalf("commands = %+v, want the start and a pause", commands)
	}
	if got := readProgress(t, srv.store, task).State; got != TaskPausing {
		t.Errorf("state = %s, want pausing", got)
	}
}

// The page's stream, or its poller, is the only writer of the regions it
// keeps current. A form response that swapped them too could arrive after
// a newer stream update, such as the scheduler admitting a resume, and
// leave the page showing the older state until the task next changed.
func TestGivenHTMXWhenPauseFormPostedThenTheStreamNotTheResponseUpdatesTheHeaderAndDisablesPrompting(t *testing.T) {
	srv := startTestServer(t)
	task, events := runningTask(t, srv)
	stream := openEventStream(t, srv.url+"/tasks/"+string(task)+"/stream?after="+strconv.FormatUint(events.seq, 10)+"-1-0", "")

	got := send(t, http.MethodPost, srv.url+"/tasks/"+string(task)+"/commands", url.Values{"kind": {"pause"}}, true)

	if got.status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", got.status, got.body)
	}
	if trigger := got.header.Get("HX-Trigger"); trigger != component.EventTaskChanged {
		t.Errorf("HX-Trigger = %q, want %q so that a polling page polls at once", trigger, component.EventTaskChanged)
	}
	requireLacks(t, got.body, "<html", `value="pause"`, "#task-header", "#permission", "#prompt-submit")
	update := receiveUpdate(t, stream)
	requireContains(t, update.data,
		`<div hx-swap-oob="innerHTML:#task-header"><header class="task-header">`,
		`<span class="badge state-pausing">pausing</span>`,
		`<div hx-swap-oob="innerHTML:#prompt-submit"><button type="submit" class="primary" disabled="">Send</button>`,
	)
}

func TestGivenEmptyFollowUpWhenPostedThenTheFormSaysWhyAndNothingIsIssued(t *testing.T) {
	srv := startTestServer(t)
	task := startTaskViaForm(t, srv, "laptop", "Run ping five times")
	form := url.Values{"kind": {"prompt"}, "text": {"  \n"}}

	plain := send(t, http.MethodPost, srv.url+"/tasks/"+string(task)+"/commands", form, false)
	htmx := send(t, http.MethodPost, srv.url+"/tasks/"+string(task)+"/commands", form, true)

	for name, got := range map[string]response{"plain": plain, "htmx": htmx} {
		if got.status != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422", name, got.status)
		}
		requireContains(t, got.body, `<p class="problem" role="alert">Write a prompt first.</p>`)
	}
	requireContains(t, plain.body, "<!DOCTYPE html>")
	requireContains(t, htmx.body, `<div hx-swap-oob="innerHTML:#prompt">`)
	if commands, _ := srv.store.commandsAfter(t.Context(), "laptop", protocol.CommandPosition{}); len(commands) != 1 {
		t.Errorf("issued %d commands, want only the start", len(commands))
	}
}

func TestGivenInvalidNewTaskWhenPostedThenTheFormSaysWhyAndKeepsWhatWasEntered(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")
	for name, tc := range map[string]struct {
		form    url.Values
		problem string
	}{
		"no prompt":        {startForm("laptop", " "), "Write a prompt for the task."},
		"unsafe daemon":    {startForm("../x", "count"), "Choose a daemon to run the task on, or any."},
		"unknown priority": {func() url.Values { f := startForm("laptop", "count"); f.Set("priority", "urgent"); return f }(), "Choose low, normal or high priority."},
		"unseen daemon":    {startForm("desktop", "count"), "Daemon desktop has not connected yet."},
		"ref without repo": {func() url.Values { f := startForm("laptop", "count <b>"); f.Set("ref", "main"); return f }(), "The task was not started: workspace needs both repo and ref."},
		"bad limit":        {func() url.Values { f := startForm("laptop", "count"); f.Set("cleanup", "soon"); return f }(), "The cleanup limit is not a duration such as 5m."},
	} {
		t.Run(name, func(t *testing.T) {
			for _, htmx := range []bool{false, true} {
				got := send(t, http.MethodPost, srv.url+"/tasks", tc.form, htmx)

				if got.status != http.StatusUnprocessableEntity {
					t.Errorf("htmx %v: status = %d (%s), want 422", htmx, got.status, got.body)
				}
				requireContains(t, got.body, `<p class="problem" role="alert">`+tc.problem+`</p>`)
				requireContains(t, got.body, ">\n"+strings.ReplaceAll(strings.ReplaceAll(tc.form.Get("prompt"), "<", "&lt;"), ">", "&gt;")+"</textarea>")
			}
		})
	}
	if tasks, _ := srv.store.tasks(t.Context()); len(tasks) != 0 {
		t.Errorf("created %d tasks, want none", len(tasks))
	}
}

func TestGivenHTMXWhenNewTaskPostedThenTheFormIsClearedAndTheListsRefreshed(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")

	got := send(t, http.MethodPost, srv.url+"/tasks", startForm("laptop", "count to three"), true)

	if got.status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", got.status, got.body)
	}
	tasks, err := srv.store.tasks(t.Context())
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks = %v, %v; want one", tasks, err)
	}
	requireContains(t, got.body,
		`<div hx-swap-oob="innerHTML:#new-task"><p class="notice">Started task <a href="/tasks/`+string(tasks[0].ID)+`">`,
		`<div hx-swap-oob="innerHTML:#dashboard">`,
		`>count to three</a></td>`,
		"<textarea name=\"prompt\" required=\"\" rows=\"4\">\n</textarea>",
	)
	if tasks[0].Model != testDefaultModel {
		t.Errorf("model = %q, want the default", tasks[0].Model)
	}
}

func TestGivenHostileAgentTextWhenTaskPageRequestedThenItIsEscaped(t *testing.T) {
	srv := startTestServer(t)
	task := startTaskViaForm(t, srv, "laptop", "<script>alert(1)</script>")
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessOutput, `{"type":"assistant","message":{"content":[{"type":"text","text":"<script>alert(2)</script>"}]}}`)
	events.ingest(t, srv, "laptop")

	for _, target := range []string{"/", "/tasks/" + string(task), "/tasks/" + string(task) + "/raw"} {
		page := getPage(t, srv.url+target)

		requireLacks(t, page, "<script>alert")
		requireContains(t, page, "&lt;script&gt;alert(")
	}
}

func TestGivenStoredEventsWhenRawViewRequestedThenEachIsARowWithItsPayload(t *testing.T) {
	srv := startTestServer(t)
	task := startTaskViaForm(t, srv, "laptop", "Touch two files")
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
	events.add(protocol.KindPermissionRequested, bashRequest)
	events.ingest(t, srv, "laptop")

	page := getPage(t, srv.url+"/tasks/"+string(task)+"/raw")

	requireContains(t, page,
		`<tr><td>1</td><td>harness_started</td><td><time datetime="2026-10-07T12:00:01Z">`,
		`<pre class="payload">{&#34;pid&#34;:7,&#34;model&#34;:&#34;haiku&#34;,&#34;workdir&#34;:&#34;/w&#34;}</pre>`,
		`<tr><td>2</td><td>permission_requested</td>`,
	)
}

func TestGivenUnknownTaskWhenItsPagesAreRequestedThenNotFound(t *testing.T) {
	srv := startTestServer(t)
	for _, target := range []string{"/tasks/nope", "/tasks/nope/raw"} {
		if got := send(t, http.MethodGet, srv.url+target, nil, false); got.status != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404", target, got.status)
		}
	}
	got := send(t, http.MethodPost, srv.url+"/tasks/nope/commands", url.Values{"kind": {"stop"}}, false)
	if got.status != http.StatusNotFound {
		t.Errorf("POST: status = %d, want 404", got.status)
	}
}

func TestGivenStaticFilesWhenRequestedThenTheyAreServedWithTheirContentTypes(t *testing.T) {
	srv := startTestServer(t)
	for file, contentType := range map[string]string{
		"style.css":           "text/css; charset=utf-8",
		"htmx.min.js":         "text/javascript; charset=utf-8",
		"htmx-ext-sse.min.js": "text/javascript; charset=utf-8",
	} {
		got := send(t, http.MethodGet, srv.url+"/static/"+file, nil, false)

		if got.status != http.StatusOK || got.header.Get("Content-Type") != contentType || len(got.body) == 0 {
			t.Errorf("%s: status %d, Content-Type %q, %d bytes; want 200, %q", file, got.status, got.header.Get("Content-Type"), len(got.body), contentType)
		}
	}
}

func TestGivenEveryTaskStateWhenShownAsABadgeThenEachHasAColour(t *testing.T) {
	for _, state := range []TaskState{TaskQueued, TaskPending, TaskRunning, TaskAwaitingPermission, TaskPausing, TaskPaused, TaskYielded, TaskFinished, TaskStopped, TaskFailed} {
		var out strings.Builder
		if err := html.Render(&out, component.StateBadge(string(state))); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "state-unknown") {
			t.Errorf("%s renders as %s", state, out.String())
		}
	}
}
