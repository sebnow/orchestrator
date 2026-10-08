package server

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// queueTaskViaForm starts a task through the new-task form with extra
// fields set, and returns its id, leaving its start to the scheduler.
func queueTaskViaForm(t *testing.T, srv testServer, daemon, prompt string, extra url.Values) protocol.TaskID {
	t.Helper()
	form := startForm(daemon, prompt)
	for name, values := range extra {
		form[name] = values
	}
	got := send(t, http.MethodPost, srv.url+"/tasks", form, false)
	task, found := strings.CutPrefix(got.header.Get("Location"), "/tasks/")
	if got.status != http.StatusSeeOther || !found {
		t.Fatalf("status = %d to %q (%s), want 303 to a task page", got.status, got.header.Get("Location"), got.body)
	}
	return protocol.TaskID(task)
}

// connect opens daemon's command stream and waits until the server
// counts the daemon connected.
func connect(t *testing.T, srv testServer, daemon protocol.DaemonID) <-chan sseEvent {
	t.Helper()
	events := openCommandStream(t, srv, daemon, "")
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Contains(srv.connectedDaemons(), daemon) {
		if time.Now().After(deadline) {
			t.Fatalf("daemon %s not connected after 5s", daemon)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return events
}

// pass runs a scheduler pass at once.
func (srv testServer) pass(t *testing.T) {
	t.Helper()
	if _, err := srv.sched.pass(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestGivenQueuedTasksAndAReadingWhenTheDashboardIsShownThenItShowsPlacesReasonsPrioritiesTheBudgetAndSlots(t *testing.T) {
	taken := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	srv := startTestServerWith(t, Options{
		Schedule: SchedulePolicy{SlotsPerDaemon: 1, FillerThreshold: 0.5, LowThreshold: 0.85},
		Now:      func() time.Time { return taken.Add(5 * time.Minute) },
	})
	connect(t, srv, "laptop")
	filler := queueTaskViaForm(t, srv, "", "Tidy the docs", url.Values{"priority": {"high"}, "filler": {"on"}})
	running := queueTaskViaForm(t, srv, "laptop", "Touch two files", nil)
	srv.pass(t)
	waiting := queueTaskViaForm(t, srv, "laptop", "Count to three", url.Values{"priority": {"low"}})
	srv.pass(t)
	events := &taskEvents{task: running}
	events.add(protocol.KindHarnessStarted, `{"pid":8,"model":"haiku","workdir":"/w"}`)
	events.add(protocol.KindQuotaObserved, `{"status":"allowed","windows":[{"name":"five_hour","utilization":0.42,"resets_at":"2026-10-08T15:00:00Z"}]}`)
	events.events[1].Time = taken
	events.ingest(t, srv, "laptop")
	srv.pass(t)

	page := getPage(t, srv.url+"/")

	requireContains(t, page,
		`<td><span class="badge state-queued">queued #1</span><span class="reason"> slots: daemon laptop has no free slot</span></td><td><a href="/tasks/`+string(waiting)+`">Count to three</a></td><td>low</td>`,
		`<td><span class="badge state-queued">queued #2</span><span class="reason"> priority: non-filler turns are waiting for a slot</span></td><td><a href="/tasks/`+string(filler)+`">Tidy the docs</a></td><td>high, filler</td><td></td>`,
		`<td><span class="badge state-running">running</span></td><td><a href="/tasks/`+string(running)+`">Touch two files</a></td><td>normal</td><td>laptop</td>`,
		`<h2>Budget</h2>`, "42% used", "Taken 5 minutes ago",
		"Filler runs while the five-hour window is below 50% used, low priority below 85%",
		`<td>laptop</td><td>claude-code 2.1.289</td>`, `<td>yes</td><td>1 of 1 in use</td>`,
		`<option value="" selected="">Any connected daemon</option>`,
		`<option value="normal" selected="">normal</option>`,
		`<input name="filler" type="checkbox" value="on">`,
	)
}

func TestGivenYieldedFillerWaitingForASlotWhenItsPageIsShownThenItIsYieldedQueuedAndOffersResume(t *testing.T) {
	srv := startTestServerWith(t, Options{Schedule: SchedulePolicy{SlotsPerDaemon: 1, FillerThreshold: 1, LowThreshold: 1}})
	connect(t, srv, "laptop")
	spec := ownersTask("filler", "laptop")
	spec.Filler = true
	spec.Priority = PriorityLow
	queueTask(t, srv.store, spec)
	readingFromVPS(t, srv.store, time.Now(), 0.1)
	srv.pass(t)
	fill := &lifecycle{t: t, store: srv.store, task: "filler", held: true}
	fill.event(protocol.KindHarnessStarted, started, TaskRunning)
	queueTask(t, srv.store, ownersTask("normal", "laptop"))
	srv.pass(t)
	fill.event(protocol.KindPauseSettled, settled, TaskYielded)
	srv.pass(t)

	page := getPage(t, srv.url+"/tasks/filler")

	requireContains(t, page,
		`<span class="badge state-yielded">yielded</span> <span class="badge state-queued">queued #1</span>`,
		`<dt>Waits</dt><dd>slots: daemon laptop has no free slot</dd>`,
		`<dt>Priority</dt><dd>low</dd><dt>Filler</dt><dd>yes</dd>`,
		`value="resume">Resume</button>`,
	)
	requireLacks(t, page, `value="pause">Pause</button>`, `value="interrupt">`)
}

func TestGivenNoDaemonNamedWhenTheOwnerCreatesATaskThenItRunsOnTheDaemonWithTheMostFreeSlots(t *testing.T) {
	srv := startTestServer(t)
	seedTask(t, srv.store, "laptop", "busy")
	if _, err := srv.store.heldSeqs(t.Context(), "vps"); err != nil {
		t.Fatal(err)
	}
	laptop := openCommandStream(t, srv, "laptop", "")
	vps := openCommandStream(t, srv, "vps", "")
	receiveCommand(t, laptop)

	created := postForTurn(t, srv.url+"/v1/tasks", `{"prompt":"p","priority":"high","filler":false,"pause_limits":{"acknowledge":"1m","cleanup":"5m"}}`, http.StatusCreated)

	if command := receiveCommand(t, vps); command.Kind != protocol.CommandStartTask || command.TaskID != created.TaskID {
		t.Errorf("vps got %+v, want the start of %s", command, created.TaskID)
	}
	var detail map[string]any
	getJSON(t, srv.url+"/v1/tasks/"+string(created.TaskID), &detail)
	if detail["daemon_id"] != "vps" || detail["priority"] != "high" || detail["filler"] != false {
		t.Errorf("task = %v, want a high-priority task on vps", detail)
	}
}

func TestGivenFillerTaskWithNoReadingWhenTheOwnerGetsItThenItsPlaceAndReasonAreReported(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")
	connect(t, srv, "laptop")
	created := postForTurn(t, srv.url+"/v1/tasks", `{"daemon_id":"laptop","prompt":"p","filler":true,"pause_limits":{"acknowledge":"1m","cleanup":"5m"}}`, http.StatusCreated)
	srv.pass(t)

	var detail struct {
		State  TaskState   `json:"state"`
		Filler bool        `json:"filler"`
		Queue  *queuePlace `json:"queue"`
	}
	getJSON(t, srv.url+"/v1/tasks/"+string(created.TaskID), &detail)

	want := queuePlace{Position: 1, Reason: "budget: filler needs a current reading of the five-hour window"}
	if detail.State != TaskQueued || !detail.Filler || detail.Queue == nil || *detail.Queue != want {
		t.Errorf("task = %+v (queue %+v), want a queued filler task at %+v", detail, detail.Queue, want)
	}
}
