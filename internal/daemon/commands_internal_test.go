package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

type dispatched struct{ id, data string }

func readAllEvents(t *testing.T, stream string) ([]dispatched, int) {
	t.Helper()
	var events []dispatched
	lines := 0
	err := readEvents(strings.NewReader(stream), func() { lines++ }, func(id, data string) error {
		events = append(events, dispatched{id, data})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return events, lines
}

func TestGivenEventsWithIDsAndDataWhenReadingTheStreamThenEachIsDispatchedWithItsID(t *testing.T) {
	events, _ := readAllEvents(t, "id: 1\ndata: {\"a\":1}\n\nid: 2\ndata: {\"b\":2}\n\n")

	want := []dispatched{{"1", `{"a":1}`}, {"2", `{"b":2}`}}
	if !slices.Equal(events, want) {
		t.Errorf("events = %q, want %q", events, want)
	}
}

func TestGivenCommentsAndUnknownFieldsWhenReadingTheStreamThenTheyAreSeenButNotDispatched(t *testing.T) {
	events, lines := readAllEvents(t, ": keepalive\n\nevent: command\nretry: 10\nid: 3\ndata: x\n\n: keepalive\n\n")

	if want := []dispatched{{"3", "x"}}; !slices.Equal(events, want) {
		t.Errorf("events = %q, want %q", events, want)
	}
	if lines != 9 {
		t.Errorf("seen %d lines, want 9", lines)
	}
}

func TestGivenMultiLineDataAndCRLFWhenReadingTheStreamThenTheLinesAreJoinedByNewlines(t *testing.T) {
	events, _ := readAllEvents(t, "id:4\r\ndata:first\r\ndata: second\r\ndata\r\n\r\n")

	if want := []dispatched{{"4", "first\nsecond\n"}}; !slices.Equal(events, want) {
		t.Errorf("events = %q, want %q", events, want)
	}
}

func TestGivenAnEventCutOffByTheEndOfTheStreamWhenReadingThenItIsNotDispatched(t *testing.T) {
	events, _ := readAllEvents(t, "id: 1\ndata: whole\n\nid: 2\ndata: torn")

	if want := []dispatched{{"1", "whole"}}; !slices.Equal(events, want) {
		t.Errorf("events = %q, want %q", events, want)
	}
}

func TestGivenEventWithoutIDWhenReadingThenItKeepsTheLastIDTheStreamSet(t *testing.T) {
	events, _ := readAllEvents(t, "id: 7\ndata: a\n\ndata: b\n\n")

	if want := []dispatched{{"7", "a"}, {"7", "b"}}; !slices.Equal(events, want) {
		t.Errorf("events = %q, want %q", events, want)
	}
}

func TestGivenDispatchErrorWhenReadingThenReadingStopsWithIt(t *testing.T) {
	stop := errors.New("stop")
	calls := 0
	err := readEvents(strings.NewReader("id: 1\ndata: a\n\nid: 2\ndata: b\n\n"), func() {}, func(string, string) error {
		calls++
		return stop
	})

	if !errors.Is(err, stop) || calls != 1 {
		t.Errorf("err = %v after %d calls", err, calls)
	}
}

// scriptedStream is a command stream server that answers each connection
// with the next script and records the Last-Event-ID each one sent.
type scriptedStream struct {
	mu          sync.Mutex
	scripts     []string
	lastEventID []string
	// order lists "facts" for each report of facts and "commands" for
	// each connection, as they arrive; facts holds the latest report.
	order []string
	facts protocol.Facts
}

func (s *scriptedStream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/facts"):
		var facts protocol.Facts
		if r.Method != http.MethodPut || json.NewDecoder(r.Body).Decode(&facts) != nil {
			http.Error(w, "not facts", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.order, s.facts = append(s.order, "facts"), facts
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(r.URL.Path, "/acks"):
		fmt.Fprint(w, "{}")
	case strings.HasSuffix(r.URL.Path, "/events"):
		// Hold nothing; the test only looks at commands.
		http.Error(w, "not stored", http.StatusServiceUnavailable)
	case strings.HasSuffix(r.URL.Path, "/commands"):
		s.mu.Lock()
		s.order = append(s.order, "commands")
		s.lastEventID = append(s.lastEventID, r.Header.Get("Last-Event-ID"))
		var script string
		if len(s.scripts) > 0 {
			script, s.scripts = s.scripts[0], s.scripts[1:]
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, script)
		if script == "" {
			// Out of script: hold the stream open until the client goes.
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	}
}

func (s *scriptedStream) connections() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.lastEventID)
}

func sseCommand(t *testing.T, id uint64, task protocol.TaskID, kind protocol.CommandKind, payload string) string {
	t.Helper()
	data := fmt.Sprintf(`{"id":%d,"daemon_id":%q,"task_id":%q,"kind":%q,"time":"2026-10-07T00:00:00Z"`, id, testDaemon, task, kind)
	if payload != "" {
		data += `,"payload":` + payload
	}
	return fmt.Sprintf("id: %d\ndata: %s}\n\n", id, data)
}

const startPayload = `{"prompt":"Do the work.","pause_limits":{"acknowledge":"2m0s","cleanup":"5m0s"}}`

func TestGivenCommandsSentAgainWhenReconnectingThenLastEventIDIsTheLastAppliedAndEachIsAppliedOnce(t *testing.T) {
	stream := &scriptedStream{scripts: []string{
		sseCommand(t, 5, "task-1", protocol.CommandStartTask, startPayload) +
			sseCommand(t, 6, "task-1", protocol.CommandInterrupt, "") +
			sseCommand(t, 6, "task-1", protocol.CommandInterrupt, ""),
		sseCommand(t, 5, "task-1", protocol.CommandStartTask, startPayload) +
			sseCommand(t, 6, "task-1", protocol.CommandInterrupt, "") +
			sseCommand(t, 7, "task-1", protocol.CommandPause, ""),
	}}
	httpServer := httptest.NewServer(stream)
	t.Cleanup(httpServer.Close)
	serverURL, _ := url.Parse(httpServer.URL)

	d := runDaemon(t, serverURL, t.TempDir())

	proc := d.nextProcess(t)
	var inputs []string
	for range 3 {
		in := proc.nextInput(t)
		inputs = append(inputs, in.kind+" "+in.text)
	}
	want := []string{"prompt Do the work.", "interrupt ", "prompt " + pausePrompt}
	if !slices.Equal(inputs, want) {
		t.Errorf("inputs = %q, want %q", inputs, want)
	}
	eventually(t, "a third connection", func() bool { return len(stream.connections()) >= 3 })
	if got := stream.connections()[:3]; !slices.Equal(got, []string{"", "6", "7"}) {
		t.Errorf("Last-Event-ID per connection = %q", got)
	}
	if len(d.harness.started) != 0 {
		t.Error("the start_task sent again started a second harness")
	}
}

func TestGivenDaemonWhenItOpensItsCommandStreamThenItReportsItsFactsFirstAndAgainOnEachReconnect(t *testing.T) {
	stream := &scriptedStream{scripts: []string{": the server ends this stream at once\n\n"}}
	httpServer := httptest.NewServer(stream)
	t.Cleanup(httpServer.Close)
	serverURL, _ := url.Parse(httpServer.URL)

	stateDir := t.TempDir()
	runDaemon(t, serverURL, stateDir)

	eventually(t, "a second connection", func() bool { return len(stream.connections()) >= 2 })
	stream.mu.Lock()
	order, facts := slices.Clone(stream.order[:4]), stream.facts
	stream.mu.Unlock()
	if !slices.Equal(order, []string{"facts", "commands", "facts", "commands"}) {
		t.Errorf("requests = %q, want facts before each connection", order)
	}
	want := detectFacts(newFakeHarness().Info())
	key, err := loadOrCreateSSHKey(stateDir, testDaemon)
	if err != nil {
		t.Fatal(err)
	}
	want[protocol.FactSSHPublicKey] = key.Blob
	if !maps.Equal(facts, want) || facts[protocol.FactOS] != runtime.GOOS || facts[protocol.FactCPUs] == "" || facts[protocol.FactHarness] == "" {
		t.Errorf("facts = %v, want %v", facts, want)
	}
}

func TestGivenMeminfoWhenReadingMemTotalThenItIsInBytes(t *testing.T) {
	got, ok := memTotal(strings.NewReader("MemTotal:       16314208 kB\nMemFree:          1048576 kB\n"))

	if !ok || got != 16314208*1024 {
		t.Errorf("memTotal = %d, %v", got, ok)
	}
	if _, ok := memTotal(strings.NewReader("MemFree: 1 kB\n")); ok {
		t.Error("memTotal found a total in meminfo without one")
	}
}
