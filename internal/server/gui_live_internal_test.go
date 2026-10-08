package server

import (
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/transcript"
)

func receiveUpdate(t *testing.T, events <-chan sseEvent) sseEvent {
	t.Helper()
	select {
	case event, ok := <-events:
		if !ok {
			t.Fatal("task stream ended")
		}
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("no update within 5s")
	}
	return sseEvent{}
}

var sseConnect = regexp.MustCompile(`sse-connect="([^"]+)"`)

// runningTask starts a task and ingests the pause-stdin run as its
// output. It returns the task and its events, to add more to.
func runningTask(t *testing.T, srv testServer) (protocol.TaskID, *taskEvents) {
	t.Helper()
	task := startTaskViaForm(t, srv, "laptop", "Run ping five times")
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
	events.addFixture(t, "pause-stdin")
	events.ingest(t, srv, "laptop")
	return task, events
}

func TestGivenTaskPageWhenANewEventIsIngestedThenItsStreamPushesTheNewEntryAndTheChangedRegions(t *testing.T) {
	srv := startTestServer(t)
	task, events := runningTask(t, srv)
	page := getPage(t, srv.url+"/tasks/"+string(task))
	shown := strconv.FormatUint(events.seq, 10) + "-1-0"
	match := sseConnect.FindStringSubmatch(page)
	if match == nil || match[1] != "/tasks/"+string(task)+"/stream?after="+shown {
		t.Fatalf("sse-connect = %v, want the stream after %s", match, shown)
	}
	stream := openEventStream(t, srv.url+match[1], "")

	events.add(protocol.KindHarnessOutput, `{"type":"assistant","message":{"content":[{"type":"text","text":"DONE-4\n\n  indented"}]}}`)
	events.ingest(t, srv, "laptop")
	update := receiveUpdate(t, stream)

	next := strconv.FormatUint(events.seq, 10) + "-1-0"
	if update.id != next {
		t.Errorf("id = %q, want %q", update.id, next)
	}
	requireContains(t, update.data,
		"<li class=\"entry from-agent\"><header><time datetime=\"2026-10-07T12:00:"+strconv.FormatUint(events.seq, 10)+"Z\">",
		"<pre>\nDONE-4\n\n  indented</pre></li>",
		`<div hx-swap-oob="innerHTML:#task-header">`,
		`<div hx-swap-oob="innerHTML:#prompt-submit">`,
		`<div hx-swap-oob="innerHTML:#task-fallback"><div hx-get="/tasks/`+string(task)+`/updates?after=`+next+`" hx-trigger="htmx:sseError from:body once"`,
	)
	requireLacks(t, update.data, "DONE-1", `innerHTML:#permission`)

	events.add(protocol.KindPermissionRequested, bashRequest)
	events.ingest(t, srv, "laptop")
	update = receiveUpdate(t, stream)

	requireContains(t, update.data, "Agent asked to run Bash", `<div hx-swap-oob="innerHTML:#permission"><article class="card">`,
		`<span class="badge state-awaiting">awaiting permission</span>`)
	requireLacks(t, update.data, "DONE-4")
}

func TestGivenReconnectingBrowserWhenStreamOpensThenItGetsWhatItMissedSinceItsLastEventID(t *testing.T) {
	srv := startTestServer(t)
	task, events := runningTask(t, srv)
	postForm(t, srv, task, url.Values{"kind": {"pause"}})

	// The query names an older position than the Last-Event-ID, which wins.
	stream := openEventStream(t, srv.url+"/tasks/"+string(task)+"/stream?after=0-0", strconv.FormatUint(events.seq, 10)+"-1")
	update := receiveUpdate(t, stream)

	if want := strconv.FormatUint(events.seq, 10) + "-2-0"; update.id != want {
		t.Errorf("id = %q, want %q", update.id, want)
	}
	requireContains(t, update.data, "Owner asked the agent to pause", `<span class="badge state-pausing">pausing</span>`,
		`<button type="submit" class="primary" disabled="">Send</button>`)
	requireLacks(t, update.data, "Harness started", "DONE-1")
}

func TestGivenPollingPageWhenItAsksForUpdatesThenItGetsTheEntriesAfterItsCursorAndANewPoller(t *testing.T) {
	srv := startTestServer(t)
	task, events := runningTask(t, srv)

	got := send(t, http.MethodGet, srv.url+"/tasks/"+string(task)+"/updates?after=1-1", nil, true)

	if got.status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", got.status, got.body)
	}
	requireContains(t, got.body, "DONE-1", "DONE-3",
		`<div hx-swap-oob="innerHTML:#task-live"><div hx-get="/tasks/`+string(task)+`/updates?after=`+strconv.FormatUint(events.seq, 10)+`-1-0" hx-trigger="every 5s, task-changed from:body"`)
	requireLacks(t, got.body, "Harness started", "Owner prompted", "sse-connect")
}

func TestGivenMalformedCursorWhenStreamingOrPollingThenBadRequest(t *testing.T) {
	srv := startTestServer(t)
	task, _ := runningTask(t, srv)
	for _, target := range []string{"/stream?after=3", "/stream?after=a-1", "/updates?after=1-", "/updates?after=-1-2", "/updates?after=1-2-3-4"} {
		if got := send(t, http.MethodGet, srv.url+"/tasks/"+string(task)+target, nil, false); got.status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, got.status)
		}
	}
	if got := send(t, http.MethodGet, srv.url+"/tasks/nope/stream", nil, false); got.status != http.StatusNotFound {
		t.Errorf("unknown task: status = %d, want 404", got.status)
	}
}

func TestGivenOpenTaskStreamWhenStreamsAreEndedThenItEnds(t *testing.T) {
	srv := startTestServer(t)
	task, _ := runningTask(t, srv)
	stream := openEventStream(t, srv.url+"/tasks/"+string(task)+"/stream?after=999-999", "")

	srv.EndStreams()

	select {
	case event, ok := <-stream:
		if ok {
			t.Fatalf("got %+v, want the stream to end", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream still open after 5s")
	}
}

func TestGivenEntriesFromEventsCommandsAndMessagesWhenTakingThoseAfterACursorThenEachKindIsCountedOnItsOwn(t *testing.T) {
	entry := func(source transcript.Source) transcript.Entry {
		return transcript.Entry{Source: source, Body: transcript.AgentText{}}
	}
	entries := []transcript.Entry{
		entry(transcript.Source{TaskID: "task-1", Seq: 4}),
		entry(transcript.Source{TaskID: "task-1", CommandID: 9}),
		entry(transcript.Source{TaskID: "child", CommandID: 12}),
		entry(transcript.Source{TaskID: "task-1", MessageID: 3}),
		entry(transcript.Source{TaskID: "task-1", MessageID: 5}),
		entry(transcript.Source{TaskID: "task-1", Seq: 6}),
	}
	at, err := parseCursor("4-9-3")
	if err != nil {
		t.Fatal(err)
	}

	fresh, next := at.after(entries)

	if len(fresh) != 3 || fresh[0].Source.CommandID != 12 || fresh[1].Source.MessageID != 5 || fresh[2].Source.Seq != 6 {
		t.Errorf("fresh = %+v", fresh)
	}
	if next.String() != "6-12-5" {
		t.Errorf("next = %s, want 6-12-5", next)
	}
}
