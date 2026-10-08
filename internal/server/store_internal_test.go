package server

import (
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

var testStart = protocol.StartTask{
	Prompt:      "count to three",
	PauseLimits: protocol.PauseLimits{Acknowledge: time.Minute, Cleanup: 5 * time.Minute},
}

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.db")
	store, err := OpenStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, path
}

// seedTask records daemon as seen, assigns a new task to it, and admits
// the task's start.
func seedTask(t *testing.T, store *Store, daemon protocol.DaemonID, task protocol.TaskID) {
	t.Helper()
	queueTask(t, store, ownersTask(task, daemon))
	admitTurns(t, store)
}

// ownersTask is task, as the owner starts it on daemon.
func ownersTask(task protocol.TaskID, daemon protocol.DaemonID) newTask {
	return newTask{ID: task, Daemon: daemon, Placement: placementBound, Priority: PriorityNormal, Start: testStart, Origin: originOwner}
}

// queueTask records task's daemon as seen and queues the task.
func queueTask(t *testing.T, store *Store, task newTask) {
	t.Helper()
	if task.Daemon != "" {
		if _, err := store.heldSeqs(t.Context(), task.Daemon); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.createTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
}

// roomyPolicy has room for every turn the store tests queue.
var roomyPolicy = SchedulePolicy{SlotsPerDaemon: 100, FillerThreshold: 1, LowThreshold: 1}

// admitTurns runs a scheduler pass with every daemon seen connected and
// room for every turn.
func admitTurns(t *testing.T, store *Store) {
	t.Helper()
	var connected []protocol.DaemonID
	rows, err := store.db.QueryContext(t.Context(), `SELECT id FROM daemons`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		connected = append(connected, protocol.DaemonID(id))
	}
	rows.Close()
	if _, err := store.schedule(t.Context(), roomyPolicy, time.Now(), connected); err != nil {
		t.Fatal(err)
	}
}

func event(task protocol.TaskID, seq uint64, payload string) protocol.Event {
	return protocol.Event{
		TaskID:  task,
		Seq:     seq,
		Kind:    protocol.KindHarnessOutput,
		Harness: protocol.Harness{Name: "claude-code", Version: "2.1.289"},
		Time:    time.Date(2026, 10, 7, 12, 0, int(seq), 0, time.FixedZone("CEST", 2*60*60)),
		Payload: json.RawMessage(payload),
	}
}

func TestGivenExistingDatabaseWhenReopeningStoreThenItsRecordIsKept(t *testing.T) {
	store, path := openTestStore(t)
	seedTask(t, store, "laptop", "task-1")
	store.Close()

	reopened, err := OpenStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	commands, err := reopened.commandsAfter(t.Context(), "laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || commands[0].Kind != protocol.CommandStartTask {
		t.Errorf("commands = %+v, want the one start_task", commands)
	}
}

func TestGivenDatabaseOfAnotherSchemaVersionWhenOpeningStoreThenItIsRefused(t *testing.T) {
	store, path := openTestStore(t)
	if _, err := store.db.ExecContext(t.Context(), `UPDATE schema_version SET version = ?`, schemaVersion+1); err != nil {
		t.Fatal(err)
	}
	store.Close()

	reopened, err := OpenStore(t.Context(), path)
	if err == nil {
		reopened.Close()
		t.Fatal("OpenStore succeeded, want an error naming the schema version")
	}
}

func TestGivenEventsWithGapsWhenAppendingThenHeldSeqIsTheEndOfTheContiguousPrefix(t *testing.T) {
	store, _ := openTestStore(t)
	seedTask(t, store, "laptop", "starts-late")
	seedTask(t, store, "laptop", "gap")
	seedTask(t, store, "laptop", "whole")

	held, conflicts, err := store.appendEvents(t.Context(), "laptop", []protocol.Event{
		event("starts-late", 2, `{}`),
		event("gap", 1, `{}`), event("gap", 2, `{}`), event("gap", 4, `{}`),
		event("whole", 2, `{}`), event("whole", 1, `{}`), event("whole", 3, `{}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	want := map[protocol.TaskID]uint64{"starts-late": 0, "gap": 2, "whole": 3}
	if !maps.Equal(held, want) || len(conflicts) != 0 {
		t.Errorf("held = %v, conflicts = %v; want %v and none", held, conflicts, want)
	}
}

func TestGivenStoredEventWhenAppendedAgainWithAnotherPayloadThenTheStoredOneIsKeptAndReported(t *testing.T) {
	store, _ := openTestStore(t)
	seedTask(t, store, "laptop", "task-1")
	if _, _, err := store.appendEvents(t.Context(), "laptop", []protocol.Event{event("task-1", 1, `{"n":1}`)}); err != nil {
		t.Fatal(err)
	}

	differing := event("task-1", 1, `{"n":2}`)
	held, conflicts, err := store.appendEvents(t.Context(), "laptop", []protocol.Event{event("task-1", 1, `{"n":1}`), differing})
	if err != nil {
		t.Fatal(err)
	}

	if held["task-1"] != 1 || len(conflicts) != 1 || string(conflicts[0].Payload) != `{"n":2}` {
		t.Errorf("held = %v, conflicts = %+v; want task-1 at 1 and the differing event", held, conflicts)
	}
	events, err := store.eventsAfter(t.Context(), "task-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || string(events[0].Payload) != `{"n":1}` {
		t.Errorf("stored events = %+v, want only the first", events)
	}
}

func TestGivenTaskOfAnotherDaemonWhenAppendingThenTheBatchIsRefusedWhole(t *testing.T) {
	store, _ := openTestStore(t)
	seedTask(t, store, "laptop", "mine")
	seedTask(t, store, "vps", "theirs")

	_, _, err := store.appendEvents(t.Context(), "laptop", []protocol.Event{event("mine", 1, `{}`), event("theirs", 1, `{}`)})

	foreign, ok := errors.AsType[*foreignTaskError](err)
	if !ok || foreign.Task != "theirs" {
		t.Fatalf("err = %v, want a foreignTaskError naming task theirs", err)
	}
	events, err := store.eventsAfter(t.Context(), "mine", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("stored events = %+v, want none", events)
	}
}

func TestGivenStoredEventWhenReadBackThenItEqualsTheOneAppended(t *testing.T) {
	store, _ := openTestStore(t)
	seedTask(t, store, "laptop", "task-1")
	appended := event("task-1", 1, `{"type":"result","text":"<b>"}`)
	if _, _, err := store.appendEvents(t.Context(), "laptop", []protocol.Event{appended}); err != nil {
		t.Fatal(err)
	}

	events, err := store.eventsAfter(t.Context(), "task-1", 0)
	if err != nil {
		t.Fatal(err)
	}

	got, _ := json.Marshal(events)
	want, _ := json.Marshal([]protocol.Event{appended})
	if string(got) != string(want) {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestGivenUnseenDaemonWhenCreatingTaskThenErrUnknownDaemon(t *testing.T) {
	store, _ := openTestStore(t)

	_, err := store.createTask(t.Context(), ownersTask("task-1", "nobody"))

	if !errors.Is(err, errUnknownDaemon) {
		t.Errorf("err = %v, want errUnknownDaemon", err)
	}
}

func TestGivenCommandsForSeveralDaemonsWhenReadingAfterAnIDThenOnlyTheDaemonsLaterOnesAreReturnedInOrder(t *testing.T) {
	store, _ := openTestStore(t)
	seedTask(t, store, "laptop", "task-1")
	seedTask(t, store, "vps", "task-2")
	prompt, err := store.issueCommand(t.Context(), "task-1", protocol.CommandPrompt, json.RawMessage(`{"text":"more"}`))
	if err != nil {
		t.Fatal(err)
	}
	pause, err := store.issueCommand(t.Context(), "task-1", protocol.CommandPause, nil)
	if err != nil {
		t.Fatal(err)
	}

	commands, err := store.commandsAfter(t.Context(), "laptop", 1)
	if err != nil {
		t.Fatal(err)
	}

	got, _ := json.Marshal(commands)
	want, _ := json.Marshal([]protocol.Command{prompt, pause})
	if string(got) != string(want) {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestGivenUnknownTaskWhenIssuingCommandThenErrUnknownTask(t *testing.T) {
	store, _ := openTestStore(t)

	_, err := store.issueCommand(t.Context(), "nothing", protocol.CommandStop, nil)

	if !errors.Is(err, errUnknownTask) {
		t.Errorf("err = %v, want errUnknownTask", err)
	}
	var count int
	if err := store.db.QueryRowContext(t.Context(), `SELECT count(*) FROM commands`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("commands stored = %d, want 0", count)
	}
}

func TestGivenQuotaReadingsInSeveralZonesWhenListingDaemonsThenEachHasItsNewestReading(t *testing.T) {
	store, _ := openTestStore(t)
	seedTask(t, store, "laptop", "task-1")
	seedTask(t, store, "laptop", "task-2")
	seedTask(t, store, "desktop", "task-3")
	reading := func(task protocol.TaskID, seq uint64, at time.Time, utilization string) protocol.Event {
		e := event(task, seq, `{"status":"allowed","windows":[{"name":"five_hour","utilization":`+utilization+`,"resets_at":"2026-10-07T17:00:00Z"}]}`)
		e.Kind, e.Time = protocol.KindQuotaObserved, at
		return e
	}
	cest := time.FixedZone("CEST", 2*60*60)
	// As text the CEST times sort after the UTC one, yet they are earlier.
	newest := time.Date(2026, 10, 7, 12, 0, 0, 123456789, time.UTC)
	if _, _, err := store.appendEvents(t.Context(), "laptop", []protocol.Event{
		reading("task-1", 1, time.Date(2026, 10, 7, 13, 59, 0, 0, cest), "0.1"),
		reading("task-2", 1, newest, "0.3"),
		reading("task-1", 2, time.Date(2026, 10, 7, 13, 30, 0, 0, cest), "0.2"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.heldSeqs(t.Context(), "idle"); err != nil {
		t.Fatal(err)
	}

	daemons, err := store.daemons(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	got := map[protocol.DaemonID]*protocol.QuotaObserved{}
	for _, daemon := range daemons {
		got[daemon.ID] = daemon.Quota
		if daemon.ID == "laptop" && !daemon.QuotaAt.Equal(newest) {
			t.Errorf("laptop reading at %v, want %v", daemon.QuotaAt, newest)
		}
		if daemon.ID == "laptop" && (daemon.Harness == nil || daemon.Harness.Name != "claude-code") {
			t.Errorf("laptop harness = %+v", daemon.Harness)
		}
	}
	if len(daemons) != 3 || got["desktop"] != nil || got["idle"] != nil {
		t.Fatalf("daemons = %+v, want desktop, idle and laptop, only laptop with a reading", daemons)
	}
	if utilization := got["laptop"].Windows[0].Utilization; utilization != 0.3 {
		t.Errorf("laptop utilization = %v, want the newest, 0.3", utilization)
	}
}
