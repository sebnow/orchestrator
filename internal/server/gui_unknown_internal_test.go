package server

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

const lifecycleLine = `{"type":"command_lifecycle","command_uuid":"c1","state":"queued","session_id":"s1"}`

// taskWithUnknownEntry starts a task whose transcript holds an agent
// reply and one line the normaliser does not recognise.
func taskWithUnknownEntry(t *testing.T, srv testServer) (protocol.TaskID, *taskEvents) {
	t.Helper()
	task := startTaskViaForm(t, srv, "laptop", "Say hello")
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
	events.add(protocol.KindHarnessOutput, lifecycleLine)
	events.add(protocol.KindHarnessOutput, `{"type":"assistant","message":{"content":[{"type":"text","text":"HELLO"}]}}`)
	events.ingest(t, srv, "laptop")
	return task, events
}

func TestGivenUnrecognisedEntryWhenTheTaskPageIsShownThenItIsHiddenAndCountedWithALinkToShowIt(t *testing.T) {
	srv := startTestServer(t)
	task, _ := taskWithUnknownEntry(t, srv)

	page := getPage(t, srv.url+"/tasks/"+string(task))

	requireContains(t, page, "HELLO", "1 unrecognised entry hidden.", `<a href="/tasks/`+string(task)+`?unknown=show">Show them</a>`)
	requireLacks(t, page, "Unrecognised harness_output", "command_lifecycle")
}

func TestGivenShowUnknownWhenTheTaskPageIsShownThenTheEntryIsShownAndTheLiveUpdatesKeepShowingThem(t *testing.T) {
	srv := startTestServer(t)
	task, events := taskWithUnknownEntry(t, srv)

	page := getPage(t, srv.url+"/tasks/"+string(task)+"?unknown=show")

	shown := strconv.FormatUint(events.seq, 10) + "-1-0"
	requireContains(t, page, "HELLO", "Unrecognised harness_output of type command_lifecycle", "Showing 1 unrecognised entry.",
		`<a href="/tasks/`+string(task)+`">Hide them</a>`,
		`sse-connect="/tasks/`+string(task)+`/stream?after=`+shown+`&amp;unknown=show"`)
}

func TestGivenRawViewWhenRequestedThenItStillListsTheUnrecognisedLine(t *testing.T) {
	srv := startTestServer(t)
	task, _ := taskWithUnknownEntry(t, srv)

	page := getPage(t, srv.url+"/tasks/"+string(task)+"/raw")

	requireContains(t, page, `<tr><td>2</td><td>harness_output</td>`, "command_lifecycle")
}

func TestGivenHiddenUnknownEntriesWhenAnotherArrivesThenTheStreamSkipsItButMovesOnAndUpdatesTheCount(t *testing.T) {
	srv := startTestServer(t)
	task, events := taskWithUnknownEntry(t, srv)
	stream := openEventStream(t, srv.url+"/tasks/"+string(task)+"/stream?after="+strconv.FormatUint(events.seq, 10)+"-1", "")

	events.add(protocol.KindHarnessOutput, lifecycleLine)
	events.ingest(t, srv, "laptop")
	update := receiveUpdate(t, stream)

	if want := strconv.FormatUint(events.seq, 10) + "-1-0"; update.id != want {
		t.Errorf("id = %q, want %q", update.id, want)
	}
	requireContains(t, update.data, `<div hx-swap-oob="innerHTML:#task-unknown"><p class="notice">2 unrecognised entries hidden.`)
	requireLacks(t, update.data, "Unrecognised harness_output")
}

func TestGivenShownUnknownEntriesWhenPollingThenTheUpdateCarriesThemAndKeepsShowingThem(t *testing.T) {
	srv := startTestServer(t)
	task, events := taskWithUnknownEntry(t, srv)
	at := strconv.FormatUint(events.seq, 10) + "-1"
	events.add(protocol.KindHarnessOutput, lifecycleLine)
	events.ingest(t, srv, "laptop")

	got := send(t, http.MethodGet, srv.url+"/tasks/"+string(task)+"/updates?after="+at+"&unknown=show", nil, false)

	next := strconv.FormatUint(events.seq, 10) + "-1-0"
	requireContains(t, got.body, "Unrecognised harness_output of type command_lifecycle",
		`hx-get="/tasks/`+string(task)+`/updates?after=`+next+`&amp;unknown=show"`)
}
