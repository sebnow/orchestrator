package server

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

const (
	pauseButton     = `value="pause">Pause</button>`
	resumeButton    = `value="resume">Resume</button>`
	interruptButton = `value="interrupt">Interrupt</button>`
	stopButton      = `value="stop">Stop</button>`
	openSend        = `<button type="submit" class="primary">Send</button>`
	closedSend      = `<button type="submit" class="primary" disabled="">Send</button>`
)

// taskThatExited starts a task whose first process ran and exited
// cleanly, with a pause settled first when paused is set.
func taskThatExited(t *testing.T, srv testServer, paused bool) protocol.TaskID {
	t.Helper()
	task := startTaskViaForm(t, srv, "laptop", "The codeword is MARMALADE.")
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
	events.ingest(t, srv, "laptop")
	if paused {
		postForm(t, srv, task, url.Values{"kind": {"pause"}})
		events.add(protocol.KindPauseAcknowledged, `{"note":"after step 1"}`)
		events.add(protocol.KindPauseSettled, `{"interrupted":false}`)
	}
	events.add(protocol.KindHarnessExited, `{"exit_code":0}`)
	events.ingest(t, srv, "laptop")
	return task
}

func TestGivenFinishedTaskWhenItsPageIsShownThenItOffersAFollowUpAndStopButNothingThatNeedsAProcess(t *testing.T) {
	srv := startTestServer(t)
	task := taskThatExited(t, srv, false)

	page := getPage(t, srv.url+"/tasks/"+string(task))

	requireContains(t, page, `<span class="badge state-finished">finished</span>`, stopButton, openSend)
	requireLacks(t, page, pauseButton, resumeButton, interruptButton, closedSend)
}

func TestGivenFinishedTaskWhenAFollowUpIsPostedThenItIsQueuedAndOnceAdmittedTheTaskRuns(t *testing.T) {
	srv := startTestServer(t)
	task := taskThatExited(t, srv, false)

	postForm(t, srv, task, url.Values{"kind": {"prompt"}, "text": {"What is the codeword?"}})
	if got := readProgress(t, srv.store, task).State; got != TaskFinished {
		t.Errorf("state before admission = %s, want finished", got)
	}
	admitTurns(t, srv.store)

	if got := readProgress(t, srv.store, task).State; got != TaskRunning {
		t.Errorf("state = %s, want running", got)
	}
}

func TestGivenPausedTaskWhenItsPageIsShownThenItOffersResumeAFollowUpAndStop(t *testing.T) {
	srv := startTestServer(t)
	task := taskThatExited(t, srv, true)

	page := getPage(t, srv.url+"/tasks/"+string(task))

	requireContains(t, page, `<span class="badge state-paused">paused</span>`, resumeButton, stopButton, openSend)
	requireLacks(t, page, pauseButton, interruptButton, closedSend)
}

func TestGivenStoppedTaskWhenItsPageIsShownThenItOffersNothingAndSaysWhy(t *testing.T) {
	srv := startTestServer(t)
	task := taskThatExited(t, srv, false)
	postForm(t, srv, task, url.Values{"kind": {"stop"}})

	page := getPage(t, srv.url+"/tasks/"+string(task))

	requireContains(t, page, `<span class="badge state-stopped">stopped</span>`, closedSend, "The task has ended; it takes no more prompts.")
	requireLacks(t, page, pauseButton, resumeButton, interruptButton, stopButton, openSend)
}

func TestGivenStoppedTaskWhenAFollowUpIsPostedThenTheFormSaysWhyAndNothingIsIssued(t *testing.T) {
	srv := startTestServer(t)
	task := taskThatExited(t, srv, false)
	postForm(t, srv, task, url.Values{"kind": {"stop"}})

	got := send(t, http.MethodPost, srv.url+"/tasks/"+string(task)+"/commands", url.Values{"kind": {"prompt"}, "text": {"More."}}, false)

	if got.status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", got.status)
	}
	requireContains(t, got.body, "The task has ended; it takes no more commands.")
	commands, err := srv.store.commandsAfter(t.Context(), "laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	if last := commands[len(commands)-1]; last.Kind != protocol.CommandStop {
		t.Errorf("last command = %s, want the stop", last.Kind)
	}
}

func TestGivenStoppedTaskWhenACommandIsIssuedThroughTheAPIThenConflict(t *testing.T) {
	srv := startTestServer(t)
	task := taskThatExited(t, srv, false)
	postForm(t, srv, task, url.Values{"kind": {"stop"}})

	status, body := doRequest(t, http.MethodPost, srv.url+"/v1/tasks/"+string(task)+"/commands", `{"kind":"prompt","payload":{"text":"More."}}`)

	if status != http.StatusConflict {
		t.Errorf("status = %d (%s), want 409", status, body)
	}
}

// taskCutShortByTheDaemon starts a task whose first turn the daemon cut
// short, with exitError as the error of the harness_exited it gave.
func taskCutShortByTheDaemon(t *testing.T, srv testServer, exitError string) protocol.TaskID {
	t.Helper()
	task := startTaskViaForm(t, srv, "laptop", "The codeword is MARMALADE.")
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
	events.add(protocol.KindHarnessExited, `{"exit_code":-1,"error":"`+exitError+`"}`)
	events.ingest(t, srv, "laptop")
	return task
}

func TestGivenTaskCutShortByADaemonRestartWhenShownThenItIsPausedForTheOwnerWithTheRestartAsTheReason(t *testing.T) {
	srv := startTestServer(t)
	task := taskCutShortByTheDaemon(t, srv, exitRestarted)

	page := getPage(t, srv.url+"/tasks/"+string(task))
	dashboard := getPage(t, srv.url+"/")

	requireContains(t, page, `<span class="badge state-paused">paused</span>`, resumeButton,
		"The daemon restarted during the turn; the task is paused", "Resume continues its session.")
	requireLacks(t, page, "Harness exited with code -1")
	requireContains(t, dashboard, `<span class="reason">paused: the daemon restarted during its turn</span>`)
}

func TestGivenTaskCutShortByADaemonShutdownWhenShownThenItIsPausedForTheOwnerWithTheShutdownAsTheReason(t *testing.T) {
	srv := startTestServer(t)
	task := taskCutShortByTheDaemon(t, srv, exitStopped)

	page := getPage(t, srv.url+"/tasks/"+string(task))
	dashboard := getPage(t, srv.url+"/")

	requireContains(t, page, `<span class="badge state-paused">paused</span>`, resumeButton,
		"The daemon stopped during the turn; the task is paused", "Resume continues its session.")
	requireLacks(t, page, "Harness exited with code -1", "restarted")
	requireContains(t, dashboard, `<span class="reason">paused: the daemon stopped during its turn</span>`)
}

func TestGivenTaskWhoseTurnTheOwnersInterruptCutShortWhenShownThenItIsPausedWithTheInterruptAsTheReason(t *testing.T) {
	srv := startTestServer(t)
	task := taskCutShortByTheDaemon(t, srv, exitInterrupted)

	page := getPage(t, srv.url+"/tasks/"+string(task))
	dashboard := getPage(t, srv.url+"/")

	requireContains(t, page, `<span class="badge state-paused">paused</span>`, resumeButton,
		"The owner interrupted the turn; the task is paused", "Resume continues its session.")
	requireLacks(t, page, "Harness exited with code -1", "The daemon")
	requireContains(t, dashboard, `<span class="reason">paused: interrupted by the owner</span>`)
}

func TestGivenTaskCutShortBeforeItReportedASessionWhenShownThenItSaysResumeStartsANewSession(t *testing.T) {
	srv := startTestServer(t)
	task := taskCutShortByTheDaemon(t, srv, exitRestartedNoSession)

	page := getPage(t, srv.url+"/tasks/"+string(task))

	requireContains(t, page, `<span class="badge state-paused">paused</span>`, resumeButton,
		"No session was recorded, so Resume starts a new session with the task&#39;s first prompt.")
}

func TestGivenTaskCutShortByARestartThenResumedAndPausedWithoutANoteWhenListedThenTheReasonIsThePauseNotTheRestart(t *testing.T) {
	srv := startTestServer(t)
	task := taskCutShortByTheDaemon(t, srv, exitRestarted)
	postForm(t, srv, task, url.Values{"kind": {"resume"}})
	admitTurns(t, srv.store)
	events := &taskEvents{task: task, seq: 2}
	events.add(protocol.KindHarnessStarted, `{"pid":8,"model":"haiku","workdir":"/w"}`)
	events.ingest(t, srv, "laptop")
	postForm(t, srv, task, url.Values{"kind": {"pause"}})
	events.add(protocol.KindPauseAcknowledged, `{"note":""}`)
	events.add(protocol.KindPauseSettled, `{"interrupted":false}`)
	events.add(protocol.KindHarnessExited, `{"exit_code":0}`)
	events.ingest(t, srv, "laptop")

	dashboard := getPage(t, srv.url+"/")

	requireContains(t, dashboard, `<span class="reason">paused</span>`)
	requireLacks(t, dashboard, "the daemon restarted during its turn")
}
