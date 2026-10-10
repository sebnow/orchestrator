package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

const (
	sendAfterTurn = `name="steer" value="">Send after this turn</button>`
	sendNow       = `name="steer" value="now">Send now</button>`
)

// startedTurn starts a task whose harness has started.
func startedTurn(t *testing.T, srv testServer) (protocol.TaskID, *taskEvents) {
	t.Helper()
	task := startTaskViaForm(t, srv, "laptop", "Count to twenty slowly.")
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
	events.ingest(t, srv, "laptop")
	return task, events
}

func TestGivenRunningTaskWhenItsPageIsShownThenTheFollowUpOffersAfterThisTurnAndNow(t *testing.T) {
	srv := startTestServer(t)
	task, _ := startedTurn(t, srv)

	page := getPage(t, srv.url+"/tasks/"+string(task))

	requireContains(t, page, sendAfterTurn, sendNow, "No prompt waits.")
	requireLacks(t, page, openSend)
}

func TestGivenFinishedTaskWhenItsPageIsShownThenTheFollowUpHasOneSendButton(t *testing.T) {
	srv := startTestServer(t)
	task := taskThatExited(t, srv, false)

	page := getPage(t, srv.url+"/tasks/"+string(task))

	requireContains(t, page, openSend)
	requireLacks(t, page, sendNow)
}

func TestGivenRunningTaskWhenTheOwnerSendsNowThenThePromptCarriesSteerAndTheTranscriptSaysTheTurnWasSteered(t *testing.T) {
	srv := startTestServer(t)
	task, _ := startedTurn(t, srv)

	postForm(t, srv, task, url.Values{"kind": {"prompt"}, "text": {"Stop at ten."}, "steer": {"now"}})
	admitTurns(t, srv.store)

	last := lastCommand(t, srv, "laptop")
	var prompt protocol.Prompt
	if err := json.Unmarshal(last.Payload, &prompt); err != nil || last.Kind != protocol.CommandPrompt || !prompt.Steer || prompt.Text != "Stop at ten." {
		t.Errorf("last command = %s %s, want the steering prompt", last.Kind, last.Payload)
	}
	page := getPage(t, srv.url+"/tasks/"+string(task))
	requireContains(t, page, "Owner steered the task: the turn is interrupted and this sent as the next prompt")
	requireLacks(t, page, "Owner interrupted the turn")
	if state := readProgress(t, srv.store, task).State; state != TaskRunning {
		t.Errorf("state = %s, want running", state)
	}
}

func TestGivenTaskAwaitingPermissionWhenTheOwnerSteersThenItRunsAndTheRequestIsNoLongerOffered(t *testing.T) {
	srv := startTestServer(t)
	task, events := startedTurn(t, srv)
	events.add(protocol.KindPermissionRequested, bashRequest)
	events.ingest(t, srv, "laptop")

	postForm(t, srv, task, url.Values{"kind": {"prompt"}, "text": {"Do not run that."}, "steer": {"now"}})
	admitTurns(t, srv.store)

	if state := readProgress(t, srv.store, task).State; state != TaskRunning {
		t.Errorf("state = %s, want running", state)
	}
	requireLacks(t, getPage(t, srv.url+"/tasks/"+string(task)), "Permission requested")
}

func TestGivenPromptTheDaemonHoldsWhenTheOwnerWithdrawsItThenTheDaemonIsToldAndTheListClearsOnceItReleasesIt(t *testing.T) {
	srv := startTestServer(t)
	task, events := startedTurn(t, srv)
	postForm(t, srv, task, url.Values{"kind": {"prompt"}, "text": {"And then the next."}})
	admitTurns(t, srv.store)
	prompt := lastCommand(t, srv, "laptop")
	events.add(protocol.KindPromptHeld, `{"prompt":`+strconv.FormatUint(prompt.ID, 10)+`}`)
	events.ingest(t, srv, "laptop")

	held := getPage(t, srv.url+"/tasks/"+string(task))
	postForm(t, srv, task, url.Values{"kind": {"withdraw"}, "prompt": {strconv.FormatUint(prompt.ID, 10)}})
	withdraw := lastCommand(t, srv, "laptop")
	events.add(protocol.KindPromptReleased, `{"prompt":`+strconv.FormatUint(prompt.ID, 10)+`,"outcome":"withdrawn"}`)
	events.ingest(t, srv, "laptop")
	released := getPage(t, srv.url+"/tasks/"+string(task))

	requireContains(t, held, "held by the daemon until the turn ends", "And then the next.", `value="withdraw"`)
	if withdraw.Kind != protocol.CommandWithdraw || string(withdraw.Payload) != `{"prompt":`+strconv.FormatUint(prompt.ID, 10)+`}` {
		t.Errorf("last command = %s %s, want the withdrawal", withdraw.Kind, withdraw.Payload)
	}
	requireContains(t, released, "No prompt waits.", "Owner withdrew a held prompt before it was sent")
	got := send(t, http.MethodPost, srv.url+"/tasks/"+string(task)+"/commands",
		url.Values{"kind": {"withdraw"}, "prompt": {strconv.FormatUint(prompt.ID, 10)}}, false)
	if got.status != http.StatusUnprocessableEntity || !strings.Contains(got.body, "That prompt no longer waits") {
		t.Errorf("second withdrawal: %d, want 422 saying it no longer waits", got.status)
	}
}

func TestGivenPromptWaitingForTheSchedulerWhenTheOwnerWithdrawsItThenTheTurnIsRemovedAndNothingIsIssued(t *testing.T) {
	srv := startTestServer(t)
	task := taskThatExited(t, srv, false)
	turn := postForTurn(t, srv.url+"/v1/tasks/"+string(task)+"/commands", `{"kind":"prompt","payload":{"text":"Later, maybe."}}`, http.StatusAccepted)
	before := lastCommand(t, srv, "laptop")

	page := getPage(t, srv.url+"/tasks/"+string(task))
	status, body := doRequest(t, http.MethodDelete, srv.url+"/v1/tasks/"+string(task)+"/turns/"+strconv.FormatUint(turn.ID, 10), "")
	admitTurns(t, srv.store)

	requireContains(t, page, "waits for the scheduler", "Later, maybe.")
	if status != http.StatusNoContent {
		t.Errorf("status = %d (%s), want 204", status, body)
	}
	if after := lastCommand(t, srv, "laptop"); after.ID != before.ID {
		t.Errorf("last command = %s, want nothing issued", after.Kind)
	}
	if status, _ := doRequest(t, http.MethodDelete, srv.url+"/v1/tasks/"+string(task)+"/turns/"+strconv.FormatUint(turn.ID, 10), ""); status != http.StatusConflict {
		t.Errorf("second withdrawal: %d, want 409", status)
	}
}
