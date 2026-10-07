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

// seedTask records daemon as seen and assigns a new task to it.
func seedTask(t *testing.T, store *Store, daemon protocol.DaemonID, task protocol.TaskID) {
	t.Helper()
	if _, err := store.heldSeqs(t.Context(), daemon); err != nil {
		t.Fatal(err)
	}
	if _, err := store.createTask(t.Context(), daemon, task, testStart); err != nil {
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

	_, err := store.createTask(t.Context(), "nobody", "task-1", testStart)

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
