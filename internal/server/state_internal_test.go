package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

func controlEvent(kind protocol.Kind, payload string) protocol.Event {
	return protocol.Event{Kind: kind, Payload: json.RawMessage(payload)}
}

func TestGivenEachStateWhenACommandIsIssuedThenTheStateFollowsTheTable(t *testing.T) {
	for _, tc := range []struct {
		from TaskState
		kind protocol.CommandKind
		want TaskState
	}{
		{TaskPending, protocol.CommandStartTask, TaskPending},
		{TaskPending, protocol.CommandPause, TaskPausing},
		{TaskRunning, protocol.CommandPause, TaskPausing},
		{TaskAwaitingPermission, protocol.CommandPause, TaskPausing},
		{TaskPaused, protocol.CommandPause, TaskPaused},
		{TaskPausing, protocol.CommandResume, TaskRunning},
		{TaskPaused, protocol.CommandResume, TaskRunning},
		{TaskRunning, protocol.CommandResume, TaskRunning},
		{TaskPaused, protocol.CommandPrompt, TaskRunning},
		{TaskPausing, protocol.CommandPrompt, TaskRunning},
		{TaskAwaitingPermission, protocol.CommandPrompt, TaskAwaitingPermission},
		{TaskAwaitingPermission, protocol.CommandAnswerPermission, TaskRunning},
		{TaskRunning, protocol.CommandAnswerPermission, TaskRunning},
		{TaskRunning, protocol.CommandInterrupt, TaskRunning},
		{TaskPausing, protocol.CommandInterrupt, TaskPausing},
		{TaskRunning, protocol.CommandStop, TaskRunning},
		{TaskFinished, protocol.CommandPrompt, TaskRunning},
		{TaskFinished, protocol.CommandResume, TaskFinished},
		{TaskFinished, protocol.CommandPause, TaskFinished},
		{TaskFinished, protocol.CommandInterrupt, TaskFinished},
		{TaskFinished, protocol.CommandStop, TaskStopped},
		{TaskPaused, protocol.CommandStop, TaskStopped},
		{TaskStopped, protocol.CommandPrompt, TaskStopped},
		{TaskFailed, protocol.CommandPrompt, TaskFailed},
		{TaskStopped, protocol.CommandResume, TaskStopped},
		{TaskFailed, protocol.CommandPause, TaskFailed},
	} {
		if got := tc.from.afterCommand(tc.kind); got != tc.want {
			t.Errorf("%s + %s command = %s, want %s", tc.from, tc.kind, got, tc.want)
		}
	}
}

func TestGivenEachStateWhenAnEventIsStoredThenTheStateFollowsTheTable(t *testing.T) {
	started := controlEvent(protocol.KindHarnessStarted, `{"pid":1}`)
	permission := controlEvent(protocol.KindPermissionRequested, `{"request_id":"r1","tool":"Bash","input":{}}`)
	settled := controlEvent(protocol.KindPauseSettled, `{"interrupted":false}`)
	exitedCleanly := controlEvent(protocol.KindHarnessExited, `{"exit_code":0}`)
	exitedNonZero := controlEvent(protocol.KindHarnessExited, `{"exit_code":1}`)
	restarted := controlEvent(protocol.KindHarnessExited, `{"exit_code":-1,"error":"daemon restarted"}`)
	neverStarted := controlEvent(protocol.KindHarnessExited, `{"exit_code":-1,"error":"clone https://example.com/r.git: exit status 128"}`)
	zeroWithError := controlEvent(protocol.KindHarnessExited, `{"exit_code":0,"error":"read harness output: broken pipe"}`)
	output := controlEvent(protocol.KindHarnessOutput, `{"type":"assistant"}`)
	for _, tc := range []struct {
		name       string
		from       TaskState
		event      protocol.Event
		stopIssued bool
		want       TaskState
	}{
		{"start", TaskPending, started, false, TaskRunning},
		{"start while a pause waits", TaskPausing, started, false, TaskPausing},
		{"permission asked", TaskRunning, permission, false, TaskAwaitingPermission},
		{"permission asked while pausing", TaskPausing, permission, false, TaskPausing},
		{"pause settles", TaskPausing, settled, false, TaskPaused},
		{"pause settles after a resume", TaskRunning, settled, false, TaskRunning},
		{"harness output", TaskRunning, output, false, TaskRunning},
		{"clean exit", TaskRunning, exitedCleanly, false, TaskFinished},
		{"clean exit while paused", TaskPaused, exitedCleanly, false, TaskPaused},
		{"clean exit before the pause settled", TaskPausing, exitedCleanly, false, TaskFinished},
		{"failed exit while paused", TaskPaused, exitedNonZero, false, TaskFailed},
		{"next process of a finished task", TaskFinished, started, false, TaskRunning},
		{"next process of a paused task", TaskPaused, started, false, TaskRunning},
		{"exit after stop", TaskRunning, exitedCleanly, true, TaskStopped},
		{"killed after stop", TaskPausing, restarted, true, TaskStopped},
		{"non-zero exit", TaskRunning, exitedNonZero, false, TaskFailed},
		{"daemon restarted", TaskAwaitingPermission, restarted, false, TaskFailed},
		{"clone failed", TaskPending, neverStarted, false, TaskFailed},
		{"exit 0 with an error", TaskRunning, zeroWithError, false, TaskFailed},
		{"undecodable exit", TaskRunning, controlEvent(protocol.KindHarnessExited, `"gone"`), false, TaskFailed},
		{"exit after a stop took effect", TaskStopped, exitedNonZero, false, TaskStopped},
		{"start after the end", TaskFailed, started, false, TaskFailed},
	} {
		if got := tc.from.afterEvent(tc.event, tc.stopIssued); got != tc.want {
			t.Errorf("%s: %s + %s = %s, want %s", tc.name, tc.from, tc.event.Kind, got, tc.want)
		}
	}
}

func readProgress(t *testing.T, store *Store, task protocol.TaskID) progress {
	t.Helper()
	tx, err := store.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	p, err := loadProgress(t.Context(), tx, task)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// lifecycle drives a stored task through events and commands, checking
// its state after each step.
type lifecycle struct {
	t     *testing.T
	store *Store
	task  protocol.TaskID
	seq   uint64
}

func (l *lifecycle) event(kind protocol.Kind, payload string, want TaskState) {
	l.t.Helper()
	l.seq++
	e := event(l.task, l.seq, payload)
	e.Kind = kind
	e.Time = time.Now()
	if _, _, err := l.store.appendEvents(l.t.Context(), "laptop", []protocol.Event{e}); err != nil {
		l.t.Fatal(err)
	}
	if got := readProgress(l.t, l.store, l.task).State; got != want {
		l.t.Fatalf("after %s event: state = %s, want %s", kind, got, want)
	}
}

func (l *lifecycle) command(kind protocol.CommandKind, payload string, want TaskState) {
	l.t.Helper()
	var raw json.RawMessage
	if payload != "" {
		raw = json.RawMessage(payload)
	}
	if _, err := l.store.issueCommand(l.t.Context(), l.task, kind, raw); err != nil {
		l.t.Fatal(err)
	}
	if got := readProgress(l.t, l.store, l.task).State; got != want {
		l.t.Fatalf("after %s command: state = %s, want %s", kind, got, want)
	}
}

func newLifecycle(t *testing.T, store *Store, task protocol.TaskID) *lifecycle {
	t.Helper()
	seedTask(t, store, "laptop", task)
	if got := readProgress(t, store, task).State; got != TaskPending {
		t.Fatalf("new task state = %s, want pending", got)
	}
	return &lifecycle{t: t, store: store, task: task}
}

func TestGivenTaskWhenItRunsThroughEveryTransitionAndExitsCleanlyThenItIsFinishedAndAPromptResumesIt(t *testing.T) {
	store, _ := openTestStore(t)
	l := newLifecycle(t, store, "task-1")

	l.event(protocol.KindHarnessStarted, `{"pid":1,"model":"haiku","workdir":"/w"}`, TaskRunning)
	l.event(protocol.KindPermissionRequested, `{"request_id":"r1","tool":"Bash","input":{}}`, TaskAwaitingPermission)
	l.command(protocol.CommandAnswerPermission, `{"request_id":"r1","allow":true}`, TaskRunning)
	l.command(protocol.CommandPause, "", TaskPausing)
	l.event(protocol.KindPauseAcknowledged, `{"note":"after step 1"}`, TaskPausing)
	l.command(protocol.CommandInterrupt, "", TaskPausing)
	l.event(protocol.KindPauseSettled, `{"interrupted":false}`, TaskPaused)
	l.event(protocol.KindHarnessExited, `{"exit_code":0}`, TaskPaused)
	l.command(protocol.CommandResume, "", TaskRunning)
	l.event(protocol.KindHarnessStarted, `{"pid":2}`, TaskRunning)
	l.command(protocol.CommandInterrupt, "", TaskRunning)
	l.event(protocol.KindHarnessExited, `{"exit_code":0}`, TaskFinished)
	l.command(protocol.CommandPrompt, `{"text":"more"}`, TaskRunning)
	l.event(protocol.KindHarnessStarted, `{"pid":3}`, TaskRunning)
	l.event(protocol.KindHarnessExited, `{"exit_code":0}`, TaskFinished)
}

func TestGivenResumeIssuedBeforeTheOldProcessExitReachesTheServerWhenEventsArriveThenTheTaskEndsUpRunning(t *testing.T) {
	store, _ := openTestStore(t)
	l := newLifecycle(t, store, "task-1")

	l.event(protocol.KindHarnessStarted, `{"pid":1}`, TaskRunning)
	l.command(protocol.CommandPause, "", TaskPausing)
	l.event(protocol.KindPauseSettled, `{"interrupted":false}`, TaskPaused)
	l.command(protocol.CommandResume, "", TaskRunning)
	l.event(protocol.KindHarnessExited, `{"exit_code":0}`, TaskFinished)
	l.event(protocol.KindHarnessStarted, `{"pid":2}`, TaskRunning)
}

func TestGivenTaskBetweenProcessesWhenStoppedThenItIsStoppedAtOnceAndTakesNoMoreCommands(t *testing.T) {
	store, _ := openTestStore(t)
	l := newLifecycle(t, store, "task-1")
	l.event(protocol.KindHarnessStarted, `{"pid":1}`, TaskRunning)
	l.event(protocol.KindHarnessExited, `{"exit_code":0}`, TaskFinished)

	l.command(protocol.CommandStop, "", TaskStopped)

	_, err := store.issueCommand(t.Context(), "task-1", protocol.CommandPrompt, json.RawMessage(`{"text":"more"}`))
	if !errors.Is(err, errTaskEnded) {
		t.Errorf("prompt to a stopped task: %v, want errTaskEnded", err)
	}
}

func TestGivenStopIssuedWhenTheHarnessExitsThenTheTaskIsStopped(t *testing.T) {
	store, _ := openTestStore(t)
	l := newLifecycle(t, store, "task-1")

	l.event(protocol.KindHarnessStarted, `{"pid":1}`, TaskRunning)
	l.command(protocol.CommandStop, "", TaskRunning)
	l.event(protocol.KindHarnessExited, `{"exit_code":0}`, TaskStopped)
}

func TestGivenRunningTaskWhenTheDaemonRestartsThenTheTaskFailed(t *testing.T) {
	store, _ := openTestStore(t)
	l := newLifecycle(t, store, "task-1")

	l.event(protocol.KindHarnessStarted, `{"pid":1}`, TaskRunning)
	l.event(protocol.KindHarnessExited, `{"exit_code":-1,"error":"daemon restarted"}`, TaskFailed)
}

func TestGivenEventsAndCommandsWhenStoredThenLastActivityIsTheLatestAndCostTheHighestRunningTotal(t *testing.T) {
	store, _ := openTestStore(t)
	seedTask(t, store, "laptop", "task-1")
	future := time.Now().Add(time.Hour).Truncate(time.Second)
	result := func(seq uint64, cost string, at time.Time) protocol.Event {
		e := event("task-1", seq, `{"type":"result","subtype":"success","total_cost_usd":`+cost+`}`)
		e.Time = at
		return e
	}
	// The second batch repeats seq 2, which must not count twice, and
	// carries a lower total stamped earlier, which must not win.
	batches := [][]protocol.Event{
		{result(1, "0.02", future), result(2, "0.03", future.Add(time.Second))},
		{result(2, "0.03", future.Add(time.Second)), result(3, "0.01", future.Add(-time.Minute))},
	}
	for _, batch := range batches {
		if _, _, err := store.appendEvents(t.Context(), "laptop", batch); err != nil {
			t.Fatal(err)
		}
	}

	p := readProgress(t, store, "task-1")
	if p.CostUSD != 0.03 || !p.LastActivity.Equal(future.Add(time.Second)) {
		t.Errorf("progress = %+v, want cost 0.03 and last activity %s", p, future.Add(time.Second))
	}

	command, err := store.issueCommand(t.Context(), "task-1", protocol.CommandPause, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p := readProgress(t, store, "task-1"); !p.LastActivity.Equal(future.Add(time.Second)) {
		t.Errorf("a command issued at %s moved last activity back to %s", command.Time, p.LastActivity)
	}
}

// createVersionOneDatabase creates a database as schema version 1 left
// it, holding one daemon and one pending task.
func createVersionOneDatabase(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=foreign_keys(1)"}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL) STRICT`,
		schema,
		`INSERT INTO schema_version (version) VALUES (1)`,
		`INSERT INTO daemons (id, first_seen, last_seen) VALUES ('laptop', '2026-10-07T10:00:00Z', '2026-10-07T10:00:00Z')`,
		`INSERT INTO tasks (id, daemon_id, state, created_at, prompt, system_prompt, model, pause_acknowledge_ns, pause_cleanup_ns)
			VALUES ('old', 'laptop', 'pending', '2026-10-07T10:00:01.5Z', 'p', '', 'haiku', 60000000000, 300000000000)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func TestGivenVersionOneDatabaseWhenOpeningStoreThenItIsMigratedAndItsTasksKeepProgressing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.db")
	createVersionOneDatabase(t, path)

	store, err := OpenStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var version int
	if err := store.db.QueryRowContext(t.Context(), `SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion || schemaVersion != 4 {
		t.Errorf("schema version = %d (server knows %d), want 4", version, schemaVersion)
	}
	if has, err := store.HasOwnerToken(t.Context()); err != nil || has {
		t.Errorf("migrated HasOwnerToken = %v, %v; want false", has, err)
	}
	created := time.Date(2026, 10, 7, 10, 0, 1, 500_000_000, time.UTC)
	if p := readProgress(t, store, "old"); p != (progress{State: TaskPending, LastActivity: created}) {
		t.Errorf("migrated progress = %+v, want pending, last active at creation, no cost", p)
	}

	started := event("old", 1, `{"pid":1}`)
	started.Kind = protocol.KindHarnessStarted
	started.Time = created.Add(time.Minute)
	if _, _, err := store.appendEvents(t.Context(), "laptop", []protocol.Event{started}); err != nil {
		t.Fatal(err)
	}
	if p := readProgress(t, store, "old"); p.State != TaskRunning || !p.LastActivity.Equal(started.Time) {
		t.Errorf("progress after start = %+v", p)
	}
}

func TestGivenMigratedDatabaseWhenComparedWithANewOneThenTheTasksTablesHaveTheSameColumns(t *testing.T) {
	migratedPath := filepath.Join(t.TempDir(), "migrated.db")
	createVersionOneDatabase(t, migratedPath)
	migrated, err := OpenStore(t.Context(), migratedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	fresh, _ := openTestStore(t)

	columns := func(store *Store) []string {
		rows, err := store.db.QueryContext(t.Context(), `SELECT name || ' ' || type || ' ' || "notnull" FROM pragma_table_info('tasks') ORDER BY cid`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var column string
			if err := rows.Scan(&column); err != nil {
				t.Fatal(err)
			}
			out = append(out, column)
		}
		return out
	}
	requireJSONEqual(t, columns(migrated), columns(fresh))
}
