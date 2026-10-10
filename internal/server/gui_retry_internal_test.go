package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/transcript"
)

// taskThatNeverStarted starts a task whose first start failed before its
// harness started, with exitError as the error of its harness_exited.
func taskThatNeverStarted(t *testing.T, srv testServer, exitError string) protocol.TaskID {
	t.Helper()
	task := startTaskViaForm(t, srv, "laptop", "The codeword is MARMALADE.")
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessExited, `{"exit_code":-1,"error":"`+exitError+`"}`)
	events.ingest(t, srv, "laptop")
	return task
}

func startPayload(t *testing.T, command protocol.Command) protocol.StartTask {
	t.Helper()
	if command.Kind != protocol.CommandStartTask {
		t.Fatalf("command = %s, want start_task", command.Kind)
	}
	var start protocol.StartTask
	if err := json.Unmarshal(command.Payload, &start); err != nil {
		t.Fatal(err)
	}
	return start
}

func TestGivenTaskWhoseHarnessNeverStartedWhenItsPageIsShownThenItOffersRetryAndSaysAPromptStartsItAfresh(t *testing.T) {
	srv := startTestServer(t)
	task := taskThatNeverStarted(t, srv, "clone https://example.com/r.git: exit status 128")

	page := getPage(t, srv.url+"/tasks/"+string(task))

	requireContains(t, page, `<span class="badge state-failed">failed</span>`, retryButton, openSend, dismissButton,
		"has no session to continue. A prompt starts it afresh")
	requireLacks(t, page, resumeButton, stopButton)
}

func TestGivenTaskWhoseHarnessNeverStartedWhenRetriedThenItIsStartedAfreshByPlacementWithItsFirstPrompt(t *testing.T) {
	srv := startTestServer(t)
	task := taskThatNeverStarted(t, srv, "clone https://example.com/r.git: exit status 128")

	postForTurn(t, srv.url+"/v1/tasks/"+string(task)+"/commands", `{"kind":"resume"}`, http.StatusAccepted)
	queued := readProgress(t, srv.store, task).State
	admitTurns(t, srv.store)

	if queued != TaskQueued {
		t.Errorf("state once retried = %s, want queued", queued)
	}
	last := lastCommand(t, srv, "laptop")
	if start := startPayload(t, last); last.TaskID != task || start.Prompt != "The codeword is MARMALADE." {
		t.Errorf("last command = %s %q, want the task's start with its first prompt", last.TaskID, start.Prompt)
	}
	if state := readProgress(t, srv.store, task).State; state != TaskPending {
		t.Errorf("state once admitted = %s, want pending", state)
	}
	// The new start numbers its events from 1 again.
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessStarted, `{"pid":8,"model":"haiku","workdir":"/w"}`)
	events.ingest(t, srv, "laptop")
	if state := readProgress(t, srv.store, task).State; state != TaskRunning {
		t.Errorf("state once started = %s, want running", state)
	}
	entries, err := srv.Transcript(t.Context(), task)
	if err != nil {
		t.Fatal(err)
	}
	var retried []transcript.TaskRetried
	for _, entry := range entries {
		if body, ok := entry.Body.(transcript.TaskRetried); ok {
			retried = append(retried, body)
		}
		if _, ok := entry.Body.(transcript.TaskMoved); ok {
			t.Errorf("transcript has a move: %+v", entry.Body)
		}
	}
	if len(retried) != 1 || retried[0].On != "laptop" || retried[0].Prompt != "The codeword is MARMALADE." {
		t.Errorf("retries = %+v, want one on laptop with the first prompt", retried)
	}
}

func TestGivenTaskWhoseHarnessNeverStartedWhenAPromptIsSentThenTheFreshStartCarriesTheFirstPromptAndThenIt(t *testing.T) {
	srv := startTestServer(t)
	task := taskThatNeverStarted(t, srv, "spawn claude: no such file")

	postForTurn(t, srv.url+"/v1/tasks/"+string(task)+"/commands", `{"kind":"prompt","payload":{"text":"Then say it twice."}}`, http.StatusAccepted)
	admitTurns(t, srv.store)

	if start := startPayload(t, lastCommand(t, srv, "laptop")); start.Prompt != "The codeword is MARMALADE.\n\nThen say it twice." {
		t.Errorf("prompt = %q, want the first prompt and then the follow-up", start.Prompt)
	}
}

func TestGivenDaemonThatHoldsNoSessionForTheTaskWhenFollowedUpThenItIsStartedAfresh(t *testing.T) {
	srv := startTestServer(t)
	task := startTaskViaForm(t, srv, "laptop", "The codeword is MARMALADE.")
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
	events.add(protocol.KindHarnessExited, `{"exit_code":-1,"error":"`+exitNoSession+`"}`)
	events.ingest(t, srv, "laptop")

	page := getPage(t, srv.url+"/tasks/"+string(task))
	postForTurn(t, srv.url+"/v1/tasks/"+string(task)+"/commands", `{"kind":"resume"}`, http.StatusAccepted)
	admitTurns(t, srv.store)

	requireContains(t, page, retryButton)
	startPayload(t, lastCommand(t, srv, "laptop"))
}

func TestGivenTaskStoppedBeforeItStartedWhenFollowedUpThenItStartsAfreshAndItsOldStartNeverRuns(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")
	turn := postForTurn(t, srv.url+"/v1/tasks", `{"daemon_id":"laptop","prompt":"p","pause_limits":{"acknowledge":"1m","cleanup":"5m"}}`, http.StatusCreated)
	if status, body := doRequest(t, http.MethodPost, srv.url+"/v1/tasks/"+string(turn.TaskID)+"/commands", `{"kind":"stop"}`); status != http.StatusNoContent {
		t.Fatalf("stop: %d %s", status, body)
	}
	admitTurns(t, srv.store)
	if commands, err := srv.store.commandsAfter(t.Context(), "laptop", 0); err != nil || len(commands) != 0 {
		t.Fatalf("commands after the stop = %v, %v; want none", commands, err)
	}

	postForTurn(t, srv.url+"/v1/tasks/"+string(turn.TaskID)+"/commands", `{"kind":"prompt","payload":{"text":"Go on."}}`, http.StatusAccepted)
	admitTurns(t, srv.store)

	commands, err := srv.store.commandsAfter(t.Context(), "laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || startPayload(t, commands[0]).Prompt != "p\n\nGo on." {
		t.Errorf("commands = %+v, want one start with the first prompt and the follow-up", commands)
	}
}

func TestGivenRunningTaskWithAWaitingTurnWhenStoppedThenTheTurnIsDroppedAndAFollowUpAfterTheStopStays(t *testing.T) {
	srv := startTestServer(t)
	task := taskThatExited(t, srv, true)
	postForm(t, srv, task, url.Values{"kind": {"resume"}})
	// The resume waits unadmitted; the stop drops it.
	postForm(t, srv, task, url.Values{"kind": {"stop"}})
	postForm(t, srv, task, url.Values{"kind": {"prompt"}, "text": {"After the stop."}})

	admitTurns(t, srv.store)

	last := lastCommand(t, srv, "laptop")
	if last.Kind != protocol.CommandPrompt || !strings.Contains(string(last.Payload), "After the stop.") {
		t.Errorf("last command = %s %s, want only the follow-up after the stop", last.Kind, last.Payload)
	}
	commands, err := srv.store.commandsAfter(t.Context(), "laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range commands {
		if command.Kind == protocol.CommandResume {
			t.Errorf("a resume was issued: %+v", command)
		}
	}
}

func TestGivenEndedTaskWhenDismissedThenItsDaemonIsToldToDiscardItAndItsWaitingTurnsAreDropped(t *testing.T) {
	srv := startTestServer(t)
	task := failedTask(t, srv, "Doomed work")
	if _, err := srv.store.db.ExecContext(t.Context(), `INSERT INTO turns (task_id, kind, payload, origin, filler, created_at) VALUES (?, 'prompt', '{"text":"x"}', 'owner', 0, '2026-10-10T00:00:00Z')`, string(task)); err != nil {
		t.Fatal(err)
	}

	if status, body := doRequest(t, http.MethodPost, srv.url+"/v1/tasks/"+string(task)+"/dismiss", ""); status != http.StatusOK {
		t.Fatalf("dismiss: %d %s", status, body)
	}
	admitTurns(t, srv.store)

	if last := lastCommand(t, srv, "laptop"); last.Kind != protocol.CommandDiscard || last.TaskID != task {
		t.Errorf("last command = %s %s, want the discard", last.TaskID, last.Kind)
	}
	var waiting int
	if err := srv.store.db.QueryRowContext(t.Context(), `SELECT count(*) FROM turns WHERE task_id = ? AND admitted_command_id IS NULL`, string(task)).Scan(&waiting); err != nil || waiting != 0 {
		t.Errorf("waiting turns = %d, %v; want none", waiting, err)
	}
	page := getPage(t, srv.url+"/tasks/"+string(task))
	requireContains(t, page, "Owner dismissed the task; daemon laptop deletes its workspace")
}
