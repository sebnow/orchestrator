package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

const startTaskBody = `{
	"daemon_id": "laptop",
	"prompt": "count to three",
	"system_prompt": "be brief",
	"workspace": {"repo": "https://example.com/o/r.git", "ref": "main"},
	"model": "haiku",
	"pause_limits": {"acknowledge": "1m", "cleanup": "5m"}
}`

// postForTurn POSTs body to url and decodes the turn the server queued,
// which it answered with status.
func postForTurn(t *testing.T, url, body string, status int) queuedTurn {
	t.Helper()
	got, response := doRequest(t, http.MethodPost, url, body)
	if got != status {
		t.Fatalf("status = %d (%s), want %d", got, response, status)
	}
	var turn queuedTurn
	if err := json.Unmarshal([]byte(response), &turn); err != nil {
		t.Fatalf("decode %q: %v", response, err)
	}
	return turn
}

// postForCommand POSTs body to url and decodes the command the server issued.
func postForCommand(t *testing.T, url, body string) protocol.Command {
	t.Helper()
	status, response := doRequest(t, http.MethodPost, url, body)
	if status != http.StatusCreated {
		t.Fatalf("status = %d (%s), want 201", status, response)
	}
	var command protocol.Command
	if err := json.Unmarshal([]byte(response), &command); err != nil {
		t.Fatalf("decode %q: %v", response, err)
	}
	return command
}

func requireJSONEqual(t *testing.T, got, want any) {
	t.Helper()
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("got  %s\nwant %s", gotJSON, wantJSON)
	}
}

func getEvents(t *testing.T, srv testServer, task protocol.TaskID, query string) []protocol.Event {
	t.Helper()
	status, body := doRequest(t, http.MethodGet, srv.url+"/v1/tasks/"+string(task)+"/events"+query, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", status, body)
	}
	var events []protocol.Event
	if err := json.Unmarshal([]byte(body), &events); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return events
}

func TestGivenStreamingDaemonWhenOwnerCreatesTaskThenTheDaemonGetsTheStartAndItsEventsAreReadable(t *testing.T) {
	srv := startTestServer(t)
	commands := openCommandStream(t, srv, "laptop", "")

	created := postForTurn(t, srv.url+"/v1/tasks", startTaskBody, http.StatusCreated)

	delivered := receiveCommand(t, commands)
	if created.Kind != turnStart || delivered.TaskID != created.TaskID {
		t.Fatalf("created %+v, delivered %+v; want the start of one task", created, delivered)
	}
	if delivered.Kind != protocol.CommandStartTask || delivered.DaemonID != "laptop" {
		t.Fatalf("delivered %+v, want a start_task for laptop", delivered)
	}
	var start protocol.StartTask
	if err := json.Unmarshal(delivered.Payload, &start); err != nil {
		t.Fatal(err)
	}
	requireJSONEqual(t, start, protocol.StartTask{
		Prompt:       "count to three",
		SystemPrompt: systemPrompt(nil, "be brief"),
		Workspace:    &protocol.Workspace{Repo: "https://example.com/o/r.git", Ref: "main"},
		Model:        "haiku",
		PauseLimits:  protocol.PauseLimits{Acknowledge: time.Minute, Cleanup: 5 * time.Minute},
	})

	task := delivered.TaskID
	first, second := event(task, 1, `{"type":"system"}`), event(task, 2, `{"type":"result","result":"1 2 3"}`)
	status, body := postEvents(t, srv, "laptop", first, second)
	requireAcks(t, status, body, map[protocol.TaskID]uint64{task: 2})

	requireJSONEqual(t, getEvents(t, srv, task, ""), []protocol.Event{first, second})
	requireJSONEqual(t, getEvents(t, srv, task, "?after=1"), []protocol.Event{second})
	requireJSONEqual(t, getEvents(t, srv, task, "?after=2"), []protocol.Event{})

	prompt := postForTurn(t, srv.url+"/v1/tasks/"+string(task)+"/commands",
		`{"kind":"prompt","payload":{"text":"now to four"}}`, http.StatusAccepted)
	if delivered := receiveCommand(t, commands); delivered.Kind != protocol.CommandPrompt || delivered.TaskID != prompt.TaskID || string(delivered.Payload) != `{"text":"now to four"}` {
		t.Errorf("delivered %+v (payload %s), want the prompt queued as %+v", delivered, delivered.Payload, prompt)
	}
}

func TestGivenUnseenDaemonWhenCreatingTaskThenUnprocessable(t *testing.T) {
	srv := startTestServer(t)

	status, body := doRequest(t, http.MethodPost, srv.url+"/v1/tasks", startTaskBody)

	if status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d (%s), want 422", status, body)
	}
}

func TestGivenInvalidTaskWhenCreatingThenBadRequestAndNoCommandIsIssued(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")
	limits := `"pause_limits":{"acknowledge":"1m","cleanup":"5m"}`
	for name, body := range map[string]string{
		"not json":               `{`,
		"unknown field":          `{"daemon_id":"laptop","prompt":"p","promt":"p",` + limits + `}`,
		"unknown priority":       `{"daemon_id":"laptop","priority":"urgent","prompt":"p",` + limits + `}`,
		"unsafe daemon":          `{"daemon_id":"../x","prompt":"p",` + limits + `}`,
		"no prompt":              `{"daemon_id":"laptop",` + limits + `}`,
		"workspace without ref":  `{"daemon_id":"laptop","prompt":"p","workspace":{"repo":"r"},` + limits + `}`,
		"workspace without repo": `{"daemon_id":"laptop","prompt":"p","workspace":{"ref":"main"},` + limits + `}`,
		"no pause limits":        `{"daemon_id":"laptop","prompt":"p"}`,
		"zero pause limit":       `{"daemon_id":"laptop","prompt":"p","pause_limits":{"acknowledge":"0s","cleanup":"5m"}}`,
		"malformed pause limit":  `{"daemon_id":"laptop","prompt":"p","pause_limits":{"acknowledge":"soon","cleanup":"5m"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			status, response := doRequest(t, http.MethodPost, srv.url+"/v1/tasks", body)
			if status != http.StatusBadRequest {
				t.Errorf("status = %d (%s), want 400", status, response)
			}
		})
	}
	commands, err := srv.store.commandsAfter(t.Context(), "laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 0 {
		t.Errorf("issued %+v, want nothing", commands)
	}
}

func TestGivenPromptOrResumeWhenPostedThenItIsQueuedAsATurn(t *testing.T) {
	srv := startTestServer(t)
	seedTask(t, srv.store, "laptop", "task-1")
	for _, tc := range []struct {
		body string
		want turnKind
	}{
		{`{"kind":"prompt","payload":{"text":"go on"}}`, turnPrompt},
		{`{"kind":"resume","payload":null}`, turnResume},
	} {
		t.Run(string(tc.want), func(t *testing.T) {
			turn := postForTurn(t, srv.url+"/v1/tasks/task-1/commands", tc.body, http.StatusAccepted)
			if turn.Kind != tc.want || turn.TaskID != "task-1" || turn.ID == 0 {
				t.Errorf("turn = %+v, want a %s for task-1", turn, tc.want)
			}
		})
	}
}

func TestGivenEachImmediateCommandKindWhenIssuedThenItsPayloadIsCarried(t *testing.T) {
	srv := startTestServer(t)
	seedTask(t, srv.store, "laptop", "task-1")
	for _, tc := range []struct {
		body        string
		wantKind    protocol.CommandKind
		wantPayload string
	}{
		{`{"kind":"pause"}`, protocol.CommandPause, ``},
		{`{"kind":"interrupt"}`, protocol.CommandInterrupt, ``},
		{`{"kind":"stop"}`, protocol.CommandStop, ``},
		{`{"kind":"answer_permission","payload":{"request_id":"req-1","allow":true}}`, protocol.CommandAnswerPermission, `{"request_id":"req-1","allow":true}`},
	} {
		t.Run(string(tc.wantKind), func(t *testing.T) {
			command := postForCommand(t, srv.url+"/v1/tasks/task-1/commands", tc.body)
			if command.Kind != tc.wantKind || command.TaskID != "task-1" || command.DaemonID != "laptop" || string(command.Payload) != tc.wantPayload {
				t.Errorf("command = %+v (payload %s), want %s for task-1 on laptop with payload %q", command, command.Payload, tc.wantKind, tc.wantPayload)
			}
		})
	}
}

func TestGivenInvalidCommandWhenIssuingThenBadRequest(t *testing.T) {
	srv := startTestServer(t)
	seedTask(t, srv.store, "laptop", "task-1")
	for name, body := range map[string]string{
		"start_task":                   `{"kind":"start_task","payload":{"prompt":"p"}}`,
		"unknown kind":                 `{"kind":"reboot"}`,
		"no kind":                      `{}`,
		"pause with payload":           `{"kind":"pause","payload":{"now":true}}`,
		"prompt without payload":       `{"kind":"prompt"}`,
		"prompt without text":          `{"kind":"prompt","payload":{"text":""}}`,
		"prompt with unknown field":    `{"kind":"prompt","payload":{"text":"t","priority":"now"}}`,
		"answer without request id":    `{"kind":"answer_permission","payload":{"allow":true}}`,
		"unknown field in the request": `{"kind":"stop","reason":"done"}`,
	} {
		t.Run(name, func(t *testing.T) {
			status, response := doRequest(t, http.MethodPost, srv.url+"/v1/tasks/task-1/commands", body)
			if status != http.StatusBadRequest {
				t.Errorf("status = %d (%s), want 400", status, response)
			}
		})
	}
}

func TestGivenUnknownTaskWhenIssuingCommandThenNotFound(t *testing.T) {
	srv := startTestServer(t)

	status, body := doRequest(t, http.MethodPost, srv.url+"/v1/tasks/ghost/commands", `{"kind":"stop"}`)

	if status != http.StatusNotFound {
		t.Errorf("status = %d (%s), want 404", status, body)
	}
}

func TestGivenUnknownTaskWhenGettingEventsThenNotFound(t *testing.T) {
	srv := startTestServer(t)

	status, body := doRequest(t, http.MethodGet, srv.url+"/v1/tasks/ghost/events", "")

	if status != http.StatusNotFound {
		t.Errorf("status = %d (%s), want 404", status, body)
	}
}

func TestGivenMalformedAfterWhenGettingEventsThenBadRequest(t *testing.T) {
	srv := startTestServer(t)
	seedTask(t, srv.store, "laptop", "task-1")

	status, body := doRequest(t, http.MethodGet, srv.url+"/v1/tasks/task-1/events?after=-1", "")

	if status != http.StatusBadRequest {
		t.Errorf("status = %d (%s), want 400", status, body)
	}
}

const testDefaultModel = "default-model"

func TestGivenTaskWithoutModelWhenCreatingThenTheDefaultModelIsSent(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")
	for body, want := range map[string]string{
		`{"daemon_id":"laptop","prompt":"p","pause_limits":{"acknowledge":"1m","cleanup":"5m"}}`:                testDefaultModel,
		`{"daemon_id":"laptop","prompt":"p","model":"","pause_limits":{"acknowledge":"1m","cleanup":"5m"}}`:     testDefaultModel,
		`{"daemon_id":"laptop","prompt":"p","model":"opus","pause_limits":{"acknowledge":"1m","cleanup":"5m"}}`: "opus",
	} {
		turn := postForTurn(t, srv.url+"/v1/tasks", body, http.StatusCreated)
		var start struct {
			Model string `json:"model"`
		}
		getJSON(t, srv.url+"/v1/tasks/"+string(turn.TaskID), &start)
		if start.Model != want {
			t.Errorf("%s: model = %q, want %q", body, start.Model, want)
		}
	}
}

func getJSON(t *testing.T, url string, into any) {
	t.Helper()
	status, body := doRequest(t, http.MethodGet, url, "")
	if status != http.StatusOK {
		t.Fatalf("GET %s: status = %d (%s), want 200", url, status, body)
	}
	if err := json.Unmarshal([]byte(body), into); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
}

func TestGivenNoTasksWhenListingThenTheListIsEmpty(t *testing.T) {
	srv := startTestServer(t)

	status, body := doRequest(t, http.MethodGet, srv.url+"/v1/tasks", "")

	if status != http.StatusOK || body != "[]\n" {
		t.Errorf("status = %d, body = %q; want 200 and []", status, body)
	}
}

func TestGivenRunningTaskWhenListingAndGettingItThenItsStateActivityAndCostAreShown(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")
	first := postForTurn(t, srv.url+"/v1/tasks", startTaskBody, http.StatusCreated)
	second := postForTurn(t, srv.url+"/v1/tasks", `{"daemon_id":"laptop","prompt":"p","pause_limits":{"acknowledge":"1m","cleanup":"5m"}}`, http.StatusCreated)
	admitTurns(t, srv.store)
	active := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	started := event(first.TaskID, 1, `{"pid":1,"model":"haiku","workdir":"/w"}`)
	started.Kind = protocol.KindHarnessStarted
	result := event(first.TaskID, 2, `{"type":"result","subtype":"success","total_cost_usd":0.25}`)
	result.Time = active
	if status, body := postEvents(t, srv, "laptop", started, result); status != http.StatusOK {
		t.Fatalf("post events: %d %s", status, body)
	}

	var list []map[string]any
	getJSON(t, srv.url+"/v1/tasks", &list)
	var detail map[string]any
	getJSON(t, srv.url+"/v1/tasks/"+string(first.TaskID), &detail)

	if len(list) != 2 || list[0]["id"] != string(first.TaskID) || list[1]["id"] != string(second.TaskID) {
		t.Fatalf("list = %v, want both tasks, oldest first", list)
	}
	created := first.CreatedAt.Format(time.RFC3339Nano)
	requireJSONEqual(t, list[0], map[string]any{
		"id": string(first.TaskID), "daemon_id": "laptop", "state": "running", "model": "haiku", "priority": "normal", "filler": false,
		"created_at": list[0]["created_at"], "last_activity_at": active.Format(time.RFC3339Nano), "cost_usd": 0.25,
	})
	if got, _ := time.Parse(time.RFC3339Nano, list[0]["created_at"].(string)); got.After(first.CreatedAt) || first.CreatedAt.Sub(got) > time.Second {
		t.Errorf("created_at = %v, want just before the start was queued at %s", list[0]["created_at"], created)
	}
	if list[1]["state"] != "pending" || list[1]["model"] != testDefaultModel || list[1]["cost_usd"] != 0.0 {
		t.Errorf("second task = %v", list[1])
	}
	requireJSONEqual(t, detail, map[string]any{
		"id": string(first.TaskID), "daemon_id": "laptop", "state": "running", "model": "haiku", "priority": "normal", "filler": false,
		"created_at": list[0]["created_at"], "last_activity_at": active.Format(time.RFC3339Nano), "cost_usd": 0.25,
		"start": map[string]any{
			"prompt": "count to three", "system_prompt": systemPrompt(nil, "be brief"),
			"workspace":    map[string]any{"repo": "https://example.com/o/r.git", "ref": "main"},
			"model":        "haiku",
			"pause_limits": map[string]any{"acknowledge": "1m0s", "cleanup": "5m0s"},
		},
	})
}

func TestGivenUnknownTaskWhenGettingItThenNotFound(t *testing.T) {
	srv := startTestServer(t)

	status, body := doRequest(t, http.MethodGet, srv.url+"/v1/tasks/ghost", "")

	if status != http.StatusNotFound {
		t.Errorf("status = %d (%s), want 404", status, body)
	}
}

func TestGivenTaskWhoseDaemonIsNotConnectedWhenTheDaemonConnectsThenItGetsTheStart(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")
	created := postForTurn(t, srv.url+"/v1/tasks", startTaskBody, http.StatusCreated)
	if state := readProgress(t, srv.store, created.TaskID).State; state != TaskQueued {
		t.Fatalf("state before the daemon connects = %s, want queued", state)
	}

	commands := openCommandStream(t, srv, "laptop", "")

	if delivered := receiveCommand(t, commands); delivered.Kind != protocol.CommandStartTask || delivered.TaskID != created.TaskID {
		t.Errorf("delivered %+v, want the start of %s", delivered, created.TaskID)
	}
}

func TestGivenTaskThatHasNotStartedWhenStoppedThenItEndsWithNothingSentAndOtherCommandsAreRefused(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")
	created := postForTurn(t, srv.url+"/v1/tasks", startTaskBody, http.StatusCreated)
	commandsURL := srv.url + "/v1/tasks/" + string(created.TaskID) + "/commands"

	pause, pauseBody := doRequest(t, http.MethodPost, commandsURL, `{"kind":"pause"}`)
	stop, stopBody := doRequest(t, http.MethodPost, commandsURL, `{"kind":"stop"}`)

	if pause != http.StatusConflict {
		t.Errorf("pause: status = %d (%s), want 409", pause, pauseBody)
	}
	if stop != http.StatusNoContent {
		t.Errorf("stop: status = %d (%s), want 204", stop, stopBody)
	}
	if state := readProgress(t, srv.store, created.TaskID).State; state != TaskStopped {
		t.Errorf("state = %s, want stopped", state)
	}
	admitTurns(t, srv.store)
	commands, err := srv.store.commandsAfter(t.Context(), "laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 0 {
		t.Errorf("issued %+v, want nothing", commands)
	}
}
