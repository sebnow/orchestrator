package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

type testServer struct {
	*Server
	url  string
	logs *syncBuffer
}

// syncBuffer is a log sink the server may write to while a test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func startTestServer(t *testing.T) testServer {
	t.Helper()
	store, _ := openTestStore(t)
	logs := &syncBuffer{}
	srv := New(store, slog.New(slog.NewTextHandler(logs, nil)), Options{DefaultModel: testDefaultModel, Insecure: true})
	httpServer := httptest.NewServer(srv)
	t.Cleanup(func() {
		srv.EndStreams()
		httpServer.Close()
	})
	return testServer{Server: srv, url: httpServer.URL, logs: logs}
}

func doRequest(t *testing.T, method, url string, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(data)
}

func postEvents(t *testing.T, srv testServer, daemon protocol.DaemonID, events ...protocol.Event) (int, string) {
	t.Helper()
	body, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	return doRequest(t, http.MethodPost, srv.url+"/v1/daemons/"+string(daemon)+"/events", string(body))
}

func requireAcks(t *testing.T, status int, body string, want map[protocol.TaskID]uint64) {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", status, body)
	}
	wantBody, _ := json.Marshal(want)
	if strings.TrimSpace(body) != string(wantBody) {
		t.Fatalf("acks = %s, want %s", body, wantBody)
	}
}

func storedEventCount(t *testing.T, srv testServer, task protocol.TaskID) int {
	t.Helper()
	events, err := srv.store.eventsAfter(t.Context(), task, 0)
	if err != nil {
		t.Fatal(err)
	}
	return len(events)
}

func TestGivenNewBatchWhenPostingEventsThenEachTaskIsAckedAndTheDaemonIsSeen(t *testing.T) {
	srv := startTestServer(t)
	seedTask(t, srv.store, "laptop", "task-1")
	seedTask(t, srv.store, "laptop", "task-2")

	status, body := postEvents(t, srv, "laptop", event("task-1", 1, `{}`), event("task-2", 1, `{}`), event("task-1", 2, `{}`))

	requireAcks(t, status, body, map[protocol.TaskID]uint64{"task-1": 2, "task-2": 1})
	var harnessName, harnessVersion string
	err := srv.store.db.QueryRowContext(t.Context(), `SELECT harness_name, harness_version FROM daemons WHERE id = 'laptop'`).
		Scan(&harnessName, &harnessVersion)
	if err != nil {
		t.Fatal(err)
	}
	if harnessName != "claude-code" || harnessVersion != "2.1.289" {
		t.Errorf("daemon harness = %s %s, want the batch's", harnessName, harnessVersion)
	}
}

func TestGivenAckedBatchWhenPostedAgainThenItIsAckedAgainAndStoredOnce(t *testing.T) {
	srv := startTestServer(t)
	seedTask(t, srv.store, "laptop", "task-1")
	batch := []protocol.Event{event("task-1", 1, `{"a":1}`), event("task-1", 2, `{"a":2}`)}
	status, body := postEvents(t, srv, "laptop", batch...)
	requireAcks(t, status, body, map[protocol.TaskID]uint64{"task-1": 2})

	status, body = postEvents(t, srv, "laptop", batch...)

	requireAcks(t, status, body, map[protocol.TaskID]uint64{"task-1": 2})
	if count := storedEventCount(t, srv, "task-1"); count != 2 {
		t.Errorf("stored events = %d, want 2", count)
	}
	if logs := srv.logs.String(); logs != "" {
		t.Errorf("logged %q, want nothing for an identical replay", logs)
	}
}

func TestGivenBatchWithAGapWhenPostingEventsThenOnlyTheContiguousPrefixIsAcked(t *testing.T) {
	srv := startTestServer(t)
	seedTask(t, srv.store, "laptop", "task-1")

	status, body := postEvents(t, srv, "laptop", event("task-1", 1, `{}`), event("task-1", 2, `{}`), event("task-1", 4, `{}`))
	requireAcks(t, status, body, map[protocol.TaskID]uint64{"task-1": 2})

	status, body = postEvents(t, srv, "laptop", event("task-1", 3, `{}`))
	requireAcks(t, status, body, map[protocol.TaskID]uint64{"task-1": 4})
}

func TestGivenDifferingDuplicateWhenPostedThenTheStoredEventIsKeptAndTheConflictLogged(t *testing.T) {
	srv := startTestServer(t)
	seedTask(t, srv.store, "laptop", "task-1")
	postEvents(t, srv, "laptop", event("task-1", 1, `{"a":1}`))

	status, body := postEvents(t, srv, "laptop", event("task-1", 1, `{"a":"changed"}`))

	requireAcks(t, status, body, map[protocol.TaskID]uint64{"task-1": 1})
	events, err := srv.store.eventsAfter(t.Context(), "task-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(events[0].Payload) != `{"a":1}` {
		t.Errorf("stored payload = %s, want the first", events[0].Payload)
	}
	if logs := srv.logs.String(); !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "task=task-1 seq=1") {
		t.Errorf("logs = %q, want a warning naming task-1 seq 1", logs)
	}
}

func TestGivenTaskOfAnotherDaemonWhenPostingEventsThenConflictNamesTheTaskAndNothingIsStored(t *testing.T) {
	srv := startTestServer(t)
	seedTask(t, srv.store, "laptop", "mine")
	seedTask(t, srv.store, "vps", "theirs")

	status, body := postEvents(t, srv, "laptop", event("mine", 1, `{}`), event("theirs", 1, `{}`))

	if status != http.StatusConflict || !strings.Contains(body, `"theirs"`) {
		t.Errorf("status = %d, body = %q; want 409 naming task theirs", status, body)
	}
	if count := storedEventCount(t, srv, "mine"); count != 0 {
		t.Errorf("stored events of mine = %d, want 0", count)
	}
}

func TestGivenUnknownTaskWhenPostingEventsThenConflictNamesTheTask(t *testing.T) {
	srv := startTestServer(t)

	status, body := postEvents(t, srv, "laptop", event("ghost", 1, `{}`))

	if status != http.StatusConflict || !strings.Contains(body, `"ghost"`) {
		t.Errorf("status = %d, body = %q; want 409 naming task ghost", status, body)
	}
}

func TestGivenMalformedBatchWhenPostingEventsThenBadRequest(t *testing.T) {
	srv := startTestServer(t)
	seedTask(t, srv.store, "laptop", "task-1")
	valid := `"kind":"harness_output","harness":{"name":"h","version":"1"},"time":"2026-10-07T12:00:00Z","payload":{}`
	for name, body := range map[string]string{
		"not json":       `[{`,
		"not an array":   `{"task_id":"task-1","seq":1,` + valid + `}`,
		"seq zero":       `[{"task_id":"task-1","seq":0,` + valid + `}]`,
		"seq too large":  `[{"task_id":"task-1","seq":9223372036854775808,` + valid + `}]`,
		"unsafe task id": `[{"task_id":"../x","seq":1,` + valid + `}]`,
		"no payload":     `[{"task_id":"task-1","seq":1,"kind":"harness_output","harness":{"name":"h","version":"1"},"time":"2026-10-07T12:00:00Z"}]`,
		"no kind":        `[{"task_id":"task-1","seq":1,"harness":{"name":"h","version":"1"},"time":"2026-10-07T12:00:00Z","payload":{}}]`,
	} {
		t.Run(name, func(t *testing.T) {
			status, response := doRequest(t, http.MethodPost, srv.url+"/v1/daemons/laptop/events", body)
			if status != http.StatusBadRequest {
				t.Errorf("status = %d (%s), want 400", status, response)
			}
		})
	}
	if count := storedEventCount(t, srv, "task-1"); count != 0 {
		t.Errorf("stored events = %d, want 0", count)
	}
}

func TestGivenHeldEventsWhenGettingAcksThenEveryTaskOfTheDaemonIsReported(t *testing.T) {
	srv := startTestServer(t)
	seedTask(t, srv.store, "laptop", "busy")
	seedTask(t, srv.store, "laptop", "gap")
	seedTask(t, srv.store, "laptop", "idle")
	seedTask(t, srv.store, "vps", "elsewhere")
	postEvents(t, srv, "laptop", event("busy", 1, `{}`), event("busy", 2, `{}`), event("gap", 1, `{}`), event("gap", 3, `{}`))
	postEvents(t, srv, "vps", event("elsewhere", 1, `{}`))

	status, body := doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")

	requireAcks(t, status, body, map[protocol.TaskID]uint64{"busy": 2, "gap": 1, "idle": 0})
}

func TestGivenNewDaemonWhenGettingAcksThenItIsSeenWithNoTasks(t *testing.T) {
	srv := startTestServer(t)

	status, body := doRequest(t, http.MethodGet, srv.url+"/v1/daemons/fresh/acks", "")

	requireAcks(t, status, body, map[protocol.TaskID]uint64{})
	if _, err := srv.store.createTask(t.Context(), "fresh", "task-1", testStart); err != nil {
		t.Errorf("createTask on the seen daemon: %v", err)
	}
}

type sseEvent struct {
	id   string
	data string
}

// openCommandStream connects to the daemon's command stream and delivers
// its events on the returned channel, which is closed when the stream
// ends. lastEventID is sent when not empty.
func openCommandStream(t *testing.T, srv testServer, daemon protocol.DaemonID, lastEventID string) <-chan sseEvent {
	t.Helper()
	return openEventStream(t, srv.url+"/v1/daemons/"+string(daemon)+"/commands", lastEventID)
}

// openEventStream connects to the event stream at url, as
// openCommandStream does. An event's data lines are joined by line feeds.
func openEventStream(t *testing.T, url, lastEventID string) <-chan sseEvent {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		response.Body.Close()
		t.Fatalf("status = %d, content type = %q; want an event stream", response.StatusCode, response.Header.Get("Content-Type"))
	}
	events := make(chan sseEvent)
	ctx := t.Context()
	go func() {
		defer close(events)
		defer response.Body.Close()
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(nil, 4<<20)
		var current sseEvent
		var data []string
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "":
				current.data = strings.Join(data, "\n")
				data = nil
				if current != (sseEvent{}) {
					select {
					case events <- current:
					case <-ctx.Done():
						return
					}
				}
				current = sseEvent{}
			case strings.HasPrefix(line, "id: "):
				current.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: "))
			}
		}
	}()
	return events
}

func receiveCommand(t *testing.T, events <-chan sseEvent) protocol.Command {
	t.Helper()
	select {
	case event, ok := <-events:
		if !ok {
			t.Fatal("command stream ended")
		}
		var command protocol.Command
		if err := json.Unmarshal([]byte(event.data), &command); err != nil {
			t.Fatalf("decode %q: %v", event.data, err)
		}
		if event.id != strconv.FormatUint(command.ID, 10) {
			t.Fatalf("event id %q differs from command id %d", event.id, command.ID)
		}
		return command
	case <-time.After(5 * time.Second):
		t.Fatal("no command within 5s")
	}
	return protocol.Command{}
}

func TestGivenLastEventIDWhenReconnectingThenExactlyTheMissedCommandsAreResentThenLiveOnes(t *testing.T) {
	srv := startTestServer(t)
	seedTask(t, srv.store, "laptop", "task-1")
	seedTask(t, srv.store, "vps", "task-2")
	prompt, err := srv.store.issueCommand(t.Context(), "task-1", protocol.CommandPrompt, json.RawMessage(`{"text":"go on"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.issueCommand(t.Context(), "task-2", protocol.CommandPause, nil); err != nil {
		t.Fatal(err)
	}
	pause, err := srv.store.issueCommand(t.Context(), "task-1", protocol.CommandPause, nil)
	if err != nil {
		t.Fatal(err)
	}

	events := openCommandStream(t, srv, "laptop", "1")
	gotPrompt := receiveCommand(t, events)
	gotPause := receiveCommand(t, events)
	resume, err := srv.store.issueCommand(t.Context(), "task-1", protocol.CommandResume, nil)
	if err != nil {
		t.Fatal(err)
	}
	gotResume := receiveCommand(t, events)

	got, _ := json.Marshal([]protocol.Command{gotPrompt, gotPause, gotResume})
	want, _ := json.Marshal([]protocol.Command{prompt, pause, resume})
	if string(got) != string(want) {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestGivenSecondStreamForADaemonWhenItConnectsThenTheFirstEnds(t *testing.T) {
	srv := startTestServer(t)
	first := openCommandStream(t, srv, "laptop", "")

	second := openCommandStream(t, srv, "laptop", "")

	select {
	case event, ok := <-first:
		if ok {
			t.Fatalf("first stream got %+v, want it ended", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first stream still open after 5s")
	}
	if _, err := srv.store.createTask(t.Context(), "laptop", "task-1", testStart); err != nil {
		t.Fatal(err)
	}
	if command := receiveCommand(t, second); command.Kind != protocol.CommandStartTask {
		t.Errorf("second stream got %+v, want the start_task", command)
	}
}

func TestGivenEndStreamsWhenAStreamIsOpenThenItEnds(t *testing.T) {
	srv := startTestServer(t)
	events := openCommandStream(t, srv, "laptop", "")

	srv.EndStreams()

	select {
	case event, ok := <-events:
		if ok {
			t.Fatalf("stream got %+v, want it ended", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream still open after 5s")
	}
}

func TestGivenMalformedLastEventIDWhenStreamingCommandsThenBadRequest(t *testing.T) {
	srv := startTestServer(t)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.url+"/v1/daemons/laptop/commands", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Last-Event-ID", "seven")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", response.StatusCode)
	}
}

func TestGivenUnsafeDaemonIDWhenPostingEventsThenBadRequest(t *testing.T) {
	srv := startTestServer(t)

	status, body := doRequest(t, http.MethodPost, srv.url+"/v1/daemons/a%20b/events", "[]")

	if status != http.StatusBadRequest {
		t.Errorf("status = %d (%s), want 400", status, body)
	}
}
